/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The two files the CLI keeps, and the line between them.
//
//   - **The credential file is the machine's**, one per account per machine,
//     in the user's config directory and readable by nobody else. It holds
//     what proves who you are, so it never travels: nothing writes it into a
//     repository, and no command prints what is in it.
//   - **The link file is the working directory's**, `.kitchen/project.json`,
//     and holds no secret at all — the project's name and which installation
//     it is on. It is the whole reason `kitchen deploy` needs no flags, and
//     committing it is a reasonable thing to want: everybody working on the
//     repository deploys the same project.
//
// Both are plain JSON with stable keys, because a CLI that keeps its state in
// a format only it can read is one more thing that has to be driven by hand.

const (
	// linkDir is the directory the link file lives in, at the root of a
	// working copy. It is a directory rather than a dotfile so that anything
	// else the CLI ever needs to keep beside a project has somewhere to go.
	linkDir = ".kitchen"
	// linkFile is where `kitchen link` records the project.
	linkFile = "project.json"
	// credentialFile is what `kitchen login` writes.
	credentialFile = "auth.json"
)

// credentials is every installation this machine has signed in to.
//
// It is keyed by API URL rather than by a name somebody chose, because that is
// the one identifier that is already unique and that every command can derive
// without being told: `--api` and the link file both name it, so a working
// copy pointed at a second installation finds the right credential without
// anybody selecting a context first.
type credentials struct {
	// Current is the installation commands use when nothing else names one.
	Current string `json:"current,omitempty"`
	// Installations is keyed by the API's base URL.
	Installations map[string]*installation `json:"installations,omitempty"`
}

// installation is one platform this machine can talk to.
type installation struct {
	// Issuer is the identity provider `/config.json` named, remembered so a
	// token exchange costs one request rather than two.
	Issuer string `json:"issuer,omitempty"`
	// APIKey is the credential itself: an API key issued by
	// `POST /projects/{name}/keys`, which is exchanged at the issuer for the
	// short-lived token the API actually sees.
	APIKey string `json:"apiKey,omitempty"`
	// Token is the last exchanged token, cached until it expires so that a
	// script running twenty commands exchanges once. It is not a credential
	// anybody has to keep — losing it costs one request.
	Token string `json:"token,omitempty"`
	// TokenExpiresAt is that token's `exp`, read from the token itself.
	TokenExpiresAt *time.Time `json:"tokenExpiresAt,omitempty"`
	// Account is who the credential turned out to belong to, kept so
	// `kitchen whoami` can say so without the network, and so that a stale
	// file is recognisable when somebody opens it.
	Account string `json:"account,omitempty"`
}

// link is what `kitchen link` writes into a working directory.
type link struct {
	// Project is the project every command in this directory is about.
	Project string `json:"project"`
	// API is the installation it lives on. It is written even though the
	// credential file has a current installation, because a working copy
	// belongs to one platform and a shell's idea of "current" does not.
	API string `json:"api,omitempty"`
}

// configHome is where the credential file lives: KITCHEN_CONFIG_HOME if it is
// set (which is how the tests, and anybody sandboxing the CLI, move it),
// otherwise the XDG config directory Go already resolves per platform.
func configHome(getenv func(string) string) (string, error) {
	if override := strings.TrimSpace(getenv("KITCHEN_CONFIG_HOME")); override != "" {
		return override, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fail(codeFailed, "no configuration directory: "+err.Error()).
			withHint("set KITCHEN_CONFIG_HOME to somewhere writable, or pass a token in KITCHEN_TOKEN")
	}
	return filepath.Join(dir, "kitchen"), nil
}

// loadCredentials reads the credential file. A file that is not there is an
// empty set rather than an error: not being signed in is a state, and the
// commands that need one say so themselves in words that name the fix.
func loadCredentials(dir string) (*credentials, error) {
	raw, err := os.ReadFile(filepath.Join(dir, credentialFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &credentials{Installations: map[string]*installation{}}, nil
	case err != nil:
		return nil, fail(codeFailed, "reading the credential file: "+err.Error())
	}

	stored := &credentials{}
	if err := json.Unmarshal(raw, stored); err != nil {
		return nil, fail(codeFailed, fmt.Sprintf("the credential file %s is not readable JSON: %v",
			filepath.Join(dir, credentialFile), err)).
			withHint("delete it and run `kitchen login` again")
	}
	if stored.Installations == nil {
		stored.Installations = map[string]*installation{}
	}
	return stored, nil
}

// saveCredentials writes the credential file, and is the only thing that does.
//
// It writes through a temporary file in the same directory and renames, so a
// crash halfway through leaves the previous credential rather than a truncated
// one — and so two `kitchen` processes racing cannot interleave into a file
// neither of them wrote. The mode is 0600 on the file and 0700 on the
// directory: this is a credential.
func saveCredentials(dir string, stored *credentials) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(codeFailed, "creating the configuration directory: "+err.Error())
	}
	body, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fail(codeFailed, "encoding the credential file: "+err.Error())
	}
	body = append(body, '\n')

	temporary, err := os.CreateTemp(dir, credentialFile+".*")
	if err != nil {
		return fail(codeFailed, "writing the credential file: "+err.Error())
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fail(codeFailed, "writing the credential file: "+err.Error())
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return fail(codeFailed, "writing the credential file: "+err.Error())
	}
	if err := temporary.Close(); err != nil {
		return fail(codeFailed, "writing the credential file: "+err.Error())
	}
	if err := os.Rename(name, filepath.Join(dir, credentialFile)); err != nil {
		return fail(codeFailed, "writing the credential file: "+err.Error())
	}
	return nil
}

// findLink walks up from a directory looking for `.kitchen/project.json`, the
// way every tool that keeps state beside a working copy does — so `kitchen
// deploy` works from a subdirectory of the repository, which is where anybody
// actually is.
//
// It answers the link and the directory it was found in. No link at all is
// (nil, "", nil): the commands that need a project turn that into the sentence
// that names the three ways to supply one.
func findLink(start string) (*link, string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, "", fail(codeFailed, "resolving the working directory: "+err.Error())
	}
	for {
		path := filepath.Join(dir, linkDir, linkFile)
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			found := &link{}
			if err := json.Unmarshal(raw, found); err != nil {
				return nil, "", fail(codeFailed, fmt.Sprintf("%s is not readable JSON: %v", path, err)).
					withHint("run `kitchen link` again to rewrite it")
			}
			if strings.TrimSpace(found.Project) == "" {
				return nil, "", fail(codeNotLinked, path+" names no project").
					withHint("run `kitchen link --project <name>`")
			}
			return found, dir, nil
		case !errors.Is(err, fs.ErrNotExist):
			return nil, "", fail(codeFailed, "reading "+path+": "+err.Error())
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, "", nil
		}
		dir = parent
	}
}

// writeLink records the project in `.kitchen/project.json` under root.
func writeLink(root string, l *link) (string, error) {
	dir := filepath.Join(root, linkDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fail(codeFailed, "creating "+dir+": "+err.Error())
	}
	body, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return "", fail(codeFailed, "encoding the link file: "+err.Error())
	}
	path := filepath.Join(dir, linkFile)
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return "", fail(codeFailed, "writing "+path+": "+err.Error())
	}
	return path, nil
}

// repositoryRoot is where a link file belongs: the root of the working copy if
// this is one, and the current directory if it is not. A repository is linked
// once for everybody in it rather than once per directory somebody happened to
// be standing in.
func repositoryRoot(dir string) string {
	if root, err := gitRoot(dir); err == nil && root != "" {
		return root
	}
	return dir
}

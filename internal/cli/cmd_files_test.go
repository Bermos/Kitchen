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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/utils/ptr"
)

// `kitchen files` — the configuration files a project places (#311).
//
// The cases here are the three things the command promises and the API makes
// possible: one file changes and every other is sent back untouched, a file
// on disk becomes the file in the container, and a secret file's content goes
// up and never comes back down.

const (
	// theConfigFile is the plain file these declare, and secretFile the one
	// whose content is a credential.
	theConfigFile = "configuration"
	secretFile    = "app-ini"
	theConfigPath = "/config/configuration.yaml"
)

// sentFiles is the file list one PATCH of the project carried.
func sentFiles(t *testing.T, h *harness) []fileWrite {
	t.Helper()
	writes := h.platform.sent("PATCH", "/projects/"+testProject)
	if len(writes) != 1 {
		t.Fatalf("wanted one write, got %d", len(writes))
	}
	sent := struct {
		Files []fileWrite `json:"files"`
	}{}
	if err := json.Unmarshal([]byte(writes[0].Body), &sent); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	return sent.Files
}

func TestFilesSetPlacesAFileFromDisk(t *testing.T) {
	const content = "logger: info\n"
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, Role: "admin"}

	if code := h.run("files", "set", theConfigFile,
		"--path", theConfigPath, "--content-file", path, "--workloads", "web,worker", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	files := sentFiles(t, h)
	if len(files) != 1 {
		t.Fatalf("wanted one file, got %+v", files)
	}
	if files[0].Name != theConfigFile || files[0].Path != theConfigPath {
		t.Fatalf("the declaration did not travel: %+v", files[0])
	}
	if files[0].Content == nil || *files[0].Content != content {
		t.Fatalf("the file on disk did not become the file in the container: %+v", files[0])
	}
	if strings.Join(files[0].Workloads, ",") != "web,worker" {
		t.Fatalf("the workloads did not travel: %+v", files[0].Workloads)
	}

	answer := list[configFile]{}
	h.answer(&answer)
	if len(answer.Items) != 1 || answer.Items[0].Name != theConfigFile {
		t.Fatalf("the answer does not name the file: %+v", answer.Items)
	}
}

// The read-modify-write the whole command is built on: one file changes, and
// every other goes back by name with no content at all — which is what lets
// this edit a list holding a secret file it has never been shown.
func TestFilesSetChangesOneAndKeepsTheRest(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, Role: "admin", Files: []configFile{
		{Name: theConfigFile, Path: theConfigPath, Content: ptr.To("logger: warn\n")},
		{Name: secretFile, Path: "/data/conf/app.ini", Secret: true, ContentHash: "abc123", Size: 40},
	}}
	h.platform.files = map[string]string{secretFile: "[server]\nSECRET_KEY = one\n"}

	if code := h.run("files", "set", theConfigFile, "--path", "/etc/app/configuration.yaml", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	files := sentFiles(t, h)
	if len(files) != 2 {
		t.Fatalf("wanted both files back, got %+v", files)
	}
	if files[0].Path != "/etc/app/configuration.yaml" {
		t.Fatalf("the file did not move: %+v", files[0])
	}
	if files[0].Content != nil {
		t.Fatalf("a file whose content was not being changed carried one: %+v", files[0])
	}
	if files[1].Name != secretFile || !files[1].Secret || files[1].Content != nil {
		t.Fatalf("the secret file was not sent back as it came: %+v", files[1])
	}
	// The platform still holds it, which is the thing a lost `content` would
	// have quietly emptied.
	if h.platform.files[secretFile] == "" {
		t.Fatalf("the secret file's content was lost by a write that never saw it")
	}
	if strings.Contains(h.stdout.String(), "SECRET_KEY") {
		t.Fatalf("the answer carried a secret file's content: %s", h.stdout.String())
	}
}

// A secret file is two requests, in one order: the declaration, then the
// content on its own route. What comes back is a digest and never the file.
func TestFilesSetWritesASecretFilesContentOnItsOwnRoute(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, Role: "admin"}

	if code := h.run("files", "set", secretFile, "--path", "/data/conf/app.ini",
		"--secret", "--content", "[server]\nSECRET_KEY = hunter2\n", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	files := sentFiles(t, h)
	if len(files) != 1 || !files[0].Secret {
		t.Fatalf("the file was not declared secret: %+v", files)
	}
	if files[0].Content != nil {
		t.Fatalf("a secret file's content travelled with its declaration: %+v", files[0])
	}

	writes := h.platform.sent("PUT", "/projects/"+testProject+"/files/"+secretFile)
	if len(writes) != 1 {
		t.Fatalf("wanted one content write, got %d", len(writes))
	}
	if !strings.Contains(writes[0].Body, "hunter2") {
		t.Fatalf("the content did not travel: %s", writes[0].Body)
	}
	if strings.Contains(h.stdout.String(), "hunter2") {
		t.Fatalf("the answer echoed the content: %s", h.stdout.String())
	}
	answer := list[configFile]{}
	h.answer(&answer)
	if len(answer.Items) != 1 || answer.Items[0].ContentHash == "" || answer.Items[0].Content != nil {
		t.Fatalf("the answer says nothing about what was stored, or too much: %+v", answer.Items)
	}
}

// Every question has a flag that answers it, and a question with nobody to
// answer it is a failure naming the flag rather than a wait.
func TestFilesSetRefusesWhatItCannotWorkOut(t *testing.T) {
	for name, spec := range map[string]struct {
		args     []string
		mentions string
	}{
		"a new file with no path": {
			[]string{"files", "set", theConfigFile, "--content", "a", "--json"}, "--path",
		},
		"a new file with no content": {
			[]string{"files", "set", theConfigFile, "--path", theConfigPath, "--json"}, "--content-file",
		},
		"a secret file with no content": {
			[]string{"files", "set", secretFile, "--path", "/data/app.ini", "--secret", "--json"}, "--content-file",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.env["KITCHEN_PROJECT"] = testProject
			h.platform.project = &project{Name: testProject, Role: "admin"}

			if code := h.run(spec.args...); code != exitUsage {
				t.Fatalf("exit %d, wanted %d: %s", code, exitUsage, h.stderr.String())
			}
			if writes := h.platform.sent("PATCH", "/projects/"+testProject); len(writes) != 0 {
				t.Fatalf("a command that could not work out what to write wrote anyway")
			}
			if !strings.Contains(h.stdout.String(), spec.mentions) {
				t.Fatalf("the failure does not name a flag that answers it: %s", h.stdout.String())
			}
		})
	}
}

func TestFilesRemoveSendsTheListWithoutIt(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, Role: "admin", Files: []configFile{
		{Name: theConfigFile, Path: theConfigPath, Content: ptr.To("logger: warn\n")},
		{Name: secretFile, Path: "/data/conf/app.ini", Secret: true, ContentHash: "abc123"},
	}}
	h.platform.files = map[string]string{secretFile: "[server]\nSECRET_KEY = one\n"}

	if code := h.run("files", "rm", secretFile, "--yes", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	files := sentFiles(t, h)
	if len(files) != 1 || files[0].Name != theConfigFile {
		t.Fatalf("the list still names the removed file: %+v", files)
	}
	// The declaration going takes the content with it, which is what the
	// platform does after the write lands.
	if _, held := h.platform.files[secretFile]; held {
		t.Fatalf("the content outlived the declaration")
	}

	// A file the project does not place is a refusal rather than a write of
	// the list it already has.
	if code := h.run("files", "rm", "nowhere", "--yes", "--json"); code != exitNotFound {
		t.Fatalf("exit %d, wanted %d: %s", code, exitNotFound, h.stderr.String())
	}
}

func TestFilesListPrintsWhatThePlatformWillSay(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, Role: "admin", Files: []configFile{
		{Name: theConfigFile, Path: theConfigPath, Content: ptr.To("logger: warn\n")},
		{Name: secretFile, Path: "/data/conf/app.ini", Secret: true},
	}}

	if code := h.run("files", "list", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	answer := list[configFile]{}
	h.answer(&answer)
	if len(answer.Items) != 2 {
		t.Fatalf("the listing does not name both files: %+v", answer.Items)
	}
	if answer.Items[1].ContentHash != "" {
		t.Fatalf("a secret file with no content yet should say so: %+v", answer.Items[1])
	}
}

// A file with no path is placed in no container: it exists to be seeded into a
// volume by a workload's init (#348), and a mounted config file is read-only,
// so one mounted where the seed writes would shadow the copy the application
// then owns. The command needs a way to say that, since a new file otherwise
// has to have a path.
func TestFilesSetPlacesAFileInNoContainer(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject}

	if code := h.run("files", "set", "configuration", "--no-path", "--content", "logger: info\n", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	sent := sentFiles(t, h)
	if len(sent) != 1 || sent[0].Name != "configuration" {
		t.Fatalf("the file did not go: %+v", sent)
	}
	if sent[0].Path != "" {
		t.Fatalf("the file was given a path: %+v", sent[0])
	}
}

func TestFilesSetRefusesBothAPathAndNone(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject}

	if code := h.run("files", "set", "configuration", "--no-path", "--path", "/config/app.yaml",
		"--content", "x", "--json"); code != exitUsage {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
}

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

// Package repoconfig reads a repository's kitchen.json: the file a project
// declares its own build and runtime settings in, committed beside the code
// those settings describe.
//
// It is the third way to configure a project, after the dashboard and the
// REST API, and the only one that travels with the commit. That is the whole
// of what it is for. A build of last week's commit builds it the way last
// week's commit asked to be built; a pull request that adds a worker adds it
// in the same change that adds the worker's code; and a project moved to
// another installation arrives configured.
//
// # What it may not do
//
// The file is in a repository, and a preview builds a commit from a pull
// request — which anybody who can open one can write. So the file's reach is
// exactly the settings that describe the code, and stops at everything that
// describes the project's standing in the platform:
//
//   - it declares no credential, and cannot point a variable at a Secret or a
//     resource claim; only literal values, which a committed file makes
//     public whether or not the platform agrees;
//   - it cannot shadow a variable the project binds to a credential, which
//     would redirect a database URL by opening a pull request;
//   - it says nothing about criticality, data class, access, promotion
//     stages, preview protection or whether previews are published at all.
//     Those are the project's owners' and the operator's, and a repository
//     arguing about them is the argument this package exists to refuse;
//   - it cannot set the build's root directory, which is how the platform
//     found the file — see [v1alpha1.RepoConfigFileName].
//
// # What a bad file does
//
// It fails the build, with the line the reader has to fix, and it fails it
// before anything is scheduled. A file that is unreadable JSON, names a field
// the platform has never heard of, or asks for something refused above is not
// built around: a setting silently ignored is the failure mode this feature
// would otherwise introduce, since the whole point is that committing a
// change to the file changes the deploy.
//
// The one thing that is not final is the repository being unreadable — the
// provider being down, a token that stopped working. That comes back as
// [detect.ErrSourceUnreadable], for the same reason detection does: the
// commit did nothing wrong, so its build waits.
package repoconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/detect"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// FileName is the file this package reads, at the project's build root.
const FileName = kitchenv1alpha1.RepoConfigFileName

// SchemaURL is where the JSON Schema for the file is published, and the value
// an editor needs in `$schema` to offer completion over it. The file may name
// any schema it likes — or none — and the platform reads the key only to
// ignore it: what the platform accepts is decided here, not by a document
// fetched from the internet at build time.
const SchemaURL = "https://raw.githubusercontent.com/Bermos/Kitchen/main/docs/schemas/kitchen.schema.json"

// ErrInvalid is a kitchen.json that was read and is wrong: bad JSON, a field
// nothing recognises, a value out of range, or a declaration the file is not
// allowed to make. It is final — the same commit will not parse differently
// on the next attempt — so a build that meets it fails rather than waits.
var ErrInvalid = errors.New("kitchen.json is not valid")

// ErrSourceUnreadable is the repository not being readable right now. It is
// detection's, deliberately: the two read the same repository through the
// same provider, and a build that waits for one waits for the other.
var ErrSourceUnreadable = detect.ErrSourceUnreadable

// Target names the file to read: the repository, the commit, and the
// project's build root within it.
type Target struct {
	Repo string
	Ref  string
	// RootDirectory is the project's build root, relative to the repository
	// root and empty for the root itself — the same value detection lists.
	RootDirectory string
}

// Path is where the file would be, relative to the repository root.
func (t Target) Path() string {
	return path.Join(t.RootDirectory, FileName)
}

// Read reads and validates the repository's kitchen.json.
//
// A repository without one is the ordinary case and not a failure: it returns
// (nil, nil), and the project is configured entirely by the dashboard as it
// was before this existed.
//
// It costs one request, made whatever the build strategy is — including
// `dockerfile`, which reads nothing else. That is deliberate: a Dockerfile
// project has a runtime to describe like any other, and a file that worked on
// two of the three strategies would be a worse surface than none.
func Read(ctx context.Context, reader gitprovider.SourceReader, target Target) (*kitchenv1alpha1.RepoConfig, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: the project's git provider cannot read a repository's contents", ErrSourceUnreadable)
	}
	filePath := target.Path()
	raw, err := reader.ReadFile(ctx, target.Repo, target.Ref, filePath)
	switch {
	case errors.Is(err, gitprovider.ErrFileNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrSourceUnreadable, err)
	}

	config, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", filePath, detect.ShortRef(target.Ref), err)
	}
	config.Path = filePath
	return config, nil
}

// Parse turns the bytes of a kitchen.json into what the platform stores,
// refusing anything it cannot act on exactly.
//
// It is exported because three callers parse without reading: the CLI's
// `kitchen config check`, which validates a working copy before it is pushed;
// the API's preflight, which answers the new-project form; and the tests.
func Parse(raw []byte) (*kitchenv1alpha1.RepoConfig, error) {
	var file File
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	// A field nothing recognises is refused rather than dropped. The file
	// exists so that changing it changes the deploy, and a typo'd key that
	// silently did nothing would make that untrue in the one case where it
	// matters most — the case where somebody is watching for the change and
	// does not get it.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, readableJSONError(err))
	}
	if decoder.More() {
		return nil, fmt.Errorf("%w: it holds more than one JSON document", ErrInvalid)
	}
	return file.config()
}

// readableJSONError turns encoding/json's messages into ones that name what
// to do. Its unknown-field error in particular reads as a fact about a Go
// struct, and the reader of a kitchen.json has never seen one.
func readableJSONError(err error) string {
	message := err.Error()
	if field, found := strings.CutPrefix(message, "json: unknown field "); found {
		return fmt.Sprintf("it sets %s, which the platform does not recognise — "+
			"see the schema at %s for what it takes", field, SchemaURL)
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("it is not valid JSON (at byte %d): %s", syntax.Offset, syntax.Error())
	}
	var unmarshal *json.UnmarshalTypeError
	if errors.As(err, &unmarshal) {
		where := unmarshal.Field
		if where == "" {
			where = "the document"
		}
		return fmt.Sprintf("%s is a %s, and the platform expects %s", where, unmarshal.Value, unmarshal.Type)
	}
	return message
}

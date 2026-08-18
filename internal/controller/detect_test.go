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

package controller

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// fakeSource is a git provider standing in for a repository: a set of file
// paths with contents, which it lists and reads the way the GitHub provider
// reads the contents API.
type fakeSource struct {
	// files are paths relative to the repository root, with their contents.
	files map[string]string
	// err, when set, is what every read fails with — a provider that is
	// down, rather than a repository that says nothing.
	err error
}

// repoWithDockerfile is what most of the suite's projects are: a repository
// that says how to build itself, which is what every build assumed before
// detection existed.
func repoWithDockerfile() *fakeSource {
	return &fakeSource{files: map[string]string{"Dockerfile": "FROM scratch\n"}}
}

func (f *fakeSource) EnsureWebhook(context.Context, string, gitprovider.WebhookSpec) (string, error) {
	return "1", nil
}

func (f *fakeSource) DeleteWebhook(context.Context, string, string) error { return nil }

func (f *fakeSource) ListDir(_ context.Context, _, _, dir string) ([]gitprovider.DirEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	dir = strings.Trim(dir, "/")

	seen := map[string]gitprovider.DirEntry{}
	for file := range f.files {
		if dir != "" && !strings.HasPrefix(file, dir+"/") {
			continue
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(file, dir), "/")
		name, _, nested := strings.Cut(rest, "/")
		if name == "" {
			continue
		}
		seen[name] = gitprovider.DirEntry{Name: name, Dir: nested}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("%w: %s", gitprovider.ErrFileNotFound, dir)
	}

	entries := make([]gitprovider.DirEntry, 0, len(seen))
	for _, entry := range seen {
		entries = append(entries, entry)
	}
	return entries, nil
}

func (f *fakeSource) ReadFile(_ context.Context, _, _, filePath string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	content, ok := f.files[path.Clean(filePath)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", gitprovider.ErrFileNotFound, filePath)
	}
	return []byte(content), nil
}

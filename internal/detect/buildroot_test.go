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

package detect

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// treeReader is a repository as a list of file paths, which is all detection
// ever asks a provider for: what is in a directory, and the contents of a
// file it saw there.
type treeReader struct {
	files []string
	// listed records every directory the reader was asked about, so a test
	// can say where detection looked as well as what it concluded.
	listed []string
}

func (t *treeReader) ListDir(_ context.Context, _, _, dir string) ([]gitprovider.DirEntry, error) {
	dir = strings.Trim(dir, "/")
	t.listed = append(t.listed, dir)

	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	seen := map[string]bool{}
	entries := []gitprovider.DirEntry{}
	for _, file := range t.files {
		rest, inside := strings.CutPrefix(file, prefix)
		if !inside || rest == "" {
			continue
		}
		name, _, nested := strings.Cut(rest, "/")
		if seen[name] {
			continue
		}
		seen[name] = true
		entries = append(entries, gitprovider.DirEntry{Name: name, Dir: nested})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: %s", gitprovider.ErrFileNotFound, dir)
	}
	return entries, nil
}

func (t *treeReader) ReadFile(_ context.Context, _, _, file string) ([]byte, error) {
	if slices.Contains(t.files, file) {
		return []byte("{}"), nil
	}
	return nil, fmt.Errorf("%w: %s", gitprovider.ErrFileNotFound, file)
}

func TestNormalizingTheBuildRoot(t *testing.T) {
	// Four spellings of one directory, and four of the repository itself.
	// The CRD's default is "." and a form field is whatever somebody typed,
	// so the build, detection and the preflight only agree if they are all
	// asking about the same string.
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{".", ""},
		{"./", ""},
		{"  /  ", ""},
		{"apps/shop", "apps/shop"},
		{"apps/shop/", "apps/shop"},
		{"/apps/shop", "apps/shop"},
		{"./apps/shop", "apps/shop"},
		{" apps/shop ", "apps/shop"},
		{"apps//shop", "apps/shop"},
	} {
		if got := NormalizeRoot(tc.in); got != tc.want {
			t.Errorf("NormalizeRoot(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizingTheDockerfilePath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", DefaultDockerfile},
		{"   ", DefaultDockerfile},
		{"Dockerfile", "Dockerfile"},
		{"./Dockerfile", "Dockerfile"},
		{"docker//prod.Dockerfile", "docker/prod.Dockerfile"},
		// Left alone rather than repaired: a path out of the build root is
		// refused where it is written, and repairing it here would hide it.
		{"../shared/Dockerfile", "../shared/Dockerfile"},
	} {
		if got := NormalizeDockerfile(tc.in); got != tc.want {
			t.Errorf("NormalizeDockerfile(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAPathThatLeavesTheBuildRoot(t *testing.T) {
	for _, in := range []string{"..", "../Dockerfile", "../../etc/Dockerfile", "/Dockerfile", "docker/../../up"} {
		if !LeavesRoot(in) {
			t.Errorf("LeavesRoot(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"", "Dockerfile", "./Dockerfile", "docker/prod.Dockerfile", "docker/../Dockerfile"} {
		if LeavesRoot(in) {
			t.Errorf("LeavesRoot(%q) = true, want false", in)
		}
	}
}

// A monorepo is the shape that sets both fields, and the two settings are
// only meaningful together: the Dockerfile path is relative to the root
// directory, so detection has to look for it under the root and nowhere else.
func TestAProjectThatSetsBothARootDirectoryAndADockerfilePath(t *testing.T) {
	reader := &treeReader{files: []string{
		"README.md",
		"Dockerfile",
		"apps/shop/package.json",
		"apps/shop/docker/prod.Dockerfile",
	}}

	// Spelled two ways, because "./docker/prod.Dockerfile" and
	// "docker/prod.Dockerfile" are one file and a build spells it one way.
	for _, declared := range []string{"docker/prod.Dockerfile", "./docker/prod.Dockerfile"} {
		reader.listed = nil
		signals, err := Signals(context.Background(), reader, Target{
			Repo: "acme/monorepo", Ref: "main",
			RootDirectory: "apps/shop", DockerfilePath: declared,
			ConsiderDockerfile: true,
		})
		if err != nil {
			t.Fatalf("detecting %q: %v", declared, err)
		}
		if !signals.Dockerfile {
			t.Errorf("%q under apps/shop was not found", declared)
		}
		// Under the build root, never beside the repository's own Dockerfile
		// at the top — which is a different project's, or nobody's.
		if !slices.Contains(reader.listed, "apps/shop/docker") {
			t.Errorf("detection looked in %v, not in apps/shop/docker", reader.listed)
		}
	}
}

func TestADockerfileAboveTheBuildRootIsNotThisProjects(t *testing.T) {
	// The file exists, one level up. No build can read it: BuildKit is handed
	// the build root as its whole context and the lifecycle is pointed at it,
	// so detection reporting it present would promise a build that cannot be
	// run.
	reader := &treeReader{files: []string{
		"docker/prod.Dockerfile",
		"apps/shop/package.json",
	}}

	signals, err := Signals(context.Background(), reader, Target{
		Repo: "acme/monorepo", Ref: "main",
		RootDirectory: "apps/shop", DockerfilePath: "../../docker/prod.Dockerfile",
		ConsiderDockerfile: true,
	})
	if err != nil {
		t.Fatalf("detecting: %v", err)
	}
	if signals.Dockerfile {
		t.Error("a Dockerfile above the build root was reported as this project's")
	}
}

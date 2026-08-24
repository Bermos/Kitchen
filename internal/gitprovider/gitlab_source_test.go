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

package gitprovider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const gitlabTreePath = "/projects/acme%2Fshop/repository/tree"

func TestGitLabListDirReadsTheRepositoryRoot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != gitlabTreePath {
			t.Errorf("unexpected path %q", r.URL.EscapedPath())
		}
		if got := r.URL.Query().Get("ref"); got != "deadbeefcafe" {
			t.Errorf("listed ref %q, want the commit under build", got)
		}
		if _, ok := r.URL.Query()["path"]; ok {
			t.Errorf("the root was listed with a path: %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[
			{"name": "package.json", "type": "blob"},
			{"name": "src", "type": "tree"}
		]`))
	}))
	defer server.Close()

	gl := &GitLab{APIURL: server.URL, Token: "tok"}
	entries, err := gl.ListDir(context.Background(), "acme/shop", "deadbeefcafe", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []DirEntry{{Name: "package.json"}, {Name: "src", Dir: true}}
	if len(entries) != len(want) {
		t.Fatalf("listed %v, want %v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

func TestGitLabListDirAnEmptySubdirectoryIsAbsent(t *testing.T) {
	// GitLab answers 200 with [] for a path that is not there, which
	// detection would otherwise read as a directory holding nothing.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("path"); got != "apps/web" {
			t.Errorf("listed path %q", got)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	gl := &GitLab{APIURL: server.URL, Token: "tok"}
	_, err := gl.ListDir(context.Background(), "acme/shop", "main", "/apps/web/")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected the directory to read as absent, got %v", err)
	}
}

func TestGitLabReadFileEscapesTheWholePath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/projects/acme%2Fshop/repository/files/apps%2Fweb%2Fpackage.json/raw" {
			t.Errorf("unexpected path %q", r.URL.EscapedPath())
		}
		if r.Header.Get("PRIVATE-TOKEN") != "tok" {
			t.Errorf("the credential did not reach the request")
		}
		_, _ = w.Write([]byte(`{"name":"shop"}`))
	}))
	defer server.Close()

	gl := &GitLab{APIURL: server.URL, Token: "tok"}
	content, err := gl.ReadFile(context.Background(), "acme/shop", "main", "apps/web/package.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != `{"name":"shop"}` {
		t.Errorf("read %q", content)
	}
}

func TestGitLabReadFileMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gl := &GitLab{APIURL: server.URL, Token: "tok"}
	_, err := gl.ReadFile(context.Background(), "acme/shop", "main", "Dockerfile")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected ErrFileNotFound, got %v", err)
	}
}

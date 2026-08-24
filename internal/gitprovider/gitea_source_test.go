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

func TestGiteaListDirReadsTheRepositoryRoot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/shop/contents" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("ref"); got != detectRef {
			t.Errorf("listed ref %q, want the commit under build", got)
		}
		_, _ = w.Write([]byte(`[
			{"name": "go.mod", "type": "file"},
			{"name": "cmd", "type": "dir"}
		]`))
	}))
	defer server.Close()

	gt := &Gitea{APIURL: server.URL, Token: "tok"}
	entries, err := gt.ListDir(context.Background(), "acme/shop", detectRef, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []DirEntry{{Name: "go.mod"}, {Name: "cmd", Dir: true}}
	if len(entries) != len(want) {
		t.Fatalf("listed %v, want %v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

func TestGiteaListDirMissingDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gt := &Gitea{APIURL: server.URL, Token: "tok"}
	_, err := gt.ListDir(context.Background(), "acme/shop", "main", "apps/web")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected ErrFileNotFound, got %v", err)
	}
}

func TestGiteaReadFileServesTheRawEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/repos/acme/shop/raw/apps/web%20app/go.mod" {
			t.Errorf("unexpected path %q", r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "token tok" {
			t.Errorf("the credential did not reach the request")
		}
		_, _ = w.Write([]byte("module shop\n"))
	}))
	defer server.Close()

	gt := &Gitea{APIURL: server.URL, Token: "tok"}
	content, err := gt.ReadFile(context.Background(), "acme/shop", "main", "/apps/web app/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "module shop\n" {
		t.Errorf("read %q", content)
	}
}

func TestGiteaReadFileMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gt := &Gitea{APIURL: server.URL, Token: "tok"}
	_, err := gt.ReadFile(context.Background(), "acme/shop", "main", "Dockerfile")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected ErrFileNotFound, got %v", err)
	}
}

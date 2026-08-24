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
	"strings"
	"testing"
)

// detectRef is the commit under build that every source-reading test lists
// at: detection never asks for a branch, only for the revision it was handed.
const detectRef = "deadbeefcafe"

func TestListDirReadsTheRepositoryRoot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/shop/contents" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("ref"); got != detectRef {
			t.Errorf("listed ref %q, want the commit under build", got)
		}
		_, _ = w.Write([]byte(`[
			{"name": "package.json", "type": "file"},
			{"name": "src", "type": "dir"}
		]`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	entries, err := gh.ListDir(context.Background(), "acme/shop", detectRef, "")
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

func TestListDirEscapesTheSubdirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/repos/acme/shop/contents/apps/web%20app" {
			t.Errorf("unexpected path %q", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	if _, err := gh.ListDir(context.Background(), "acme/shop", "main", "/apps/web app/"); err != nil {
		t.Fatal(err)
	}
}

func TestListDirMissingDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	_, err := gh.ListDir(context.Background(), "acme/shop", "main", "apps/web")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("listing a missing directory returned %v, want ErrFileNotFound", err)
	}
}

func TestReadFileTakesTheRawMediaType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github.raw" {
			t.Errorf("Accept header %q, want the raw media type", got)
		}
		if r.URL.Path != "/repos/acme/shop/contents/package.json" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"name":"shop"}`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	body, err := gh.ReadFile(context.Background(), "acme/shop", "main", "package.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"name":"shop"}` {
		t.Errorf("read %q", body)
	}
}

func TestReadFileMissingFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	_, err := gh.ReadFile(context.Background(), "acme/shop", "main", "Dockerfile")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("reading a missing file returned %v, want ErrFileNotFound", err)
	}
}

// A repository can commit a file of any size under a name detection looks
// for; what comes back is capped rather than trusted.
func TestReadFileIsCapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", maxSourceFileBytes+4096)))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	body, err := gh.ReadFile(context.Background(), "acme/shop", "main", "package.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != maxSourceFileBytes {
		t.Errorf("read %d bytes, want the %d-byte cap", len(body), maxSourceFileBytes)
	}
}

// The GitHub provider is a source reader; a provider that is not stays out of
// detection rather than failing it.
func TestSourceNarrowsTheProvider(t *testing.T) {
	if _, ok := Source(&GitHub{}); !ok {
		t.Error("the GitHub provider does not read source")
	}
	if _, ok := Source(providerWithoutSource{}); ok {
		t.Error("a provider that cannot read source was narrowed to one")
	}
}

type providerWithoutSource struct{}

func (providerWithoutSource) EnsureWebhook(context.Context, string, WebhookSpec) (string, error) {
	return "", nil
}
func (providerWithoutSource) DeleteWebhook(context.Context, string, string) error { return nil }

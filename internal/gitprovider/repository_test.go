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

// The probe every provider gained so that a repository a credential cannot
// read stops looking like a path that is not in one it can.

// oneRepository serves a single repository at `path` and answers 404 for
// everything else, which is what a provider does for a repository a token may
// not know about.
func oneRepository(t *testing.T, path, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath rather than Path: GitLab's project id is one segment
		// with the slash escaped inside it, and Path has already undone that.
		if r.URL.EscapedPath() != path {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message": "Not Found"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestGitHubProbesARepository(t *testing.T) {
	server := oneRepository(t, "/repos/acme/shop",
		`{"full_name": "acme/shop", "default_branch": "main", "private": true, "description": "the shop"}`)
	gh := &GitHub{APIURL: server.URL, Token: "tok"}

	found, err := gh.Repository(context.Background(), "acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	want := Repository{FullName: "acme/shop", DefaultBranch: "main", Private: true, Description: "the shop"}
	if found != want {
		t.Fatalf("read %+v, want %+v", found, want)
	}

	// The 404 GitHub answers for a repository a token cannot see is the same
	// 404 it answers for one that does not exist, on purpose. Both are this
	// error, and neither is a path inside a repository.
	if _, err := gh.Repository(context.Background(), "acme/private"); !errors.Is(err, ErrRepositoryNotFound) {
		t.Fatalf("a repository the token cannot see gave %v", err)
	}
}

func TestGiteaProbesARepository(t *testing.T) {
	server := oneRepository(t, "/repos/acme/shop",
		`{"full_name": "acme/shop", "default_branch": "trunk", "private": false}`)
	gitea := &Gitea{APIURL: server.URL, Token: "tok"}

	found, err := gitea.Repository(context.Background(), "acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	if found.FullName != "acme/shop" || found.DefaultBranch != "trunk" {
		t.Fatalf("read %+v", found)
	}
	if _, err := gitea.Repository(context.Background(), "acme/private"); !errors.Is(err, ErrRepositoryNotFound) {
		t.Fatalf("a repository the token cannot see gave %v", err)
	}
}

func TestGitLabProbesAProject(t *testing.T) {
	// GitLab addresses a project as one escaped path segment, slashes
	// included, which is the whole of why this is not "/projects/acme/shop".
	server := oneRepository(t, "/projects/acme%2Fshop",
		`{"path_with_namespace": "acme/shop", "default_branch": "main", "visibility": "internal"}`)
	gl := &GitLab{APIURL: server.URL, Token: "tok"}

	found, err := gl.Repository(context.Background(), "acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	if found.FullName != "acme/shop" || found.DefaultBranch != "main" {
		t.Fatalf("read %+v", found)
	}
	// Anything short of public is visible only to somebody the project
	// admits, which is what the platform means by private.
	if !found.Private {
		t.Fatalf("an internal project read as public: %+v", found)
	}
	if _, err := gl.Repository(context.Background(), "acme/private"); !errors.Is(err, ErrRepositoryNotFound) {
		t.Fatalf("a project the token cannot see gave %v", err)
	}
}

func TestProbeNarrowsAProvider(t *testing.T) {
	for _, provider := range []Provider{&GitHub{}, &Gitea{}, &GitLab{}} {
		if _, ok := Probe(provider); !ok {
			t.Errorf("%T cannot be asked about a repository", provider)
		}
	}
	// A provider that only reads contents is not a failure: the caller keeps
	// the ambiguous message rather than gaining a wrong one.
	if _, ok := Probe(struct{}{}); ok {
		t.Error("something that answers no questions about repositories was accepted as a probe")
	}
}

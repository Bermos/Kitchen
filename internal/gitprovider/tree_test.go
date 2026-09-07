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

// The tree object every recorded answer below names for services/api, and the
// root tree of the commit. They are the shape a real provider answers with —
// a 40-character hex object id — because the whole comparison is string
// equality and a fixture that was not one would prove nothing.
const (
	apiTreeObject  = "6f3c1a9d0b7e4f52a8c1d3e5b7092f4a6c8d1e30"
	rootTreeObject = "1c2b3a4958677685949302a1b0c9d8e7f6051423"
)

func TestSplitTreePathReadsTheBuildRootTheWayABuildDoes(t *testing.T) {
	for _, tc := range []struct {
		dir            string
		parent, expect string
	}{
		{"", "", ""},
		{".", "", ""},
		{"./", "", ""},
		{"/", "", ""},
		{"services", "", "services"},
		{"services/api", "services", "api"},
		{"/services/api/", "services", "api"},
		{"./services/api", "services", "api"},
		{"apps/shop/web", "apps/shop", "web"},
	} {
		parent, name := splitTreePath(tc.dir)
		if parent != tc.parent || name != tc.expect {
			t.Errorf("splitTreePath(%q) = (%q, %q), want (%q, %q)", tc.dir, parent, name, tc.parent, tc.expect)
		}
	}
}

func TestGitHubResolvesTheTreeOfASubdirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/shop/contents/services" {
			t.Errorf("listed %q, want the parent of the build root", r.URL.Path)
		}
		if got := r.URL.Query().Get("ref"); got != detectRef {
			t.Errorf("listed ref %q, want the commit under build", got)
		}
		_, _ = w.Write([]byte(`[
			{"name": "README.md", "type": "file", "sha": "aaaa"},
			{"name": "api", "type": "dir", "sha": "` + apiTreeObject + `"},
			{"name": "web", "type": "dir", "sha": "bbbb"}
		]`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	object, err := gh.TreeAt(context.Background(), "acme/shop", detectRef, "services/api")
	if err != nil {
		t.Fatal(err)
	}
	if object != apiTreeObject {
		t.Errorf("tree object %q, want %q", object, apiTreeObject)
	}
}

func TestGitHubResolvesTheRootTreeFromTheCommit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/shop/commits/"+detectRef {
			t.Errorf("asked for %q, want the commit", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"sha": "` + detectRef + `", "commit": {"tree": {"sha": "` + rootTreeObject + `"}}}`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	for _, root := range []string{"", ".", "/"} {
		object, err := gh.TreeAt(context.Background(), "acme/shop", detectRef, root)
		if err != nil {
			t.Fatalf("root %q: %v", root, err)
		}
		if object != rootTreeObject {
			t.Errorf("root %q: tree object %q, want %q", root, object, rootTreeObject)
		}
	}
}

// A build root that is not a directory at this commit — deleted, or a file by
// that name — is an answer rather than a failure: there is nothing to compare
// and the commit is built.
func TestGitHubReportsABuildRootThatIsNotThere(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"name": "api", "type": "file", "sha": "cccc"}]`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	if _, err := gh.TreeAt(context.Background(), "acme/shop", detectRef, "services/api"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("a file where a directory was expected answered %v, want ErrFileNotFound", err)
	}

	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer missing.Close()
	gone := &GitHub{APIURL: missing.URL, Token: "tok"}
	if _, err := gone.TreeAt(context.Background(), "acme/shop", detectRef, "services/api"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("a parent that is not there answered %v, want ErrFileNotFound", err)
	}
}

func TestGiteaResolvesTheTreeOfASubdirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/shop/contents/services" {
			t.Errorf("listed %q, want the parent of the build root", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
			{"name": "api", "type": "dir", "sha": "` + apiTreeObject + `"},
			{"name": "docker-compose.yml", "type": "file", "sha": "dddd"}
		]`))
	}))
	defer server.Close()

	gitea := &Gitea{APIURL: server.URL, Token: "tok"}
	object, err := gitea.TreeAt(context.Background(), "acme/shop", detectRef, "services/api")
	if err != nil {
		t.Fatal(err)
	}
	if object != apiTreeObject {
		t.Errorf("tree object %q, want %q", object, apiTreeObject)
	}
}

func TestGiteaResolvesTheRootTreeFromTheCommit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/shop/git/commits/"+detectRef {
			t.Errorf("asked for %q, want the git data API's commit", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"sha": "` + detectRef + `", "commit": {"tree": {"sha": "` + rootTreeObject + `"}}}`))
	}))
	defer server.Close()

	gitea := &Gitea{APIURL: server.URL, Token: "tok"}
	object, err := gitea.TreeAt(context.Background(), "acme/shop", detectRef, "")
	if err != nil {
		t.Fatal(err)
	}
	if object != rootTreeObject {
		t.Errorf("tree object %q, want %q", object, rootTreeObject)
	}
}

func TestGitLabResolvesTheTreeOfASubdirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/projects/acme%2Fshop/repository/tree" {
			t.Errorf("listed %q, want the repository tree", r.URL.EscapedPath())
		}
		if got := r.URL.Query().Get("path"); got != "services" {
			t.Errorf("listed path %q, want the parent of the build root", got)
		}
		if got := r.URL.Query().Get("ref"); got != detectRef {
			t.Errorf("listed ref %q, want the commit under build", got)
		}
		_, _ = w.Write([]byte(`[
			{"id": "eeee", "name": "README.md", "type": "blob"},
			{"id": "` + apiTreeObject + `", "name": "api", "type": "tree"}
		]`))
	}))
	defer server.Close()

	gl := &GitLab{APIURL: server.URL, Token: "tok"}
	object, err := gl.TreeAt(context.Background(), "acme/shop", detectRef, "services/api")
	if err != nil {
		t.Fatal(err)
	}
	if object != apiTreeObject {
		t.Errorf("tree object %q, want %q", object, apiTreeObject)
	}
}

// GitLab's REST API names the id of everything inside a tree and never the id
// of a commit's root tree, so a project whose build root is the whole
// repository has nothing to compare. It says so rather than deriving
// something, and the caller builds.
func TestGitLabCannotNameACommitsRootTree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("asked the provider for %q; the root tree needs no request", r.URL.Path)
	}))
	defer server.Close()

	gl := &GitLab{APIURL: server.URL, Token: "tok"}
	if _, err := gl.TreeAt(context.Background(), "acme/shop", detectRef, "."); !errors.Is(err, ErrTreeUnavailable) {
		t.Fatalf("the repository root answered %v, want ErrTreeUnavailable", err)
	}
}

// The tree endpoint answers 200 with [] for a path that is not there, which
// is why the absence is read off the listing rather than off a status code.
func TestGitLabReportsABuildRootThatIsNotThere(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	gl := &GitLab{APIURL: server.URL, Token: "tok"}
	if _, err := gl.TreeAt(context.Background(), "acme/shop", detectRef, "services/api"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("an empty listing answered %v, want ErrFileNotFound", err)
	}
}

// A parent directory with more siblings than fit on one page still finds the
// build root: the walk pages until it does or the pages run short.
func TestGitLabPagesThroughALargeParentDirectory(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("page") == "1" {
			entries := make([]byte, 0, 4096)
			entries = append(entries, '[')
			for i := range treePageSize {
				if i > 0 {
					entries = append(entries, ',')
				}
				entries = append(entries, []byte(`{"id":"f00d","name":"filler`+string(rune('a'+i%26))+string(rune('a'+i/26))+`","type":"tree"}`)...)
			}
			entries = append(entries, ']')
			_, _ = w.Write(entries)
			return
		}
		_, _ = w.Write([]byte(`[{"id": "` + apiTreeObject + `", "name": "api", "type": "tree"}]`))
	}))
	defer server.Close()

	gl := &GitLab{APIURL: server.URL, Token: "tok"}
	object, err := gl.TreeAt(context.Background(), "acme/shop", detectRef, "services/api")
	if err != nil {
		t.Fatal(err)
	}
	if object != apiTreeObject {
		t.Errorf("tree object %q, want %q", object, apiTreeObject)
	}
	if pages != 2 {
		t.Errorf("asked for %d pages, want 2", pages)
	}
}

// Every built-in provider resolves a tree, because a project on any of them
// may set skipUnchanged and one that could not would silently never skip.
func TestEveryProviderResolvesATree(t *testing.T) {
	for _, provider := range []Provider{
		&GitHub{APIURL: "https://api.github.com"},
		&GitLab{APIURL: "https://gitlab.com/api/v4"},
		&Gitea{APIURL: "https://gitea.com/api/v1"},
	} {
		if _, ok := Trees(provider); !ok {
			t.Errorf("%T cannot name a tree object", provider)
		}
	}
}

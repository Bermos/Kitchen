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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListRepositoriesReadsWhatTheTokenCanSee(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		// Organisation repositories are the ones a platform deploys, so the
		// listing must not be narrowed to the account's own.
		if got := r.URL.Query().Get("affiliation"); !strings.Contains(got, "organization_member") {
			t.Errorf("affiliation %q leaves out the repositories an organisation shares", got)
		}
		_, _ = w.Write([]byte(`[
			{"full_name": "acme/shop", "default_branch": "main", "private": true, "description": "the shop"},
			{"full_name": "acme/blog", "default_branch": "trunk"}
		]`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	listing, err := gh.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Repository{
		{FullName: "acme/shop", DefaultBranch: "main", Private: true, Description: "the shop"},
		{FullName: "acme/blog", DefaultBranch: "trunk"},
	}
	if len(listing.Repositories) != len(want) {
		t.Fatalf("listed %+v, want %+v", listing.Repositories, want)
	}
	for i := range want {
		if listing.Repositories[i] != want[i] {
			t.Errorf("repository %d = %+v, want %+v", i, listing.Repositories[i], want[i])
		}
	}
	if listing.Truncated {
		t.Error("a single short page reported itself as cut short")
	}
}

// repoPages serves `total` repositories a page at a time, so the walk's
// paging and its cap can both be driven.
func repoPages(t *testing.T, total int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if _, err := fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page); err != nil {
			t.Errorf("the walk asked for no page: %q", r.URL.RawQuery)
		}
		repos := []githubRepository{}
		for i := (page - 1) * repositoryPageSize; i < page*repositoryPageSize && i < total; i++ {
			repos = append(repos, githubRepository{FullName: fmt.Sprintf("acme/repo-%d", i), DefaultBranch: "main"})
		}
		_ = json.NewEncoder(w).Encode(repos)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestListRepositoriesFollowsThePages(t *testing.T) {
	gh := &GitHub{APIURL: repoPages(t, repositoryPageSize+7).URL, Token: "tok"}

	listing, err := gh.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Repositories) != repositoryPageSize+7 {
		t.Fatalf("listed %d repositories, want %d", len(listing.Repositories), repositoryPageSize+7)
	}
	if listing.Truncated {
		t.Error("a listing that reached the end reported itself as cut short")
	}
}

// A credential on a large organisation sees more than a picker is any use
// for. The cap is fine; a cap nothing mentions is not — somebody would read a
// missing repository as one that does not exist.
func TestListRepositoriesSaysWhenItStopped(t *testing.T) {
	gh := &GitHub{APIURL: repoPages(t, repositoryPageSize*repositoryPageLimit*2).URL, Token: "tok"}

	listing, err := gh.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Repositories) != repositoryPageSize*repositoryPageLimit {
		t.Fatalf("listed %d repositories, want the cap of %d",
			len(listing.Repositories), repositoryPageSize*repositoryPageLimit)
	}
	if !listing.Truncated {
		t.Error("a listing cut short by the cap did not say so")
	}
}

func TestListRepositoriesCarriesTheProvidersRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message": "Bad credentials"}`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "stale"}
	_, err := gh.ListRepositories(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("want the provider's own words, got %v", err)
	}
}

// The listing is asked for with a type assertion, so the one provider that
// implements it has to satisfy the interface the API narrows to.
func TestGitHubIsARepositoryLister(t *testing.T) {
	if _, ok := Repositories(&GitHub{}); !ok {
		t.Fatal("the github provider cannot be asked what it can see")
	}
}

// trunkBranch is what the fakes call their default branch. It is deliberately
// not "main": a resolver that answered with the guess rather than with the
// repository would look right against a repository called "main".
const trunkBranch = "trunk"

// The default branch is what a caller who was handed a repository name — the
// checkout's origin, rather than a pick out of the listing — has no other way
// to learn, and it must be read from the repository rather than assumed to be
// "main".
func TestDefaultBranchReadsTheRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/repos/acme/shop" {
			t.Errorf("unexpected path %q", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"full_name": "acme/shop", "default_branch": "trunk"}`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	branch, err := gh.DefaultBranch(context.Background(), "acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	if branch != trunkBranch {
		t.Errorf("default branch %q, want the repository's own", branch)
	}
}

// A repository the token cannot see and one that is not there are the same
// 404, and both are the caller's to act on rather than to retry.
func TestDefaultBranchOfARepositoryThatIsNotThere(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "Not Found"}`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	if _, err := gh.DefaultBranch(context.Background(), "acme/typo"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected ErrFileNotFound, got %v", err)
	}
}

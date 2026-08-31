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

func TestHeadRevisionResolvesABranch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/shop/commits/main" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"sha": "0123456789abcdef0123456789abcdef01234567",
			"commit": {"message": "add the checkout\n\nwith a body the build page shows", "author": {"name": "Ada Lovelace"}},
			"author": {"login": "ada"}
		}`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	revision, err := gh.HeadRevision(context.Background(), "acme/shop", "main")
	if err != nil {
		t.Fatal(err)
	}
	want := Revision{
		SHA:     "0123456789abcdef0123456789abcdef01234567",
		Branch:  "main",
		Message: "add the checkout",
		Body:    "with a body the build page shows",
		Author:  "ada",
	}
	if revision != want {
		t.Errorf("resolved %+v, want %+v", revision, want)
	}
}

// A commit by somebody with no account on the provider still has an author,
// and the commit's own name is the only one there is.
func TestHeadRevisionFallsBackToTheCommitAuthor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sha": "abc1234", "commit": {"message": "first", "author": {"name": "Ada"}}}`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	revision, err := gh.HeadRevision(context.Background(), "acme/shop", "main")
	if err != nil {
		t.Fatal(err)
	}
	if revision.Author != "Ada" {
		t.Errorf("author %q, want the commit's own", revision.Author)
	}
}

// An empty repository and a branch that is not there are the same answer:
// nothing to build, and nothing a retry improves.
func TestHeadRevisionReportsAMissingRef(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	if _, err := gh.HeadRevision(context.Background(), "acme/shop", "trunk"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("missing ref returned %v, want ErrFileNotFound", err)
	}
}

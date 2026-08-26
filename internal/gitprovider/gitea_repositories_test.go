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

func TestGiteaDefaultBranchReadsTheRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/repos/acme/shop" {
			t.Errorf("unexpected path %q", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"full_name": "acme/shop", "default_branch": "trunk"}`))
	}))
	defer server.Close()

	gt := &Gitea{APIURL: server.URL, Token: "tok"}
	branch, err := gt.DefaultBranch(context.Background(), "acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	if branch != trunkBranch {
		t.Errorf("default branch %q, want the repository's own", branch)
	}
}

func TestGiteaDefaultBranchOfARepositoryThatIsNotThere(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gt := &Gitea{APIURL: server.URL, Token: "tok"}
	if _, err := gt.DefaultBranch(context.Background(), "acme/typo"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected ErrFileNotFound, got %v", err)
	}
}

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
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGiteaSetCommitStatus(t *testing.T) {
	posted := giteaCommitStatus{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "POST /repos/acme/shop/statuses/"+detectRef {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	gt := &Gitea{APIURL: server.URL, Token: "tok"}
	err := gt.SetCommitStatus(context.Background(), testRepo, CommitStatus{
		SHA:         detectRef,
		State:       CommitFailure,
		Context:     testStatusContext,
		Description: "the build failed",
		TargetURL:   "https://kitchen.example.com/builds/shop-bld-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Gitea takes the shared vocabulary unchanged, which is the reason
	// CommitState is not translated for it.
	if posted.State != "failure" || posted.Context != testStatusContext {
		t.Errorf("unexpected status %+v", posted)
	}
	if posted.TargetURL == "" {
		t.Error("the build's page did not reach the status")
	}
}

func TestGiteaUpsertCommentCreatesThenEdits(t *testing.T) {
	var lastMethod, lastPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastMethod, lastPath = r.Method, r.URL.Path
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 77}`))
		default:
			_, _ = w.Write([]byte(`{"id": 77}`))
		}
	}))
	defer server.Close()

	gt := &Gitea{APIURL: server.URL, Token: "tok"}
	comment := Comment{PullRequest: 42, Marker: "<!-- kitchen -->", Body: "<!-- kitchen -->\npreview is up"}

	id, err := gt.UpsertComment(context.Background(), testRepo, comment)
	if err != nil {
		t.Fatal(err)
	}
	if id != "77" {
		t.Fatalf("created comment id %q", id)
	}
	if lastMethod != http.MethodPost || lastPath != "/repos/acme/shop/issues/42/comments" {
		t.Errorf("created at %s %s", lastMethod, lastPath)
	}

	// The second write addresses the comment directly rather than hunting.
	comment.ID = id
	if _, err := gt.UpsertComment(context.Background(), testRepo, comment); err != nil {
		t.Fatal(err)
	}
	if lastMethod != http.MethodPatch || lastPath != "/repos/acme/shop/issues/comments/77" {
		t.Errorf("edited at %s %s", lastMethod, lastPath)
	}
}

func TestGiteaUpsertCommentFindsTheMarkerAgain(t *testing.T) {
	// A comment whose remembered ID is gone is found by its marker rather
	// than posted a second time.
	var edited string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			if r.URL.Path == "/repos/acme/shop/issues/comments/11" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			edited = r.URL.Path
		case http.MethodGet:
			_, _ = w.Write([]byte(`[
				{"id": 9, "body": "looks good to me"},
				{"id": 12, "body": "<!-- kitchen -->\npreview is up"}
			]`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	gt := &Gitea{APIURL: server.URL, Token: "tok"}
	id, err := gt.UpsertComment(context.Background(), testRepo, Comment{
		PullRequest: 42, ID: "11", Marker: "<!-- kitchen -->", Body: "<!-- kitchen -->\nnew",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "12" {
		t.Fatalf("expected the marked comment, got %q", id)
	}
	if edited != "/repos/acme/shop/issues/comments/12" {
		t.Errorf("edited %q", edited)
	}
}

func TestGiteaPublishesNoDeployments(t *testing.T) {
	// Gitea has no deployments API, so it must not claim the half. The
	// operator asks with this assertion and skips publishing when it fails,
	// which is what keeps the pull request comment being written.
	var provider Provider = &Gitea{APIURL: "https://git.example.com/api/v1"}
	if _, ok := Deployments(provider); ok {
		t.Error("gitea claims to publish deployments")
	}
	if _, ok := Reporter(provider); !ok {
		t.Error("gitea does not report commit statuses")
	}
}

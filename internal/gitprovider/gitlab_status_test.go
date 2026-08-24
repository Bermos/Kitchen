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

const gitlabDeploymentsPath = "/projects/acme%2Fshop/deployments"

func TestGitLabSetCommitStatusTranslatesTheState(t *testing.T) {
	// GitLab has no "error": a platform that could not run the build and a
	// build that failed both read as failed.
	for _, tc := range []struct {
		state CommitState
		want  string
	}{
		{CommitPending, "pending"},
		{CommitSuccess, "success"},
		{CommitFailure, "failed"},
		{CommitError, "failed"},
	} {
		posted := gitlabCommitStatus{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.EscapedPath() != "/projects/acme%2Fshop/statuses/"+detectRef {
				t.Errorf("unexpected path %q", r.URL.EscapedPath())
			}
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusCreated)
		}))

		gl := &GitLab{APIURL: server.URL, Token: "tok"}
		err := gl.SetCommitStatus(context.Background(), testRepo, CommitStatus{
			SHA: detectRef, State: tc.state, Context: testStatusContext, Description: "built",
		})
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if posted.State != tc.want {
			t.Errorf("%s posted as %q, want %q", tc.state, posted.State, tc.want)
		}
		// GitLab names a status rather than giving it a context.
		if posted.Name != testStatusContext {
			t.Errorf("status name %q", posted.Name)
		}
	}
}

func TestGitLabUpsertCommentWritesAMergeRequestNote(t *testing.T) {
	var lastMethod, lastPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastMethod, lastPath = r.Method, r.URL.Path
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 88}`))
		}
	}))
	defer server.Close()

	gl := &GitLab{APIURL: server.URL, Token: "tok"}
	comment := Comment{PullRequest: 42, Marker: "<!-- kitchen -->", Body: "<!-- kitchen -->\npreview is up"}

	id, err := gl.UpsertComment(context.Background(), testRepo, comment)
	if err != nil {
		t.Fatal(err)
	}
	if id != "88" {
		t.Fatalf("created note id %q", id)
	}
	if lastMethod != http.MethodPost || lastPath != "/projects/acme/shop/merge_requests/42/notes" {
		t.Errorf("created at %s %s", lastMethod, lastPath)
	}

	// An edit is a PUT under the merge request, not a global comment path.
	comment.ID = id
	if _, err := gl.UpsertComment(context.Background(), testRepo, comment); err != nil {
		t.Fatal(err)
	}
	if lastMethod != http.MethodPut || lastPath != "/projects/acme/shop/merge_requests/42/notes/88" {
		t.Errorf("edited at %s %s", lastMethod, lastPath)
	}
}

func TestGitLabPublishDeploymentCreatesThenUpdates(t *testing.T) {
	var created gitlabDeployment
	var updatedPath, updatedStatus string
	// held flips once GitLab has a deployment for this commit, which is what
	// the second publish has to find instead of creating another.
	held := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == gitlabDeploymentsPath:
			if got := r.URL.Query().Get("environment"); got != "shop-pr-42" {
				t.Errorf("listed environment %q", got)
			}
			if held {
				_, _ = w.Write([]byte(`[{"id": 5, "sha": "` + detectRef + `"}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.EscapedPath() == gitlabDeploymentsPath:
			if held {
				t.Error("a second deployment record was created for the same commit")
			}
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Fatal(err)
			}
			held = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 5}`))
		case r.Method == http.MethodPut:
			body := gitlabDeployment{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			updatedPath, updatedStatus = r.URL.EscapedPath(), body.Status
			_, _ = w.Write([]byte(`{"id": 5}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer server.Close()

	gl := &GitLab{APIURL: server.URL, Token: "tok"}
	deployment := Deployment{
		SHA: detectRef, Ref: "feat/checkout", Environment: "shop-pr-42",
		State: DeploymentInProgress, Transient: true,
	}
	if err := gl.PublishDeployment(context.Background(), testRepo, deployment); err != nil {
		t.Fatal(err)
	}
	if created.Status != gitlabRunning || created.SHA != detectRef || created.Ref != "feat/checkout" {
		t.Errorf("created %+v", created)
	}

	deployment.State = DeploymentSuccess
	if err := gl.PublishDeployment(context.Background(), testRepo, deployment); err != nil {
		t.Fatal(err)
	}
	if updatedPath != gitlabDeploymentsPath+"/5" || updatedStatus != gitlabSuccess {
		t.Errorf("updated %q to %q", updatedPath, updatedStatus)
	}
}

func TestGitLabRetiringADeploymentWritesNothing(t *testing.T) {
	// GitLab's four statuses hold no "this environment is gone", and calling
	// a deployment that succeeded canceled would be false. The retirement is
	// carried by the merge request note instead.
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("retiring reached the API: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	gl := &GitLab{APIURL: server.URL, Token: "tok"}
	err := gl.PublishDeployment(context.Background(), testRepo, Deployment{
		SHA: detectRef, Environment: "shop-pr-42", State: DeploymentInactive, Transient: true,
	})
	if err != nil {
		t.Fatalf("retiring a deployment failed rather than doing nothing: %v", err)
	}
}

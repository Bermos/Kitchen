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
	"strings"
	"testing"
)

const (
	testRepo = "acme/shop"
	testSHA  = "8f3a2c1d0abc456789ab"

	// listDeployments is the lookup that turns a commit and an environment
	// into the deployment the status hangs off.
	listDeployments = "GET /repos/acme/shop/deployments"
)

func TestSetCommitStatusPostsToTheCommit(t *testing.T) {
	var posted githubCommitStatus
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "POST /repos/acme/shop/statuses/"+testSHA {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	err := gh.SetCommitStatus(context.Background(), testRepo, CommitStatus{
		SHA:         testSHA,
		State:       CommitSuccess,
		Context:     "kitchen/shop",
		Description: "image pushed",
		TargetURL:   "https://kitchen.apps.example.com/builds/shop-bld-8f3a2c1d0abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if posted.State != "success" || posted.Context != "kitchen/shop" {
		t.Errorf("unexpected status %+v", posted)
	}
	if posted.TargetURL == "" {
		t.Error("expected the build page to be linked")
	}
}

// A build failure's message is routinely longer than what GitHub accepts, and
// arriving as a 422 would lose the status entirely.
func TestCommitStatusDescriptionIsTruncated(t *testing.T) {
	var posted githubCommitStatus
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	err := gh.SetCommitStatus(context.Background(), testRepo, CommitStatus{
		SHA:         testSHA,
		State:       CommitFailure,
		Context:     "kitchen/shop",
		Description: strings.Repeat("é", 400),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(posted.Description) > githubDescriptionLimit {
		t.Errorf("description is %d bytes, over the %d limit", len(posted.Description), githubDescriptionLimit)
	}
	if !strings.HasSuffix(posted.Description, "…") {
		t.Errorf("expected the cut to be marked, got %q", posted.Description)
	}
	if !json.Valid([]byte(`"` + posted.Description + `"`)) {
		t.Error("truncation produced invalid UTF-8")
	}
}

func TestPublishDeploymentCreatesTheDeploymentOnce(t *testing.T) {
	var created githubDeployment
	var status githubDeploymentStatus
	deployments := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case listDeployments:
			if got := r.URL.Query().Get("environment"); got != "shop-pr-7" {
				t.Errorf("expected the environment to be filtered on, got %q", got)
			}
			_, _ = w.Write([]byte(`[]`))
		case "POST /repos/acme/shop/deployments":
			deployments++
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 99}`))
		case "POST /repos/acme/shop/deployments/99/statuses":
			if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	err := gh.PublishDeployment(context.Background(), testRepo, Deployment{
		SHA:         testSHA,
		Ref:         "feature/checkout",
		Environment: "shop-pr-7",
		State:       DeploymentSuccess,
		Description: "preview is live",
		URL:         "https://shop-pr-7.apps.example.com",
		Transient:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if deployments != 1 {
		t.Errorf("expected one deployment to be created, got %d", deployments)
	}
	if created.Ref != testSHA || !created.TransientEnv {
		t.Errorf("unexpected deployment %+v", created)
	}
	// GitHub would otherwise merge the base branch in and answer 202 without
	// creating anything, and refuse the deployment until other checks pass.
	if created.AutoMerge || created.RequiredContexts == nil {
		t.Errorf("expected auto_merge off and required_contexts empty, got %+v", created)
	}
	if status.State != "success" || status.EnvironmentURL != "https://shop-pr-7.apps.example.com" {
		t.Errorf("unexpected deployment status %+v", status)
	}
}

func TestPublishDeploymentReusesTheDeployment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case listDeployments:
			_, _ = w.Write([]byte(`[{"id": 12}]`))
		case "POST /repos/acme/shop/deployments/12/statuses":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	err := gh.PublishDeployment(context.Background(), testRepo, Deployment{
		SHA:         testSHA,
		Environment: "shop-pr-7",
		State:       DeploymentInProgress,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A retired environment has no address left to hand a reader.
func TestInactiveDeploymentDropsTheURL(t *testing.T) {
	var status githubDeploymentStatus
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case listDeployments:
			_, _ = w.Write([]byte(`[{"id": 12}]`))
		case "POST /repos/acme/shop/deployments/12/statuses":
			if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	err := gh.PublishDeployment(context.Background(), testRepo, Deployment{
		SHA:         testSHA,
		Environment: "shop-pr-7",
		State:       DeploymentInactive,
		URL:         "https://shop-pr-7.apps.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.EnvironmentURL != "" {
		t.Errorf("expected no environment URL on an inactive deployment, got %q", status.EnvironmentURL)
	}
}

func TestUpsertCommentCreatesThenRewritesInPlace(t *testing.T) {
	const marker = "<!-- kitchen-preview: shop-pr-7 -->"
	posts, patches := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/acme/shop/issues/7/comments":
			_, _ = w.Write([]byte(`[{"id": 1, "body": "looks good to me"}]`))
		case "POST /repos/acme/shop/issues/7/comments":
			posts++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 55}`))
		case "PATCH /repos/acme/shop/issues/comments/55":
			patches++
			_, _ = w.Write([]byte(`{"id": 55}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	comment := Comment{PullRequest: 7, Marker: marker, Body: marker + "\nthe preview is live"}
	id, err := gh.UpsertComment(context.Background(), testRepo, comment)
	if err != nil {
		t.Fatal(err)
	}
	if id != "55" {
		t.Fatalf("expected comment id 55, got %s", id)
	}

	comment.ID = id
	if _, err := gh.UpsertComment(context.Background(), testRepo, comment); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || patches != 1 {
		t.Errorf("expected one create and one rewrite, got %d creates and %d rewrites", posts, patches)
	}
}

// Somebody deleting the comment should not cost the pull request its preview
// URL forever.
func TestUpsertCommentRecoversFromADeletedComment(t *testing.T) {
	const marker = "<!-- kitchen-preview: shop-pr-7 -->"
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "PATCH /repos/acme/shop/issues/comments/55":
			w.WriteHeader(http.StatusNotFound)
		case "GET /repos/acme/shop/issues/7/comments":
			_, _ = w.Write([]byte(`[]`))
		case "POST /repos/acme/shop/issues/7/comments":
			created = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 56}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	id, err := gh.UpsertComment(context.Background(), testRepo, Comment{
		PullRequest: 7, ID: "55", Marker: marker, Body: marker + "\nthe preview is live",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created || id != "56" {
		t.Errorf("expected a replacement comment, got id %q (created=%v)", id, created)
	}
}

// The platform's own comment is found by its marker when its ID was never
// recorded — an environment reported on by an older operator, or a status
// write that was lost.
func TestUpsertCommentAdoptsTheMarkedComment(t *testing.T) {
	const marker = "<!-- kitchen-preview: shop-pr-7 -->"
	patched := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/acme/shop/issues/7/comments":
			_, _ = w.Write([]byte(`[{"id": 1, "body": "looks good"}, {"id": 9, "body": "` + marker + ` old"}]`))
		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/repos/acme/shop/issues/comments/"):
			patched = strings.TrimPrefix(r.URL.Path, "/repos/acme/shop/issues/comments/")
			_, _ = w.Write([]byte(`{"id": 9}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	id, err := gh.UpsertComment(context.Background(), testRepo, Comment{
		PullRequest: 7, Marker: marker, Body: marker + " new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if patched != "9" || id != "9" {
		t.Errorf("expected the marked comment to be rewritten, patched %q and got id %q", patched, id)
	}
}

// The operator asks for the reporting half with a type assertion, so a
// provider that cannot report status simply fails it.
func TestReporterNarrowsTheProvider(t *testing.T) {
	if _, ok := Reporter(&GitHub{}); !ok {
		t.Error("expected GitHub to report status")
	}
	if _, ok := Reporter(sourceOnlyProvider{}); ok {
		t.Error("expected a source-only provider not to report status")
	}
}

// sourceOnlyProvider stands in for a git provider that can be a source but
// cannot post anything back yet — where gitlab and gitea will land.
type sourceOnlyProvider struct{}

func (sourceOnlyProvider) EnsureWebhook(context.Context, string, WebhookSpec) (string, error) {
	return "", nil
}
func (sourceOnlyProvider) DeleteWebhook(context.Context, string, string) error { return nil }

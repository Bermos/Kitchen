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

package receiver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	webhookSecret = "hunter2"
	// commitAuthor is who every fixture's commit is attributed to, asserted
	// once per provider.
	commitAuthor = "bermos"
)

func newReceiver(t *testing.T, objs ...runtime.Object) (*GitWebhookReceiver, http.Handler) {
	return newReceiverForProvider(t, "github", objs...)
}

func newReceiverForProvider(
	t *testing.T,
	provider string,
	objs ...runtime.Object,
) (*GitWebhookReceiver, http.Handler) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "default"},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source: kitchenv1alpha1.GitSourceSpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
				Repo:          "acme/shop",
			},
			Registry: kitchenv1alpha1.RegistrySpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kitchen-webhook-shop", Namespace: "default"},
		Data:       map[string][]byte{"secret": []byte(webhookSecret)},
	}
	connection := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "default"},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             provider,
			CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "gh-creds"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(append([]runtime.Object{project, secret, connection}, objs...)...).Build()
	r := &GitWebhookReceiver{Client: c, Namespace: "default"}
	return r, r.handler()
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func deliver(handler http.Handler, event string, body []byte, signature string) *httptest.ResponseRecorder {
	return deliverAs("github", handler, event, body, signature)
}

func deliverAs(
	provider string,
	handler http.Handler,
	event string,
	body []byte,
	signature string,
) *httptest.ResponseRecorder {
	return deliverTo(provider, handler, "gh", event, body, signature)
}

func deliverTo(
	provider string,
	handler http.Handler,
	connection string,
	event string,
	body []byte,
	signature string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/git/"+connection, bytes.NewReader(body))
	switch provider {
	case "gitlab":
		req.Header.Set("X-Gitlab-Event", event)
		if signature != "" {
			req.Header.Set("X-Gitlab-Token", signature)
		}
	case "gitea":
		req.Header.Set("X-Gitea-Event", event)
		if signature != "" {
			req.Header.Set("X-Gitea-Signature", signature)
		}
	default:
		req.Header.Set("X-GitHub-Event", event)
		if signature != "" {
			req.Header.Set("X-Hub-Signature-256", signature)
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestPushCreatesBuild(t *testing.T) {
	r, handler := newReceiver(t)
	body := []byte(`{
		"ref": "refs/heads/main",
		"after": "8f3a2c1d0abc456789ab",
		"repository": {"full_name": "acme/shop"},
		"head_commit": {"message": "Add checkout", "author": {"username": "bermos"}}
	}`)

	rec := deliver(handler, "push", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Name: "shop-bld-8f3a2c1d0abc", Namespace: "default"}
	if err := r.Client.Get(context.Background(), key, build); err != nil {
		t.Fatalf("expected build to be created: %v", err)
	}
	if build.Spec.Git.Branch != "main" || build.Spec.Git.SHA != "8f3a2c1d0abc456789ab" {
		t.Errorf("unexpected git revision %+v", build.Spec.Git)
	}
	if build.Spec.Git.Author != commitAuthor || build.Spec.Git.Message != "Add checkout" {
		t.Errorf("unexpected commit metadata %+v", build.Spec.Git)
	}
	if build.Spec.Git.PullRequest != nil {
		t.Error("push build should have no pull request")
	}

	// Redelivery must not fail or duplicate.
	rec = deliver(handler, "push", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 on redelivery, got %d", rec.Code)
	}
}

func TestBadSignatureRejected(t *testing.T) {
	r, handler := newReceiver(t)
	body := []byte(`{"ref": "refs/heads/main", "after": "8f3a2c1d0abc", "repository": {"full_name": "acme/shop"}}`)

	rec := deliver(handler, "push", body, "sha256=deadbeef")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	builds := &kitchenv1alpha1.BuildList{}
	if err := r.Client.List(context.Background(), builds); err != nil {
		t.Fatal(err)
	}
	if len(builds.Items) != 0 {
		t.Errorf("no build should be created for a bad signature, found %d", len(builds.Items))
	}
}

func TestPullRequestCreatesBuild(t *testing.T) {
	r, handler := newReceiver(t)
	body := []byte(`{
		"action": "opened",
		"number": 42,
		"pull_request": {"head": {"ref": "feat/checkout", "sha": "aaaabbbbccccdddd"}},
		"repository": {"full_name": "acme/shop"}
	}`)

	rec := deliver(handler, "pull_request", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Name: "shop-bld-aaaabbbbcccc", Namespace: "default"}
	if err := r.Client.Get(context.Background(), key, build); err != nil {
		t.Fatalf("expected build to be created: %v", err)
	}
	if build.Spec.Git.PullRequest == nil || *build.Spec.Git.PullRequest != 42 {
		t.Errorf("expected pull request 42, got %+v", build.Spec.Git.PullRequest)
	}
	if build.Spec.Git.Branch != "feat/checkout" {
		t.Errorf("unexpected branch %q", build.Spec.Git.Branch)
	}
}

// TestPullRequestAfterPushAssociatesTheExistingBuild is issue #201: the
// ordinary way a request is opened is to push the branch and then open it, and
// every provider delivers the push first. The Build is named after the commit,
// so the request event finds the name taken — and used to be discarded as a
// redelivery, which left the platform believing the commit belonged to no
// request and no preview environment was ever created for it.
func TestPullRequestAfterPushAssociatesTheExistingBuild(t *testing.T) {
	r, handler := newReceiver(t)
	push := []byte(`{
		"ref": "refs/heads/feat/checkout",
		"after": "8f3a2c1d0abc456789ab",
		"repository": {"full_name": "acme/shop"},
		"head_commit": {"message": "Add checkout", "author": {"username": "bermos"}}
	}`)
	if rec := deliver(handler, "push", push, sign(push)); rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for the push, got %d: %s", rec.Code, rec.Body.String())
	}

	opened := []byte(`{
		"action": "opened",
		"number": 10,
		"pull_request": {"head": {"ref": "feat/checkout", "sha": "8f3a2c1d0abc456789ab"}},
		"repository": {"full_name": "acme/shop"}
	}`)
	rec := deliver(handler, "pull_request", opened, sign(opened))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for the pull request, got %d: %s", rec.Code, rec.Body.String())
	}
	// The delivery names the build it acted on: a body of {"builds":null} is
	// how this bug looked from the provider's delivery log.
	if !strings.Contains(rec.Body.String(), "shop-bld-8f3a2c1d0abc") {
		t.Errorf("the delivery should name the build it associated, got %s", rec.Body.String())
	}

	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Name: "shop-bld-8f3a2c1d0abc", Namespace: "default"}
	if err := r.Client.Get(context.Background(), key, build); err != nil {
		t.Fatalf("expected the push's build to still be there: %v", err)
	}
	if build.Spec.Git.PullRequest != nil {
		t.Error("the spec is immutable and was created from a push: it must still name no request")
	}
	if pr := build.PullRequestNumber(); pr == nil || *pr != 10 {
		t.Fatalf("expected the build to know about pull request 10, got %v", pr)
	}

	builds := &kitchenv1alpha1.BuildList{}
	if err := r.Client.List(context.Background(), builds); err != nil {
		t.Fatal(err)
	}
	if len(builds.Items) != 1 {
		t.Errorf("the commit should be built once, found %d builds", len(builds.Items))
	}
}

// TestPushRedeliveryDoesNotUnlearnThePullRequest guards the other order: once
// a commit is associated with a request, a re-sent push for the same commit is
// still nothing to do, and the association survives it.
func TestPushRedeliveryDoesNotUnlearnThePullRequest(t *testing.T) {
	r, handler := newReceiver(t)
	opened := []byte(`{
		"action": "opened",
		"number": 10,
		"pull_request": {"head": {"ref": "feat/checkout", "sha": "8f3a2c1d0abc456789ab"}},
		"repository": {"full_name": "acme/shop"}
	}`)
	if rec := deliver(handler, "pull_request", opened, sign(opened)); rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	push := []byte(`{
		"ref": "refs/heads/feat/checkout",
		"after": "8f3a2c1d0abc456789ab",
		"repository": {"full_name": "acme/shop"},
		"head_commit": {"message": "Add checkout", "author": {"username": "bermos"}}
	}`)
	if rec := deliver(handler, "push", push, sign(push)); rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Name: "shop-bld-8f3a2c1d0abc", Namespace: "default"}
	if err := r.Client.Get(context.Background(), key, build); err != nil {
		t.Fatal(err)
	}
	if pr := build.PullRequestNumber(); pr == nil || *pr != 10 {
		t.Errorf("expected pull request 10 to survive the push, got %v", pr)
	}
}

// TestTheFirstPullRequestToClaimACommitKeepsIt: one head commit can belong to
// two open requests. The preview already exists under the first one's name, so
// letting the second take the association would move it.
func TestTheFirstPullRequestToClaimACommitKeepsIt(t *testing.T) {
	r, handler := newReceiver(t)
	push := []byte(`{
		"ref": "refs/heads/feat/checkout",
		"after": "8f3a2c1d0abc456789ab",
		"repository": {"full_name": "acme/shop"},
		"head_commit": {"message": "Add checkout", "author": {"username": "bermos"}}
	}`)
	if rec := deliver(handler, "push", push, sign(push)); rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, number := range []int{10, 11} {
		opened := []byte(fmt.Sprintf(`{
			"action": "opened",
			"number": %d,
			"pull_request": {"head": {"ref": "feat/checkout", "sha": "8f3a2c1d0abc456789ab"}},
			"repository": {"full_name": "acme/shop"}
		}`, number))
		if rec := deliver(handler, "pull_request", opened, sign(opened)); rec.Code != http.StatusAccepted {
			t.Fatalf("expected 202 for #%d, got %d: %s", number, rec.Code, rec.Body.String())
		}
	}

	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Name: "shop-bld-8f3a2c1d0abc", Namespace: "default"}
	if err := r.Client.Get(context.Background(), key, build); err != nil {
		t.Fatal(err)
	}
	if pr := build.PullRequestNumber(); pr == nil || *pr != 10 {
		t.Errorf("expected the first request to keep the commit, got %v", pr)
	}
}

// TestGitLabMergeRequestAfterPushAssociatesTheExistingBuild is the same order
// of events on GitLab, which delivers a push hook and then a merge request
// hook exactly as GitHub does.
func TestGitLabMergeRequestAfterPushAssociatesTheExistingBuild(t *testing.T) {
	r, handler := newReceiverForProvider(t, "gitlab")
	push := []byte(`{
		"ref": "refs/heads/feat/checkout",
		"after": "aaaabbbbccccdddd",
		"project": {"path_with_namespace": "acme/shop"},
		"user_username": "bermos",
		"commits": [{"message": "Add checkout", "author": {"username": "bermos"}}]
	}`)
	if rec := deliverAs("gitlab", handler, "Push Hook", push, webhookSecret); rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for the push, got %d: %s", rec.Code, rec.Body.String())
	}
	opened := []byte(`{
		"object_attributes": {
			"action": "open", "iid": 42, "source_branch": "feat/checkout",
			"last_commit": {"id": "aaaabbbbccccdddd", "message": "Add checkout"}
		},
		"project": {"path_with_namespace": "acme/shop"},
		"user": {"username": "bermos"}
	}`)
	if rec := deliverAs("gitlab", handler, "Merge Request Hook", opened, webhookSecret); rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for the merge request, got %d: %s", rec.Code, rec.Body.String())
	}

	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Name: "shop-bld-aaaabbbbcccc", Namespace: "default"}
	if err := r.Client.Get(context.Background(), key, build); err != nil {
		t.Fatal(err)
	}
	if pr := build.PullRequestNumber(); pr == nil || *pr != 42 {
		t.Errorf("expected merge request 42, got %v", pr)
	}
}

func TestPullRequestClosedDeletesPreview(t *testing.T) {
	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-pr-42", Namespace: "default"},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:       kitchenv1alpha1.EnvironmentPreview,
			ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: "shop-rel-1"},
			Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 42, Branch: "feat/checkout"},
		},
	}
	r, handler := newReceiver(t, env)
	body := []byte(`{
		"action": "closed",
		"number": 42,
		"pull_request": {"head": {"ref": "feat/checkout", "sha": "aaaabbbbccccdddd"}},
		"repository": {"full_name": "acme/shop"}
	}`)

	rec := deliver(handler, "pull_request", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	err := r.Client.Get(context.Background(),
		types.NamespacedName{Name: "shop-pr-42", Namespace: "default"}, &kitchenv1alpha1.Environment{})
	if !errors.IsNotFound(err) {
		t.Errorf("expected preview environment to be deleted, got %v", err)
	}
}

func TestPingAndUnknownRepo(t *testing.T) {
	_, handler := newReceiver(t)

	rec := deliver(handler, "ping", []byte(`{}`), "")
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for ping, got %d", rec.Code)
	}

	body := []byte(`{"ref": "refs/heads/main", "after": "8f3a2c1d0abc", "repository": {"full_name": "acme/other"}}`)
	rec = deliver(handler, "push", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Errorf("expected 202 for unknown repo, got %d", rec.Code)
	}

	// Branch deletions are ignored.
	body = []byte(`{"ref": "refs/heads/gone", "after": "0000000000000000", "deleted": true, "repository": {"full_name": "acme/shop"}}`)
	rec = deliver(handler, "push", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Errorf("expected 202 for branch deletion, got %d", rec.Code)
	}
}

func TestGitLabPushCreatesBuild(t *testing.T) {
	r, handler := newReceiverForProvider(t, "gitlab")
	body := []byte(`{
		"ref": "refs/heads/main",
		"after": "8f3a2c1d0abc456789ab",
		"user_name": "bermos",
		"project": {"path_with_namespace": "acme/shop"},
		"commits": [{"message": "Add checkout", "author": {"username": "bermos"}}]
	}`)

	rec := deliverAs("gitlab", handler, "Push Hook", body, webhookSecret)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Name: "shop-bld-8f3a2c1d0abc", Namespace: "default"}
	if err := r.Client.Get(context.Background(), key, build); err != nil {
		t.Fatalf("expected build to be created: %v", err)
	}
	if build.Spec.Git.Author != commitAuthor || build.Spec.Git.Message != "Add checkout" {
		t.Errorf("unexpected git metadata %+v", build.Spec.Git)
	}
}

func TestGiteaPullRequestCreatesBuild(t *testing.T) {
	r, handler := newReceiverForProvider(t, "gitea")
	body := []byte(`{
		"action": "opened",
		"number": 42,
		"pull_request": {"head": {"ref": "feat/checkout", "sha": "aaaabbbbccccdddd"}},
		"repository": {"full_name": "acme/shop"}
	}`)

	rec := deliverAs("gitea", handler, "pull_request", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Name: "shop-bld-aaaabbbbcccc", Namespace: "default"}
	if err := r.Client.Get(context.Background(), key, build); err != nil {
		t.Fatalf("expected build to be created: %v", err)
	}
	if build.Spec.Git.PullRequest == nil || *build.Spec.Git.PullRequest != 42 {
		t.Errorf("expected pull request 42, got %+v", build.Spec.Git.PullRequest)
	}
}

func TestPingIsAnsweredWithoutAConnection(t *testing.T) {
	// The chart's own liveness probe pings /webhooks/git/none, and GitHub
	// pings the moment a webhook is registered — before anything guarantees
	// the Connection behind it is readable.
	_, handler := newReceiver(t)

	rec := deliverTo("github", handler, "none", "ping", []byte(`{}`), "")
	if rec.Code != http.StatusOK || rec.Body.String() != "pong" {
		t.Fatalf("expected 200 pong for a ping at an unknown connection, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeliveryForAMissingConnectionIsAccepted(t *testing.T) {
	// A webhook outliving its Connection has no project behind it either.
	// An error would only make the provider disable the hook.
	r, handler := newReceiver(t)
	body := []byte(`{"ref": "refs/heads/main", "after": "8f3a2c1d0abc456789ab", "repository": {"full_name": "acme/shop"}}`)

	rec := deliverTo("github", handler, "gone", "push", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	builds := &kitchenv1alpha1.BuildList{}
	if err := r.Client.List(context.Background(), builds); err != nil {
		t.Fatalf("listing builds: %v", err)
	}
	if len(builds.Items) != 0 {
		t.Errorf("expected no builds, got %d", len(builds.Items))
	}
}

func TestGiteaPushCreatesBuild(t *testing.T) {
	r, handler := newReceiverForProvider(t, "gitea")
	body := []byte(`{
		"ref": "refs/heads/main",
		"after": "8f3a2c1d0abc456789ab",
		"repository": {"full_name": "acme/shop"},
		"head_commit": {"message": "Add checkout", "author": {"username": "bermos"}},
		"pusher": {"login": "someone-else"}
	}`)

	rec := deliverAs("gitea", handler, "push", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Name: "shop-bld-8f3a2c1d0abc", Namespace: "default"}
	if err := r.Client.Get(context.Background(), key, build); err != nil {
		t.Fatalf("expected build to be created: %v", err)
	}
	if build.Spec.Git.Author != commitAuthor || build.Spec.Git.Branch != "main" {
		t.Errorf("unexpected git metadata %+v", build.Spec.Git)
	}
}

func TestGiteaSynchronizedRebuildsThePreview(t *testing.T) {
	// Gitea's word for "the branch moved" is "synchronized"; GitHub's is
	// "synchronize". Reading only GitHub's leaves a preview stuck on the
	// commit that opened it.
	r, handler := newReceiverForProvider(t, "gitea")
	body := []byte(`{
		"action": "synchronized",
		"number": 42,
		"pull_request": {"head": {"ref": "feat/checkout", "sha": "ccccddddeeeeffff"}},
		"repository": {"full_name": "acme/shop"}
	}`)

	rec := deliverAs("gitea", handler, "pull_request", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Name: "shop-bld-ccccddddeeee", Namespace: "default"}
	if err := r.Client.Get(context.Background(), key, build); err != nil {
		t.Fatalf("expected the new commit to build: %v", err)
	}
	if build.Spec.Git.PullRequest == nil || *build.Spec.Git.PullRequest != 42 {
		t.Errorf("expected pull request 42, got %+v", build.Spec.Git.PullRequest)
	}
}

func TestGitLabMergeRequestOpenedAndUpdated(t *testing.T) {
	r, handler := newReceiverForProvider(t, "gitlab")
	opened := []byte(`{
		"object_attributes": {
			"action": "open", "iid": 42, "source_branch": "feat/checkout",
			"last_commit": {"id": "aaaabbbbccccdddd", "message": "Add checkout"}
		},
		"project": {"path_with_namespace": "acme/shop"},
		"user": {"username": "bermos"}
	}`)

	rec := deliverAs("gitlab", handler, "Merge Request Hook", opened, webhookSecret)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Name: "shop-bld-aaaabbbbcccc", Namespace: "default"}
	if err := r.Client.Get(context.Background(), key, build); err != nil {
		t.Fatalf("expected build to be created: %v", err)
	}
	if build.Spec.Git.PullRequest == nil || *build.Spec.Git.PullRequest != 42 {
		t.Errorf("expected merge request 42, got %+v", build.Spec.Git.PullRequest)
	}

	// An update that did not move the source branch — a label, a title, an
	// assignee — carries no oldrev and must not redeploy the preview.
	relabelled := []byte(`{
		"object_attributes": {
			"action": "update", "iid": 42, "source_branch": "feat/checkout",
			"last_commit": {"id": "1111222233334444", "message": "Add checkout"}
		},
		"project": {"path_with_namespace": "acme/shop"},
		"user": {"username": "bermos"}
	}`)
	rec = deliverAs("gitlab", handler, "Merge Request Hook", relabelled, webhookSecret)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	err := r.Client.Get(context.Background(),
		types.NamespacedName{Name: "shop-bld-111122223333", Namespace: "default"}, &kitchenv1alpha1.Build{})
	if !errors.IsNotFound(err) {
		t.Errorf("a merge request edit that moved nothing built anyway: %v", err)
	}

	// One that did move it carries oldrev, and builds.
	pushed := []byte(`{
		"object_attributes": {
			"action": "update", "iid": 42, "source_branch": "feat/checkout",
			"oldrev": "aaaabbbbccccdddd",
			"last_commit": {"id": "1111222233334444", "message": "Fix the total"}
		},
		"project": {"path_with_namespace": "acme/shop"},
		"user": {"username": "bermos"}
	}`)
	rec = deliverAs("gitlab", handler, "Merge Request Hook", pushed, webhookSecret)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := r.Client.Get(context.Background(),
		types.NamespacedName{Name: "shop-bld-111122223333", Namespace: "default"}, build); err != nil {
		t.Fatalf("expected the new commit to build: %v", err)
	}
}

func TestGitLabMergeRequestMergedDeletesPreview(t *testing.T) {
	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-pr-42", Namespace: "default"},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:       kitchenv1alpha1.EnvironmentPreview,
			ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: "shop-rel-1"},
			Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 42, Branch: "feat/checkout"},
		},
	}
	r, handler := newReceiverForProvider(t, "gitlab", env)
	body := []byte(`{
		"object_attributes": {"action": "merge", "iid": 42, "source_branch": "feat/checkout"},
		"project": {"path_with_namespace": "acme/shop"},
		"user": {"username": "bermos"}
	}`)

	rec := deliverAs("gitlab", handler, "Merge Request Hook", body, webhookSecret)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	err := r.Client.Get(context.Background(),
		types.NamespacedName{Name: "shop-pr-42", Namespace: "default"}, &kitchenv1alpha1.Environment{})
	if !errors.IsNotFound(err) {
		t.Errorf("expected the preview to be torn down, got %v", err)
	}
}

func TestSignatureIsCheckedInEachProvidersOwnScheme(t *testing.T) {
	body := []byte(`{
		"ref": "refs/heads/main", "after": "8f3a2c1d0abc456789ab",
		"repository": {"full_name": "acme/shop"},
		"project": {"path_with_namespace": "acme/shop"}
	}`)

	for _, tc := range []struct {
		provider  string
		event     string
		signature string
	}{
		// GitLab compares the token whole; a valid HMAC of the body is not it.
		{provider: "gitlab", event: "Push Hook", signature: sign(body)},
		// Gitea signs the body; the bare secret is not a signature.
		{provider: "gitea", event: "push", signature: webhookSecret},
		{provider: "gitea", event: "push", signature: "deadbeef"},
	} {
		r, handler := newReceiverForProvider(t, tc.provider)
		rec := deliverAs(tc.provider, handler, tc.event, body, tc.signature)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s accepted the wrong credential: %d", tc.provider, rec.Code)
		}
		builds := &kitchenv1alpha1.BuildList{}
		if err := r.Client.List(context.Background(), builds); err != nil {
			t.Fatalf("listing builds: %v", err)
		}
		if len(builds.Items) != 0 {
			t.Errorf("%s built from an unverified delivery", tc.provider)
		}
	}
}

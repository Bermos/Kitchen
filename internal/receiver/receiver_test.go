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
	"net/http"
	"net/http/httptest"
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

const webhookSecret = "hunter2"

func newReceiver(t *testing.T, objs ...runtime.Object) (*GitWebhookReceiver, http.Handler) {
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

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(append([]runtime.Object{project, secret}, objs...)...).Build()
	r := &GitWebhookReceiver{Client: c, Namespace: "default"}
	return r, r.handler()
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func deliver(handler http.Handler, event string, body []byte, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/git/gh", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
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
	if build.Spec.Git.Author != "bermos" || build.Spec.Git.Message != "Add checkout" {
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

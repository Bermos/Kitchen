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

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The write endpoints issue #60 added: everything a user used to need kubectl
// for. Each test drives the endpoint and then reads the cluster the way the
// reconcilers would, because the writes only matter if the reconcilers see
// what they expect.

func getSecret(t *testing.T, h *harness, name string) (*corev1.Secret, error) {
	t.Helper()
	secret := &corev1.Secret{}
	err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: name}, secret)
	return secret, err
}

func TestPatchingAProjectsSettings(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{
		"productionBranch": "stable",
		"previews": false,
		"previewsProtected": false,
		"buildStrategy": "buildpacks",
		"dockerfilePath": "build/Dockerfile",
		"rootDirectory": "apps/shop",
		"env": [
			{"name": "PUBLIC_URL", "value": "https://shop.example.com", "previewValue": "preview"},
			{"name": "DATABASE_URL", "fromClaim": {"name": "shop-db", "key": "url"}},
			{"name": "API_KEY", "fromSecret": {"name": "shop-api-key", "key": "key"}}
		],
		"port": 8080,
		"replicas": 3,
		"cpu": "250m",
		"memory": "512Mi"
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if view.ProductionBranch != "stable" || view.Previews || view.PreviewsProtected {
		t.Fatalf("the response does not echo the request: %+v", view)
	}
	if view.CPU != "250m" || view.Memory != "512Mi" || view.Port != 8080 {
		t.Fatalf("the runtime settings did not echo: %+v", view)
	}
	if len(view.Env) != 3 || view.Env[1].FromClaim == nil || view.Env[2].FromSecret == nil {
		t.Fatalf("the env vars did not echo: %+v", view.Env)
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Source.ProductionBranch != "stable" || stored.Spec.Previews.Enabled {
		t.Fatalf("the settings did not stick: %+v", stored.Spec)
	}
	if stored.Spec.Build.Strategy != kitchenv1alpha1.BuildStrategyBuildpacks ||
		stored.Spec.Build.RootDirectory != "apps/shop" {
		t.Fatalf("the build settings did not stick: %+v", stored.Spec.Build)
	}
	if stored.Spec.Previews.IsProtected() {
		t.Fatal("previewsProtected=false did not stick")
	}
	if len(stored.Spec.Env) != 3 || stored.Spec.Env[1].FromResourceClaim == nil || stored.Spec.Env[2].SecretRef == nil {
		t.Fatalf("the env vars did not stick: %+v", stored.Spec.Env)
	}
	if stored.Spec.Runtime.Replicas == nil || *stored.Spec.Runtime.Replicas != 3 {
		t.Fatalf("the replicas did not stick: %+v", stored.Spec.Runtime)
	}
	// Requests and limits are set alike: the guaranteed class.
	cpu := stored.Spec.Runtime.Resources.Requests[corev1.ResourceCPU]
	if cpu.String() != "250m" {
		t.Fatalf("want the cpu request set with the limit, got %q", cpu.String())
	}
}

func TestPatchingAProjectLeavesTheRestAlone(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"previews": false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Source.ProductionBranch != defaultProductionBranch || stored.Spec.Source.Repo != "acme/shop" {
		t.Fatalf("fields that were not patched changed: %+v", stored.Spec.Source)
	}
}

func TestPatchingAProjectRejectsUnusableRequests(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, body := range map[string]string{
		"an empty production branch": `{"productionBranch": " "}`,
		"a strategy that is not one": `{"buildStrategy": "guess"}`,
		"a port out of range":        `{"port": 70000}`,
		"zero replicas":              `{"replicas": 0}`,
		"cpu that is not a quantity": `{"cpu": "fast"}`,
		"an env var with no name":    `{"env": [{"value": "x"}]}`,
		"an env var named twice":     `{"env": [{"name": "A", "value": "1"}, {"name": "A", "value": "2"}]}`,
		"an env var with two sources": `{"env": [{"name": "A", "value": "x",
			"fromSecret": {"name": "s", "key": "k"}}]}`,
		"an unknown field": `{"branch": "main"}`,
		"not JSON":         `{`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestDeletingAProject(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodDelete, "/api/v1/projects/shop", "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := h.server.get(context.Background(), "shop", &kitchenv1alpha1.Project{}); err == nil {
		t.Fatal("the project is still there")
	}
}

func TestDeletingAProjectThatDoesNotExist(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodDelete, "/api/v1/projects/nope", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// runningBuild is a build mid-flight, with the BuildKit job that runs it.
func runningBuild() (*kitchenv1alpha1.Build, *batchv1.Job) {
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-running00000", Namespace: testNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Git:        kitchenv1alpha1.GitRevision{SHA: "cafe1234567890a", Branch: defaultProductionBranch},
		},
		Status: kitchenv1alpha1.BuildStatus{Phase: kitchenv1alpha1.BuildRunning},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      build.Name,
		Namespace: controller.AppNamespace("shop"),
	}}
	return build, job
}

func TestCancellingARunningBuild(t *testing.T) {
	build, job := runningBuild()
	h := newHarness(t, nil, append(fixtures(), build, job)...)

	recorder := h.do(t, http.MethodPost, "/api/v1/builds/"+build.Name+"/cancel", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[buildView](t, recorder); view.Phase != string(kitchenv1alpha1.BuildCancelled) {
		t.Fatalf("want the build cancelled, got %q", view.Phase)
	}

	stored := &kitchenv1alpha1.Build{}
	if err := h.server.get(context.Background(), build.Name, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Phase != kitchenv1alpha1.BuildCancelled || stored.Status.CompletedAt == nil {
		t.Fatalf("the cancellation did not stick: %+v", stored.Status)
	}

	// The BuildKit job goes with it — a cancelled build must not keep building.
	err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: controller.AppNamespace("shop"), Name: build.Name}, &batchv1.Job{})
	if err == nil {
		t.Fatal("the build job is still there")
	}
}

func TestCancellingAFinishedBuildIsRefused(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/builds/"+testBuild+"/cancel", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "already finished") {
		t.Fatalf("the error should say why: %s", recorder.Body.String())
	}
}

func previewEnvironment() *kitchenv1alpha1.Environment {
	return &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-pr-7", Namespace: testNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:       kitchenv1alpha1.EnvironmentPreview,
			Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 7, Branch: "feature"},
			ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: testRelease},
		},
	}
}

func TestDeletingAPreviewEnvironment(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), previewEnvironment())...)

	recorder := h.do(t, http.MethodDelete, "/api/v1/environments/shop-pr-7", "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := h.server.get(context.Background(), "shop-pr-7", &kitchenv1alpha1.Environment{}); err == nil {
		t.Fatal("the preview is still there")
	}
}

func TestDeletingTheProductionEnvironmentIsRefused(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodDelete, "/api/v1/environments/"+testEnvironment, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := h.server.get(context.Background(), testEnvironment, &kitchenv1alpha1.Environment{}); err != nil {
		t.Fatal("production was deleted anyway")
	}
}

func TestCreatingAGitConnection(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections",
		`{"name": "hub", "provider": "github", "credential": {"token": "ghp_secret"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "ghp_secret") ||
		strings.Contains(recorder.Body.String(), "kitchen-connection") {
		t.Fatalf("the response leaks the credential or its secret: %s", recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Connection{}
	if err := h.server.get(context.Background(), "hub", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Provider != "github" {
		t.Fatalf("unexpected connection: %+v", stored.Spec)
	}

	// The secret carries the token under the key the reconcilers read, and
	// the label that marks it the platform's own.
	secret, err := getSecret(t, h, stored.Spec.CredentialsSecretRef.Name)
	if err != nil {
		t.Fatal(err)
	}
	if string(secret.Data[gitTokenKey]) != "ghp_secret" {
		t.Fatalf("the token is not where the reconcilers read it: %v", secret.Data)
	}
	if secret.Labels[managedByLabelKey] != managedByLabelValue {
		t.Fatalf("the secret is not marked as the platform's: %v", secret.Labels)
	}
}

func TestCreatingARegistryConnection(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections",
		`{"name": "harbor", "provider": "dockerRegistry",
		  "config": {"url": "harbor.example.com/kitchen"},
		  "credential": {"username": "robot", "password": "hunter2"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "hunter2") {
		t.Fatalf("the response leaks the credential: %s", recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Connection{}
	if err := h.server.get(context.Background(), "harbor", stored); err != nil {
		t.Fatal(err)
	}
	secret, err := getSecret(t, h, stored.Spec.CredentialsSecretRef.Name)
	if err != nil {
		t.Fatal(err)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("want a dockerconfigjson secret, got %q", secret.Type)
	}
	// The build mounts this under the registry host, not the full prefix.
	dockerConfig := string(secret.Data[corev1.DockerConfigJsonKey])
	if !strings.Contains(dockerConfig, `"harbor.example.com"`) || !strings.Contains(dockerConfig, "robot") {
		t.Fatalf("the docker config does not authenticate the registry host: %s", dockerConfig)
	}
}

func TestCreatingAConnectionRejectsUnusableRequests(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, body := range map[string]string{
		"no name":                          `{"provider": "github", "credential": {"token": "t"}}`,
		"a name that is not a DNS label":   `{"name": "Hub!", "provider": "github", "credential": {"token": "t"}}`,
		"no credential":                    `{"name": "hub", "provider": "github"}`,
		"an unknown provider":              `{"name": "hub", "provider": "svn", "credential": {"token": "t"}}`,
		"a token provider without a token": `{"name": "hub", "provider": "github", "credential": {"username": "u", "password": "p"}}`,
		"a registry without a url": `{"name": "harbor", "provider": "dockerRegistry",
			"credential": {"username": "u", "password": "p"}}`,
		"a registry without a password": `{"name": "harbor", "provider": "dockerRegistry",
			"config": {"url": "harbor.example.com"}, "credential": {"username": "u"}}`,
		"an unknown field": `{"name": "hub", "provider": "github", "token": "t"}`,
		"not JSON":         `{`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/connections", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestCreatingAConnectionThatAlreadyExists(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections",
		`{"name": "gh", "provider": "github", "credential": {"token": "t"}}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// Existence is checked before the secret write, so the collision must not
	// have rotated the existing connection's credentials.
	if _, err := getSecret(t, h, connectionSecretPrefix+"gh"); err == nil {
		t.Fatal("a secret was written for a connection that was never created")
	}
}

func TestRotatingAConnectionsCredential(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	if recorder := h.do(t, http.MethodPost, "/api/v1/connections",
		`{"name": "hub", "provider": "github", "credential": {"token": "old"}}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodPatch, "/api/v1/connections/hub", `{"credential": {"token": "new"}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), `"new"`) {
		t.Fatalf("the response leaks the credential: %s", recorder.Body.String())
	}

	secret, err := getSecret(t, h, connectionSecretPrefix+"hub")
	if err != nil {
		t.Fatal(err)
	}
	if string(secret.Data[gitTokenKey]) != "new" {
		t.Fatalf("the rotation did not stick: %v", secret.Data)
	}
}

func TestPatchingAConnectionsConfig(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/connections/gh",
		`{"config": {"apiUrl": "https://github.internal/api/v3"}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Connection{}
	if err := h.server.get(context.Background(), "gh", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Config == nil || !strings.Contains(string(stored.Spec.Config.Raw), "github.internal") {
		t.Fatalf("the config did not stick: %+v", stored.Spec.Config)
	}
}

func TestPatchingAConnectionWithNothingToChange(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/connections/gh", `{}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeletingAConnectionInUseIsRefused(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	// "gh" is the shop project's git source.
	recorder := h.do(t, http.MethodDelete, "/api/v1/connections/gh", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "project shop") {
		t.Fatalf("the error should name what uses it: %s", recorder.Body.String())
	}
	if err := h.server.get(context.Background(), "gh", &kitchenv1alpha1.Connection{}); err != nil {
		t.Fatal("the connection was deleted anyway")
	}
}

func TestDeletingAnUnusedConnection(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	if recorder := h.do(t, http.MethodPost, "/api/v1/connections",
		`{"name": "hub", "provider": "github", "credential": {"token": "t"}}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodDelete, "/api/v1/connections/hub", "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := h.server.get(context.Background(), "hub", &kitchenv1alpha1.Connection{}); err == nil {
		t.Fatal("the connection is still there")
	}
	if _, err := getSecret(t, h, connectionSecretPrefix+"hub"); err == nil {
		t.Fatal("the platform-managed credentials secret is still there")
	}
}

func TestDeletingAConnectionKeepsSecretsItDidNotWrite(t *testing.T) {
	// A connection whose credentials something else manages — an Infisical
	// sync, a hand-written manifest — loses the connection, keeps the secret.
	external := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "synced-credentials", Namespace: testNamespace},
		Data:       map[string][]byte{gitTokenKey: []byte("synced")},
	}
	conn := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "synced", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "github",
			CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "synced-credentials"},
		},
	}
	h := newHarness(t, nil, append(fixtures(), external, conn)...)

	recorder := h.do(t, http.MethodDelete, "/api/v1/connections/synced", "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, err := getSecret(t, h, "synced-credentials"); err != nil {
		t.Fatal("the externally managed secret was deleted")
	}
}

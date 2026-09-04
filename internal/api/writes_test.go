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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The write endpoints issue #60 added: everything a user used to need kubectl
// for. Each test drives the endpoint and then reads the cluster the way the
// reconcilers would, because the writes only matter if the reconcilers see
// what they expect.

// envPath is the one URL a project's environment variables are written on. It
// is a route of its own because they are the developer's day job, where the
// project's settings next door are the admin's.
const envPath = "/api/v1/projects/" + feedProject + "/env"

func getSecret(t *testing.T, h *harness, name string) (*corev1.Secret, error) {
	t.Helper()
	secret := &corev1.Secret{}
	err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: name}, secret)
	return secret, err
}

// The project's own preview ceiling has three states and the API has to be
// able to write all three: the platform's (unset), a number, and none at all
// (0). A negative number is what clears it back to the platform's, since 0 is
// a setting here and cannot also mean "unset".
func TestAProjectsOwnPreviewCeilingHasThreeStates(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Previews.Max != nil {
		t.Fatalf("a project starts on the platform's ceiling: %+v", stored.Spec.Previews)
	}

	for _, testCase := range []struct {
		name string
		body string
		want *int32
	}{
		{"a ceiling of its own", `{"previewsMax": 3}`, ptr.To(int32(3))},
		{"no ceiling for this project", `{"previewsMax": 0}`, ptr.To(int32(0))},
		{"back to the platform's", `{"previewsMax": -1}`, nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", testCase.body)
			if recorder.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
			}
			view := decode[projectView](t, recorder)
			if (view.PreviewsMax == nil) != (testCase.want == nil) ||
				(testCase.want != nil && *view.PreviewsMax != *testCase.want) {
				t.Fatalf("the answer does not echo the ceiling: %+v", view.PreviewsMax)
			}
			if err := h.server.get(context.Background(), "shop", stored); err != nil {
				t.Fatal(err)
			}
			if (stored.Spec.Previews.Max == nil) != (testCase.want == nil) ||
				(testCase.want != nil && *stored.Spec.Previews.Max != *testCase.want) {
				t.Fatalf("the ceiling did not stick: %+v", stored.Spec.Previews)
			}
		})
	}
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

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Source.GitSource().ProductionBranch != "stable" || stored.Spec.Previews.IsEnabled() {
		t.Fatalf("the settings did not stick: %+v", stored.Spec)
	}
	if stored.Spec.Build.Strategy != kitchenv1alpha1.BuildStrategyBuildpacks ||
		stored.Spec.Build.RootDirectory != "apps/shop" {
		t.Fatalf("the build settings did not stick: %+v", stored.Spec.Build)
	}
	if stored.Spec.Previews.IsProtected() {
		t.Fatal("previewsProtected=false did not stick")
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

// The posture a project's workloads run under: written, read back resolved,
// and taken away again — the three things the dashboard's form does, since it
// sends the whole posture every time and an emptied form is how one is
// withdrawn.
func TestPatchingAProjectsSecurityPosture(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{
		"security": {"runAsNonRoot": true, "runAsUser": 1001,
		             "readOnlyRootFilesystem": true, "dropCapabilities": ["net_raw"]}
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if view.Security == nil || !view.Security.RunAsNonRoot || view.Security.RunAsUser != 1001 {
		t.Fatalf("the posture did not echo: %+v", view.Security)
	}
	if view.Security.AllowPrivilegeEscalation || view.Security.SeccompProfile != "RuntimeDefault" {
		t.Fatalf("the platform's own half of the posture is not reported: %+v", view.Security)
	}
	if len(view.Security.Declared) != 4 {
		t.Fatalf("want every constraint named for the failure message, got %v", view.Security.Declared)
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	security := stored.Spec.Runtime.Security
	if security == nil || !security.ReadOnlyRootFilesystem {
		t.Fatalf("the posture did not stick: %+v", stored.Spec.Runtime)
	}
	// Capabilities are stored the way the kernel spells them, whatever case
	// they arrived in, so the container's own list is not two spellings of
	// one capability.
	if len(security.DropCapabilities) != 1 || security.DropCapabilities[0] != "NET_RAW" {
		t.Fatalf("want the capability normalized, got %v", security.DropCapabilities)
	}

	// An empty posture is no posture: the platform's default is what an
	// absent block already means, so clearing the form clears the field.
	recorder = h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"security": {}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Runtime.Security != nil {
		t.Fatalf("an empty posture should be no posture, got %+v", stored.Spec.Runtime.Security)
	}

	// A capability the kernel has never heard of is refused with a sentence
	// rather than reaching a container that would not start.
	recorder = h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"security": {"dropCapabilities": ["CAP_NET_RAW!"]}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a capability that is not one, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// The gid that owns the volumes a non-root workload is given (#347), through
// the settings body that already carries the rest of the posture.
func TestPatchingAProjectsVolumeGroup(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{
		"security": {"runAsUser": 1001, "fsGroup": 1001,
		             "fsGroupChangePolicy": "OnRootMismatch"}
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if view.Security == nil || view.Security.FSGroup != 1001 ||
		view.Security.FSGroupChangePolicy != "OnRootMismatch" {
		t.Fatalf("the volume group did not echo: %+v", view.Security)
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	security := stored.Spec.Runtime.Security
	if security == nil || security.FSGroup != 1001 ||
		security.FSGroupChangePolicy != kitchenv1alpha1.FSGroupChangeOnRootMismatch {
		t.Fatalf("the volume group did not stick: %+v", stored.Spec.Runtime)
	}

	// A gid is a gid: there is no negative one, and the refusal says what 0
	// means rather than leaving the caller to guess.
	recorder = h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"security": {"fsGroup": -1}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a negative gid, got %d: %s", recorder.Code, recorder.Body.String())
	}

	// The change policy is when an ownership is applied, so it is refused
	// without an ownership to apply rather than stored doing nothing.
	recorder = h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"security": {"fsGroupChangePolicy": "OnRootMismatch"}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a policy with no group, got %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"security": {"fsGroup": 1001, "fsGroupChangePolicy": "Sometimes"}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a policy that is not one, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestPatchingAProjectsEnvVars(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, envPath, `{
		"env": [
			{"name": "PUBLIC_URL", "value": "https://shop.example.com", "previewValue": "preview"},
			{"name": "DATABASE_URL", "fromClaim": {"name": "shop-db", "key": "url"}},
			{"name": "API_KEY", "fromSecret": {"name": "shop-api-key", "key": "key"}}
		]
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if len(view.Env) != 3 || view.Env[1].FromClaim == nil || view.Env[2].FromSecret == nil {
		t.Fatalf("the env vars did not echo: %+v", view.Env)
	}
	// The literal variable comes back as presence, not as a value.
	if !view.Env[0].Set || !view.Env[0].PreviewSet {
		t.Fatalf("want the literal variable reported as set: %+v", view.Env[0])
	}
	if view.Env[1].Set || view.Env[2].Set {
		t.Fatalf("want reference-backed variables reported as unset: %+v", view.Env)
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Env) != 3 || stored.Spec.Env[1].FromResourceClaim == nil || stored.Spec.Env[2].SecretRef == nil {
		t.Fatalf("the env vars did not stick: %+v", stored.Spec.Env)
	}
	// And nothing else on the project moved.
	if stored.Spec.Source.GitSource().ProductionBranch != defaultProductionBranch {
		t.Fatalf("the env write touched the project's settings: %+v", stored.Spec.Source)
	}
}

// The two halves of a project are now two routes, and the settings route says
// so rather than dropping a list it no longer writes.
func TestPatchingAProjectRefusesEnvVarsAndNamesTheRouteThatTakesThem(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"previews": false, "env": [{"name": "PUBLIC_URL", "value": "https://shop.example.com"}]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(errorOf(t, recorder.Body.String()), "PATCH /projects/shop/env") {
		t.Fatalf("want the other route named, got %q", recorder.Body.String())
	}

	// A refused request changes nothing at all — including the settings it
	// carried alongside the variables.
	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Env) != 0 {
		t.Fatalf("the refused variables were written anyway: %+v", stored.Spec.Env)
	}
	if !stored.Spec.Previews.IsEnabled() {
		t.Fatal("the settings in a refused request were written anyway")
	}
}

// A body with no list at all is refused rather than read as "clear them": the
// route replaces the whole list, and an empty body is a client that forgot the
// field.
func TestPatchingEnvVarsWantsTheListAndClearsItWhenAsked(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPatch, envPath,
		`{"env": [{"name": "PUBLIC_URL", "value": "https://shop.example.com"}]}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodPatch, envPath, `{}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Env) != 1 {
		t.Fatalf("a body with no list cleared the variables: %+v", stored.Spec.Env)
	}

	// An empty list is how somebody says it on purpose.
	if recorder := h.do(t, http.MethodPatch, envPath, `{"env": []}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Env) != 0 {
		t.Fatalf("want every variable gone, got %+v", stored.Spec.Env)
	}
}

// The value of a plain env var is where somebody pastes an API key, so it is
// held to the same rule as a connection's credential: it goes in and it never
// comes back out — of the patch's own answer or of any later read.
func TestProjectEnvVarValuesAreNeverReadBack(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	const secret = "sk-live-3f9a1c-never-echo-me"
	const previewSecret = "sk-test-77b2-never-echo-me-either"

	patch := h.do(t, http.MethodPatch, envPath,
		`{"env": [{"name": "API_KEY", "value": "`+secret+`", "previewValue": "`+previewSecret+`"}]}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", patch.Code, patch.Body.String())
	}
	if body := patch.Body.String(); strings.Contains(body, secret) || strings.Contains(body, previewSecret) {
		t.Fatalf("the patch echoed the value it was given: %s", body)
	}

	for _, path := range []string{"/api/v1/projects/shop", "/api/v1/projects"} {
		recorder := h.do(t, http.MethodGet, path, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("want 200 from %s, got %d: %s", path, recorder.Code, recorder.Body.String())
		}
		if body := recorder.Body.String(); strings.Contains(body, secret) || strings.Contains(body, previewSecret) {
			t.Fatalf("%s handed the value back: %s", path, body)
		}
	}

	view := decode[projectView](t, h.do(t, http.MethodGet, "/api/v1/projects/shop", ""))
	if len(view.Env) != 1 || !view.Env[0].Set || !view.Env[0].PreviewSet {
		t.Fatalf("want the variable reported as set: %+v", view.Env)
	}

	// The value is still there for the reconciler to read; only the API stops
	// short of it.
	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Env[0].Value != secret || stored.Spec.Env[0].PreviewValue != previewSecret {
		t.Fatalf("the value did not stick: %+v", stored.Spec.Env)
	}
}

// A client can no longer read a value to write it back, so a variable whose
// value the request leaves out keeps the one it has. Sending an empty value is
// how it is cleared, because that is a thing someone had to type.
func TestPatchingEnvVarsKeepsTheValuesTheRequestOmits(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	const value = "https://shop.example.com"
	seed := h.do(t, http.MethodPatch, envPath,
		`{"env": [{"name": "PUBLIC_URL", "value": "`+value+`", "previewValue": "https://preview.invalid"}]}`)
	if seed.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", seed.Code, seed.Body.String())
	}

	// Renaming nothing and adding a variable: the untouched one keeps both of
	// its values, exactly as a UI that never saw them would send it.
	recorder := h.do(t, http.MethodPatch, envPath,
		`{"env": [{"name": "PUBLIC_URL"}, {"name": "LOG_LEVEL", "value": "debug"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Env) != 2 {
		t.Fatalf("want both variables, got %+v", stored.Spec.Env)
	}
	if stored.Spec.Env[0].Value != value || stored.Spec.Env[0].PreviewValue != "https://preview.invalid" {
		t.Fatalf("an omitted value was blanked: %+v", stored.Spec.Env[0])
	}
	if stored.Spec.Env[1].Value != "debug" {
		t.Fatalf("the new variable did not stick: %+v", stored.Spec.Env[1])
	}

	// An empty value is a value: it clears what was there.
	recorder = h.do(t, http.MethodPatch, envPath,
		`{"env": [{"name": "PUBLIC_URL", "value": "", "previewValue": ""}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Env) != 1 || stored.Spec.Env[0].Value != "" || stored.Spec.Env[0].PreviewValue != "" {
		t.Fatalf("an empty value did not clear the stored one: %+v", stored.Spec.Env)
	}
}

// Repointing a variable at a secret drops the value it used to carry: the
// reference is what replaces it, and carrying the old value forward would make
// the request name two sources.
func TestPatchingAnEnvVarToASecretDropsItsValue(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	seed := h.do(t, http.MethodPatch, envPath, `{"env": [{"name": "API_KEY", "value": "pasted"}]}`)
	if seed.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", seed.Code, seed.Body.String())
	}

	recorder := h.do(t, http.MethodPatch, envPath,
		`{"env": [{"name": "API_KEY", "fromSecret": {"name": "shop-api-key", "key": "key"}}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Env) != 1 || stored.Spec.Env[0].SecretRef == nil || stored.Spec.Env[0].Value != "" {
		t.Fatalf("want a reference and no value left: %+v", stored.Spec.Env)
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
	if stored.Spec.Source.GitSource().ProductionBranch != defaultProductionBranch || stored.Spec.Source.GitSource().Repo != "acme/shop" {
		t.Fatalf("fields that were not patched changed: %+v", stored.Spec.Source)
	}
}

func TestPatchingAProjectRejectsUnusableRequests(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, body := range map[string]string{
		"an empty production branch":       `{"productionBranch": " "}`,
		"a strategy that is not one":       `{"buildStrategy": "guess"}`,
		"a port out of range":              `{"port": 70000}`,
		"zero replicas":                    `{"replicas": 0}`,
		"a health path that is not a path": `{"health": {"path": "healthz"}}`,
		"a health port out of range":       `{"health": {"port": 70000}}`,
		"a negative health period":         `{"health": {"periodSeconds": -1}}`,
		"cpu that is not a quantity":       `{"cpu": "fast"}`,
		"an unknown field":                 `{"branch": "main"}`,
		"not JSON":                         `{`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestPatchingEnvVarsRejectsUnusableRequests(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, body := range map[string]string{
		"an env var with no name": `{"env": [{"value": "x"}]}`,
		"an env var named twice":  `{"env": [{"name": "A", "value": "1"}, {"name": "A", "value": "2"}]}`,
		"an env var with two sources": `{"env": [{"name": "A", "value": "x",
			"fromSecret": {"name": "s", "key": "k"}}]}`,
		"a project setting": `{"previews": false}`,
		"not JSON":          `{`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, envPath, body)
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

// fakeGitHub answers /user the way GitHub does: the token decides, and a good
// one comes back with a login and its scopes.
func fakeGitHub(t *testing.T, goodToken, scopes string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/user" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if req.Header.Get("Authorization") != "Bearer "+goodToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message": "Bad credentials"}`))
			return
		}
		w.Header().Set("X-OAuth-Scopes", scopes)
		_, _ = w.Write([]byte(`{"login": "octocat"}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func testConnection(t *testing.T, h *harness, body string) connectionTestView {
	t.Helper()
	recorder := h.do(t, http.MethodPost, "/api/v1/connections/test", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := connectionTestView{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func TestTestingACredentialBeforeItIsStored(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	github := fakeGitHub(t, "ghp_good", "admin:repo_hook, repo")

	view := testConnection(t, h, `{"name": "hub", "provider": "github",
		"config": {"apiUrl": "`+github.URL+`"}, "credential": {"token": "ghp_good"}}`)
	if !view.Reachable || !view.CredentialChecked || !view.CredentialValid {
		t.Fatalf("a working token did not come back green: %+v", view)
	}
	if !strings.Contains(view.Message, "octocat") {
		t.Fatalf("the verdict does not say who the token authenticates as: %+v", view)
	}
	if strings.Contains(view.Message, "ghp_good") {
		t.Fatalf("the verdict leaks the credential: %+v", view)
	}

	// A test stores nothing: the credential that was tried leaves no secret
	// behind, and the connection it was for still does not exist.
	if _, err := getSecret(t, h, connectionSecretPrefix+"hub"); err == nil {
		t.Fatal("testing a credential wrote a secret")
	}
	if err := h.server.get(context.Background(), "hub", &kitchenv1alpha1.Connection{}); err == nil {
		t.Fatal("testing a credential created a connection")
	}
}

func TestTestingACredentialThatIsShortAPermission(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	// Webhooks and nothing else: valid, and not enough for deploy reporting.
	github := fakeGitHub(t, "ghp_good", "admin:repo_hook")

	view := testConnection(t, h, `{"provider": "github",
		"config": {"apiUrl": "`+github.URL+`"}, "credential": {"token": "ghp_good"}}`)
	if !view.CredentialValid {
		t.Fatalf("a narrow token is still a working token: %+v", view)
	}
	if len(view.Warnings) == 0 || !strings.Contains(strings.Join(view.Warnings, " "), "repo:status") {
		t.Fatalf("the verdict does not say what the token cannot do: %+v", view)
	}
}

func TestTestingACredentialTheProviderRejects(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	github := fakeGitHub(t, "ghp_good", "admin:repo_hook, repo")

	view := testConnection(t, h, `{"provider": "github",
		"config": {"apiUrl": "`+github.URL+`"}, "credential": {"token": "ghp_stale"}}`)
	// Reached, ruled on, refused — the three parts have to stay distinct.
	if !view.Reachable || !view.CredentialChecked || view.CredentialValid {
		t.Fatalf("a rejected token did not read as rejected: %+v", view)
	}
	if !strings.Contains(view.Message, "Bad credentials") {
		t.Fatalf("the verdict does not carry the provider's words: %+v", view)
	}
}

func TestTestingAProviderThatCannotBeReached(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	// A port nothing listens on: unreachable is not the same as refused, and
	// the credential was never judged.
	view := testConnection(t, h, `{"provider": "github",
		"config": {"apiUrl": "http://127.0.0.1:1/api/v3"}, "credential": {"token": "ghp_good"}}`)
	if view.Reachable || view.CredentialChecked || view.CredentialValid {
		t.Fatalf("an unreachable provider did not read as unreachable: %+v", view)
	}
}

func TestTestingAStoredCredential(t *testing.T) {
	github := fakeGitHub(t, "ghp_stored", "admin:repo_hook, repo")
	connection := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "hub", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "github",
			CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: connectionSecretPrefix + "hub"},
			Config:               &runtime.RawExtension{Raw: []byte(`{"apiUrl": "` + github.URL + `"}`)},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: connectionSecretPrefix + "hub", Namespace: testNamespace},
		Data:       map[string][]byte{gitTokenKey: []byte("ghp_stored")},
	}
	h := newHarness(t, nil, append(fixtures(), connection, secret)...)

	// No credential in the request: the connection's own is re-checked, which
	// is what the edit flow does without retyping a token.
	view := testConnection(t, h, `{"name": "hub"}`)
	if !view.CredentialValid {
		t.Fatalf("the stored credential did not come back green: %+v", view)
	}
}

func TestTestingAGitLabCredential(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	gitlab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/user" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("PRIVATE-TOKEN") != "glpat-good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("denied"))
			return
		}
		_, _ = w.Write([]byte(`{"username":"alice"}`))
	}))
	defer gitlab.Close()

	view := testConnection(t, h, `{"provider": "gitlab", "credential": {"token": "glpat-good"},
		"config": {"apiUrl":"`+gitlab.URL+`/api/v4"}}`)
	if !view.Reachable || !view.CredentialChecked || !view.CredentialValid {
		t.Fatalf("a working gitlab token did not come back green: %+v", view)
	}
	if !strings.Contains(view.Message, "alice") {
		t.Fatalf("the verdict does not carry the authenticated identity: %+v", view)
	}
}

func TestTestingAConnectionRejectsUnusableRequests(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, body := range map[string]string{
		"nothing at all":                   `{}`,
		"a name nothing answers to":        `{"name": "ghost"}`,
		"a provider with no credential":    `{"provider": "github"}`,
		"an unknown provider":              `{"provider": "svn", "credential": {"token": "t"}}`,
		"a token provider without a token": `{"provider": "github", "credential": {"username": "u", "password": "p"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/connections/test", body)
			if recorder.Code != http.StatusBadRequest && recorder.Code != http.StatusNotFound {
				t.Fatalf("want 400 or 404, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
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
			CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "synced-credentials"},
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

// neonConnection is a database-capable connection, as the Connection
// reconciler leaves one once it has validated the credentials.
func neonConnection() *kitchenv1alpha1.Connection {
	return &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "neon", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "neon",
			CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "neon-credentials"},
		},
		Status: kitchenv1alpha1.ConnectionStatus{
			Capabilities: []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityDatabase},
		},
	}
}

func TestCreatingAClaim(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), neonConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "orders-db", "project": "shop", "connection": "neon", "type": "postgres", "previewMode": "branch"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[claimView](t, recorder)
	if view.Name != "orders-db" || view.Project != feedProject || view.Connection != "neon" || view.PreviewChoice != "branch" {
		t.Fatalf("the response does not echo the request: %+v", view)
	}

	stored := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(context.Background(), "orders-db", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.ProjectRef.Name != feedProject || stored.Spec.ConnectionRef.Name != "neon" || stored.Spec.Type != "postgres" {
		t.Fatalf("the claim did not stick: %+v", stored.Spec)
	}
	if stored.PreviewChoice() != "branch" {
		t.Fatal("previewMode did not reach spec.config")
	}
}

func TestCreatingAClaimWithDeletionPolicyDelete(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), neonConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "scratch-db", "project": "shop", "connection": "neon", "type": "postgres", "deletionPolicy": "Delete"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stored := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(context.Background(), "scratch-db", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.DeletionPolicy != kitchenv1alpha1.ClaimDelete {
		t.Fatalf("want deletionPolicy Delete, got %q", stored.Spec.DeletionPolicy)
	}
	if stored.PreviewChoice() != "" {
		t.Fatal("no previewMode was asked for, so the provider's own applies")
	}
}

// A claim's class narrows its project's, never widens it — and the check is
// the API's, because by the time a promotion could notice, the production
// data would already be in the wider claim.
func TestAClaimsClassMayNotExceedItsProjects(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), neonConnection())...)

	// The shop project is unclassified: a classified claim has no ceiling to
	// narrow, and the refusal says to classify the project first.
	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "orders-db", "project": "shop", "connection": "neon", "type": "postgres",
			"dataClass": "internal"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "classify the project first") {
		t.Fatalf("the refusal must say what to do, got %q", got)
	}

	// Classify the project confidential; a strictlyConfidential claim still
	// exceeds it, a confidential one narrows into it exactly.
	project := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", project); err != nil {
		t.Fatal(err)
	}
	project.Spec.DataClass = kitchenv1alpha1.DataClassConfidential
	if err := h.server.Client.Update(context.Background(), project); err != nil {
		t.Fatal(err)
	}

	recorder = h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "orders-db", "project": "shop", "connection": "neon", "type": "postgres",
			"dataClass": "strictlyConfidential"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "never exceeds") {
		t.Fatalf("the refusal must explain the rule, got %q", got)
	}

	recorder = h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "orders-db", "project": "shop", "connection": "neon", "type": "postgres",
			"dataClass": "confidential"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[claimView](t, recorder); view.DataClass != inventoryClassConfidential {
		t.Fatalf("the class must come back on the view, got %+v", view)
	}

	// A class nobody defined is refused with the vocabulary.
	recorder = h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "cache-db", "project": "shop", "connection": "neon", "type": "postgres",
			"dataClass": "secret"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "strictlyConfidential") {
		t.Fatalf("the refusal must name the vocabulary, got %q", got)
	}
}

// Reclassifying a project is always possible — the correction must never be
// refused because environments lag behind — and it is a privileged audit
// record carrying the previous value, because the class decides what the
// policy engine refuses.
func TestReclassifyingAProjectIsRecordedWithThePreviousValue(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"dataClass": "`+inventoryClassConfidential+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[projectView](t, recorder); view.DataClass != inventoryClassConfidential {
		t.Fatalf("the class must come back, got %+v", view)
	}

	recorder = h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"dataClass": "internal"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.DataClass != kitchenv1alpha1.DataClassInternal {
		t.Fatalf("the reclassification must stick, got %q", stored.Spec.DataClass)
	}

	// The record's details, held up to the light apart from the store: a
	// class change is privileged and carries the previous value.
	next := kitchenv1alpha1.DataClassInternal
	before := &kitchenv1alpha1.Project{
		Spec: kitchenv1alpha1.ProjectSpec{DataClass: kitchenv1alpha1.DataClassConfidential},
	}
	class := "internal"
	details := projectSettingsDetails(before, patchProjectRequest{DataClass: &class}, &next,
		continuityChange{})
	if details["previousDataClass"] != inventoryClassConfidential || details["dataClass"] != "internal" {
		t.Fatalf("the record must carry the previous value: %v", details)
	}
}

func TestCreatingAClaimRejectsUnusableRequests(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), neonConnection())...)

	for name, body := range map[string]string{
		"no name":                  `{"project": "shop", "connection": "neon", "type": "postgres"}`,
		"a name that is no label":  `{"name": "Orders DB", "project": "shop", "connection": "neon", "type": "postgres"}`,
		"a type nobody provisions": `{"name": "cache", "project": "shop", "connection": "neon", "type": "redis"}`,
		"no project":               `{"name": "orders-db", "connection": "neon", "type": "postgres"}`,
		"a project that is not there": `{"name": "orders-db", "project": "nope", "connection": "neon",
			"type": "postgres"}`,
		"a connection that is not there": `{"name": "orders-db", "project": "shop", "connection": "nope",
			"type": "postgres"}`,
		"a connection without the capability": `{"name": "orders-db", "project": "shop", "connection": "gh",
			"type": "postgres"}`,
		"a policy that is neither": `{"name": "orders-db", "project": "shop", "connection": "neon",
			"type": "postgres", "deletionPolicy": "Keep"}`,
		"an unknown field": `{"name": "orders-db", "project": "shop", "connection": "neon", "type": "postgres",
			"branching": true}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/claims", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestCreatingAnOIDCClientClaim(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), neonConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-auth", "project": "shop", "connection": "", "type": "oidcClient",
			"callbackPaths": ["/auth/callback"], "redirectURIs": ["http://localhost:3000/auth/callback"],
			"scopes": ["openid", "email"]}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[claimView](t, recorder)
	if view.Connection != "" {
		t.Fatalf("an oidcClient claim has no connection: %+v", view)
	}
	if len(view.CallbackPaths) != 1 || view.CallbackPaths[0] != "/auth/callback" {
		t.Fatalf("the callback paths did not come back: %+v", view.CallbackPaths)
	}

	stored := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(context.Background(), "shop-auth", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.ConnectionRef != nil {
		t.Fatalf("a Connection was named for a claim that has none: %+v", stored.Spec.ConnectionRef)
	}
	config := stored.OIDCClient()
	if len(config.Scopes) != 2 || config.Scopes[0] != "openid" {
		t.Fatalf("the scopes did not reach spec.config: %+v", config)
	}
	if len(config.RedirectURIs) != 1 {
		t.Fatalf("the verbatim redirect URIs did not reach spec.config: %+v", config)
	}
}

// TestAnOIDCClientClaimTakesThePlatformsDefaults: the promise is one claim and
// no configuration, so a request with neither paths nor scopes has to answer
// with the ones the reconciler will actually register.
func TestAnOIDCClientClaimTakesThePlatformsDefaults(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "bare-auth", "project": "shop", "connection": "", "type": "oidcClient"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[claimView](t, recorder)
	if len(view.CallbackPaths) != len(kitchenv1alpha1.DefaultOIDCCallbackPaths) {
		t.Fatalf("the defaults were not answered: %+v", view.CallbackPaths)
	}
	if len(view.Scopes) != len(kitchenv1alpha1.DefaultOIDCScopes) {
		t.Fatalf("the default scopes were not answered: %+v", view.Scopes)
	}

	stored := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(context.Background(), "bare-auth", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Config != nil {
		t.Fatal("a claim that configured nothing must not store a config")
	}
}

// TestCreatingAClaimRefusesTheOtherTypesFields: each type is refused the
// other's fields rather than ignoring them, because a claim that quietly did
// half of what it was asked is worse than one that says so.
func TestCreatingAClaimRefusesTheOtherTypesFields(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), neonConnection())...)

	for name, body := range map[string]string{
		"a connection on an oidcClient claim": `{"name": "shop-auth", "project": "shop", "connection": "neon",
			"type": "oidcClient"}`,
		"a preview mode the platform cannot give an oidcClient claim": `{"name": "shop-auth", "project": "shop",
			"connection": "", "type": "oidcClient", "previewMode": "fresh"}`,
		"a deletion policy on an oidcClient claim": `{"name": "shop-auth", "project": "shop", "connection": "",
			"type": "oidcClient", "deletionPolicy": "Delete"}`,
		"callback paths on a postgres claim": `{"name": "orders-db", "project": "shop", "connection": "neon",
			"type": "postgres", "callbackPaths": ["/auth/callback"]}`,
		"a callback path that is a URL": `{"name": "shop-auth", "project": "shop", "connection": "",
			"type": "oidcClient", "callbackPaths": ["https://shop.example.com/auth/callback"]}`,
		"a redirect URI that is a path": `{"name": "shop-auth", "project": "shop", "connection": "",
			"type": "oidcClient", "redirectURIs": ["/auth/callback"]}`,
		"scopes without openid": `{"name": "shop-auth", "project": "shop", "connection": "",
			"type": "oidcClient", "scopes": ["email"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/claims", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestCreatingAClaimUnderATakenNameIsRefused(t *testing.T) {
	// shop-db is already in the fixtures.
	h := newHarness(t, nil, append(fixtures(), neonConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-db", "project": "shop", "connection": "neon", "type": "postgres"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeletingAClaim(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodDelete, "/api/v1/claims/shop-db", "")
	// 202: the reconciler's finalizer still has the binding, branches and —
	// under deletionPolicy Delete — the instance to remove.
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := h.server.get(context.Background(), "shop-db", &kitchenv1alpha1.ResourceClaim{}); err == nil {
		t.Fatal("the claim is still there")
	}
}

func TestDeletingAClaimThatDoesNotExist(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodDelete, "/api/v1/claims/nope", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// The health check a project is probed with (#236). Every project has one
// whether or not it declared anything, so reading one back always answers
// with resolved timings rather than with silence.
func TestPatchingAProjectsHealthCheck(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	before := decode[projectView](t, h.do(t, http.MethodGet, "/api/v1/projects/shop", ""))
	if before.Health == nil || before.Health.Path != "" ||
		before.Health.PeriodSeconds != kitchenv1alpha1.DefaultProbePeriodSeconds ||
		before.Health.StartupFailureThreshold != kitchenv1alpha1.DefaultStartupFailureThreshold {
		t.Fatalf("a project that declared nothing must still report the default check: %+v", before.Health)
	}

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"health": {"path": "/healthz", "port": 9000, "failureThreshold": 5}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if view.Health == nil || view.Health.Path != "/healthz" || view.Health.Port != 9000 ||
		view.Health.FailureThreshold != 5 {
		t.Fatalf("the health check did not echo: %+v", view.Health)
	}
	// The timings nobody set come back resolved, not empty: "what is checked,
	// how often" is the question, and a blank answers it only for somebody
	// who already knows the defaults.
	if view.Health.PeriodSeconds != kitchenv1alpha1.DefaultProbePeriodSeconds {
		t.Errorf("want the default period resolved, got %d", view.Health.PeriodSeconds)
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Runtime.Health == nil || stored.Spec.Runtime.Health.Path != "/healthz" {
		t.Fatalf("the health check did not stick: %+v", stored.Spec.Runtime)
	}

	// An empty object is how a declared check is taken back off — it is
	// exactly the default one, so no removal sentinel is needed.
	if code := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"health": {}}`).Code; code != http.StatusOK {
		t.Fatalf("want 200 restoring the default check, got %d", code)
	}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Runtime.Health.HTTPPath() != "" {
		t.Fatalf("an empty health check must clear the path: %+v", stored.Spec.Runtime.Health)
	}
}

// A workload two of which must never run at once (#239). The combination is
// refused rather than clamped: a replica count quietly lowered reads back as
// a setting that did not take.
func TestASingletonProjectRefusesMoreThanOneReplica(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	patch := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		return h.do(t, http.MethodPatch, "/api/v1/projects/shop", body)
	}

	if code := patch(`{"singleton": true}`).Code; code != http.StatusOK {
		t.Fatalf("want 200 declaring a singleton, got %d", code)
	}
	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Spec.Runtime.Singleton {
		t.Fatalf("the declaration did not stick: %+v", stored.Spec.Runtime)
	}

	for name, body := range map[string]string{
		"raising the replicas of a project already declared a singleton": `{"replicas": 3}`,
		"declaring one while raising the replicas in the same request":   `{"singleton": true, "replicas": 3}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := patch(body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			// The refusal has to say what to do about it, since the caller
			// sent one of two fields and the other one is the problem.
			if !strings.Contains(recorder.Body.String(), "singleton") {
				t.Errorf("the refusal does not name the declaration: %s", recorder.Body.String())
			}
		})
	}

	// Turning the declaration off in the same request that raises the count
	// is not the refused combination.
	if code := patch(`{"singleton": false, "replicas": 3}`).Code; code != http.StatusOK {
		t.Fatalf("want 200 turning it off and scaling out, got %d", code)
	}
}

// A workload that does work nobody asked for (#240). It is a project setting
// like the rest, and it reads back so the dashboard can say why an
// environment does not idle.
func TestDeclaringAWorkloadNotRequestDriven(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	before := decode[projectView](t, h.do(t, http.MethodGet, "/api/v1/projects/shop", ""))
	if before.NotRequestDriven {
		t.Fatal("a project that said nothing must not read as not request-driven")
	}

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"notRequestDriven": true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !decode[projectView](t, recorder).NotRequestDriven {
		t.Fatal("the declaration did not echo")
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Spec.Runtime.NotRequestDriven {
		t.Fatalf("the declaration did not stick: %+v", stored.Spec.Runtime)
	}
}

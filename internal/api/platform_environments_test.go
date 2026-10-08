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
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

func TestPlatformEnvironmentsCRUD(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	path := "/api/v1/platform/environments"

	created := h.do(t, http.MethodPost, path, `{
		"name":"prod-shared",
		"owners":["risk@example.com"],
		"requirements":{"bundleDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"serves":["stage","preview"],
		"dataClass":"confidential",
		"residency":"CH",
		"criticality":"critical",
		"rto":"15m",
		"rpo":"5m"
	}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", created.Code, created.Body.String())
	}

	got := decode[platformEnvironmentView](t, created)
	if got.Name != "prod-shared" || got.DataClass != "confidential" || len(got.Serves) != 2 {
		t.Fatalf("unexpected platform environment view: %+v", got)
	}

	list := decode[listBody[platformEnvironmentView]](t, h.do(t, http.MethodGet, path, ""))
	if len(list.Items) == 0 {
		t.Fatal("the created platform environment is not listed")
	}

	patched := h.do(t, http.MethodPatch, path+"/prod-shared", `{"serves":["stage"],"rto":"30m"}`)
	if patched.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", patched.Code, patched.Body.String())
	}
	view := decode[platformEnvironmentView](t, patched)
	if len(view.Serves) != 1 || view.Serves[0] != "stage" || view.RTO != "30m" {
		t.Fatalf("patch did not land: %+v", view)
	}
}

func TestDeclaredEnvironmentBindsToPlatformEnvironment(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	if recorder := h.do(t, http.MethodPost, "/api/v1/platform/environments", `{
		"name":"stage",
		"owners":["risk@example.com"],
		"requirements":{"bundleDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		"dataClass":"internal"
	}`); recorder.Code != http.StatusCreated {
		t.Fatalf("creating platform environment failed: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/environments", `{
		"name":"shop-staging",
		"policyEnvironment":"stage"
	}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("declaring environment failed: %d %s", recorder.Code, recorder.Body.String())
	}

	env := &kitchenv1alpha1.Environment{}
	if err := h.server.get(context.Background(), "shop-staging", env); err != nil {
		t.Fatal(err)
	}
	if env.Spec.PolicyEnvironmentRef == nil || env.Spec.PolicyEnvironmentRef.Name != "stage" {
		t.Fatalf("binding missing: %+v", env.Spec.PolicyEnvironmentRef)
	}
	if env.Spec.Requirements == nil || env.Spec.Requirements.BundleDigest == "" {
		t.Fatalf("bound policy was not copied onto environment: %+v", env.Spec.Requirements)
	}
}

func TestBoundEnvironmentRequirementsWriteMovesToPlatformEndpoint(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	if recorder := h.do(t, http.MethodPost, "/api/v1/platform/environments", `{"name":"stage"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("creating platform environment failed: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/environments", `{
		"name":"shop-staging",
		"policyEnvironment":"stage"
	}`); recorder.Code != http.StatusCreated {
		t.Fatalf("declaring environment failed: %d %s", recorder.Code, recorder.Body.String())
	}

	refused := h.do(t, http.MethodPatch, "/api/v1/environments/shop-staging/requirements",
		`{"bundleDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", refused.Code, refused.Body.String())
	}
}

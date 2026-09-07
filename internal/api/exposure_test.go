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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The two words, as the tests say them. `internal` is five occurrences of one
// string otherwise, which is what a constant is for.
const (
	exposurePublicWord   = "public"
	exposureInternalWord = "internal"
)

// Whether a project is on the internet at all (#492). It is one field, two
// words and three refusals, and the refusals are the point: an internal
// project that quietly acquired a route, a certificate or an OAuth client
// would be the setting failing silently, which is the failure mode it exists
// to remove.

func TestAProjectsExposureIsWritableAndAlwaysReadBack(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if view := decode[projectView](t, h.do(t, http.MethodGet, "/api/v1/projects/"+feedProject, "")); view.Exposure != exposurePublicWord {
		t.Fatalf("a project written before the field existed reads as public, got %q", view.Exposure)
	}

	for _, testCase := range []struct {
		name string
		body string
		code int
		want kitchenv1alpha1.ProjectExposure
	}{
		{"internal is allowed", `{"exposure": "internal"}`, http.StatusOK, kitchenv1alpha1.ExposureInternal},
		{"and back to public", `{"exposure": "public"}`, http.StatusOK, kitchenv1alpha1.ExposurePublic},
		{"anything else is refused", `{"exposure": "private"}`, http.StatusBadRequest, kitchenv1alpha1.ExposurePublic},
		{"and so is an empty string", `{"exposure": ""}`, http.StatusBadRequest, kitchenv1alpha1.ExposurePublic},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+feedProject, testCase.body)
			if recorder.Code != testCase.code {
				t.Fatalf("want %d, got %d: %s", testCase.code, recorder.Code, recorder.Body.String())
			}
			stored := &kitchenv1alpha1.Project{}
			if err := h.server.get(context.Background(), feedProject, stored); err != nil {
				t.Fatal(err)
			}
			if got := stored.Spec.Exposure.Normalized(); got != testCase.want {
				t.Fatalf("the setting is %q, want %q", got, testCase.want)
			}
		})
	}
}

// An empty exposure on the *create* is the CRD's default rather than a
// refusal: the field is optional there, where on the PATCH an empty string is
// somebody sending a value they did not mean.
func TestCreatingAnInternalProject(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name": "duckdb", "repo": "acme/duckdb", "connection": "gh", "registry": "registry",
			"exposure": "internal"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[projectView](t, recorder); view.Exposure != exposureInternalWord {
		t.Fatalf("the create does not echo the exposure it was given: %q", view.Exposure)
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "duckdb", stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Spec.Exposure.IsInternal() {
		t.Fatalf("the project was created public: %q", stored.Spec.Exposure)
	}

	if recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name": "sideways", "repo": "acme/sideways", "connection": "gh", "registry": "registry",
			"exposure": "sideways"}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unknown exposure must be refused at the create too: %d", recorder.Code)
	}
}

// A custom domain rides the environment's own route, and an internal project
// has none. Creating one would be the single route `exposure: internal` exists
// to prevent — so it is refused, in words that name the setting.
func TestADomainIsRefusedOnAnInternalProject(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+feedProject,
		`{"exposure": "internal"}`); recorder.Code != http.StatusOK {
		t.Fatalf("turning the project internal failed: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodPost, "/api/v1/domains",
		`{"hostname": "store.example.net", "environment": "`+testEnvironment+`", "tls": "acme"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "exposure") {
		t.Fatalf("the refusal does not name the setting behind it: %s", recorder.Body.String())
	}

	refused := &kitchenv1alpha1.Domain{}
	if err := h.server.get(context.Background(), "store-example-net", refused); err == nil {
		t.Fatal("a refused domain must not have been created")
	}

	// And it is the setting rather than the hostname: public again, and the
	// same request is an ordinary attachment.
	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+feedProject,
		`{"exposure": "public"}`); recorder.Code != http.StatusOK {
		t.Fatalf("turning the project public again failed: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodPost, "/api/v1/domains",
		`{"hostname": "store.example.net", "environment": "`+testEnvironment+`", "tls": "acme"}`,
	); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201 once the project is public again, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// An oidcClient claim registers redirect URIs built from the addresses the
// project's environments are published at, and an internal project is
// published at none. Half-registering it would be an OAuth client that works
// from a laptop and nowhere the application runs.
func TestAnOIDCClientClaimIsRefusedOnAnInternalProject(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+feedProject,
		`{"exposure": "internal"}`); recorder.Code != http.StatusOK {
		t.Fatalf("turning the project internal failed: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-auth", "project": "`+feedProject+`", "connection": "", "type": "oidcClient",
			"callbackPaths": ["/auth/callback"]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "exposure") || !strings.Contains(body, "redirect URIs") {
		t.Fatalf("the refusal has to say what could not be registered and why: %s", body)
	}

	stored := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(context.Background(), "shop-auth", stored); err == nil {
		t.Fatal("a refused claim must not have been created")
	}
}

// Every environment carries its project's exposure, because that is where it
// is read: a row with no URL is either an environment of an internal project
// or one still waiting on a route, and only the second is a fault.
func TestAnEnvironmentCarriesItsProjectsExposure(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	one := decode[environmentView](t, h.do(t, http.MethodGet, "/api/v1/environments/"+testEnvironment, ""))
	if one.Exposure != exposurePublicWord {
		t.Fatalf("an environment of a project written before the field existed reads as public, got %q", one.Exposure)
	}

	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+feedProject,
		`{"exposure": "internal"}`); recorder.Code != http.StatusOK {
		t.Fatalf("turning the project internal failed: %d %s", recorder.Code, recorder.Body.String())
	}

	one = decode[environmentView](t, h.do(t, http.MethodGet, "/api/v1/environments/"+testEnvironment, ""))
	if one.Exposure != exposureInternalWord {
		t.Fatalf("the single environment does not carry it: %q", one.Exposure)
	}
	listed := decode[struct {
		Items []environmentView `json:"items"`
	}](t, h.do(t, http.MethodGet, "/api/v1/environments", ""))
	if len(listed.Items) == 0 {
		t.Fatal("no environments were listed")
	}
	for _, view := range listed.Items {
		if view.Project == feedProject && view.Exposure != exposureInternalWord {
			t.Fatalf("a listed environment does not carry it: %+v", view)
		}
	}
}

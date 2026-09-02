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
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/inngest"
)

func inngestConnection() *kitchenv1alpha1.Connection {
	return &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: inngest.ProviderCloud, Namespace: testNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             inngest.ProviderCloud,
			CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "inngest-credentials"},
		},
		Status: kitchenv1alpha1.ConnectionStatus{
			Capabilities: []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityBackgroundJobs},
		},
	}
}

// An inngest claim names its app, the Inngest environment production reads,
// and the mode; the answer fills the defaults in so nothing reads "unset".
func TestAnInngestClaimNamesItsAppAndEnvironment(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), inngestConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "jobs", "project": "shop", "connection": "inngest", "type": "inngest",
		  "inngest": {"app": "shop-worker", "environment": "staging"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[claimView](t, recorder)
	if view.Inngest == nil || view.Inngest.App != "shop-worker" || view.Inngest.Environment != "staging" ||
		view.Inngest.Mode != "connect" {
		t.Fatalf("the answer must carry the app, the environment and the mode with defaults filled in: %+v", view.Inngest)
	}
	if view.Connection != inngest.ProviderCloud || view.DeletionPolicy != "" {
		t.Fatalf("an inngest claim binds through its connection and takes no deletion policy: %+v", view)
	}

	// Every default at once: the claim's name is the app.
	recorder = h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "jobs-2", "project": "shop", "connection": "inngest", "type": "inngest"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view = decode[claimView](t, recorder)
	if view.Inngest == nil || view.Inngest.App != "jobs-2" || view.Inngest.Environment != "production" {
		t.Fatalf("the defaults must be answered: %+v", view.Inngest)
	}
}

// Only connect is provisioned, and the refusal says why serve is not; a
// Connection without the capability, and a deletionPolicy on a type that
// holds no data, are refused the same way every type's are.
func TestAnInngestClaimIsRefusedWhatCannotBeProvisioned(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), inngestConnection(), neonConnection())...)

	for _, testCase := range []struct {
		name, body, says string
	}{
		{"serve mode", `"connection": "inngest", "type": "inngest", "inngest": {"mode": "serve"}`, "login page"},
		{"an app that is not an ID", `"connection": "inngest", "type": "inngest", "inngest": {"app": "Shop Worker"}`,
			"inngest.app"},
		{"a connection without the capability", `"connection": "neon", "type": "inngest"`, "backgroundJobs"},
		{"a deletion policy", `"connection": "inngest", "type": "inngest", "deletionPolicy": "Delete"`,
			"takes no deletionPolicy"},
		{"a mode the provider cannot give", `"connection": "inngest", "type": "inngest", "previewMode": "fresh"`,
			"it gives branch"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/claims",
				`{"name": "refused", "project": "shop", `+testCase.body+`}`)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, testCase.says) {
				t.Fatalf("the refusal does not say %q: %q", testCase.says, got)
			}
		})
	}
}

// The catalogue says what an inngest claim costs before one exists: a
// branch environment per preview, and no scale to zero.
func TestTheCatalogueDeclaresInngest(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/claim-types", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	for _, view := range decode[[]claimTypeView](t, recorder) {
		if view.Type != kitchenv1alpha1.ClaimTypeInngest {
			continue
		}
		if view.Capability != "backgroundJobs" || view.HoldsData || view.Resource != "Inngest app" {
			t.Fatalf("inngest is a backgroundJobs type that holds no data: %+v", view)
		}
		if len(view.Providers) != 1 || view.Providers[0].Provider != inngest.ProviderCloud ||
			view.Providers[0].PreviewMode != string(contract.PreviewBranch) || !view.Providers[0].KeepsPodsRunning ||
			!strings.Contains(view.Providers[0].WorkloadNote, "every environment of the project") {
			t.Fatalf("Inngest Cloud branches previews and holds every environment's pods up: %+v", view.Providers)
		}
		return
	}
	t.Fatal("the catalogue does not list inngest")
}

// Deleting says what happens: the preview environments are archived and
// nothing at Inngest is destroyed.
func TestDeletingAnInngestClaimSaysWhatStays(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), inngestConnection())...)
	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "jobs", "project": "shop", "connection": "inngest", "type": "inngest"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = h.do(t, http.MethodDelete, "/api/v1/claims/jobs", "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if outcome := (inngestClaimShaper{}).deletionOutcome(nil); !strings.Contains(outcome, "archived") ||
		!strings.Contains(outcome, "stay at Inngest") {
		t.Fatalf("the outcome must say the branches are archived and the app stays: %q", outcome)
	}
}

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
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/ui"
)

func TestReadingTheSettings(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodGet, "/api/v1/settings", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[settingsView](t, recorder)
	if body.BaseDomain != "apps.example.com" {
		t.Fatalf("want the base domain, got %+v", body)
	}
	if !body.AuthEnabled || body.AuthHost == "" {
		t.Fatalf("want the identity provider reported, got %+v", body)
	}
	if body.APIExternalURL != "https://kitchen.apps.example.com" {
		t.Fatalf("want the derived external URL, got %q", body.APIExternalURL)
	}
}

func TestChangingTheSettings(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodPatch, "/api/v1/settings",
		`{"buildStrategy": "dockerfile", "buildConcurrency": 4, "releaseRetention": 25, "logRetentionDays": 7}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[settingsView](t, recorder)
	if body.BuildStrategy != "dockerfile" || body.BuildConcurrency != 4 ||
		body.ReleaseRetention != 25 || body.LogRetentionDays != 7 {
		t.Fatalf("the answer does not carry the change: %+v", body)
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if kitchen.Spec.Builds.DefaultStrategy != kitchenv1alpha1.BuildStrategyDockerfile ||
		kitchen.Spec.Builds.Concurrency != 4 ||
		kitchen.Spec.Builds.ReleaseRetention != 25 ||
		kitchen.Spec.Observability.ClickHouse.RetentionDays != 7 {
		t.Fatalf("the singleton was not updated: %+v", kitchen.Spec)
	}
}

func TestChangingTheSettingsLeavesOmittedFieldsAlone(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodPatch, "/api/v1/settings", `{"buildConcurrency": 3}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if kitchen.Spec.Builds.Concurrency != 3 {
		t.Fatalf("the concurrency was not updated: %+v", kitchen.Spec.Builds)
	}
	if kitchen.Spec.Builds.DefaultStrategy != "" {
		t.Fatalf("an omitted field was changed: %+v", kitchen.Spec.Builds)
	}
}

// Zero is the one number here that means "no bound" rather than "unset": the
// platform keeps every release a project ever built, which is what it did
// before there was a count at all.
func TestChangingTheSettingsAcceptsUnboundedReleases(t *testing.T) {
	h := newHarness(t, nil)

	if recorder := h.do(t, http.MethodPatch, "/api/v1/settings", `{"releaseRetention": 5}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder := h.do(t, http.MethodPatch, "/api/v1/settings", `{"releaseRetention": 0}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if kitchen.Spec.Builds.ReleaseRetention != 0 {
		t.Fatalf("the count was not cleared: %+v", kitchen.Spec.Builds)
	}
}

func TestChangingTheSettingsRejectsNonsense(t *testing.T) {
	h := newHarness(t, nil)

	for name, body := range map[string]string{
		"an unknown strategy":      `{"buildStrategy": "guess"}`,
		"zero concurrency":         `{"buildConcurrency": 0}`,
		"no retention at all":      `{"logRetentionDays": 0}`,
		"a negative release count": `{"releaseRetention": -1}`,
		"a field it never knew":    `{"baseDomain": "elsewhere.example.com"}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, "/api/v1/settings", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestTheDashboardIsServedNextToTheAPI proves the split: everything under
// /api/ keeps its token check while the SPA and its bootstrap configuration
// are public.
func TestTheDashboardIsServedNextToTheAPI(t *testing.T) {
	h := newHarness(t, nil)
	h.server.UI = ui.Handler(UIConfig(h.server.Client, "kitchen-ui"))
	handler := h.server.Handler()

	// The SPA answers anonymously, on deep links too.
	for _, path := range []string{"/", "/projects/shop", "/auth/callback"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", path, recorder.Code)
		}
		if !strings.Contains(recorder.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: want the app shell, got %q", path, recorder.Header().Get("Content-Type"))
		}
	}

	// Its bootstrap configuration says where to sign in, and for what.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/config.json", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("config.json: want 200, got %d", recorder.Code)
	}
	config := decode[ui.Config](t, recorder)
	if config.ClientID != "kitchen-ui" || config.Issuer == "" ||
		config.APIURL != "https://kitchen.apps.example.com" {
		t.Fatalf("the config does not add up: %+v", config)
	}

	// The API next door still refuses an anonymous caller.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("the API answered an anonymous caller: %d", recorder.Code)
	}
}

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
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
)

// The catalogue answers every registered type with every provider's
// declaration, so the dashboard can say what a preview will get before the
// claim exists.
func TestListingClaimTypes(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/claim-types", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	types := decode[[]claimTypeView](t, recorder)
	if len(types) != len(kitchenv1alpha1.ClaimTypes) {
		t.Fatalf("want every registered type, got %d of %d", len(types), len(kitchenv1alpha1.ClaimTypes))
	}
	byName := map[string]claimTypeView{}
	for _, view := range types {
		byName[view.Type] = view
		if len(view.Providers) == 0 {
			t.Errorf("type %q lists no provider that can fulfil it", view.Type)
		}
		for _, provider := range view.Providers {
			if provider.PreviewMode == "" || provider.PreviewNote == "" {
				t.Errorf("%s via %s declares no preview mode, or no reason", view.Type, provider.Provider)
			}
			if !slices.Contains(provider.PreviewChoices, string(contract.PreviewShared)) ||
				!slices.Contains(provider.PreviewChoices, string(contract.PreviewNone)) {
				t.Errorf("%s via %s must offer shared and none among %v", view.Type, provider.Provider, provider.PreviewChoices)
			}
			if provider.PreviewChoices[0] != provider.PreviewMode {
				t.Errorf("%s via %s lists its own mode first: %v", view.Type, provider.Provider, provider.PreviewChoices)
			}
		}
	}
	postgres := byName["postgres"]
	if postgres.Capability != "database" || !postgres.HoldsData {
		t.Errorf("postgres is a data-holding type through a database connection: %+v", postgres)
	}
	modes := map[string]string{}
	for _, provider := range postgres.Providers {
		modes[provider.Provider] = provider.PreviewMode
	}
	if modes["neon"] != string(contract.PreviewBranch) || modes["cnpg"] != string(contract.PreviewFresh) {
		t.Errorf("Neon branches and CloudNativePG gives a fresh database: %v", modes)
	}
	oidc := byName["oidcClient"]
	if oidc.Capability != "" || oidc.HoldsData || oidc.Providers[0].PreviewMode != string(contract.PreviewShared) {
		t.Errorf("oidcClient takes no connection, holds no data and shares its one client: %+v", oidc)
	}
}

// A claim asks for a preview mode its provider declares, or for shared or
// none; anything else is refused with what the provider does give.
func TestAClaimsPreviewModeIsCheckedAgainstTheProvider(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), neonConnection(), cnpgConnection())...)

	for i, testCase := range []struct {
		name, body string
		want       int
		says       string
	}{
		{"the provider's own mode", `"connection": "neon", "type": "postgres", "previewMode": "branch"`, http.StatusCreated, ""},
		{"shared, by name", `"connection": "postgres", "type": "postgres", "previewMode": "shared"`, http.StatusCreated, ""},
		{"none", `"connection": "neon", "type": "postgres", "previewMode": "none"`, http.StatusCreated, ""},
		{"a mode this provider cannot give", `"connection": "neon", "type": "postgres", "previewMode": "fresh"`,
			http.StatusBadRequest, "it gives branch"},
		{"a mode that is not one", `"connection": "neon", "type": "postgres", "previewMode": "copy"`,
			http.StatusBadRequest, "branch, fresh, shared, none"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/claims",
				fmt.Sprintf(`{"name": "pm-%d", "project": "shop", %s}`, i, testCase.body))
			if recorder.Code != testCase.want {
				t.Fatalf("want %d, got %d: %s", testCase.want, recorder.Code, recorder.Body.String())
			}
			if testCase.says != "" {
				if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, testCase.says) {
					t.Fatalf("the refusal does not say %q: %q", testCase.says, got)
				}
			}
		})
	}
}

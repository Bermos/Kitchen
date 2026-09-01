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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// TestEveryClaimTypeHasAShaper is the API's half of what makes a claim type
// a registration rather than a branch: a row in kitchenv1alpha1.ClaimTypes
// without a shaper here would be a type the reconciler provisions and the
// API refuses to create.
func TestEveryClaimTypeHasAShaper(t *testing.T) {
	for _, claimType := range kitchenv1alpha1.ClaimTypes {
		shaper, ok := claimShapers[claimType.Name]
		if !ok {
			t.Errorf("claim type %q is in kitchenv1alpha1.ClaimTypes but has no shaper in claimShapers", claimType.Name)
			continue
		}
		for _, field := range shaper.fields() {
			if field.name == "" || field.set == nil || field.lacks == "" {
				t.Errorf("claim type %q declares a field without a name, a test or a reason: %+v", claimType.Name, field)
			}
		}
	}
	for name := range claimShapers {
		if _, ok := kitchenv1alpha1.LookupClaimType(name); !ok {
			t.Errorf("claimShapers registers %q, which kitchenv1alpha1.ClaimTypes does not admit", name)
		}
	}
}

// A field of one type sent on a claim of another is refused with a sentence
// naming whose it is — for every pair, not only the two the platform
// started with.
func TestAClaimIsRefusedAnotherTypesFields(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), cnpgConnection())...)

	for _, claimType := range kitchenv1alpha1.ClaimTypes {
		for _, other := range kitchenv1alpha1.ClaimTypes {
			if other.Name == claimType.Name {
				continue
			}
			for _, field := range claimShapers[other.Name].fields() {
				t.Run(claimType.Name+" sent "+field.name, func(t *testing.T) {
					connection := ""
					if claimType.TakesConnection() {
						connection = cnpgConnection().Name
					}
					recorder := h.do(t, http.MethodPost, "/api/v1/claims",
						`{"name": "cross", "project": "shop", "connection": "`+connection+`", "type": "`+
							claimType.Name+`", `+sampleField(field.name)+`}`)
					if recorder.Code != http.StatusBadRequest {
						t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
					}
					got := errorOf(t, recorder.Body.String())
					if !strings.Contains(got, field.name) || !strings.Contains(got, other.Name) {
						t.Fatalf("the refusal names neither the field nor whose it is: %q", got)
					}
				})
			}
		}
	}
}

// sampleField is a set value for each type-specific request field, so the
// cross-type refusal can be exercised without knowing what the field means.
func sampleField(name string) string {
	switch name {
	case kitchenv1alpha1.ClaimTypePostgres:
		return `"postgres": {"version": "17"}`
	case "callbackPaths":
		return `"callbackPaths": ["/auth/callback"]`
	case "redirectURIs":
		return `"redirectURIs": ["http://localhost:3000/auth/callback"]`
	case "scopes":
		return `"scopes": ["openid"]`
	default:
		return `"` + name + `": true`
	}
}

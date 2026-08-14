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

package controller

import (
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/previewgate"
)

// Every URL the platform hands out has to name a scheme the shared Gateway
// serves. In TLS mode "none" the Gateway gets an HTTP listener and nothing
// else, so an https URL there — a webhook address given to a git provider, a
// redirect URI registered at the identity provider — points at a port nothing
// is listening on.
func TestGeneratedURLsFollowTheTLSMode(t *testing.T) {
	for name, want := range map[kitchenv1alpha1.TLSMode]struct {
		api      string
		callback string
	}{
		kitchenv1alpha1.TLSModeACME: {
			api:      "https://kitchen.apps.example.com",
			callback: "https://previews.apps.example.com" + previewgate.CallbackPath,
		},
		// The tunnel terminates TLS at the edge, so the platform is still
		// reached over https even though the Gateway listens on HTTP.
		kitchenv1alpha1.TLSModeCloudflared: {
			api:      "https://kitchen.apps.example.com",
			callback: "https://previews.apps.example.com" + previewgate.CallbackPath,
		},
		kitchenv1alpha1.TLSModeNone: {
			api:      "http://kitchen.apps.example.com",
			callback: "http://previews.apps.example.com" + previewgate.CallbackPath,
		},
	} {
		t.Run(string(name), func(t *testing.T) {
			kitchen := &kitchenv1alpha1.Kitchen{Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        kitchenv1alpha1.TLSSpec{Mode: name},
			}}
			if got := apiExternalURL(kitchen); got != want.api {
				t.Errorf("want the API at %q, got %q", want.api, got)
			}
			if got := previewGateCallbackURL(kitchen); got != want.callback {
				t.Errorf("want the gate's callback at %q, got %q", want.callback, got)
			}
		})
	}
}

// An explicit external URL is taken as given: it is the one place an operator
// can say the platform is reached over something other than what the Gateway
// itself terminates.
func TestAnExplicitExternalURLKeepsItsScheme(t *testing.T) {
	kitchen := &kitchenv1alpha1.Kitchen{Spec: kitchenv1alpha1.KitchenSpec{
		BaseDomain: "apps.example.com",
		API:        kitchenv1alpha1.APISpec{ExternalURL: "https://kitchen.example.com"},
		TLS:        kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeNone},
	}}
	if got := apiExternalURL(kitchen); got != "https://kitchen.example.com" {
		t.Fatalf("want the configured URL untouched, got %q", got)
	}
}

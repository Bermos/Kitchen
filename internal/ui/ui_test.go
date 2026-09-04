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

package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Bermos/Kitchen/internal/version"
)

func getConfig(t *testing.T, handler http.Handler) (*httptest.ResponseRecorder, Config) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/config.json", nil))
	cfg := Config{}
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &cfg); err != nil {
			t.Fatalf("decoding /config.json: %v", err)
		}
	}
	return recorder, cfg
}

func TestConfigCarriesTheBuiltVersion(t *testing.T) {
	original := version.Version
	version.Version = "1.2.3"
	t.Cleanup(func() { version.Version = original })

	// The resolver supplies what it reads off the Kitchen singleton, and
	// deliberately no version: the version belongs to the binary, so the
	// handler stamps it whatever the resolver returns.
	handler := Handler(func(context.Context) (Config, error) {
		return Config{Issuer: "https://auth.apps.example.com", ClientID: "kitchen-ui"}, nil
	})

	recorder, cfg := getConfig(t, handler)
	if recorder.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", recorder.Code)
	}
	if cfg.Version != "1.2.3" {
		t.Errorf("got version %q, want %q", cfg.Version, "1.2.3")
	}
	if cfg.Issuer != "https://auth.apps.example.com" {
		t.Errorf("the resolver's fields were lost: %+v", cfg)
	}
}

func TestConfigIsUnavailableBeforeThePlatformIsConfigured(t *testing.T) {
	handler := Handler(func(context.Context) (Config, error) {
		return Config{}, errors.New("no Kitchen singleton yet")
	})

	recorder, _ := getConfig(t, handler)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want 503", recorder.Code)
	}
}

// configuredPlatform is what a configured platform's resolver answers with:
// an issuer and an API on two hosts, so a policy that collapsed them into one
// would show.
func configuredPlatform() Config {
	return Config{
		Issuer:   "https://auth.apps.example.com",
		ClientID: "kitchen-ui",
		APIURL:   "https://kitchen.apps.example.com",
	}
}

// TestEveryDashboardResponseIsHardened walks every kind of response the
// handler can produce — the app shell, a deep link into it, a static file and
// the bootstrap config — because a policy sent on the shell alone is a policy
// the browser applies to one page rather than to the origin.
func TestEveryDashboardResponseIsHardened(t *testing.T) {
	handler := Handler(func(context.Context) (Config, error) { return configuredPlatform(), nil })

	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'; script-src 'self'; " +
			"style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; " +
			"connect-src 'self' https://kitchen.apps.example.com https://auth.apps.example.com; " +
			"frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "no-referrer",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
	}

	// ".gitkeep" is the one file a source checkout embeds — the built
	// dashboard is copied in at image build time — so it is what stands in
	// for an asset and proves the static-file branch is covered too.
	for _, path := range []string{"/", "/index.html", "/projects/shop", "/.gitkeep", "/config.json"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("got status %d, want 200", recorder.Code)
			}
			for header, value := range want {
				if got := recorder.Header().Get(header); got != value {
					t.Errorf("%s: got %q, want %q", header, got, value)
				}
			}
			if recorder.Header().Get("Permissions-Policy") == "" {
				t.Error("no Permissions-Policy")
			}
		})
	}
}

// TestThePolicyNamesOriginsAndNotURLs pins the shape a CSP source list takes:
// a scheme and a host, never the path a configured URL may carry.
func TestThePolicyNamesOriginsAndNotURLs(t *testing.T) {
	policy := contentSecurityPolicy(Config{
		APIURL: "https://kitchen.apps.example.com/base/",
		Issuer: "https://auth.apps.example.com/oidc",
	})
	if !strings.Contains(policy, "connect-src 'self' https://kitchen.apps.example.com https://auth.apps.example.com;") {
		t.Errorf("the paths reached the policy: %s", policy)
	}
}

// TestThePolicyRepeatsNoOrigin covers the ordinary installation where the
// issuer is served by the same host as the dashboard: a duplicate source is
// harmless but says the policy was assembled without looking.
func TestThePolicyRepeatsNoOrigin(t *testing.T) {
	policy := contentSecurityPolicy(Config{
		APIURL: "https://kitchen.apps.example.com",
		Issuer: "https://kitchen.apps.example.com",
	})
	if strings.Count(policy, "https://kitchen.apps.example.com") != 1 {
		t.Errorf("the origin is named twice: %s", policy)
	}
}

// TestHSTSFollowsThePlatformScheme: `tls.mode: none` publishes http:// URLs,
// and pinning HSTS on a host with no certificate locks the installation out
// of its own dashboard.
func TestHSTSFollowsThePlatformScheme(t *testing.T) {
	for name, test := range map[string]struct {
		apiURL string
		want   string
	}{
		"https": {apiURL: "https://kitchen.apps.example.com", want: hstsMaxAge},
		"http":  {apiURL: "http://kitchen.apps.example.com", want: ""},
		"unset": {apiURL: "", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			header := http.Header{}
			setSecurityHeaders(header, Config{APIURL: test.apiURL})
			if got := header.Get("Strict-Transport-Security"); got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

// TestAnUnconfiguredPlatformIsStillHardened: the config resolver answers with
// nothing until the singleton exists, and the response that says so is served
// from the same origin as everything else.
func TestAnUnconfiguredPlatformIsStillHardened(t *testing.T) {
	handler := Handler(func(context.Context) (Config, error) {
		return Config{}, errors.New("no Kitchen singleton yet")
	})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	policy := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "connect-src 'self';") {
		t.Errorf("an unresolved config widened the policy: %s", policy)
	}
	if recorder.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("the response is framable")
	}
}

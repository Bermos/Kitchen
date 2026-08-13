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

package previewgate

import (
	"strings"
	"testing"
	"time"
)

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func completeEnv() map[string]string {
	return map[string]string{
		"KITCHEN_GATE_ISSUER":        "https://auth.apps.example.com",
		"KITCHEN_GATE_CLIENT_ID":     "gate",
		"KITCHEN_GATE_CLIENT_SECRET": "shhh",
		"KITCHEN_GATE_CALLBACK_URL":  "https://previews.apps.example.com" + CallbackPath,
		"KITCHEN_GATE_COOKIE_SECRET": testSecret,
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(lookupFrom(completeEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != DefaultAddr || cfg.HealthAddr != DefaultHealthAddr {
		t.Fatalf("unexpected listeners: %s, %s", cfg.Addr, cfg.HealthAddr)
	}
	if cfg.SessionTTL != DefaultSessionTTL || cfg.Scopes != DefaultScopes {
		t.Fatalf("unexpected session defaults: %s, %s", cfg.SessionTTL, cfg.Scopes)
	}
	if !cfg.CookieSecure {
		t.Fatal("cookies must be Secure unless an installation says otherwise")
	}
	if cfg.GateHost() != "previews.apps.example.com" {
		t.Fatalf("the gate host is %q", cfg.GateHost())
	}
	if cfg.issuerBase() != cfg.Issuer {
		t.Fatalf("without an internal URL the issuer is reached at %q", cfg.issuerBase())
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	env := completeEnv()
	env["KITCHEN_GATE_COOKIE_SECURE"] = "false"
	env["KITCHEN_GATE_SESSION_TTL"] = "30m"
	env["KITCHEN_GATE_ISSUER_INTERNAL_URL"] = "http://kitchen-auth.kitchen-system.svc:80/"

	cfg, err := LoadConfig(lookupFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CookieSecure {
		t.Fatal("cookie security was not turned off")
	}
	if cfg.SessionTTL != 30*time.Minute {
		t.Fatalf("the session TTL is %s", cfg.SessionTTL)
	}
	if cfg.issuerBase() != "http://kitchen-auth.kitchen-system.svc:80" {
		t.Fatalf("the issuer is reached at %q", cfg.issuerBase())
	}
}

// TestLoadConfigReportsEveryProblem: a misconfigured Deployment should report
// all of it in one crash loop, not one problem per restart.
func TestLoadConfigReportsEveryProblem(t *testing.T) {
	_, err := LoadConfig(lookupFrom(map[string]string{
		"KITCHEN_GATE_ISSUER":        "auth.apps.example.com",
		"KITCHEN_GATE_CALLBACK_URL":  "https://previews.apps.example.com/elsewhere",
		"KITCHEN_GATE_COOKIE_SECRET": "short",
	}))
	if err == nil {
		t.Fatal("expected the configuration to be refused")
	}
	for _, want := range []string{
		"KITCHEN_GATE_CLIENT_ID is required",
		"KITCHEN_GATE_CLIENT_SECRET is required",
		"KITCHEN_GATE_ISSUER must be an absolute http(s) URL",
		"KITCHEN_GATE_CALLBACK_URL must end in " + CallbackPath,
		"KITCHEN_GATE_COOKIE_SECRET must be at least",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%s", want, err)
		}
	}
}

func TestParseUpstream(t *testing.T) {
	for _, valid := range []string{
		"shop-pr-42.kitchen-shop.svc.cluster.local:80",
		"shop.kitchen-shop.svc:8080",
	} {
		if _, err := parseUpstream(valid); err != nil {
			t.Errorf("%q should be routable: %v", valid, err)
		}
	}
	for _, invalid := range []string{
		"",
		"evil.example.com:80",
		"kitchen-shop.svc:80",
		"shop.kitchen-shop.svc.cluster.local:0",
		"shop.kitchen-shop.svc.cluster.local:not-a-port",
		"http://shop.kitchen-shop.svc/",
	} {
		if _, err := parseUpstream(invalid); err == nil {
			t.Errorf("%q should not be routable", invalid)
		}
	}
}

// TestTokensAreNotInterchangeable: every signed value the gate hands out
// carries what it is for, so none of them works anywhere else.
func TestTokensAreNotInterchangeable(t *testing.T) {
	s := newSigner(testSecret)
	token, err := s.mint(claims{Purpose: purposeHandoff, Host: testPreviewHost, Subject: "user-1"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.verify(token, purposeSession, testPreviewHost); err == nil {
		t.Fatal("a hand-off token was accepted as a session")
	}
	if _, err := s.verify(token, purposeHandoff, "other.apps.example.com"); err == nil {
		t.Fatal("a token for one host was accepted on another")
	}
	if _, err := s.verify(token, purposeHandoff, testPreviewHost); err != nil {
		t.Fatalf("the token should verify for what it was minted for: %v", err)
	}
}

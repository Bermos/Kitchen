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

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The one robot account every registry test authenticates with.
const (
	probeUser     = "robot"
	probePassword = "hunter2"
)

func TestRegistryProbeAcceptsBasicAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, _ := r.BasicAuth(); user != probeUser || pass != probePassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	result := (&RegistryProbe{BaseURL: server.URL, Username: probeUser, Password: probePassword}).Probe(context.Background())
	if !result.CredentialValid {
		t.Errorf("expected an accepted credential, got %+v", result)
	}
}

func TestRegistryProbeFollowsBearerChallenge(t *testing.T) {
	tokenService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("service"); got != "registry.example.com" {
			t.Errorf("unexpected service param %q", got)
		}
		if user, pass, _ := r.BasicAuth(); user != probeUser || pass != probePassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"token": "opaque"}`))
	}))
	defer tokenService.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="`+tokenService.URL+`/token",service="registry.example.com"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registry.Close()

	result := (&RegistryProbe{BaseURL: registry.URL, Username: probeUser, Password: probePassword}).Probe(context.Background())
	if !result.Reachable || !result.CredentialChecked || !result.CredentialValid {
		t.Errorf("expected the token service's yes to count, got %+v", result)
	}
}

func TestRegistryProbeRejectsThroughTokenService(t *testing.T) {
	tokenService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors": [{"code": "UNAUTHORIZED"}]}`))
	}))
	defer tokenService.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenService.URL+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registry.Close()

	result := (&RegistryProbe{BaseURL: registry.URL, Username: probeUser, Password: "wrong"}).Probe(context.Background())
	if !result.Reachable || !result.CredentialChecked || result.CredentialValid {
		t.Errorf("expected a rejected credential, got %+v", result)
	}
	if !strings.Contains(result.Message, "UNAUTHORIZED") {
		t.Errorf("expected the auth service's error in the message, got %q", result.Message)
	}
}

func TestRegistryProbeRejectsPlainUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No challenge header at all: the registry itself said no.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	result := (&RegistryProbe{BaseURL: server.URL, Username: probeUser, Password: "wrong"}).Probe(context.Background())
	if !result.Reachable || !result.CredentialChecked || result.CredentialValid {
		t.Errorf("expected a rejected credential, got %+v", result)
	}
}

func TestRegistryProbeDistinguishesDownFromWrong(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()

	result := (&RegistryProbe{BaseURL: server.URL, Username: probeUser, Password: probePassword}).Probe(context.Background())
	if result.Reachable || result.CredentialChecked {
		t.Errorf("a registry that is down must not read as a bad password, got %+v", result)
	}
}

func TestBearerChallengeParsing(t *testing.T) {
	realm, service, ok := bearerChallenge(`Bearer realm="https://auth.example.com/token",service="reg",scope="pull"`)
	if !ok || realm != "https://auth.example.com/token" || service != "reg" {
		t.Errorf("unexpected parse: realm=%q service=%q ok=%v", realm, service, ok)
	}
	if _, _, ok := bearerChallenge(`Basic realm="registry"`); ok {
		t.Error("a Basic challenge is not a Bearer challenge")
	}
	if _, _, ok := bearerChallenge(""); ok {
		t.Error("an absent header is not a challenge")
	}
}

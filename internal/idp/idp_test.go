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

package idp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const publicIssuer = "https://auth.apps.example.com"

// issuerServer stands in for the identity provider, reachable at a
// cluster-internal address while calling itself by its public name.
type issuerServer struct {
	*httptest.Server
	registerRequest struct {
		apiKey  string
		host    string
		payload map[string]any
	}
	refuse bool
}

func newIssuerServer(t *testing.T) *issuerServer {
	t.Helper()
	server := &issuerServer{}
	mux := http.NewServeMux()
	mux.HandleFunc(DiscoveryPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 publicIssuer,
			"authorization_endpoint": publicIssuer + "/oauth2/authorize",
			"token_endpoint":         publicIssuer + "/oauth2/token",
			"registration_endpoint":  publicIssuer + "/oauth2/register",
			"jwks_uri":               publicIssuer + "/jwks",
		})
	})
	mux.HandleFunc("/oauth2/register", func(w http.ResponseWriter, r *http.Request) {
		server.registerRequest.apiKey = r.Header.Get(serviceKeyHeader)
		server.registerRequest.host = r.Host
		_ = json.NewDecoder(r.Body).Decode(&server.registerRequest.payload)
		if server.refuse {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"client_id":     "the-client",
			"client_secret": "the-secret",
		})
	})
	server.Server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func internalConfig(server *issuerServer) Config {
	return Config{Issuer: publicIssuer, BaseURL: server.URL, ServiceKey: "service-key"}
}

// TestDiscoverRebasesEndpoints: an issuer reached inside the cluster
// advertises its public URLs, which the operator cannot use.
func TestDiscoverRebasesEndpoints(t *testing.T) {
	server := newIssuerServer(t)
	metadata, err := New(internalConfig(server)).WithHTTPClient(server.Client()).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Issuer != publicIssuer {
		t.Fatalf("the issuer is %q", metadata.Issuer)
	}
	for _, endpoint := range []string{
		metadata.RegistrationEndpoint, metadata.TokenEndpoint, metadata.AuthorizationEndpoint, metadata.JWKSURI,
	} {
		if !strings.HasPrefix(endpoint, server.URL) {
			t.Errorf("%q was not rebased onto the address it was fetched from", endpoint)
		}
	}
}

func TestRegisterUsesTheServiceCredential(t *testing.T) {
	server := newIssuerServer(t)
	client, err := New(internalConfig(server)).WithHTTPClient(server.Client()).
		Register(context.Background(), ClientRegistration{
			Name:         "Kitchen preview gate",
			RedirectURIs: []string{"https://previews.apps.example.com/_kitchen/gate/callback"},
			GrantTypes:   []string{"authorization_code"},
			Scopes:       []string{"openid", "email"},
		})
	if err != nil {
		t.Fatal(err)
	}
	if client.ID != "the-client" || client.Secret != "the-secret" {
		t.Fatalf("unexpected credentials: %+v", client)
	}
	if server.registerRequest.apiKey != "service-key" {
		t.Fatal("registration went out without the operator's credential")
	}
	// Reached at its Service address, the issuer is still asked for by name.
	if got := server.registerRequest.host; got != "auth.apps.example.com" {
		t.Fatalf("the request carried the Host %q", got)
	}
	if got := server.registerRequest.payload["scope"]; got != "openid email" {
		t.Fatalf("the requested scope is %v", got)
	}
	if got := server.registerRequest.payload["token_endpoint_auth_method"]; got != "client_secret_basic" {
		t.Fatalf("the client authenticates with %v", got)
	}
}

// TestRegisterReportsRefusal: a rejected registration has to reach a
// condition, with enough of the issuer's answer to act on.
func TestRegisterReportsRefusal(t *testing.T) {
	server := newIssuerServer(t)
	server.refuse = true
	_, err := New(internalConfig(server)).WithHTTPClient(server.Client()).
		Register(context.Background(), ClientRegistration{Name: "gate"})
	if err == nil {
		t.Fatal("expected the registration to fail")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("the error says too little: %v", err)
	}
}

func TestConfigFromSecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kitchen-auth", Namespace: "kitchen-system"},
		Data: map[string][]byte{
			SecretKeyIssuer:      []byte(publicIssuer + "/"),
			SecretKeyServiceKey:  []byte("service-key"),
			SecretKeyInternalURL: []byte("http://kitchen-auth.kitchen-system.svc:80"),
		},
	}
	cfg, err := ConfigFromSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Issuer != publicIssuer {
		t.Fatalf("the issuer kept its trailing slash: %q", cfg.Issuer)
	}
	if cfg.BaseURL != "http://kitchen-auth.kitchen-system.svc:80" {
		t.Fatalf("the internal address is %q", cfg.BaseURL)
	}

	// Without an internal address, the issuer is reached at its own URL.
	delete(secret.Data, SecretKeyInternalURL)
	cfg, err = ConfigFromSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != cfg.Issuer {
		t.Fatalf("the issuer is reached at %q", cfg.BaseURL)
	}

	// And a secret with nothing usable in it names what is missing.
	delete(secret.Data, SecretKeyServiceKey)
	if _, err := ConfigFromSecret(secret); err == nil || !strings.Contains(err.Error(), SecretKeyServiceKey) {
		t.Fatalf("expected the missing key to be named, got %v", err)
	}
}

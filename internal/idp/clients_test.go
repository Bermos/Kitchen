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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// managedIssuer is an issuer that can be asked to change a client it
// registered — one way or the other. `standard` decides which: an issuer that
// implements RFC 7592 hands out a client configuration endpoint at
// registration, and one that does not is reached at the operator's own prefix
// instead. Both have to work, because the platform's own identity provider is
// the second kind and a federated one may be the first.
type managedIssuer struct {
	*httptest.Server
	standard bool

	updated  map[string]any
	deleted  string
	apiKey   string
	bearer   string
	notFound bool
}

// theClient is the id the fake issuer hands out, and the one every
// assertion here is about.
const theClient = "the-client"

func newManagedIssuer(t *testing.T, standard bool) *managedIssuer {
	t.Helper()
	server := &managedIssuer{standard: standard}
	mux := http.NewServeMux()

	mux.HandleFunc(DiscoveryPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                publicIssuer,
			"token_endpoint":        publicIssuer + "/oauth2/token",
			"registration_endpoint": publicIssuer + "/oauth2/register",
		})
	})
	mux.HandleFunc("/oauth2/register", func(w http.ResponseWriter, _ *http.Request) {
		answer := map[string]string{"client_id": theClient, "client_secret": "the-secret"}
		if server.standard {
			answer["registration_client_uri"] = server.URL + "/oauth2/register/" + theClient
			answer["registration_access_token"] = "the-registration-token"
		}
		_ = json.NewEncoder(w).Encode(answer)
	})
	mux.HandleFunc("/oauth2/register/the-client", func(w http.ResponseWriter, r *http.Request) {
		server.bearer = r.Header.Get("authorization")
		if server.notFound {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&server.updated)
			_ = json.NewEncoder(w).Encode(map[string]string{"client_id": theClient})
		case http.MethodDelete:
			server.deleted = theClient
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc(ClientsPath, func(w http.ResponseWriter, r *http.Request) {
		if server.standard {
			// An issuer with a client configuration endpoint has no reason to
			// serve Kitchen's prefix, and this asserts that nothing calls it
			// when the standard route is available.
			t.Errorf("the operator's prefix was called on an issuer that implements RFC 7592")
		}
		server.apiKey = r.Header.Get(serviceKeyHeader)
		if server.notFound {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such endpoint"}`))
			return
		}
		switch r.Method {
		case http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&server.updated)
			_ = json.NewEncoder(w).Encode(map[string]any{"clientId": theClient})
		case http.MethodDelete:
			server.deleted = r.URL.Query().Get("clientId")
			_ = json.NewEncoder(w).Encode(map[string]any{"clientId": server.deleted})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	server.Server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func (m *managedIssuer) client() *Client {
	return New(Config{Issuer: publicIssuer, BaseURL: m.URL, ServiceKey: "service-key"}).
		WithHTTPClient(m.Client())
}

func appClient(uris ...string) ClientRegistration {
	return ClientRegistration{
		Name:         "shop",
		RedirectURIs: uris,
		GrantTypes:   []string{"authorization_code", "refresh_token"},
		Scopes:       []string{"openid", "email"},
	}
}

// TestRegisterKeepsTheManagementHandle: RFC 7592's two fields are handed out
// once, at registration, and a client whose handle was dropped is a client
// nothing can change afterwards.
func TestRegisterKeepsTheManagementHandle(t *testing.T) {
	server := newManagedIssuer(t, true)
	registered, err := server.client().Register(context.Background(), appClient("https://shop.example.com/cb"))
	if err != nil {
		t.Fatal(err)
	}
	if !registered.Management.Manageable() {
		t.Fatalf("the handle was not kept: %+v", registered.Management)
	}
	if registered.Management.ID != theClient {
		t.Fatalf("the handle names %q", registered.Management.ID)
	}
}

// TestUpdateClientPrefersTheStandardRoute: an issuer that implements client
// management is talked to in its own terms, with the registration access
// token rather than the operator's service credential — the token is the
// issuer's statement about this one client.
func TestUpdateClientPrefersTheStandardRoute(t *testing.T) {
	server := newManagedIssuer(t, true)
	client := server.client()
	registered, err := client.Register(context.Background(), appClient("https://shop.example.com/cb"))
	if err != nil {
		t.Fatal(err)
	}

	want := appClient("https://shop.example.com/cb", "https://shop-pr-1.example.com/cb")
	if err := client.UpdateClient(context.Background(), registered.Management, want); err != nil {
		t.Fatal(err)
	}
	if server.bearer != "Bearer the-registration-token" {
		t.Fatalf("the update was authorized with %q", server.bearer)
	}
	uris, _ := server.updated["redirect_uris"].([]any)
	if len(uris) != 2 {
		t.Fatalf("the issuer was sent %v", server.updated["redirect_uris"])
	}
	if server.updated["client_id"] != theClient {
		t.Fatal("RFC 7592 replaces the whole registration, client id included")
	}
}

// TestUpdateClientFallsBackToTheOperatorsPrefix: the identity provider the
// chart ships implements registration and not management, so the redirect
// list is maintained where the account directory is — with the operator's
// service credential.
func TestUpdateClientFallsBackToTheOperatorsPrefix(t *testing.T) {
	server := newManagedIssuer(t, false)
	client := server.client()
	registered, err := client.Register(context.Background(), appClient("https://shop.example.com/cb"))
	if err != nil {
		t.Fatal(err)
	}
	if registered.Management.Manageable() {
		t.Fatal("an issuer without RFC 7592 must not look manageable")
	}

	want := appClient("https://shop.example.com/cb", "https://shop-pr-1.example.com/cb")
	if err := client.UpdateClient(context.Background(), registered.Management, want); err != nil {
		t.Fatal(err)
	}
	if server.apiKey != "service-key" {
		t.Fatalf("the update went out with %q", server.apiKey)
	}
	uris, _ := server.updated["redirectURIs"].([]any)
	if len(uris) != 2 || uris[1] != "https://shop-pr-1.example.com/cb" {
		t.Fatalf("the prefix was sent %v", server.updated["redirectURIs"])
	}
}

// TestUpdateClientReportsAnIssuerThatCannotBeAsked: a federated issuer
// implementing neither route is not a fault to retry. The claim reports it
// and keeps the client it has.
func TestUpdateClientReportsAnIssuerThatCannotBeAsked(t *testing.T) {
	server := newManagedIssuer(t, false)
	server.notFound = true

	err := server.client().UpdateClient(context.Background(),
		ClientHandle{ID: theClient}, appClient("https://shop.example.com/cb"))
	if !errors.Is(err, ErrNoClientManagement) {
		t.Fatalf("the caller cannot tell what happened: %v", err)
	}
}

// TestDeleteClientRemovesItBothWays, and is idempotent: a finalizer runs
// again after a failure, and a client that is already gone must not wedge the
// claim's deletion.
func TestDeleteClientRemovesItBothWays(t *testing.T) {
	for _, standard := range []bool{true, false} {
		server := newManagedIssuer(t, standard)
		client := server.client()
		registered, err := client.Register(context.Background(), appClient("https://shop.example.com/cb"))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.DeleteClient(context.Background(), registered.Management); err != nil {
			t.Fatalf("standard=%v: %v", standard, err)
		}
		if server.deleted != theClient {
			t.Fatalf("standard=%v: the issuer was asked to remove %q", standard, server.deleted)
		}

		server.notFound = true
		if err := client.DeleteClient(context.Background(), registered.Management); err != nil {
			t.Fatalf("standard=%v: removing a client that is already gone: %v", standard, err)
		}
	}
}

// TestDeleteClientWithoutAnIDDoesNothing: a claim that never got as far as
// registering has no client, and its finalizer must not call anything.
func TestDeleteClientWithoutAnIDDoesNothing(t *testing.T) {
	server := newManagedIssuer(t, false)
	if err := server.client().DeleteClient(context.Background(), ClientHandle{}); err != nil {
		t.Fatal(err)
	}
	if server.deleted != "" {
		t.Fatalf("the issuer was asked about %q", server.deleted)
	}
}

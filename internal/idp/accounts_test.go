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
	"strings"
	"testing"
)

// directoryServer stands in for the identity provider's account directory:
// the accounts it holds, and a record of how it was asked.
type directoryServer struct {
	*httptest.Server
	accounts []Account
	request  struct {
		apiKey string
		host   string
		email  string
	}
	status int
}

func newDirectoryServer(t *testing.T, accounts ...Account) *directoryServer {
	t.Helper()
	server := &directoryServer{accounts: accounts}
	mux := http.NewServeMux()
	mux.HandleFunc(AccountsPath, func(w http.ResponseWriter, r *http.Request) {
		server.request.apiKey = r.Header.Get(serviceKeyHeader)
		server.request.host = r.Host
		server.request.email = r.URL.Query().Get("email")
		if server.status != 0 {
			w.WriteHeader(server.status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		if wanted := server.request.email; wanted != "" {
			for _, account := range server.accounts {
				if strings.EqualFold(account.Email, wanted) {
					_ = json.NewEncoder(w).Encode(account)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such account"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(accountsResponse{Accounts: server.accounts})
	})
	server.Server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func directoryConfig(server *directoryServer) Config {
	return Config{Issuer: publicIssuer, BaseURL: server.URL, ServiceKey: testServiceKey}
}

var (
	anna = Account{Subject: "user_anna", Email: "anna@example.com", Name: "Anna", EmailVerified: true}
	bo   = Account{Subject: "user_bo", Email: "bo@example.com", Name: "Bo"}
)

// TestAccountsReadsTheDirectory: the seeding of the operator list, and the
// dashboard's people picker, both start here.
func TestAccountsReadsTheDirectory(t *testing.T) {
	server := newDirectoryServer(t, anna, bo)
	accounts, err := New(directoryConfig(server)).WithHTTPClient(server.Client()).
		Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0] != anna || accounts[1] != bo {
		t.Fatalf("the directory answered %+v", accounts)
	}
	if server.request.apiKey != testServiceKey {
		t.Fatal("the read went out without the operator's credential")
	}
	// Reached at its Service address, the issuer is still asked for by name.
	if got := server.request.host; got != "auth.apps.example.com" {
		t.Fatalf("the request carried the Host %q", got)
	}
}

func TestAccountByEmail(t *testing.T) {
	server := newDirectoryServer(t, anna, bo)
	client := New(directoryConfig(server)).WithHTTPClient(server.Client())

	account, err := client.AccountByEmail(context.Background(), "Anna@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if *account != anna {
		t.Fatalf("resolved to %+v", account)
	}
	if server.request.email != "Anna@Example.com" {
		t.Fatalf("the address was sent as %q; matching it is the issuer's job", server.request.email)
	}

	// Nobody holds it: a sentinel the caller can branch on, not a message it
	// has to read.
	_, err = client.AccountByEmail(context.Background(), "stranger@example.com")
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound, got %v", err)
	}
}

// TestAccountsReportsAnIssuerWithoutADirectory: a federated issuer answers
// 404, which is neither an outage nor an empty platform. Saying which is the
// difference between an operator list that is never seeded for a reason and
// one that is never seeded for none.
func TestAccountsReportsAnIssuerWithoutADirectory(t *testing.T) {
	server := newDirectoryServer(t)
	server.status = http.StatusNotFound
	_, err := New(directoryConfig(server)).WithHTTPClient(server.Client()).
		Accounts(context.Background())
	if !errors.Is(err, ErrNoDirectory) {
		t.Fatalf("expected ErrNoDirectory, got %v", err)
	}
}

// TestAccountsReportsARefusal: the operator's credential is the only thing
// that gets in, so a refusal has to reach a condition with enough of the
// issuer's answer to act on.
func TestAccountsReportsARefusal(t *testing.T) {
	server := newDirectoryServer(t)
	server.status = http.StatusForbidden
	_, err := New(directoryConfig(server)).WithHTTPClient(server.Client()).
		Accounts(context.Background())
	if err == nil {
		t.Fatal("expected the read to fail")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("the error says too little: %v", err)
	}
	if errors.Is(err, ErrNoDirectory) {
		t.Fatal("a refusal is not an issuer without a directory")
	}
}

// TestAccountsOnAnEmptyPlatform: no accounts is not an error. It is a fresh
// install before anybody has followed the bootstrap link, and the operator
// has to be able to tell it from a failure — it seeds nothing and tries
// again rather than writing an empty operator list.
func TestAccountsOnAnEmptyPlatform(t *testing.T) {
	server := newDirectoryServer(t)
	accounts, err := New(directoryConfig(server)).WithHTTPClient(server.Client()).
		Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("the directory answered %+v", accounts)
	}
}

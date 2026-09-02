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

func TestInngestProbeAccepts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/account" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Errorf("unexpected auth header %q", got)
		}
		_, _ = w.Write([]byte(`{"data": {"id": "acct", "email": "ops@example.com"}}`))
	}))
	defer server.Close()

	result := (&InngestProbe{APIURL: server.URL, Token: "key"}).Probe(context.Background())
	if !result.CredentialValid || !strings.Contains(result.Message, "ops@example.com") {
		t.Errorf("expected an accepted credential naming the account, got %+v", result)
	}
}

func TestInngestProbeRejects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`Authentication failed`))
	}))
	defer server.Close()

	result := (&InngestProbe{APIURL: server.URL, Token: "wrong"}).Probe(context.Background())
	if !result.Reachable || !result.CredentialChecked || result.CredentialValid {
		t.Errorf("expected a rejected credential, got %+v", result)
	}
	if !strings.Contains(result.Message, "Authentication failed") {
		t.Errorf("expected the provider's own error in the message, got %q", result.Message)
	}
}

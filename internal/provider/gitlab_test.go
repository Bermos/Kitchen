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

func TestGitLabProbeAcceptsCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != testToken {
			t.Errorf("unexpected token header %q", got)
		}
		_, _ = w.Write([]byte(`{"username":"bermos"}`))
	}))
	defer server.Close()

	result := (&GitLabProbe{APIURL: server.URL, Token: testToken}).Probe(context.Background())
	if !result.Reachable || !result.CredentialChecked || !result.CredentialValid {
		t.Fatalf("expected accepted credential, got %+v", result)
	}
	if !strings.Contains(result.Message, "bermos") {
		t.Fatalf("expected username in message, got %q", result.Message)
	}
}

func TestGitLabProbeRejectsCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("denied"))
	}))
	defer server.Close()

	result := (&GitLabProbe{APIURL: server.URL, Token: "bad"}).Probe(context.Background())
	if !result.Reachable || !result.CredentialChecked || result.CredentialValid {
		t.Fatalf("expected rejected credential, got %+v", result)
	}
}

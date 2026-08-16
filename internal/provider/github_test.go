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

func TestGitHubProbeAcceptsAndReportsScopes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("unexpected auth header %q", got)
		}
		w.Header().Set("X-OAuth-Scopes", "repo, admin:repo_hook")
		_, _ = w.Write([]byte(`{"login": "octocat"}`))
	}))
	defer server.Close()

	result := (&GitHubProbe{APIURL: server.URL, Token: "tok"}).Probe(context.Background())
	if !result.Reachable || !result.CredentialChecked || !result.CredentialValid {
		t.Errorf("expected an accepted credential, got %+v", result)
	}
	if !strings.Contains(result.Message, "octocat") || !strings.Contains(result.Message, "admin:repo_hook") {
		t.Errorf("expected login and scopes in message, got %q", result.Message)
	}
}

func TestGitHubProbeRejectsBadCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message": "Bad credentials"}`))
	}))
	defer server.Close()

	result := (&GitHubProbe{APIURL: server.URL, Token: "wrong"}).Probe(context.Background())
	if !result.Reachable || !result.CredentialChecked || result.CredentialValid {
		t.Errorf("expected a rejected credential, got %+v", result)
	}
	if !strings.Contains(result.Message, "Bad credentials") {
		t.Errorf("expected the provider's own error in the message, got %q", result.Message)
	}
}

func TestGitHubProbeLeavesServerErrorsUnjudged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	result := (&GitHubProbe{APIURL: server.URL, Token: "tok"}).Probe(context.Background())
	if !result.Reachable || result.CredentialChecked {
		t.Errorf("expected reachable but unjudged, got %+v", result)
	}
}

func TestGitHubProbeReportsUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close() // The address exists; nothing answers on it.

	result := (&GitHubProbe{APIURL: server.URL, Token: "tok"}).Probe(context.Background())
	if result.Reachable || result.CredentialChecked {
		t.Errorf("expected unreachable, got %+v", result)
	}
	if result.Message == "" {
		t.Error("expected the transport error in the message")
	}
}

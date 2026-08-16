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
	// repo covers everything the platform does, deploy reporting included.
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings for a repo-scoped token, got %v", result.Warnings)
	}
}

// probeWithScopes runs the probe against a server that hands back the given
// scope header. present=false is a fine-grained token, which sends no header
// at all.
func probeWithScopes(t *testing.T, scopes string, present bool) Result {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if present {
			w.Header().Set("X-OAuth-Scopes", scopes)
		}
		_, _ = w.Write([]byte(`{"login": "octocat"}`))
	}))
	defer server.Close()
	return (&GitHubProbe{APIURL: server.URL, Token: "tok"}).Probe(context.Background())
}

func TestGitHubProbeWarnsAboutWhatAScopeCannotDo(t *testing.T) {
	// The webhook scope alone is enough for what the platform does today and
	// for nothing it is about to do: the token stays valid, and each thing it
	// cannot do names the scope that would fix it.
	result := probeWithScopes(t, "admin:repo_hook", true)
	if !result.CredentialValid {
		t.Fatalf("a working token was not accepted: %+v", result)
	}
	warnings := strings.Join(result.Warnings, "\n")
	for _, want := range []string{"commit statuses", "repo:status", "deployment statuses", "repo_deployment", "pull requests"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("expected %q among the warnings, got %v", want, result.Warnings)
		}
	}
	if strings.Contains(warnings, "webhook") {
		t.Errorf("warned about the one thing the token can do: %v", result.Warnings)
	}
}

func TestGitHubProbeReadsPublicRepoAsCoveringEverything(t *testing.T) {
	if result := probeWithScopes(t, "public_repo", true); len(result.Warnings) != 0 {
		t.Errorf("public_repo covers every use; got %v", result.Warnings)
	}
}

func TestGitHubProbeWarnsAboutAScopelessToken(t *testing.T) {
	result := probeWithScopes(t, "", true)
	if len(result.Warnings) != len(githubTokenUses) {
		t.Errorf("a token with no scopes can do nothing; got %v", result.Warnings)
	}
	if !strings.Contains(result.Message, "no scopes") {
		t.Errorf("expected the message to say the token carries no scopes, got %q", result.Message)
	}
}

func TestGitHubProbeJudgesNoFineGrainedToken(t *testing.T) {
	// No scope header at all: GitHub offers no way to ask a fine-grained token
	// what it may do, and a guess would be worse than silence.
	result := probeWithScopes(t, "", false)
	if !result.CredentialValid {
		t.Fatalf("a working token was not accepted: %+v", result)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings for a token whose scopes cannot be read, got %v", result.Warnings)
	}
	if strings.Contains(result.Message, "scopes") {
		t.Errorf("expected no scope talk for a fine-grained token, got %q", result.Message)
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

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

package gitprovider

import (
	"errors"
	"testing"
)

func TestWebLinksForHostedProviders(t *testing.T) {
	cases := map[string]struct {
		provider    string
		config      string
		repository  string
		commit      string
		branch      string
		pullRequest string
	}{
		"github": {
			provider:    ProviderGitHub,
			repository:  "https://github.com/acme/shop",
			commit:      "https://github.com/acme/shop/commit/986cbe89",
			branch:      "https://github.com/acme/shop/tree/main",
			pullRequest: "https://github.com/acme/shop/pull/42",
		},
		"github enterprise": {
			provider:    ProviderGitHub,
			config:      `{"apiUrl":"https://ghe.example.com/api/v3"}`,
			repository:  "https://ghe.example.com/acme/shop",
			commit:      "https://ghe.example.com/acme/shop/commit/986cbe89",
			branch:      "https://ghe.example.com/acme/shop/tree/main",
			pullRequest: "https://ghe.example.com/acme/shop/pull/42",
		},
		"github enterprise cloud": {
			provider:    ProviderGitHub,
			config:      `{"apiUrl":"https://api.acme.ghe.com"}`,
			repository:  "https://acme.ghe.com/acme/shop",
			commit:      "https://acme.ghe.com/acme/shop/commit/986cbe89",
			branch:      "https://acme.ghe.com/acme/shop/tree/main",
			pullRequest: "https://acme.ghe.com/acme/shop/pull/42",
		},
		"gitlab": {
			provider:    ProviderGitLab,
			repository:  "https://gitlab.com/acme/shop",
			commit:      "https://gitlab.com/acme/shop/-/commit/986cbe89",
			branch:      "https://gitlab.com/acme/shop/-/tree/main",
			pullRequest: "https://gitlab.com/acme/shop/-/merge_requests/42",
		},
		"self-hosted gitlab under a path": {
			provider:    ProviderGitLab,
			config:      `{"apiUrl":"https://example.com/gitlab/api/v4"}`,
			repository:  "https://example.com/gitlab/acme/shop",
			commit:      "https://example.com/gitlab/acme/shop/-/commit/986cbe89",
			branch:      "https://example.com/gitlab/acme/shop/-/tree/main",
			pullRequest: "https://example.com/gitlab/acme/shop/-/merge_requests/42",
		},
		"gitea": {
			provider:    ProviderGitea,
			config:      `{"apiUrl":"https://git.example.com/api/v1"}`,
			repository:  "https://git.example.com/acme/shop",
			commit:      "https://git.example.com/acme/shop/commit/986cbe89",
			branch:      "https://git.example.com/acme/shop/src/branch/main",
			pullRequest: "https://git.example.com/acme/shop/pulls/42",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			links, err := Web(providerConnection(tc.provider, tc.config))
			if err != nil {
				t.Fatal(err)
			}
			if got := links.Repository("acme/shop"); got != tc.repository {
				t.Errorf("repository = %q, want %q", got, tc.repository)
			}
			if got := links.Commit("acme/shop", "986cbe89"); got != tc.commit {
				t.Errorf("commit = %q, want %q", got, tc.commit)
			}
			if got := links.Branch("acme/shop", "main"); got != tc.branch {
				t.Errorf("branch = %q, want %q", got, tc.branch)
			}
			if got := links.PullRequest("acme/shop", 42); got != tc.pullRequest {
				t.Errorf("pull request = %q, want %q", got, tc.pullRequest)
			}
		})
	}
}

// A branch is one field of a webhook payload and it is written by whoever
// pushed: it can be nested, and it can hold characters that mean something in
// a URL. The nesting has to survive — every provider browses `release/1.2` as
// two path segments — and the rest has to be escaped.
func TestWebLinksKeepBranchPathsAndEscapeTheRest(t *testing.T) {
	links, err := Web(providerConnection(ProviderGitHub, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := links.Branch("acme/shop", "release/1.2"), "https://github.com/acme/shop/tree/release/1.2"; got != want {
		t.Errorf("branch = %q, want %q", got, want)
	}
	if got, want := links.Branch("acme/shop", "fix/#1 space"), "https://github.com/acme/shop/tree/fix/%231%20space"; got != want {
		t.Errorf("branch = %q, want %q", got, want)
	}
}

// A link that cannot be composed is no link. Everything that renders one has
// a half-empty revision to render at some point — an acquisition has no
// commit at all — and "" is what a client shows as plain text.
func TestWebLinksAnswerNothingForWhatTheyWereNotGiven(t *testing.T) {
	links, err := Web(providerConnection(ProviderGitLab, ""))
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"repository":     links.Repository(""),
		"commit":         links.Commit("acme/shop", ""),
		"commit no repo": links.Commit("", "986cbe89"),
		"branch":         links.Branch("acme/shop", ""),
		"pull request":   links.PullRequest("acme/shop", 0),
	} {
		if got != "" {
			t.Errorf("%s = %q, want the empty string", name, got)
		}
	}
}

func TestWebRefusesAProviderItCannotAddress(t *testing.T) {
	if _, err := Web(providerConnection("bitbucket", "")); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("err = %v, want ErrUnsupportedProvider", err)
	}
	if _, err := Web(providerConnection(ProviderGitea, `{"apiUrl":"://nonsense"}`)); err == nil {
		t.Fatal("an api url that is not a url should not resolve to a web url")
	}
}

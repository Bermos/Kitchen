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
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
)

// GitHubProbe checks a GitHub token by asking who it authenticates as. APIURL
// is overridable for GitHub Enterprise (and tests) via the Connection
// config's "apiUrl" field, the same way internal/gitprovider does it.
type GitHubProbe struct {
	APIURL string
	Token  string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// Probe calls /user: the one request that both proves the token works and,
// through the X-OAuth-Scopes header, says what it is allowed to do.
func (g *GitHubProbe) Probe(ctx context.Context) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.APIURL+"/user", nil)
	if err != nil {
		return unreachable(err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.Token)

	resp, err := httpClientOr(g.HTTPClient).Do(req)
	if err != nil {
		return unreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if deniedStatus(resp.StatusCode) {
		return rejected(fmt.Sprintf("github API returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	}
	if resp.StatusCode >= 300 {
		return unjudged(fmt.Sprintf("github API returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	}

	var user struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return unjudged("github API answered /user with an unparseable body: " + err.Error())
	}
	message := "authenticated as " + user.Login
	// A classic token always sends this header, empty when it was minted with
	// no scopes at all; a fine-grained token does not send it. Reading the
	// header's presence rather than its value keeps those two apart, and the
	// difference matters: a classic token can be judged, a fine-grained one
	// cannot be judged at all from here.
	scopes, classic := resp.Header[textproto.CanonicalMIMEHeaderKey(githubScopesHeader)]
	if !classic {
		return accepted(message)
	}
	granted := strings.Join(scopes, ", ")
	if granted == "" {
		return accepted(message + " (the token carries no scopes)").
			withWarnings(missingGitHubScopes(granted)...)
	}
	return accepted(message + " (token scopes: " + granted + ")").
		withWarnings(missingGitHubScopes(granted)...)
}

// githubScopesHeader is where a classic token's scopes come back.
const githubScopesHeader = "X-OAuth-Scopes"

// githubTokenUses is everything the platform does with a GitHub token, and the
// classic scopes that allow each. Only the first is exercised today; the rest
// is deploy reporting — a commit status per build, a deployment status per
// preview environment, and the pull-request comment carrying the preview URL
// (issue #71). They are checked now anyway, because the cost of a token minted
// without them is minting it again later, and a warning while someone is
// looking at the connection is much cheaper than a silent gap afterwards.
var githubTokenUses = []struct {
	// does is what the token would be doing, in the platform's terms.
	does string
	// anyOf are the scopes that allow it; holding one is enough.
	anyOf []string
	// add is the narrowest of them, which is what the warning suggests.
	add string
}{
	{"register the repository's webhook", []string{"admin:repo_hook", "write:repo_hook", "repo", "public_repo"}, "admin:repo_hook"},
	{"post commit statuses on builds", []string{"repo:status", "repo", "public_repo"}, "repo:status"},
	{"post deployment statuses for preview environments", []string{"repo_deployment", "repo", "public_repo"}, "repo_deployment"},
	{"comment the preview URL on pull requests", []string{"repo", "public_repo"}, "repo"},
}

// missingGitHubScopes turns the scope header into one warning per thing the
// token is not allowed to do. It speaks only for classic tokens: a
// fine-grained token reports no scopes, and GitHub offers no way to ask what
// one may do, so the platform finds out when it first tries.
func missingGitHubScopes(header string) []string {
	granted := map[string]bool{}
	for _, scope := range strings.Split(header, ",") {
		if scope = strings.TrimSpace(scope); scope != "" {
			granted[scope] = true
		}
	}

	warnings := make([]string, 0, len(githubTokenUses))
	for _, use := range githubTokenUses {
		if slices.ContainsFunc(use.anyOf, func(scope string) bool { return granted[scope] }) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("this token cannot %s: add the %s scope", use.does, use.add))
	}
	return warnings
}

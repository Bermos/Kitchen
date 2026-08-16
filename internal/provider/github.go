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
	// Classic tokens list their scopes here; fine-grained tokens send nothing.
	if scopes := resp.Header.Get("X-OAuth-Scopes"); scopes != "" {
		message += " (token scopes: " + scopes + ")"
	}
	return accepted(message)
}

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

// NeonProbe checks a Neon API key by asking for the account it belongs to.
// APIURL is overridable (for tests) via the Connection config's "apiUrl"
// field.
type NeonProbe struct {
	APIURL string
	Token  string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// Probe calls /users/me, the cheapest authenticated request the Neon API has.
func (n *NeonProbe) Probe(ctx context.Context) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.APIURL+"/users/me", nil)
	if err != nil {
		return unreachable(err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+n.Token)

	resp, err := httpClientOr(n.HTTPClient).Do(req)
	if err != nil {
		return unreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if deniedStatus(resp.StatusCode) {
		return rejected(fmt.Sprintf("neon API returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	}
	if resp.StatusCode >= 300 {
		return unjudged(fmt.Sprintf("neon API returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	}

	var user struct {
		Email string `json:"email"`
	}
	message := "API key accepted"
	if err := json.NewDecoder(resp.Body).Decode(&user); err == nil && user.Email != "" {
		message = "authenticated as " + user.Email
	}
	return accepted(message)
}

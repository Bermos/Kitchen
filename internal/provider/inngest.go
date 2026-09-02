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

// InngestProbe checks an Inngest Cloud API key by asking for the account it
// belongs to. APIURL is overridable (for tests) via the Connection config's
// "apiUrl" field.
type InngestProbe struct {
	APIURL string
	Token  string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// Probe calls GET /account, the cheapest authenticated request the v2 API
// has (https://api-docs.inngest.com/v2/account/FetchAccount).
func (p *InngestProbe) Probe(ctx context.Context) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.APIURL+"/account", nil)
	if err != nil {
		return unreachable(err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.Token)

	resp, err := httpClientOr(p.HTTPClient).Do(req)
	if err != nil {
		return unreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if deniedStatus(resp.StatusCode) {
		return rejected(fmt.Sprintf("inngest API returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	}
	if resp.StatusCode >= 300 {
		return unjudged(fmt.Sprintf("inngest API returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	}

	var account struct {
		Data struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"data"`
	}
	message := "API key accepted"
	if err := json.NewDecoder(resp.Body).Decode(&account); err == nil {
		switch {
		case account.Data.Email != "":
			message = "authenticated as " + account.Data.Email
		case account.Data.Name != "":
			message = "authenticated as " + account.Data.Name
		}
	}
	return accepted(message)
}

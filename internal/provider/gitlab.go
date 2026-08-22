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

// GitLabProbe checks a GitLab token by asking who it authenticates as.
type GitLabProbe struct {
	APIURL string
	Token  string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// Probe calls /user and returns the provider's verdict on the token.
func (g *GitLabProbe) Probe(ctx context.Context) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.APIURL+"/user", nil)
	if err != nil {
		return unreachable(err)
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)

	resp, err := httpClientOr(g.HTTPClient).Do(req)
	if err != nil {
		return unreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if deniedStatus(resp.StatusCode) {
		return rejected(fmt.Sprintf("gitlab API returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	}
	if resp.StatusCode >= 300 {
		return unjudged(fmt.Sprintf("gitlab API returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	}

	var user struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return unjudged("gitlab API answered /user with an unparseable body: " + err.Error())
	}
	identity := user.Username
	if identity == "" {
		identity = user.Name
	}
	if identity == "" {
		identity = "unknown"
	}
	return accepted("authenticated as " + identity)
}

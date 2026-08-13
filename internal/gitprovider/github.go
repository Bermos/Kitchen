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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// GitHub implements Provider against the GitHub REST API. APIURL is
// overridable for GitHub Enterprise (and tests) via the Connection config's
// "apiUrl" field.
type GitHub struct {
	APIURL string
	Token  string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

type githubHook struct {
	ID     int64            `json:"id,omitempty"`
	Name   string           `json:"name,omitempty"`
	Active bool             `json:"active"`
	Events []string         `json:"events,omitempty"`
	Config githubHookConfig `json:"config"`
}

type githubHookConfig struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Secret      string `json:"secret,omitempty"`
}

// EnsureWebhook finds an existing hook by delivery URL and updates it, or
// creates a new one.
func (g *GitHub) EnsureWebhook(ctx context.Context, repo string, spec WebhookSpec) (string, error) {
	existing := []githubHook{}
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/hooks?per_page=100", repo), nil, &existing); err != nil {
		return "", err
	}

	desired := githubHook{
		Name:   "web",
		Active: true,
		Events: spec.Events,
		Config: githubHookConfig{URL: spec.URL, ContentType: "json", Secret: spec.Secret},
	}

	for _, hook := range existing {
		if hook.Config.URL == spec.URL {
			path := fmt.Sprintf("/repos/%s/hooks/%d", repo, hook.ID)
			if err := g.do(ctx, http.MethodPatch, path, &desired, nil); err != nil {
				return "", err
			}
			return strconv.FormatInt(hook.ID, 10), nil
		}
	}

	created := githubHook{}
	if err := g.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/hooks", repo), &desired, &created); err != nil {
		return "", err
	}
	return strconv.FormatInt(created.ID, 10), nil
}

// DeleteWebhook removes the hook; a 404 is treated as already deleted.
func (g *GitHub) DeleteWebhook(ctx context.Context, repo, id string) error {
	err := g.do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/hooks/%s", repo, id), nil, nil)
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("github API returned %d: %s", e.status, e.body)
}

func isNotFound(err error) bool {
	httpErr, ok := err.(*httpError)
	return ok && httpErr.status == http.StatusNotFound
}

func (g *GitHub) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.APIURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	httpClient := g.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &httpError{status: resp.StatusCode, body: string(snippet)}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

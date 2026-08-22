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
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Gitea implements Provider against the Gitea REST API.
type Gitea struct {
	APIURL string
	Token  string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

type giteaHook struct {
	ID     int64           `json:"id,omitempty"`
	Active bool            `json:"active"`
	Events []string        `json:"events,omitempty"`
	Config giteaHookConfig `json:"config"`
	Type   string          `json:"type,omitempty"`
}

type giteaHookConfig struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Secret      string `json:"secret,omitempty"`
}

// EnsureWebhook finds an existing hook by delivery URL and updates it, or
// creates a new one.
func (g *Gitea) EnsureWebhook(ctx context.Context, repo string, spec WebhookSpec) (string, error) {
	existing := []giteaHook{}
	if err := g.do(ctx, http.MethodGet, "/repos/"+repoPath(repo)+"/hooks?page=1&limit=100", nil, &existing); err != nil {
		return "", err
	}

	desired := giteaHook{
		Type:   "gitea",
		Active: true,
		Events: spec.Events,
		Config: giteaHookConfig{URL: spec.URL, ContentType: "json", Secret: spec.Secret},
	}

	for _, hook := range existing {
		if hook.Config.URL == spec.URL {
			path := "/repos/" + repoPath(repo) + "/hooks/" + strconv.FormatInt(hook.ID, 10)
			if err := g.do(ctx, http.MethodPatch, path, &desired, nil); err != nil {
				return "", err
			}
			return strconv.FormatInt(hook.ID, 10), nil
		}
	}

	created := giteaHook{}
	if err := g.do(ctx, http.MethodPost, "/repos/"+repoPath(repo)+"/hooks", &desired, &created); err != nil {
		return "", err
	}
	return strconv.FormatInt(created.ID, 10), nil
}

// DeleteWebhook removes the hook; a 404 is treated as already deleted.
func (g *Gitea) DeleteWebhook(ctx context.Context, repo, id string) error {
	err := g.do(ctx, http.MethodDelete, "/repos/"+repoPath(repo)+"/hooks/"+id, nil, nil)
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (g *Gitea) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(g.APIURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+g.Token)
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
		return &httpError{provider: ProviderGitea, status: resp.StatusCode, body: string(snippet)}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

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
	"net/url"
	"strconv"
	"strings"
)

// GitLab implements Provider against the GitLab REST API.
type GitLab struct {
	APIURL string
	Token  string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

type gitlabHook struct {
	ID                 int64  `json:"id,omitempty"`
	URL                string `json:"url"`
	Token              string `json:"token,omitempty"`
	PushEvents         bool   `json:"push_events"`
	MergeRequestEvents bool   `json:"merge_requests_events"`
}

// EnsureWebhook finds an existing hook by delivery URL and updates it, or
// creates a new one.
func (g *GitLab) EnsureWebhook(ctx context.Context, repo string, spec WebhookSpec) (string, error) {
	existing := []gitlabHook{}
	if err := g.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(repo)+"/hooks?per_page=100", nil, &existing); err != nil {
		return "", err
	}

	desired := gitlabHook{
		URL:                spec.URL,
		Token:              spec.Secret,
		PushEvents:         hasWebhookEvent(spec.Events, "push"),
		MergeRequestEvents: hasWebhookEvent(spec.Events, "pull_request") || hasWebhookEvent(spec.Events, "merge_request"),
	}

	for _, hook := range existing {
		if hook.URL == spec.URL {
			path := "/projects/" + url.PathEscape(repo) + "/hooks/" + strconv.FormatInt(hook.ID, 10)
			if err := g.do(ctx, http.MethodPut, path, &desired, nil); err != nil {
				return "", err
			}
			return strconv.FormatInt(hook.ID, 10), nil
		}
	}

	created := gitlabHook{}
	if err := g.do(ctx, http.MethodPost, "/projects/"+url.PathEscape(repo)+"/hooks", &desired, &created); err != nil {
		return "", err
	}
	return strconv.FormatInt(created.ID, 10), nil
}

// DeleteWebhook removes the hook; a 404 is treated as already deleted.
func (g *GitLab) DeleteWebhook(ctx context.Context, repo, id string) error {
	err := g.do(ctx, http.MethodDelete, "/projects/"+url.PathEscape(repo)+"/hooks/"+url.PathEscape(id), nil, nil)
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (g *GitLab) do(ctx context.Context, method, path string, in, out any) error {
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
	req.Header.Set("PRIVATE-TOKEN", g.Token)
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
		return &httpError{provider: ProviderGitLab, status: resp.StatusCode, body: string(snippet)}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func hasWebhookEvent(events []string, want string) bool {
	for _, event := range events {
		if strings.EqualFold(event, want) {
			return true
		}
	}
	return false
}

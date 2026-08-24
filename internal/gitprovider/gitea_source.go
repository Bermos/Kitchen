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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Gitea reads a repository as well as receiving its webhooks, which is what
// the "auto" build strategy needs: without it a Gitea project's every build
// parks waiting for a repository nothing can look at.
var _ SourceReader = (*Gitea)(nil)

// giteaContent is one entry of the contents API's answer. As on GitHub the
// same endpoint serves a directory and a file; detection only asks it for
// listings, and files come back raw from a separate endpoint.
type giteaContent struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ListDir implements SourceReader against Gitea's contents API.
func (g *Gitea) ListDir(ctx context.Context, repo, ref, dir string) ([]DirEntry, error) {
	contents := []giteaContent{}
	path := "/repos/" + repoPath(repo) + "/contents/" + escapePath(dir)
	path = strings.TrimSuffix(path, "/") + "?ref=" + url.QueryEscape(ref)
	if err := g.do(ctx, http.MethodGet, path, nil, &contents); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrFileNotFound, displayPath(dir))
		}
		return nil, err
	}

	entries := make([]DirEntry, 0, len(contents))
	for _, c := range contents {
		entries = append(entries, DirEntry{Name: c.Name, Dir: c.Type == "dir"})
	}
	return entries, nil
}

// ReadFile implements SourceReader. Gitea serves the file itself under /raw/,
// which spares the base64 envelope the contents endpoint would wrap it in.
func (g *Gitea) ReadFile(ctx context.Context, repo, ref, filePath string) ([]byte, error) {
	path := "/repos/" + repoPath(repo) + "/raw/" + escapePath(filePath) + "?ref=" + url.QueryEscape(ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(g.APIURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+g.Token)

	httpClient := g.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrFileNotFound, filePath)
	}
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &httpError{provider: ProviderGitea, status: resp.StatusCode, body: string(snippet)}
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxSourceFileBytes))
}

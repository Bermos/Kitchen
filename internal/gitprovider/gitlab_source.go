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

// GitLab reads a repository as well as receiving its webhooks, which is what
// the "auto" build strategy needs: without it a GitLab project's every build
// parks waiting for a repository nothing can look at.
var _ SourceReader = (*GitLab)(nil)

// gitlabTreeEntry is one entry of the repository tree. GitLab says "tree" and
// "blob" where the others say "dir" and "file".
type gitlabTreeEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// treePageSize is what one listing asks for. Detection reads the root and the
// occasional subdirectory looking for manifests, so a page is the whole
// answer in every repository this is pointed at; GitLab caps it at 100.
const treePageSize = 100

// ListDir implements SourceReader against the repository tree API.
func (g *GitLab) ListDir(ctx context.Context, repo, ref, dir string) ([]DirEntry, error) {
	tree := []gitlabTreeEntry{}
	query := url.Values{}
	query.Set("ref", ref)
	query.Set("per_page", fmt.Sprint(treePageSize))
	if trimmed := strings.Trim(dir, "/"); trimmed != "" {
		query.Set("path", trimmed)
	}
	path := "/projects/" + url.PathEscape(repo) + "/repository/tree?" + query.Encode()
	if err := g.do(ctx, http.MethodGet, path, nil, &tree); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrFileNotFound, displayPath(dir))
		}
		return nil, err
	}

	// An empty listing for a path that is not the root is GitLab's way of
	// saying the directory is not there — the tree endpoint answers 200 with
	// [] rather than 404, which detection would otherwise read as "the
	// directory exists and holds nothing".
	if len(tree) == 0 && strings.Trim(dir, "/") != "" {
		return nil, fmt.Errorf("%w: %s", ErrFileNotFound, displayPath(dir))
	}

	entries := make([]DirEntry, 0, len(tree))
	for _, e := range tree {
		entries = append(entries, DirEntry{Name: e.Name, Dir: e.Type == "tree"})
	}
	return entries, nil
}

// ReadFile implements SourceReader against the raw file endpoint, which
// answers with the file rather than a JSON envelope holding it base64'd.
func (g *GitLab) ReadFile(ctx context.Context, repo, ref, filePath string) ([]byte, error) {
	// GitLab addresses a file as one escaped path segment, slashes included,
	// which is why this is PathEscape over the whole path rather than the
	// segment-by-segment escaping the others take.
	escaped := url.PathEscape(strings.Trim(filePath, "/"))
	path := "/projects/" + url.PathEscape(repo) + "/repository/files/" + escaped +
		"/raw?ref=" + url.QueryEscape(ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(g.APIURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)

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
		return nil, &httpError{provider: ProviderGitLab, status: resp.StatusCode, body: string(snippet)}
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxSourceFileBytes))
}

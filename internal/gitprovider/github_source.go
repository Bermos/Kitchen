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

// maxSourceFileBytes caps what ReadFile brings back. Detection reads package
// manifests, which are kilobytes; the cap is what keeps a repository from
// deciding how much memory the operator uses by committing a large file under
// a name detection looks for.
const maxSourceFileBytes = 1 << 20

// githubContent is one entry of the contents API's answer. The same endpoint
// serves a directory (as an array of these) and a file (as one of them), and
// detection only ever asks it for listings — files come back through the raw
// media type instead, which needs no base64 and has no size ceiling of its
// own.
type githubContent struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ListDir implements SourceReader against the contents API.
func (g *GitHub) ListDir(ctx context.Context, repo, ref, dir string) ([]DirEntry, error) {
	contents := []githubContent{}
	if err := g.do(ctx, http.MethodGet, contentsPath(repo, ref, dir), nil, &contents); err != nil {
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

// ReadFile implements SourceReader. It asks for the raw media type, so what
// comes back is the file rather than a JSON envelope with the file base64'd
// inside it — which the API refuses for anything over a megabyte anyway.
func (g *GitHub) ReadFile(ctx context.Context, repo, ref, filePath string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.APIURL+contentsPath(repo, ref, filePath), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.raw")
	req.Header.Set("Authorization", "Bearer "+g.Token)

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
		return nil, &httpError{status: resp.StatusCode, body: string(snippet)}
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxSourceFileBytes))
}

// contentsPath addresses a path in a repository at a revision. The path is
// escaped segment by segment: a repository is free to name a directory
// anything at all, and the revision arrives from a webhook.
func contentsPath(repo, ref, filePath string) string {
	endpoint := "/repos/" + repo + "/contents/" + escapePath(filePath)
	return strings.TrimSuffix(endpoint, "/") + "?ref=" + url.QueryEscape(ref)
}

func escapePath(filePath string) string {
	segments := strings.Split(strings.Trim(filePath, "/"), "/")
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" || segment == "." {
			continue
		}
		escaped = append(escaped, url.PathEscape(segment))
	}
	return strings.Join(escaped, "/")
}

// displayPath is the directory a listing failure names, so the message reads
// as a repository path rather than as an empty string at the root.
func displayPath(dir string) string {
	if dir = strings.Trim(dir, "/"); dir != "" {
		return dir
	}
	return "the repository root"
}

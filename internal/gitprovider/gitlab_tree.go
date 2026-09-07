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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// GitLab can name the tree object at a build root, with one documented gap —
// see TreeAt.
var _ TreeResolver = (*GitLab)(nil)

// gitlabTreeObject is one entry of the repository tree with the object id
// GitLab calls `id`. For a `tree` entry that id is the directory's own tree
// object, which is exactly what a build root is compared on.
type gitlabTreeObject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// treeMaxPages bounds the walk below. A hundred entries a page over twenty
// pages is two thousand siblings of one build root, which is far past any
// monorepo this is pointed at and still a bounded number of requests.
const treeMaxPages = 20

// TreeAt implements TreeResolver.
//
// A subdirectory is one entry of its parent's tree listing, which GitLab
// serves with the object id of every entry. **The repository root is not**:
// GitLab's REST API names the id of everything inside a tree and never the id
// of the root tree itself, and its commits endpoint does not carry one
// either. So a GitLab project whose build root is the whole repository gets
// ErrTreeUnavailable and is built, every time, rather than compared against
// something derived — which is the safe direction and the only honest one.
// Skipping unchanged builds is a monorepo feature, and a monorepo's project
// has a root directory.
func (g *GitLab) TreeAt(ctx context.Context, repo, ref, dir string) (string, error) {
	parent, name := splitTreePath(dir)
	if name == "" {
		return "", fmt.Errorf(
			"%w: GitLab does not publish the id of a commit's root tree, so a project "+
				"whose build root is the whole repository has nothing to compare", ErrTreeUnavailable)
	}

	for page := 1; page <= treeMaxPages; page++ {
		query := url.Values{}
		query.Set("ref", ref)
		query.Set("per_page", strconv.Itoa(treePageSize))
		query.Set("page", strconv.Itoa(page))
		if parent != "" {
			query.Set("path", parent)
		}
		entries := []gitlabTreeObject{}
		path := "/projects/" + url.PathEscape(repo) + "/repository/tree?" + query.Encode()
		if err := g.do(ctx, http.MethodGet, path, nil, &entries); err != nil {
			if isNotFound(err) {
				return "", fmt.Errorf("%w: %s", ErrFileNotFound, displayPath(dir))
			}
			return "", err
		}
		listing := make([]treeEntry, 0, len(entries))
		for _, entry := range entries {
			listing = append(listing, treeEntry{Name: entry.Name, Dir: entry.Type == "tree", ID: entry.ID})
		}
		id, err := pickTree(listing, name, dir)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, ErrFileNotFound) {
			return "", err
		}
		// A short page is the last one, and an empty one is a path that is
		// not there — GitLab answers the tree endpoint with 200 and [] for
		// both, which is why the absence is read off the length here.
		if len(entries) < treePageSize {
			break
		}
	}
	return "", fmt.Errorf("%w: %s", ErrFileNotFound, displayPath(dir))
}

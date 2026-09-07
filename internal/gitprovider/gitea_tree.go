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
	"net/http"
	"net/url"
	"strings"
)

// Gitea can name the tree object at a build root, the same way GitHub can and
// through the same two shapes — its API is modelled on GitHub's here.
var _ TreeResolver = (*Gitea)(nil)

// giteaCommitTree is the root tree of a commit, from the git data API. Gitea
// nests it under `commit` the way GitHub's commits API does, rather than at
// the top level the way GitHub's own git data API does.
type giteaCommitTree struct {
	Commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	} `json:"commit"`
}

// giteaTreeEntry is one entry of a contents listing, with its object id.
type giteaTreeEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// TreeAt implements TreeResolver. See GitHub.TreeAt for why a subdirectory is
// one entry of its parent's listing.
func (g *Gitea) TreeAt(ctx context.Context, repo, ref, dir string) (string, error) {
	parent, name := splitTreePath(dir)
	if name == "" {
		commit := giteaCommitTree{}
		path := "/repos/" + repoPath(repo) + "/git/commits/" + url.PathEscape(ref)
		if err := g.do(ctx, http.MethodGet, path, nil, &commit); err != nil {
			if isNotFound(err) {
				return "", fmt.Errorf("%w: %s at %s", ErrFileNotFound, ref, repo)
			}
			return "", err
		}
		if commit.Commit.Tree.SHA == "" {
			return "", fmt.Errorf("%w: %s named no tree at %s", ErrTreeUnavailable, ref, repo)
		}
		return commit.Commit.Tree.SHA, nil
	}

	entries := []giteaTreeEntry{}
	path := "/repos/" + repoPath(repo) + "/contents/" + escapePath(parent)
	path = strings.TrimSuffix(path, "/") + "?ref=" + url.QueryEscape(ref)
	if err := g.do(ctx, http.MethodGet, path, nil, &entries); err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("%w: %s", ErrFileNotFound, displayPath(dir))
		}
		return "", err
	}
	listing := make([]treeEntry, 0, len(entries))
	for _, entry := range entries {
		listing = append(listing, treeEntry{Name: entry.Name, Dir: entry.Type == dirEntryType, ID: entry.SHA})
	}
	return pickTree(listing, name, dir)
}

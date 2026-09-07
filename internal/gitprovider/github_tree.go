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
)

// GitHub can name the tree object at a build root, which is what lets a push
// that changed nothing under it be skipped (#500).
var _ TreeResolver = (*GitHub)(nil)

// githubCommitTree is the one field of the commits API's answer this needs:
// the root tree of the commit.
type githubCommitTree struct {
	Commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	} `json:"commit"`
}

// githubTreeEntry is one entry of a contents listing, with the object id the
// listing carries beside the name — which for a directory is that
// directory's tree object.
type githubTreeEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// TreeAt implements TreeResolver.
//
// The repository root is the commit's own tree, which the commits API serves
// beside everything else it says about a commit. Any other directory is one
// entry of its *parent's* listing: a git tree names its children and their
// object ids, so the id of `services/api` is what the listing of `services`
// says it is — one request, and no walk.
func (g *GitHub) TreeAt(ctx context.Context, repo, ref, dir string) (string, error) {
	parent, name := splitTreePath(dir)
	if name == "" {
		commit := githubCommitTree{}
		path := "/repos/" + repoPath(repo) + "/commits/" + url.PathEscape(ref)
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

	entries := []githubTreeEntry{}
	if err := g.do(ctx, http.MethodGet, contentsPath(repo, ref, parent), nil, &entries); err != nil {
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

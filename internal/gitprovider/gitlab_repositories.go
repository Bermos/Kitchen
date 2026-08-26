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
)

// GitLab can be asked about a project as well as read one, which is what
// keeps a project its token cannot see from being reported as a root
// directory that is not there — and what spares a caller that named no branch
// from having to guess "main".
var (
	_ DefaultBranchResolver = (*GitLab)(nil)
	_ RepositoryProbe       = (*GitLab)(nil)
)

// gitlabProject is the part of GitLab's project object the platform uses.
// GitLab says "project" where the others say "repository" and "path with
// namespace" where they say "full name", spells visibility as a word rather
// than as a flag, and addresses a project as one escaped path segment rather
// than as owner and name.
type gitlabProject struct {
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
	Visibility        string `json:"visibility"`
	Description       string `json:"description"`
}

// Repository implements RepositoryProbe against the project endpoint, which
// is the one request that tells a project the credential cannot read apart
// from a path inside one it can — and the same request DefaultBranch is after.
func (g *GitLab) Repository(ctx context.Context, repo string) (Repository, error) {
	found := gitlabProject{}
	path := "/projects/" + url.PathEscape(repo)
	if err := g.do(ctx, http.MethodGet, path, nil, &found); err != nil {
		if isNotFound(err) {
			return Repository{}, fmt.Errorf("%w: %s", ErrRepositoryNotFound, repo)
		}
		return Repository{}, err
	}
	return Repository{
		FullName:      found.PathWithNamespace,
		DefaultBranch: found.DefaultBranch,
		// Anything short of public is visible only to somebody the project
		// admits, which is what the platform means by private: an internal
		// project is not one an anonymous visitor can read.
		Private:     found.Visibility != "public",
		Description: found.Description,
	}, nil
}

// DefaultBranch implements DefaultBranchResolver from the project itself,
// reported in the terms that interface promises rather than in the probe's.
func (g *GitLab) DefaultBranch(ctx context.Context, repo string) (string, error) {
	found, err := g.Repository(ctx, repo)
	if errors.Is(err, ErrRepositoryNotFound) {
		return "", fmt.Errorf("%w: %s", ErrFileNotFound, repo)
	}
	if err != nil {
		return "", err
	}
	return found.DefaultBranch, nil
}

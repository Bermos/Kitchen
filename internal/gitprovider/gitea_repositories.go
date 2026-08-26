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
)

// Gitea can be asked about a repository as well as read one, which is what
// keeps a repository its token cannot see from being reported as a root
// directory that is not there — and what spares a caller that named no branch
// from having to guess "main".
var (
	_ DefaultBranchResolver = (*Gitea)(nil)
	_ RepositoryProbe       = (*Gitea)(nil)
)

// giteaRepository is the part of Gitea's repository object the platform uses.
// Gitea models a repository the way GitHub does, field names and all, as it
// copied its status codes.
type giteaRepository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Description   string `json:"description"`
}

// Repository implements RepositoryProbe against Gitea's repository endpoint,
// which is the one request that tells a repository the credential cannot read
// apart from a path inside one it can — and the same request DefaultBranch is
// after.
func (g *Gitea) Repository(ctx context.Context, repo string) (Repository, error) {
	found := giteaRepository{}
	if err := g.do(ctx, http.MethodGet, "/repos/"+repoPath(repo), nil, &found); err != nil {
		if isNotFound(err) {
			return Repository{}, fmt.Errorf("%w: %s", ErrRepositoryNotFound, repo)
		}
		return Repository{}, err
	}
	return Repository(found), nil
}

// DefaultBranch implements DefaultBranchResolver from the repository itself,
// reported in the terms that interface promises rather than in the probe's.
func (g *Gitea) DefaultBranch(ctx context.Context, repo string) (string, error) {
	found, err := g.Repository(ctx, repo)
	if errors.Is(err, ErrRepositoryNotFound) {
		return "", fmt.Errorf("%w: %s", ErrFileNotFound, repo)
	}
	if err != nil {
		return "", err
	}
	return found.DefaultBranch, nil
}

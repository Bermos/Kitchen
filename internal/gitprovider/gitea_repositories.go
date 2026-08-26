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
)

// Gitea can say what a repository's trunk is, which is what spares a caller
// that named no branch from having to guess "main".
var _ DefaultBranchResolver = (*Gitea)(nil)

// giteaRepository is the part of Gitea's repository object this needs. Gitea
// models a repository the way GitHub does, `default_branch` included.
type giteaRepository struct {
	DefaultBranch string `json:"default_branch"`
}

// DefaultBranch implements DefaultBranchResolver against the repository
// endpoint.
func (g *Gitea) DefaultBranch(ctx context.Context, repo string) (string, error) {
	found := giteaRepository{}
	if err := g.do(ctx, http.MethodGet, "/repos/"+repoPath(repo), nil, &found); err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("%w: %s", ErrFileNotFound, repo)
		}
		return "", err
	}
	return found.DefaultBranch, nil
}

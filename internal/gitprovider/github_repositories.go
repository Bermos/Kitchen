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

// The listing's bounds. GitHub pages at 100, and a credential on a large
// organisation can see far more repositories than a picker is any use for, so
// the walk stops at repositoryPageLimit pages and says it stopped. Five pages
// is the most a person will ever scroll past the filter field, and it is four
// requests more than the common case makes.
const (
	repositoryPageSize  = 100
	repositoryPageLimit = 5
)

// githubRepository is the part of GitHub's repository object the picker uses.
type githubRepository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Description   string `json:"description"`
}

// ListRepositories implements RepositoryLister against /user/repos: every
// repository the token can reach, whether it is owned by the account, shared
// with it, or reached through an organisation it belongs to. That is
// deliberately wider than "the account's own" — the repositories a platform
// deploys are usually an organisation's — and it is the same set the webhook
// this platform will register would be accepted on.
//
// Ordering is the provider's: most recently pushed first, so a truncated
// listing keeps the repositories somebody is actually working on.
func (g *GitHub) ListRepositories(ctx context.Context) (RepositoryListing, error) {
	listing := RepositoryListing{Repositories: []Repository{}}

	for page := 1; page <= repositoryPageLimit; page++ {
		path := fmt.Sprintf("/user/repos?per_page=%d&page=%d&sort=pushed&affiliation=owner,collaborator,organization_member",
			repositoryPageSize, page)
		batch := []githubRepository{}
		if err := g.do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return RepositoryListing{}, err
		}
		for _, repo := range batch {
			// The wire shape and the platform's shape are the same fields
			// under different names, so this is a conversion: a field added
			// to Repository stops compiling here rather than arriving empty.
			listing.Repositories = append(listing.Repositories, Repository(repo))
		}
		// A short page is the last one. A full final page is the only case
		// the cap can have cut something off, and it is reported rather than
		// guessed at.
		if len(batch) < repositoryPageSize {
			return listing, nil
		}
		if page == repositoryPageLimit {
			listing.Truncated = true
		}
	}
	return listing, nil
}

// GitHub answers about one repository as readily as about all of them, which
// is what a caller that was handed a repository name — rather than picking one
// out of the listing — needs before it can read anything at a ref.
var (
	_ DefaultBranchResolver = (*GitHub)(nil)
	_ RepositoryProbe       = (*GitHub)(nil)
)

// Repository implements RepositoryProbe against /repos/{owner}/{name}, which
// is the one request that distinguishes a repository this credential cannot
// read from a path inside a repository it can. A fine-grained token left this
// repository out of its selection is a 404 here, exactly as a repository that
// was never created is.
//
// It is also the request DefaultBranch is after — the repository endpoint
// carries the same `default_branch` the listing does — so the two questions
// are one implementation and one round trip.
func (g *GitHub) Repository(ctx context.Context, repo string) (Repository, error) {
	found := githubRepository{}
	if err := g.do(ctx, http.MethodGet, "/repos/"+repoPath(repo), nil, &found); err != nil {
		if isNotFound(err) {
			return Repository{}, fmt.Errorf("%w: %s", ErrRepositoryNotFound, repo)
		}
		return Repository{}, err
	}
	return Repository(found), nil
}

// DefaultBranch implements DefaultBranchResolver from the repository itself.
// A repository the token cannot see is a 404 here too, which is the same
// answer for this caller's purposes — there is no repository to name a branch
// of — so it is reported in the terms this interface promises rather than in
// the probe's.
func (g *GitHub) DefaultBranch(ctx context.Context, repo string) (string, error) {
	found, err := g.Repository(ctx, repo)
	if errors.Is(err, ErrRepositoryNotFound) {
		return "", fmt.Errorf("%w: %s", ErrFileNotFound, repo)
	}
	if err != nil {
		return "", err
	}
	return found.DefaultBranch, nil
}

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
	"strconv"
	"strings"
)

// Gitea reports commit statuses and pull request comments. It implements no
// DeploymentPublisher: Gitea has no deployments API, so there is no record to
// write and nothing is lost by saying so rather than failing a call.
var _ StatusReporter = (*Gitea)(nil)

// giteaDescriptionLimit is a conservative cut for the one-line description.
// Gitea stores it in a bounded column, and a build failure's message is
// routinely longer than anything a forge will take.
const giteaDescriptionLimit = 255

type giteaCommitStatus struct {
	State       string `json:"state"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Context     string `json:"context"`
}

// SetCommitStatus posts a status check on a commit. Gitea keys statuses by
// context and shows the newest per context, exactly as GitHub does, so this
// is an upsert without a lookup — and it takes the same four state words,
// which is why CommitState needs no translating here.
func (g *Gitea) SetCommitStatus(ctx context.Context, repo string, status CommitStatus) error {
	body := giteaCommitStatus{
		State:       string(status.State),
		TargetURL:   status.TargetURL,
		Description: truncate(status.Description, giteaDescriptionLimit),
		Context:     status.Context,
	}
	path := "/repos/" + repoPath(repo) + "/statuses/" + status.SHA
	return g.do(ctx, http.MethodPost, path, &body, nil)
}

type giteaComment struct {
	ID   int64  `json:"id,omitempty"`
	Body string `json:"body"`
}

// UpsertComment rewrites the platform's comment on a pull request, in this
// order: the comment whose ID the caller remembered, the comment carrying the
// marker, or a new one. A pull request is an issue to Gitea as it is to
// GitHub, which is why the paths say "issues".
func (g *Gitea) UpsertComment(ctx context.Context, repo string, comment Comment) (string, error) {
	body := giteaComment{Body: comment.Body}

	if comment.ID != "" {
		path := "/repos/" + repoPath(repo) + "/issues/comments/" + comment.ID
		err := g.do(ctx, http.MethodPatch, path, &body, nil)
		if err == nil {
			return comment.ID, nil
		}
		// Anything but a comment that is no longer there is the caller's
		// problem; a deleted comment is written again below.
		if !isNotFound(err) {
			return "", err
		}
	}

	id, err := g.findComment(ctx, repo, comment)
	if err != nil {
		return "", err
	}
	if id != 0 {
		path := "/repos/" + repoPath(repo) + "/issues/comments/" + strconv.FormatInt(id, 10)
		if err := g.do(ctx, http.MethodPatch, path, &body, nil); err != nil {
			return "", err
		}
		return strconv.FormatInt(id, 10), nil
	}

	created := giteaComment{}
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repoPath(repo), comment.PullRequest)
	if err := g.do(ctx, http.MethodPost, path, &body, &created); err != nil {
		return "", err
	}
	return strconv.FormatInt(created.ID, 10), nil
}

// findComment looks for the marker among the pull request's comments and
// returns 0 when it is not there.
func (g *Gitea) findComment(ctx context.Context, repo string, comment Comment) (int64, error) {
	const perPage = 100
	for page := 1; page <= commentSearchPages; page++ {
		existing := []giteaComment{}
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?limit=%d&page=%d",
			repoPath(repo), comment.PullRequest, perPage, page)
		if err := g.do(ctx, http.MethodGet, path, nil, &existing); err != nil {
			return 0, err
		}
		for _, candidate := range existing {
			if strings.Contains(candidate.Body, comment.Marker) {
				return candidate.ID, nil
			}
		}
		if len(existing) < perPage {
			return 0, nil
		}
	}
	return 0, nil
}

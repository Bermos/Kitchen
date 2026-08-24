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
	"strconv"
	"strings"
)

// GitLab reports commit statuses and merge request notes, and keeps a
// deployment record.
var (
	_ StatusReporter      = (*GitLab)(nil)
	_ DeploymentPublisher = (*GitLab)(nil)
)

// gitlabDescriptionLimit is where GitLab documents the cut for a status
// description, and for target_url with it.
const gitlabDescriptionLimit = 255

// GitLab's own state words, which neither the commit-status nor the
// deployment vocabulary shares with the other providers.
const (
	gitlabPending = "pending"
	gitlabRunning = "running"
	gitlabSuccess = "success"
	gitlabFailed  = "failed"
)

// deploymentSearchPage bounds the hunt for the deployment already recorded
// for a commit. GitLab's deployments endpoint filters by environment but not
// by commit, so the match is made here — newest first, which is where a
// deployment the platform created moments ago is.
const deploymentSearchPage = 100

type gitlabCommitStatus struct {
	State       string `json:"state"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	TargetURL   string `json:"target_url,omitempty"`
}

// gitlabCommitState translates the four words every provider shares into
// GitLab's vocabulary. GitLab draws no line between a build that failed and a
// platform that could not run one — it has no "error" — so both arrive as
// failed, which is the truthful half of the distinction: it did not pass.
func gitlabCommitState(state CommitState) string {
	switch state {
	case CommitPending:
		return gitlabPending
	case CommitSuccess:
		return gitlabSuccess
	case CommitFailure, CommitError:
		return gitlabFailed
	default:
		return gitlabFailed
	}
}

// SetCommitStatus posts a status check on a commit. GitLab keys a status by
// its name and replaces the previous one under that name.
func (g *GitLab) SetCommitStatus(ctx context.Context, repo string, status CommitStatus) error {
	body := gitlabCommitStatus{
		State:       gitlabCommitState(status.State),
		Name:        status.Context,
		Description: truncate(status.Description, gitlabDescriptionLimit),
	}
	// GitLab caps target_url at the same length. A cut URL is a broken link
	// rather than a shorter one, so an over-long one is left off instead —
	// nothing the platform builds comes close, and this is the honest cut.
	if len(status.TargetURL) <= gitlabDescriptionLimit {
		body.TargetURL = status.TargetURL
	}
	path := "/projects/" + url.PathEscape(repo) + "/statuses/" + url.PathEscape(status.SHA)
	return g.do(ctx, http.MethodPost, path, &body, nil)
}

type gitlabNote struct {
	ID   int64  `json:"id,omitempty"`
	Body string `json:"body"`
}

// UpsertComment rewrites the platform's note on a merge request, in this
// order: the note whose ID the caller remembered, the note carrying the
// marker, or a new one. GitLab calls a comment a note, and addresses it under
// the merge request rather than globally, so an edit needs the iid too.
func (g *GitLab) UpsertComment(ctx context.Context, repo string, comment Comment) (string, error) {
	body := gitlabNote{Body: comment.Body}
	notes := fmt.Sprintf("/projects/%s/merge_requests/%d/notes", url.PathEscape(repo), comment.PullRequest)

	if comment.ID != "" {
		err := g.do(ctx, http.MethodPut, notes+"/"+url.PathEscape(comment.ID), &body, nil)
		if err == nil {
			return comment.ID, nil
		}
		// Anything but a note that is no longer there is the caller's
		// problem; a deleted note is written again below.
		if !isNotFound(err) {
			return "", err
		}
	}

	id, err := g.findNote(ctx, notes, comment.Marker)
	if err != nil {
		return "", err
	}
	if id != 0 {
		if err := g.do(ctx, http.MethodPut, notes+"/"+strconv.FormatInt(id, 10), &body, nil); err != nil {
			return "", err
		}
		return strconv.FormatInt(id, 10), nil
	}

	created := gitlabNote{}
	if err := g.do(ctx, http.MethodPost, notes, &body, &created); err != nil {
		return "", err
	}
	return strconv.FormatInt(created.ID, 10), nil
}

// findNote looks for the marker among the merge request's notes and returns 0
// when it is not there.
func (g *GitLab) findNote(ctx context.Context, notes, marker string) (int64, error) {
	const perPage = 100
	for page := 1; page <= commentSearchPages; page++ {
		existing := []gitlabNote{}
		path := fmt.Sprintf("%s?per_page=%d&page=%d", notes, perPage, page)
		if err := g.do(ctx, http.MethodGet, path, nil, &existing); err != nil {
			return 0, err
		}
		for _, candidate := range existing {
			if strings.Contains(candidate.Body, marker) {
				return candidate.ID, nil
			}
		}
		if len(existing) < perPage {
			return 0, nil
		}
	}
	return 0, nil
}

type gitlabDeployment struct {
	ID          int64  `json:"id,omitempty"`
	Environment string `json:"environment,omitempty"`
	SHA         string `json:"sha,omitempty"`
	Ref         string `json:"ref,omitempty"`
	Tag         bool   `json:"tag"`
	Status      string `json:"status,omitempty"`
}

// gitlabDeploymentEntry is one row of the deployments listing, which nests the
// commit rather than carrying a sha of its own.
type gitlabDeploymentEntry struct {
	ID         int64 `json:"id"`
	Deployable struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	} `json:"deployable"`
	SHA string `json:"sha"`
}

// PublishDeployment records the deployment of a commit to an environment,
// creating it on first use and moving its status afterwards.
//
// DeploymentInactive has no counterpart: GitLab's four statuses are running,
// success, failed and canceled, and none of them means "this environment is
// gone". Marking a deployment that succeeded as canceled would be a lie, so a
// retirement writes nothing here — the pull request comment is what tells a
// reviewer the preview went away.
func (g *GitLab) PublishDeployment(ctx context.Context, repo string, deployment Deployment) error {
	status, ok := gitlabDeploymentStatus(deployment.State)
	if !ok {
		return nil
	}

	id, err := g.deploymentID(ctx, repo, deployment)
	if err != nil {
		return err
	}
	if id != 0 {
		body := gitlabDeployment{Status: status}
		path := fmt.Sprintf("/projects/%s/deployments/%d", url.PathEscape(repo), id)
		return g.do(ctx, http.MethodPut, path, &body, nil)
	}

	// GitLab requires a ref on create and the platform does not always have
	// one — a retired preview carries only the commit — so the commit stands
	// in for it, which GitLab accepts.
	ref := deployment.Ref
	if ref == "" {
		ref = deployment.SHA
	}
	body := gitlabDeployment{
		Environment: deployment.Environment,
		SHA:         deployment.SHA,
		Ref:         ref,
		Tag:         false,
		Status:      status,
	}
	path := "/projects/" + url.PathEscape(repo) + "/deployments"
	return g.do(ctx, http.MethodPost, path, &body, nil)
}

// gitlabDeploymentStatus maps a state onto GitLab's four. The second return is
// false for a state GitLab cannot hold, which the caller writes nothing for.
func gitlabDeploymentStatus(state DeploymentState) (string, bool) {
	switch state {
	case DeploymentInProgress:
		return gitlabRunning, true
	case DeploymentSuccess:
		return gitlabSuccess, true
	case DeploymentFailure:
		return gitlabFailed, true
	default:
		return "", false
	}
}

// deploymentID returns the deployment GitLab already holds for this commit and
// environment, or 0 when there is none. The listing filters by environment
// only, so the commit is matched here.
func (g *GitLab) deploymentID(ctx context.Context, repo string, deployment Deployment) (int64, error) {
	query := url.Values{
		"environment": {deployment.Environment},
		"order_by":    {"id"},
		"sort":        {"desc"},
		"per_page":    {strconv.Itoa(deploymentSearchPage)},
	}
	existing := []gitlabDeploymentEntry{}
	path := "/projects/" + url.PathEscape(repo) + "/deployments?" + query.Encode()
	if err := g.do(ctx, http.MethodGet, path, nil, &existing); err != nil {
		// An environment nobody has deployed to is not an error, it is the
		// first deployment.
		if isNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	for _, candidate := range existing {
		if candidate.SHA == deployment.SHA || candidate.Deployable.Commit.ID == deployment.SHA {
			return candidate.ID, nil
		}
	}
	return 0, nil
}

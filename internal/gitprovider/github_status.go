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
	"unicode/utf8"
)

// GitHub implements StatusReporter as well as Provider.
var _ StatusReporter = (*GitHub)(nil)

// githubDescriptionLimit is where GitHub rejects a status description. It
// applies to commit statuses and deployment statuses alike, and a build
// failure's message is routinely longer than that, so descriptions are cut
// rather than allowed to fail the request.
const githubDescriptionLimit = 140

// commentSearchPages bounds the hunt for the platform's own pull request
// comment. It only runs when the comment's ID is unknown — the first post, or
// after somebody deleted it — and GitHub returns comments oldest first, so
// the platform's own comment is on the first page of all but the busiest
// pull requests. Giving up after this many pages posts a second comment,
// which is a better outcome than paging through a thousand review replies.
const commentSearchPages = 3

type githubCommitStatus struct {
	State       string `json:"state"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Context     string `json:"context"`
}

// SetCommitStatus posts a status check on a commit. GitHub keys statuses by
// context and shows the newest one per context, so this is an upsert without
// a lookup.
func (g *GitHub) SetCommitStatus(ctx context.Context, repo string, status CommitStatus) error {
	body := githubCommitStatus{
		State:       string(status.State),
		TargetURL:   status.TargetURL,
		Description: truncate(status.Description, githubDescriptionLimit),
		Context:     status.Context,
	}
	path := fmt.Sprintf("/repos/%s/statuses/%s", repo, status.SHA)
	return g.do(ctx, http.MethodPost, path, &body, nil)
}

type githubDeployment struct {
	ID int64 `json:"id,omitempty"`
	// Ref is the commit (or branch) being deployed.
	Ref         string `json:"ref,omitempty"`
	Environment string `json:"environment,omitempty"`
	Description string `json:"description,omitempty"`
	// AutoMerge off: the platform deploys the commit it was handed, and
	// GitHub's default would merge the base branch into it first and answer
	// 202 without creating anything. It is sent even when false, which is
	// the whole point of setting it.
	AutoMerge bool `json:"auto_merge"`
	// RequiredContexts empty: the deployment records what already happened,
	// so waiting for other checks to be green would only refuse it.
	RequiredContexts []string `json:"required_contexts"`
	TransientEnv     bool     `json:"transient_environment,omitempty"`
	ProductionEnv    bool     `json:"production_environment,omitempty"`
}

type githubDeploymentStatus struct {
	State          string `json:"state"`
	Description    string `json:"description,omitempty"`
	EnvironmentURL string `json:"environment_url,omitempty"`
	// AutoInactive off: retiring an environment is the platform's call, made
	// when the Environment goes away, not GitHub's on every new deployment.
	AutoInactive bool `json:"auto_inactive"`
}

// PublishDeployment finds the deployment for this commit and environment (or
// creates it) and appends the state to it.
func (g *GitHub) PublishDeployment(ctx context.Context, repo string, deployment Deployment) error {
	id, err := g.deploymentID(ctx, repo, deployment)
	if err != nil {
		return err
	}

	status := githubDeploymentStatus{
		State:       string(deployment.State),
		Description: truncate(deployment.Description, githubDescriptionLimit),
	}
	// A retired deployment has no address worth handing a reader.
	if deployment.State != DeploymentInactive {
		status.EnvironmentURL = deployment.URL
	}
	path := fmt.Sprintf("/repos/%s/deployments/%d/statuses", repo, id)
	return g.do(ctx, http.MethodPost, path, &status, nil)
}

// deploymentID returns the deployment GitHub already holds for this commit and
// environment, creating it when there is none.
func (g *GitHub) deploymentID(ctx context.Context, repo string, deployment Deployment) (int64, error) {
	query := url.Values{
		"sha":         {deployment.SHA},
		"environment": {deployment.Environment},
		"per_page":    {"1"},
	}
	existing := []githubDeployment{}
	listPath := fmt.Sprintf("/repos/%s/deployments?%s", repo, query.Encode())
	if err := g.do(ctx, http.MethodGet, listPath, nil, &existing); err != nil {
		return 0, err
	}
	if len(existing) > 0 {
		return existing[0].ID, nil
	}

	created := githubDeployment{}
	body := githubDeployment{
		Ref:              deployment.SHA,
		Environment:      deployment.Environment,
		Description:      truncate(deployment.Description, githubDescriptionLimit),
		RequiredContexts: []string{},
		TransientEnv:     deployment.Transient,
		ProductionEnv:    deployment.Production,
	}
	if err := g.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/deployments", repo), &body, &created); err != nil {
		return 0, err
	}
	if created.ID == 0 {
		return 0, fmt.Errorf("github accepted the deployment for %s but returned no id", deployment.Environment)
	}
	return created.ID, nil
}

type githubComment struct {
	ID   int64  `json:"id,omitempty"`
	Body string `json:"body"`
}

// UpsertComment rewrites the platform's comment on a pull request, in this
// order: the comment whose ID the caller remembered, the comment carrying the
// marker, or a new one. Pull request comments are issue comments to GitHub,
// which is why the paths say "issues".
func (g *GitHub) UpsertComment(ctx context.Context, repo string, comment Comment) (string, error) {
	body := githubComment{Body: comment.Body}

	if comment.ID != "" {
		updated := githubComment{}
		path := fmt.Sprintf("/repos/%s/issues/comments/%s", repo, comment.ID)
		err := g.do(ctx, http.MethodPatch, path, &body, &updated)
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
		updated := githubComment{}
		path := fmt.Sprintf("/repos/%s/issues/comments/%d", repo, id)
		if err := g.do(ctx, http.MethodPatch, path, &body, &updated); err != nil {
			return "", err
		}
		return strconv.FormatInt(id, 10), nil
	}

	created := githubComment{}
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, comment.PullRequest)
	if err := g.do(ctx, http.MethodPost, path, &body, &created); err != nil {
		return "", err
	}
	return strconv.FormatInt(created.ID, 10), nil
}

// findComment looks for the marker among the pull request's comments and
// returns 0 when it is not there.
func (g *GitHub) findComment(ctx context.Context, repo string, comment Comment) (int64, error) {
	const perPage = 100
	for page := 1; page <= commentSearchPages; page++ {
		existing := []githubComment{}
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d&page=%d",
			repo, comment.PullRequest, perPage, page)
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

// truncate cuts a string to a byte budget, marking that it was cut. Providers
// count bytes, so the cut does too — it is only walked back off a partial rune
// so a commit message full of emoji cannot arrive as invalid UTF-8.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const ellipsis = "…"
	cut := limit - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

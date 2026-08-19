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
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// GitHub's answer to "how did this commit get here".
//
// Two calls, because GitHub keeps the two halves apart: which pull request a
// commit is associated with, and who reviewed that request.
//
// The first is `/commits/{sha}/pulls`, and using it rather than reading the
// commit is the whole point. GitHub tracks the association across **squash and
// rebase merges**, where the commit that lands on the default branch is a new
// object that names neither the request nor the reviewer. Parsing the commit
// message for "(#123)" is the usual shortcut and it is wrong exactly when it
// matters: on a repository whose merge button was configured differently, or a
// request merged with a hand-edited message.

// githubPullSummary is one pull request as the association endpoint reports it.
type githubPullSummary struct {
	Number int32  `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	MergedAt string `json:"merged_at"`
	MergedBy *struct {
		Login string `json:"login"`
	} `json:"merged_by"`
}

// githubReview is one review. GitHub keeps every review ever left, so state and
// ordering are both load-bearing.
type githubReview struct {
	State string `json:"state"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
	SubmittedAt string `json:"submitted_at"`
}

// CommitProvenance implements ChangeReader.
func (g *GitHub) CommitProvenance(ctx context.Context, repo, sha string) (ChangeProvenance, error) {
	provenance := ChangeProvenance{Provider: ProviderGitHub}

	pulls := []githubPullSummary{}
	path := "/repos/" + repoPath(repo) + "/commits/" + url.PathEscape(sha) + "/pulls"
	if err := g.do(ctx, http.MethodGet, path, nil, &pulls); err != nil {
		if isNotFound(err) {
			// The commit is not one GitHub can associate with anything. That
			// is an answer, not a failure: it is what a direct push looks
			// like, and refusing to distinguish it from an outage would make
			// every outage look like a policy violation.
			return provenance, nil
		}
		return ChangeProvenance{}, err
	}
	pull, found := mergedPull(pulls)
	if !found {
		return provenance, nil
	}

	provenance.PullRequest = pull.Number
	provenance.Title = pull.Title
	provenance.Author = pull.User.Login
	if pull.MergedBy != nil {
		provenance.MergedBy = pull.MergedBy.Login
	}

	reviews := []githubReview{}
	reviewPath := "/repos/" + repoPath(repo) + "/pulls/" + strconv.Itoa(int(pull.Number)) + "/reviews?per_page=100"
	if err := g.do(ctx, http.MethodGet, reviewPath, nil, &reviews); err != nil {
		if isNotFound(err) {
			return provenance, nil
		}
		return ChangeProvenance{}, err
	}
	provenance.Approvals = standingApprovals(reviews, provenance.Author)
	return provenance, nil
}

// mergedPull picks the request the commit actually arrived through.
//
// A commit can be associated with several — a branch cut from another branch
// produces that routinely — and the merged one is the one that put it on the
// default branch. Where none is merged the commit is still on a branch of its
// own, which is the ordinary state of a pull request build and not a fact worth
// asserting anything about.
func mergedPull(pulls []githubPullSummary) (githubPullSummary, bool) {
	for _, pull := range pulls {
		if pull.MergedAt != "" {
			return pull, true
		}
	}
	return githubPullSummary{}, false
}

// standingApprovals reduces every review ever left to the ones that still
// stand: the newest per reviewer, and only where that newest one is an
// approval.
//
// The reduction is the substance. GitHub returns the full history, so a
// reviewer who approved and then requested changes appears twice, and an
// approval a later push dismissed is still in the list with its state changed
// to DISMISSED. Counting either would produce evidence that a change was
// approved when the approval had been withdrawn before it merged — which is a
// worse outcome than having no evidence, because somebody would rely on it.
func standingApprovals(reviews []githubReview, author string) []Approval {
	latest := map[string]githubReview{}
	for _, review := range reviews {
		// COMMENTED reviews leave the previous verdict standing on GitHub, so
		// they are skipped rather than treated as the newest word.
		if strings.EqualFold(review.State, "COMMENTED") {
			continue
		}
		if review.User.Login == "" {
			continue
		}
		held, seen := latest[review.User.Login]
		if !seen || review.SubmittedAt >= held.SubmittedAt {
			latest[review.User.Login] = review
		}
	}

	reviewers := make([]string, 0, len(latest))
	for reviewer := range latest {
		reviewers = append(reviewers, reviewer)
	}
	// Stable order, so that two reads of the same request produce the same
	// evidence and a diff of two attestations means something.
	sort.Strings(reviewers)

	approvals := []Approval{}
	for _, reviewer := range reviewers {
		review := latest[reviewer]
		if !strings.EqualFold(review.State, "APPROVED") {
			continue
		}
		approvals = append(approvals, Approval{
			Reviewer:     reviewer,
			SubmittedAt:  review.SubmittedAt,
			SelfApproval: author != "" && strings.EqualFold(reviewer, author),
		})
	}
	return approvals
}

// repoPath escapes an owner/name pair for a URL path.
func repoPath(repo string) string {
	owner, name, found := strings.Cut(repo, "/")
	if !found {
		return url.PathEscape(repo)
	}
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

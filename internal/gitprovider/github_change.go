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
	provenance.Approvals = standingApprovals(githubVerdicts(reviews), provenance.Author)
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

// githubVerdicts translates GitHub's review history into the vocabulary
// standingApprovals reduces, which is where "still stands" is decided.
//
// Two GitHub facts are applied here and nowhere else. A dismissed review is
// still in the list with its state rewritten to DISMISSED, so it arrives as
// the reviewer's newest word and not as an approval — which is exactly what
// supersedes the approval before it. A COMMENTED review leaves the previous
// verdict standing, so it is dropped rather than treated as the newest word.
func githubVerdicts(reviews []githubReview) []reviewVerdict {
	verdicts := make([]reviewVerdict, 0, len(reviews))
	for _, review := range reviews {
		if strings.EqualFold(review.State, "COMMENTED") {
			continue
		}
		verdicts = append(verdicts, reviewVerdict{
			Reviewer:    review.User.Login,
			SubmittedAt: review.SubmittedAt,
			Approved:    strings.EqualFold(review.State, stateApproved),
		})
	}
	return verdicts
}

// repoPath escapes an owner/name pair for a URL path.
func repoPath(repo string) string {
	owner, name, found := strings.Cut(repo, "/")
	if !found {
		return url.PathEscape(repo)
	}
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

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

// Gitea's answer to "how did this commit get here".
//
// Gitea's review model is GitHub's — a pull request, reviews carrying a state,
// an approval that can be dismissed — and this reads it the same way. The three
// places it is not GitHub are the ones this file is careful about.
var _ ChangeReader = (*Gitea)(nil)

// # One request, not a list
//
// `/commits/{sha}/pull` answers with **the** merged pull request for a commit,
// where GitHub answers with every request a commit is associated with and
// leaves the choosing to the caller. Gitea resolves it from the pull request's
// recorded merge commit, so a squash merge — the case this whole interface
// exists for, where the commit on the default branch names neither the request
// nor the approver — still resolves. A commit that arrived by direct push is a
// 404, which is an answer rather than a failure.
//
// # A different vocabulary for the same verdicts
//
// Gitea spells them `APPROVED`, `REQUEST_CHANGES`, `COMMENT`, `PENDING` and
// `REQUEST_REVIEW`. Only the first two are verdicts; the rest leave whatever
// the reviewer said before standing, so they are dropped rather than treated as
// the newest word — the same rule as GitHub's `COMMENTED`, in Gitea's spelling.
//
// # A dismissal is a flag, not a rewritten state
//
// Dismissing an approval on GitHub rewrites its state to `DISMISSED`. Gitea
// leaves the state saying `APPROVED` and sets `dismissed` beside it, so reading
// the state alone counts an approval that was explicitly taken away. `stale` is
// the same fact arriving by a different route: the review was left on a commit
// that a later push superseded. Both are excluded — an approval of a revision
// that is not the one that merged is not an approval of what merged, and it is
// the direction to be wrong in.
//
// Gitea's own `official` flag is not read. It says whether the approval counts
// towards the branch protection rule, which is Gitea's policy question; what is
// recorded here is who approved. That is the same choice GitLab's approval
// rules get, for the same reason.

// giteaPullRequest is the pull request the commit arrived through.
type giteaPullRequest struct {
	Number int32  `json:"number"`
	Title  string `json:"title"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	Merged   bool   `json:"merged"`
	MergedAt string `json:"merged_at"`
	MergedBy *struct {
		Login string `json:"login"`
	} `json:"merged_by"`
}

// giteaReview is one review. Gitea keeps every review ever left, as GitHub
// does, so the state, the flags beside it and the ordering are all
// load-bearing.
type giteaReview struct {
	State string `json:"state"`
	// User is absent on a review left by a team rather than a person, which
	// Gitea models as a reviewer too. A team is not a pair of eyes and has no
	// username to record, so such a review is not a verdict here.
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
	SubmittedAt string `json:"submitted_at"`
	Dismissed   bool   `json:"dismissed"`
	Stale       bool   `json:"stale"`
}

// CommitProvenance implements ChangeReader.
func (g *Gitea) CommitProvenance(ctx context.Context, repo, sha string) (ChangeProvenance, error) {
	provenance := ChangeProvenance{Provider: ProviderGitea}

	pull := giteaPullRequest{}
	path := "/repos/" + repoPath(repo) + "/commits/" + url.PathEscape(sha) + "/pull"
	if err := g.do(ctx, http.MethodGet, path, nil, &pull); err != nil {
		if isNotFound(err) {
			// The commit belongs to no pull request Gitea knows of. That is
			// an answer, not a failure: it is what a direct push looks like,
			// and refusing to distinguish it from an outage would make every
			// outage look like a policy violation.
			return provenance, nil
		}
		return ChangeProvenance{}, err
	}
	if pull.Number == 0 || (!pull.Merged && pull.MergedAt == "") {
		// A request that has not merged did not put this commit on the
		// default branch; the commit is still on a branch of its own, which
		// is the ordinary state of a pull request build.
		return provenance, nil
	}

	provenance.PullRequest = pull.Number
	provenance.Title = pull.Title
	provenance.Author = pull.User.Login
	if pull.MergedBy != nil {
		provenance.MergedBy = pull.MergedBy.Login
	}

	reviews := []giteaReview{}
	reviewPath := "/repos/" + repoPath(repo) + "/pulls/" +
		strconv.Itoa(int(pull.Number)) + "/reviews?limit=100"
	if err := g.do(ctx, http.MethodGet, reviewPath, nil, &reviews); err != nil {
		if isNotFound(err) {
			return provenance, nil
		}
		return ChangeProvenance{}, err
	}
	provenance.Approvals = standingApprovals(giteaVerdicts(reviews), provenance.Author)
	return provenance, nil
}

// giteaVerdicts translates Gitea's review history into the vocabulary
// standingApprovals reduces, which is where "still stands" is decided.
//
// A dismissed or stale review arrives as the reviewer's newest word and not as
// an approval, which is what makes it supersede the approval before it rather
// than merely vanish — a reviewer whose only approval was dismissed has not
// approved, and one who approved again afterwards has.
func giteaVerdicts(reviews []giteaReview) []reviewVerdict {
	verdicts := make([]reviewVerdict, 0, len(reviews))
	for _, review := range reviews {
		if review.User == nil {
			continue
		}
		if !giteaVerdictState(review.State) {
			continue
		}
		verdicts = append(verdicts, reviewVerdict{
			Reviewer:    review.User.Login,
			SubmittedAt: review.SubmittedAt,
			Approved: strings.EqualFold(review.State, stateApproved) &&
				!review.Dismissed && !review.Stale,
		})
	}
	return verdicts
}

// giteaVerdictState reports whether a review state is a verdict at all.
// `COMMENT`, `PENDING` and `REQUEST_REVIEW` are not: each leaves whatever the
// reviewer said before it standing.
func giteaVerdictState(state string) bool {
	return strings.EqualFold(state, stateApproved) ||
		strings.EqualFold(state, "REQUEST_CHANGES")
}

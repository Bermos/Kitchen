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
)

// GitLab's answer to "how did this commit get here".
//
// Two calls, as on GitHub, and neither of them is GitHub's — which is the
// substance of this file. GitLab does not have reviews with an approving
// state; it has **approvals**, which are their own resource on the merge
// request, and the difference decides how "still stands" is answered here.
var _ ChangeReader = (*GitLab)(nil)

// # Which merge request the commit arrived through
//
// `/repository/commits/{sha}/merge_requests` is GitLab's own association, and
// it is asked rather than the commit for the reason the whole interface exists:
// GitLab matches the sha against a merge request's commits, its merge commit
// **and its squash commit**, so a squash-merged change — whose commit on the
// default branch is a new object naming neither the request nor the approver —
// still resolves. Parsing `See merge request !123` out of the message is the
// usual shortcut and it is wrong exactly where it matters.
//
// # What an approval is, and what a dismissed one is
//
// GitLab keeps no history of withdrawn approvals to filter. Revoking an
// approval removes it, and a project with "remove all approvals when commits
// are added" resets them on a push — in both cases the approval is *gone* from
// the resource rather than left in it with its state rewritten, which is how
// GitHub and Gitea record the same event.
//
// So the standing set is exactly what `/approvals` answers with, and the rule
// for this provider is to ask that resource and nothing else. The note history
// on a merge request does carry "approved this merge request" system notes, and
// reconstructing approvals from them would resurrect precisely the approvals
// GitLab has since removed — evidence that a change was approved when the
// approval had been withdrawn before it merged, which is worse than no
// evidence because somebody would rely on it.
//
// # An approval rule satisfied by a group still names a human
//
// GitLab's approval *rules* can be satisfied by any member of a group, which is
// a shape GitHub's reviews do not have. It changes nothing here, deliberately:
// `approved_by` names the person who clicked approve, never the group they
// satisfied the rule for, and it is the person that is recorded. A group is not
// a pair of eyes, and whether the rule the approval satisfied was the right one
// is a policy question about the project rather than a fact about the change.

// gitlabUser is however GitLab names a person in these payloads. The username
// is the identity; `name` is a display name and is not one.
type gitlabUser struct {
	Username string `json:"username"`
}

// gitlabMergeRequest is one merge request as the association endpoint reports
// it, which is GitLab's ordinary merge request shape.
type gitlabMergeRequest struct {
	// IID is the project-scoped number — the `!42` a person reading the
	// evidence later will recognise. `id` is instance-wide and means nothing
	// to anybody looking at the project.
	IID      int32      `json:"iid"`
	Title    string     `json:"title"`
	State    string     `json:"state"`
	Author   gitlabUser `json:"author"`
	MergedAt string     `json:"merged_at"`
	// MergeUser is who pressed merge. GitLab deprecated `merged_by` in favour
	// of it and still serves both, so both are read and the current spelling
	// wins — an instance that answers with only the old one is not a fact
	// worth losing.
	MergeUser *gitlabUser `json:"merge_user"`
	MergedBy  *gitlabUser `json:"merged_by"`
}

// merger is who merged it, whichever of the two spellings the instance used.
func (m gitlabMergeRequest) merger() string {
	if m.MergeUser != nil && m.MergeUser.Username != "" {
		return m.MergeUser.Username
	}
	if m.MergedBy != nil {
		return m.MergedBy.Username
	}
	return ""
}

// merged reports whether this is the request that put the commit on the
// branch. `state` and `merged_at` are both read because an instance that has
// carried its data across several upgrades has merge requests with the state
// and no timestamp.
func (m gitlabMergeRequest) merged() bool {
	return m.State == "merged" || m.MergedAt != ""
}

// gitlabApprovals is the approvals resource: who has approved, right now.
type gitlabApprovals struct {
	ApprovedBy []struct {
		User gitlabUser `json:"user"`
	} `json:"approved_by"`
}

// CommitProvenance implements ChangeReader.
func (g *GitLab) CommitProvenance(ctx context.Context, repo, sha string) (ChangeProvenance, error) {
	provenance := ChangeProvenance{Provider: ProviderGitLab}

	requests := []gitlabMergeRequest{}
	path := "/projects/" + url.PathEscape(repo) + "/repository/commits/" +
		url.PathEscape(sha) + "/merge_requests?per_page=100"
	if err := g.do(ctx, http.MethodGet, path, nil, &requests); err != nil {
		if isNotFound(err) {
			// A commit GitLab can associate with nothing. That is an answer,
			// not a failure: it is what a direct push looks like, and refusing
			// to distinguish it from an outage would make every outage look
			// like a policy violation.
			return provenance, nil
		}
		return ChangeProvenance{}, err
	}
	request, found := mergedMergeRequest(requests)
	if !found {
		return provenance, nil
	}

	provenance.PullRequest = request.IID
	provenance.Title = request.Title
	provenance.Author = request.Author.Username
	provenance.MergedBy = request.merger()

	approvals := gitlabApprovals{}
	approvalPath := "/projects/" + url.PathEscape(repo) + "/merge_requests/" +
		strconv.Itoa(int(request.IID)) + "/approvals"
	if err := g.do(ctx, http.MethodGet, approvalPath, nil, &approvals); err != nil {
		if isNotFound(err) {
			// The request is reported; its approvals are not. Same shape as
			// the association above: what could not be read is left unsaid
			// rather than asserted either way.
			return provenance, nil
		}
		return ChangeProvenance{}, err
	}
	provenance.Approvals = gitlabStandingApprovals(approvals, provenance.Author)
	return provenance, nil
}

// mergedMergeRequest picks the request the commit actually arrived through.
//
// A commit can be associated with several — a branch cut from another branch
// does it routinely — and the merged one is the one that put it on the default
// branch. Where none is merged the commit is still on a branch of its own,
// which is the ordinary state of a merge request build and not a fact worth
// asserting anything about.
func mergedMergeRequest(requests []gitlabMergeRequest) (gitlabMergeRequest, bool) {
	for _, request := range requests {
		if request.merged() {
			return request, true
		}
	}
	return gitlabMergeRequest{}, false
}

// gitlabStandingApprovals is the approvals resource read as evidence.
//
// There is no reduction to do — GitLab has already dropped everything that no
// longer stands — so this is the mapping alone: the username as the identity,
// the author compared case-insensitively, and no timestamp.
//
// **GitLab reports no per-approval timestamp.** Neither `/approvals` nor
// `/approval_state` carries one; only the merge request's system notes do, and
// those are the history this deliberately does not read. So `SubmittedAt` is
// left empty rather than filled with the merge request's own `merged_at`,
// which would be a time the platform made up and recorded as the provider's.
func gitlabStandingApprovals(approvals gitlabApprovals, author string) []Approval {
	reviewers := make([]string, 0, len(approvals.ApprovedBy))
	seen := map[string]bool{}
	for _, approved := range approvals.ApprovedBy {
		reviewer := approved.User.Username
		if reviewer == "" || seen[reviewer] {
			continue
		}
		seen[reviewer] = true
		reviewers = append(reviewers, reviewer)
	}
	// Sorted for the same reason the other two providers' approvals are: two
	// reads of one merge request have to produce the same evidence, or a diff
	// of two attestations means nothing.
	sort.Strings(reviewers)

	standing := make([]Approval, 0, len(reviewers))
	for _, reviewer := range reviewers {
		standing = append(standing, Approval{
			Reviewer:     reviewer,
			SelfApproval: isAuthor(reviewer, author),
		})
	}
	return standing
}

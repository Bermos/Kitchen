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
	"sort"
	"strings"
)

// How a commit came to be on the branch it is on: who wrote it, who agreed to
// it, and whether anybody did.
//
// Four-eyes on a production change is the control a supervisor asks about
// first, and it has to be recorded rather than inferred from a git provider's
// UI months later. This is the interface that goes and asks while the answer
// is still true.
//
// # Why it asks the provider instead of reading the commit
//
// Because the commit does not know. A squash merge produces a *new* commit on
// the default branch whose author is whoever pressed the button and whose
// message may or may not mention the pull request; the approver appears nowhere
// in it. Rebase merges lose the merge commit entirely. Reading commit metadata
// would therefore answer confidently and wrongly on the two merge strategies
// most organisations actually use — so the provider is asked which pull request
// a commit arrived through, and who reviewed it.
//
// # What "approved" has to mean
//
// Only a review that still stands. A provider records every review ever left,
// including ones a later push dismissed, so the newest review per reviewer is
// the one that counts and a dismissed one counts for nothing. Getting this
// wrong produces evidence that a change was approved when the approval was
// withdrawn before it merged, which is worse than having no evidence.

// Approval is one review that still stands, by one person.
type Approval struct {
	// Reviewer is the provider's identity for whoever approved: a username,
	// not a display name, because a display name is not an identity.
	Reviewer string

	// SubmittedAt is when the review that counts was submitted, as the
	// provider reported it. It is a string rather than a time because it is
	// recorded verbatim into evidence and never arithmetic'd.
	SubmittedAt string

	// SelfApproval is true when the reviewer is the change's own author.
	//
	// It is recorded rather than filtered out. A self-approval is a fact
	// about how the change was made, and whether it is acceptable is a policy
	// question — an installation whose rules permit it on a two-person team
	// and an installation whose rules forbid it outright both need to be able
	// to see that it happened.
	SelfApproval bool
}

// ChangeProvenance is what the provider says about how a commit arrived.
type ChangeProvenance struct {
	// PullRequest is the number of the pull or merge request the commit
	// arrived through. Zero means the provider knows of none, which is the
	// case this whole interface exists to be able to state.
	PullRequest int32

	// Title of that request, recorded because it is what a person reading the
	// evidence later will recognise it by.
	Title string

	// Author is who opened it. Empty when there is no request.
	Author string

	// MergedBy is who merged it, which is not always who approved it and is
	// occasionally the only human involved.
	MergedBy string

	// Approvals are the reviews that still stand, newest per reviewer, with
	// dismissed and superseded ones already dropped.
	Approvals []Approval

	// Provider names who asserted all of this — `github`, `gitlab`, `gitea`.
	// It is carried into the attestation because the platform did not witness
	// any of it: it is repeating a third party's claim, and evidence that
	// hides whose claim it is repeating is evidence about nothing.
	Provider string
}

// Independent reports whether at least one approval came from somebody other
// than the author. It is the question four-eyes actually asks, and it is
// separate from "was it approved" because a change its own author approved has
// been approved.
func (c ChangeProvenance) Independent() bool {
	for _, approval := range c.Approvals {
		if !approval.SelfApproval {
			return true
		}
	}
	return false
}

// SelfApproved reports whether the only approvals are the author's own.
func (c ChangeProvenance) SelfApproved() bool {
	return len(c.Approvals) > 0 && !c.Independent()
}

// Approvers is every reviewer whose approval stands, in the order the provider
// reported them.
func (c ChangeProvenance) Approvers() []string {
	names := make([]string, 0, len(c.Approvals))
	for _, approval := range c.Approvals {
		names = append(names, approval.Reviewer)
	}
	return names
}

// ChangeReader is the capability of answering how a commit arrived.
//
// It is a separate interface from being a source or a status reporter, for the
// same reason those are separate from each other: a provider can land as a
// source without being able to answer this, and where a Connection cannot, the
// platform says so rather than assuming the answer.
type ChangeReader interface {
	// CommitProvenance answers how the commit reached the repository. A
	// commit that arrived through no pull request is not an error — it is an
	// answer with PullRequest zero, and it is the answer a direct push during
	// an incident produces.
	CommitProvenance(ctx context.Context, repo, sha string) (ChangeProvenance, error)
}

// Change resolves the provenance capability of a provider, reporting whether it
// has one at all.
func Change(provider Provider) (ChangeReader, bool) {
	reader, ok := provider.(ChangeReader)
	return reader, ok
}

// stateApproved is the word GitHub and Gitea both spell an approving review
// with. GitLab has no equivalent: an approval there is an entry in a list of
// approvals rather than a review carrying a state.
const stateApproved = "APPROVED"

// reviewVerdict is one review a provider left in its history, reduced to the
// three things the standing-approval rule turns on: who left it, when, and
// whether it was an approval.
//
// It exists because GitHub and Gitea keep the same kind of record in two
// vocabularies — `CHANGES_REQUESTED` against `REQUEST_CHANGES`, a dismissal
// that rewrites the state against a dismissal that sets a flag beside it — and
// the reduction over that record is identical and subtle enough that it should
// have one implementation. Each provider translates its own history into these
// and nothing else; what "still stands" means is decided here, once.
//
// GitLab has no such history to translate: its approvals are a resource that
// holds only the ones still standing, so it builds Approval values directly.
type reviewVerdict struct {
	// Reviewer is the provider's identity for whoever left it.
	Reviewer string
	// SubmittedAt is when, verbatim from the provider. Providers report it in
	// RFC 3339 with a `Z`, which orders lexicographically — so it is compared
	// as a string rather than parsed, and a provider that reports nothing
	// sorts as the oldest rather than failing.
	SubmittedAt string
	// Approved is whether this verdict was an approval that still stands. A
	// verdict that is not one is still the reviewer's newest word, and that
	// is the whole reason a dismissed review is passed in rather than
	// dropped: it is what supersedes the approval before it.
	Approved bool
}

// standingApprovals reduces every verdict a provider recorded to the approvals
// that still stand: the newest per reviewer, and only where that newest one is
// an approval.
//
// The reduction is the substance. A provider returns the full history, so a
// reviewer who approved and then requested changes appears twice, and an
// approval a later push dismissed is still in the list. Counting either would
// produce evidence that a change was approved when the approval had been
// withdrawn before it merged — which is a worse outcome than having no
// evidence, because somebody would rely on it.
func standingApprovals(verdicts []reviewVerdict, author string) []Approval {
	latest := map[string]reviewVerdict{}
	for _, verdict := range verdicts {
		if verdict.Reviewer == "" {
			continue
		}
		held, seen := latest[verdict.Reviewer]
		if !seen || verdict.SubmittedAt >= held.SubmittedAt {
			latest[verdict.Reviewer] = verdict
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
		verdict := latest[reviewer]
		if !verdict.Approved {
			continue
		}
		approvals = append(approvals, Approval{
			Reviewer:     reviewer,
			SubmittedAt:  verdict.SubmittedAt,
			SelfApproval: isAuthor(reviewer, author),
		})
	}
	return approvals
}

// isAuthor reports whether an approver is the change's own author. Providers
// are inconsistent about the case they echo a username in, and none of the
// three treats case as part of an identity.
func isAuthor(reviewer, author string) bool {
	return author != "" && strings.EqualFold(reviewer, author)
}

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
	"net/http/httptest"
	"strings"
	"testing"
)

// The edge cases are the whole feature, and on GitLab they are not GitHub's.
//
// Anyone can read a list of approvers off a merge request. What decides whether
// this is evidence or a liability is what happens to a squash merge, a
// self-approval, and an approval a later push reset — and GitLab records the
// last of those by *removing* it, which is why the approvals resource is asked
// and the note history never is.

// provenanceCommit is the sha being asked about. It is a merge commit's on
// GitLab, a squash commit's on Gitea; neither says anything about the request
// it came from, which is the point.
const provenanceCommit = "9f7e3c1d2b4a5e60"

// gitlabMergeRequestsPath is the association endpoint, project path escaped as
// GitLab wants it: one segment, slash included.
const gitlabMergeRequestsPath = "/projects/acme%2Fshop/repository/commits/" +
	provenanceCommit + "/merge_requests"

// gitlabApprovalsPath is the approvals resource for the request below.
const gitlabApprovalsPath = "/projects/acme%2Fshop/merge_requests/42/approvals"

// gitlabChange serves the two endpoints CommitProvenance is allowed to call and
// fails the test on anything else.
//
// The refusal is an assertion rather than housekeeping: a merge request's note
// history carries "approved this merge request" system notes for approvals
// GitLab has since removed, and an implementation that reached for them to
// recover a timestamp would resurrect exactly the approvals that no longer
// stand.
func gitlabChange(t *testing.T, mergeRequests, approvals string) *GitLab {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch path := req.URL.EscapedPath(); {
		case path == gitlabMergeRequestsPath:
			_, _ = w.Write([]byte(mergeRequests))
		case path == gitlabApprovalsPath && approvals != "":
			_, _ = w.Write([]byte(approvals))
		default:
			t.Errorf("asked GitLab for %s, which is not a resource this may read", path)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message": "404 Not Found"}`))
		}
	}))
	t.Cleanup(server.Close)
	return &GitLab{APIURL: server.URL, Token: "tok", HTTPClient: server.Client()}
}

func TestGitLabCommitProvenance(t *testing.T) {
	// A squash-merged request, as GitLab reports it: the commit on the default
	// branch is a new object, and `merged_at` plus `state` are what say this is
	// the request that put it there.
	const squashMerged = `[{
	  "id": 9901, "iid": 42, "project_id": 12, "title": "Add checkout",
	  "state": "merged", "target_branch": "main", "source_branch": "checkout",
	  "author": {"id": 3, "username": "alice", "name": "Alice Ito"},
	  "merged_at": "2026-08-19T10:00:00.000Z",
	  "merge_user": {"id": 4, "username": "bob", "name": "Bob Ruiz"},
	  "merged_by": {"id": 4, "username": "bob", "name": "Bob Ruiz"},
	  "squash_commit_sha": "` + provenanceCommit + `", "squash": true
	}]`

	for _, tc := range []struct {
		name          string
		mergeRequests string
		approvals     string
		request       int32
		author        string
		mergedBy      string
		approvers     []string
		independent   bool
		selfApproved  bool
	}{
		{
			// The commit names neither the request nor the approver. Asking
			// the provider is the only way to know either.
			name:          "a squash-merged commit still names its merge request and approver",
			mergeRequests: squashMerged,
			approvals: `{"id": 9901, "iid": 42, "project_id": 12, "state": "merged",
			  "approvals_required": 1, "approvals_left": 0,
			  "approved_by": [{"user": {"id": 4, "username": "bob", "name": "Bob Ruiz"}}],
			  "user_has_approved": false, "user_can_approve": false}`,
			request:     42,
			author:      "alice",
			mergedBy:    "bob",
			approvers:   []string{"bob"},
			independent: true,
		},
		{
			// Recorded rather than dropped: a change its author approved has
			// been approved. What must not happen is it reading like a second
			// pair of eyes.
			name:          "a self-approval is recorded and is not independent",
			mergeRequests: squashMerged,
			approvals: `{"iid": 42, "approvals_required": 1, "approvals_left": 0,
			  "approved_by": [{"user": {"id": 3, "username": "alice", "name": "Alice Ito"}}]}`,
			request:      42,
			author:       "alice",
			mergedBy:     "bob",
			approvers:    []string{"alice"},
			selfApproved: true,
		},
		{
			// GitLab does not mark a reset approval, it removes it — so the
			// resource that holds the standing ones is the whole answer, and
			// the fake above fails the test if anything reaches for the note
			// history that still remembers it.
			name:          "an approval a later push reset does not count",
			mergeRequests: squashMerged,
			approvals: `{"iid": 42, "approvals_required": 1, "approvals_left": 1,
			  "approved_by": [], "user_has_approved": false}`,
			request:   42,
			author:    "alice",
			mergedBy:  "bob",
			approvers: []string{},
		},
		{
			// A rule a group satisfies is a shape GitHub's reviews do not
			// have, and it changes nothing: `approved_by` names the person who
			// approved, never the group they approved for.
			name:          "a rule satisfied by a group records the human who approved",
			mergeRequests: squashMerged,
			approvals: `{"iid": 42, "approvals_required": 2, "approvals_left": 0,
			  "approved_by": [{"user": {"id": 7, "username": "carol"}},
			                  {"user": {"id": 4, "username": "bob"}}],
			  "approval_rules_left": [],
			  "has_approval_rules": true, "multiple_approval_rules_available": true}`,
			request:  42,
			author:   "alice",
			mergedBy: "bob",
			// Sorted, so that two reads of one merge request produce the same
			// evidence.
			approvers:   []string{"bob", "carol"},
			independent: true,
		},
		{
			// An instance old enough to answer with only the deprecated
			// spelling still knows who merged it.
			name: "who merged it is read from either spelling",
			mergeRequests: `[{"iid": 42, "state": "merged", "title": "Add checkout",
			  "author": {"username": "alice"}, "merged_at": "2026-08-19T10:00:00.000Z",
			  "merged_by": {"username": "dave"}}]`,
			approvals: `{"iid": 42, "approved_by": [{"user": {"username": "bob"}}]}`,
			request:   42,
			author:    "alice",
			mergedBy:  "dave",
			approvers: []string{"bob"}, independent: true,
		},
		{
			// A commit nothing can be associated with is what a direct push
			// looks like. It has to be reportable as such: refusing to
			// distinguish it from an outage would make every outage look like
			// a policy violation.
			name:          "a direct push is an answer rather than an error",
			mergeRequests: `[]`,
			approvers:     nil,
		},
		{
			// A commit can be associated with several requests — a branch cut
			// from another branch does it routinely — and only a merged one
			// put it on the default branch.
			name: "an open merge request is not taken for the one it arrived through",
			mergeRequests: `[{"iid": 51, "state": "opened", "title": "Spike",
			  "author": {"username": "erin"}, "merged_at": null}]`,
			approvers: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := gitlabChange(t, tc.mergeRequests, tc.approvals)

			provenance, err := provider.CommitProvenance(context.Background(), testRepo, provenanceCommit)
			if err != nil {
				t.Fatal(err)
			}
			if provenance.Provider != ProviderGitLab {
				t.Errorf("the claim is attributed to %q", provenance.Provider)
			}
			if provenance.PullRequest != tc.request {
				t.Errorf("merge request %d, want %d", provenance.PullRequest, tc.request)
			}
			if provenance.Author != tc.author {
				t.Errorf("author %q, want %q", provenance.Author, tc.author)
			}
			if provenance.MergedBy != tc.mergedBy {
				t.Errorf("merged by %q, want %q", provenance.MergedBy, tc.mergedBy)
			}
			if got := strings.Join(provenance.Approvers(), ","); got != strings.Join(tc.approvers, ",") {
				t.Errorf("approvers %v, want %v", provenance.Approvers(), tc.approvers)
			}
			if provenance.Independent() != tc.independent {
				t.Errorf("independent %v, want %v", provenance.Independent(), tc.independent)
			}
			if provenance.SelfApproved() != tc.selfApproved {
				t.Errorf("selfApproved %v, want %v", provenance.SelfApproved(), tc.selfApproved)
			}
		})
	}
}

func TestGitLabReportsNoApprovalTimestampRatherThanInventingOne(t *testing.T) {
	// Neither of GitLab's approval resources carries a per-approval time, and
	// the merge request's own `merged_at` is not one: recording it would be the
	// platform making up a fact and attributing it to the provider.
	provider := gitlabChange(t,
		`[{"iid": 42, "state": "merged", "author": {"username": "alice"},
		   "merged_at": "2026-08-19T10:00:00.000Z"}]`,
		`{"iid": 42, "approved_by": [{"user": {"username": "bob"}}]}`)

	provenance, err := provider.CommitProvenance(context.Background(), testRepo, provenanceCommit)
	if err != nil {
		t.Fatal(err)
	}
	if len(provenance.Approvals) != 1 {
		t.Fatalf("approvals %+v", provenance.Approvals)
	}
	if provenance.Approvals[0].SubmittedAt != "" {
		t.Errorf("a time GitLab never reported was recorded: %q", provenance.Approvals[0].SubmittedAt)
	}
}

func TestGitLabProviderThatCannotBeReachedIsAnError(t *testing.T) {
	// Distinct from "no merge request". The caller must be able to tell an
	// outage from a finding about the commit, because it refuses the build on
	// one and not on the other.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	provider := &GitLab{APIURL: server.URL, Token: "tok", HTTPClient: server.Client()}

	if _, err := provider.CommitProvenance(context.Background(), testRepo, provenanceCommit); err == nil {
		t.Fatal("a provider that answered 500 was taken for a commit with no merge request")
	}
}

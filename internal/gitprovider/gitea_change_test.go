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

// Gitea's review model is GitHub's, which makes the cases where it is not the
// only ones worth the fixtures: it spells the verdicts differently, and it
// dismisses an approval by setting a flag beside a state that still reads
// APPROVED. An implementation that read the state alone would count an approval
// somebody had explicitly taken away.

const (
	giteaPullPath    = "/repos/acme/shop/commits/" + provenanceCommit + "/pull"
	giteaReviewsPath = "/repos/acme/shop/pulls/42/reviews"
)

// giteaMergedPull is the request the commit arrived through, as Gitea reports
// it: one object rather than GitHub's list, resolved from the recorded merge
// commit so a squash merge still finds it.
const giteaMergedPull = `{
  "id": 771, "number": 42, "title": "Add checkout", "state": "closed",
  "user": {"id": 3, "login": "alice", "full_name": "Alice Ito"},
  "merged": true, "merged_at": "2026-08-19T10:00:00Z",
  "merge_commit_sha": "` + provenanceCommit + `",
  "merged_by": {"id": 4, "login": "bob", "full_name": "Bob Ruiz"},
  "base": {"ref": "main"}, "head": {"ref": "checkout"}
}`

// giteaChange serves the two endpoints CommitProvenance calls. An empty pull
// fixture is a commit Gitea associates with nothing, which it answers 404 to.
func giteaChange(t *testing.T, pull, reviews string) *Gitea {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch path := req.URL.EscapedPath(); {
		case path == giteaPullPath && pull != "":
			_, _ = w.Write([]byte(pull))
		case path == giteaReviewsPath && reviews != "":
			_, _ = w.Write([]byte(reviews))
		case path == giteaPullPath || path == giteaReviewsPath:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message": "Not Found", "url": "https://gitea.com/api/swagger"}`))
		default:
			t.Errorf("asked Gitea for %s, which is not a resource this may read", path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return &Gitea{APIURL: server.URL, Token: "tok", HTTPClient: server.Client()}
}

func TestGiteaCommitProvenance(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pull         string
		reviews      string
		request      int32
		author       string
		mergedBy     string
		approvers    []string
		independent  bool
		selfApproved bool
	}{
		{
			// The commit on the default branch names neither the request nor
			// the reviewer; Gitea's own association is what answers.
			name: "a squash-merged commit still names its pull request and approver",
			pull: giteaMergedPull,
			reviews: `[{"id": 5, "user": {"login": "bob"}, "state": "APPROVED",
			  "commit_id": "` + provenanceCommit + `", "stale": false, "official": true,
			  "dismissed": false, "submitted_at": "2026-08-19T09:00:00Z"}]`,
			request: 42, author: "alice", mergedBy: "bob",
			approvers: []string{"bob"}, independent: true,
		},
		{
			// Recorded rather than dropped, and not mistakable for a second
			// pair of eyes.
			name: "a self-approval is recorded and is not independent",
			pull: giteaMergedPull,
			reviews: `[{"id": 6, "user": {"login": "alice"}, "state": "APPROVED",
			  "stale": false, "dismissed": false, "submitted_at": "2026-08-19T09:00:00Z"}]`,
			request: 42, author: "alice", mergedBy: "bob",
			approvers: []string{"alice"}, selfApproved: true,
		},
		{
			// The case this file exists for. Gitea leaves the state saying
			// APPROVED and sets `dismissed` beside it, so reading the state
			// alone counts an approval somebody took away.
			name: "a dismissed approval does not count",
			pull: giteaMergedPull,
			reviews: `[{"id": 7, "user": {"login": "bob"}, "state": "APPROVED",
			  "stale": false, "official": true, "dismissed": true,
			  "submitted_at": "2026-08-19T09:00:00Z"}]`,
			request: 42, author: "alice", mergedBy: "bob",
			approvers: []string{},
		},
		{
			// The same fact by the other route: the review was left on a
			// commit a later push superseded, so it is not an approval of what
			// merged.
			name: "an approval a later push left stale does not count",
			pull: giteaMergedPull,
			reviews: `[{"id": 8, "user": {"login": "bob"}, "state": "APPROVED",
			  "commit_id": "0000000000000000", "stale": true, "official": true,
			  "dismissed": false, "submitted_at": "2026-08-19T08:00:00Z"}]`,
			request: 42, author: "alice", mergedBy: "bob",
			approvers: []string{},
		},
		{
			// A reviewer who approved and then asked for changes has not
			// approved; the reverse has. Gitea spells the second verdict
			// REQUEST_CHANGES where GitHub spells it CHANGES_REQUESTED.
			name: "only a reviewer's newest verdict counts",
			pull: giteaMergedPull,
			reviews: `[{"id": 9, "user": {"login": "bob"}, "state": "APPROVED",
			   "submitted_at": "2026-08-19T08:00:00Z"},
			  {"id": 10, "user": {"login": "bob"}, "state": "REQUEST_CHANGES",
			   "submitted_at": "2026-08-19T09:00:00Z"},
			  {"id": 11, "user": {"login": "carol"}, "state": "REQUEST_CHANGES",
			   "submitted_at": "2026-08-19T08:00:00Z"},
			  {"id": 12, "user": {"login": "carol"}, "state": "APPROVED",
			   "submitted_at": "2026-08-19T09:30:00Z"}]`,
			request: 42, author: "alice", mergedBy: "bob",
			approvers: []string{"carol"}, independent: true,
		},
		{
			// A COMMENT — and a review still PENDING, and a re-request —
			// leave whatever the reviewer said before standing. Treating any
			// of them as the newest word would silently drop real approvals.
			name: "a comment does not override a standing approval",
			pull: giteaMergedPull,
			reviews: `[{"id": 13, "user": {"login": "bob"}, "state": "APPROVED",
			   "submitted_at": "2026-08-19T08:00:00Z"},
			  {"id": 14, "user": {"login": "bob"}, "state": "COMMENT",
			   "submitted_at": "2026-08-19T09:00:00Z"},
			  {"id": 15, "user": {"login": "bob"}, "state": "PENDING",
			   "submitted_at": "2026-08-19T09:30:00Z"}]`,
			request: 42, author: "alice", mergedBy: "bob",
			approvers: []string{"bob"}, independent: true,
		},
		{
			// Gitea models a team as a reviewer. A team has no username to
			// record and is not a pair of eyes, so a review left by one is not
			// a verdict here — and must not be mistaken for an anonymous
			// approval.
			name: "a review left by a team rather than a person is not an approval",
			pull: giteaMergedPull,
			reviews: `[{"id": 16, "team": {"id": 2, "name": "platform"}, "state": "APPROVED",
			  "official": true, "submitted_at": "2026-08-19T09:00:00Z"}]`,
			request: 42, author: "alice", mergedBy: "bob",
			approvers: []string{},
		},
		{
			// A commit Gitea associates with nothing is what a direct push
			// looks like: an answer, not an outage.
			name:      "a direct push is an answer rather than an error",
			pull:      "",
			approvers: nil,
		},
		{
			// A request that has not merged did not put the commit anywhere;
			// the commit is still on a branch of its own.
			name: "an unmerged request is not taken for the one it arrived through",
			pull: `{"id": 780, "number": 51, "title": "Spike", "state": "open",
			  "user": {"login": "erin"}, "merged": false, "merged_at": null}`,
			approvers: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := giteaChange(t, tc.pull, tc.reviews)

			provenance, err := provider.CommitProvenance(context.Background(), testRepo, provenanceCommit)
			if err != nil {
				t.Fatal(err)
			}
			if provenance.Provider != ProviderGitea {
				t.Errorf("the claim is attributed to %q", provenance.Provider)
			}
			if provenance.PullRequest != tc.request {
				t.Errorf("pull request %d, want %d", provenance.PullRequest, tc.request)
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

func TestGiteaRecordsWhenTheApprovalWasSubmitted(t *testing.T) {
	// Gitea reports it, unlike GitLab, so it is carried verbatim into the
	// evidence rather than reformatted.
	provider := giteaChange(t, giteaMergedPull,
		`[{"id": 17, "user": {"login": "bob"}, "state": "APPROVED",
		   "submitted_at": "2026-08-19T09:00:00Z"}]`)

	provenance, err := provider.CommitProvenance(context.Background(), testRepo, provenanceCommit)
	if err != nil {
		t.Fatal(err)
	}
	if len(provenance.Approvals) != 1 || provenance.Approvals[0].SubmittedAt != "2026-08-19T09:00:00Z" {
		t.Fatalf("the approval was not recorded with its time: %+v", provenance.Approvals)
	}
}

func TestGiteaProviderThatCannotBeReachedIsAnError(t *testing.T) {
	// Distinct from "no pull request". The caller must be able to tell an
	// outage from a finding about the commit, because it refuses the build on
	// one and not on the other.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	provider := &Gitea{APIURL: server.URL, Token: "tok", HTTPClient: server.Client()}

	if _, err := provider.CommitProvenance(context.Background(), testRepo, provenanceCommit); err == nil {
		t.Fatal("a provider that answered 500 was taken for a commit with no pull request")
	}
}

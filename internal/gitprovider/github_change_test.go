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

// The edge cases are the whole feature.
//
// Anyone can read `state == "APPROVED"` off a list of reviews. What decides
// whether this is evidence or a liability is what happens to a squash merge, a
// self-approval, and an approval a later push dismissed — because each of those
// is a case where the obvious implementation answers confidently and wrongly.

// githubChange serves the two endpoints CommitProvenance calls.
func githubChange(t *testing.T, pulls, reviews string) *GitHub {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls") && strings.Contains(req.URL.Path, "/commits/"):
			_, _ = w.Write([]byte(pulls))
		case strings.HasSuffix(req.URL.Path, "/reviews"):
			_, _ = w.Write([]byte(reviews))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	}))
	t.Cleanup(server.Close)
	return &GitHub{APIURL: server.URL, Token: "t", HTTPClient: server.Client()}
}

func TestASquashMergedCommitStillNamesItsPullRequestAndApprover(t *testing.T) {
	// The commit on the default branch is a new object that names neither the
	// request nor the reviewer. Asking the provider is the only way to know,
	// and it is why this does not parse commit messages.
	provider := githubChange(t,
		`[{"number": 42, "title": "Add checkout", "state": "closed",
		   "user": {"login": "alice"}, "merged_at": "2026-08-19T10:00:00Z",
		   "merged_by": {"login": "bob"}}]`,
		`[{"state": "APPROVED", "user": {"login": "bob"}, "submitted_at": "2026-08-19T09:00:00Z"}]`)

	provenance, err := provider.CommitProvenance(context.Background(), "acme/shop", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if provenance.PullRequest != 42 || provenance.Author != "alice" {
		t.Fatalf("the request was not found: %+v", provenance)
	}
	if provenance.MergedBy != "bob" {
		t.Errorf("merged by %q", provenance.MergedBy)
	}
	if !provenance.Independent() {
		t.Error("an approval by somebody other than the author was not counted as independent")
	}
	if provenance.SelfApproved() {
		t.Error("an independently approved change was reported as self-approved")
	}
	if provenance.Provider != "github" {
		t.Errorf("the claim is not attributed: %q", provenance.Provider)
	}
}

func TestASelfApprovalIsRecordedAndIsNotIndependent(t *testing.T) {
	// Recorded rather than dropped: a change its author approved has been
	// approved, and whether that is acceptable is a policy question. What must
	// not happen is it reading like a second pair of eyes.
	provider := githubChange(t,
		`[{"number": 7, "title": "Hotfix", "user": {"login": "alice"},
		   "merged_at": "2026-08-19T10:00:00Z"}]`,
		`[{"state": "APPROVED", "user": {"login": "alice"}, "submitted_at": "2026-08-19T09:00:00Z"}]`)

	provenance, err := provider.CommitProvenance(context.Background(), "acme/shop", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if len(provenance.Approvals) != 1 || !provenance.Approvals[0].SelfApproval {
		t.Fatalf("the self-approval was not recorded as one: %+v", provenance.Approvals)
	}
	if provenance.Independent() {
		t.Error("a self-approval was counted as independent review")
	}
	if !provenance.SelfApproved() {
		t.Error("a change approved only by its author is not reported as self-approved")
	}
}

func TestAnApprovalALaterPushDismissedDoesNotCount(t *testing.T) {
	// GitHub keeps the dismissed review in the list with its state changed.
	// Counting it would produce evidence that a change was approved when the
	// approval had been withdrawn before it merged — which is worse than no
	// evidence, because somebody would rely on it.
	provider := githubChange(t,
		`[{"number": 9, "user": {"login": "alice"}, "merged_at": "2026-08-19T10:00:00Z"}]`,
		`[{"state": "DISMISSED", "user": {"login": "bob"}, "submitted_at": "2026-08-19T09:00:00Z"}]`)

	provenance, err := provider.CommitProvenance(context.Background(), "acme/shop", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if len(provenance.Approvals) != 0 {
		t.Errorf("a dismissed approval was counted: %+v", provenance.Approvals)
	}
	if provenance.Independent() {
		t.Error("a dismissed approval satisfied four-eyes")
	}
}

func TestOnlyAReviewersNewestVerdictCounts(t *testing.T) {
	// A reviewer who approved and then asked for changes has not approved.
	// The reverse — changes requested, then approved — has.
	provider := githubChange(t,
		`[{"number": 11, "user": {"login": "alice"}, "merged_at": "2026-08-19T10:00:00Z"}]`,
		`[{"state": "APPROVED", "user": {"login": "bob"}, "submitted_at": "2026-08-19T08:00:00Z"},
		  {"state": "CHANGES_REQUESTED", "user": {"login": "bob"}, "submitted_at": "2026-08-19T09:00:00Z"},
		  {"state": "CHANGES_REQUESTED", "user": {"login": "carol"}, "submitted_at": "2026-08-19T08:00:00Z"},
		  {"state": "APPROVED", "user": {"login": "carol"}, "submitted_at": "2026-08-19T09:30:00Z"}]`)

	provenance, err := provider.CommitProvenance(context.Background(), "acme/shop", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if got := provenance.Approvers(); len(got) != 1 || got[0] != "carol" {
		t.Errorf("the standing approvals are %v, want carol alone", got)
	}
}

func TestACommentDoesNotOverrideAStandingApproval(t *testing.T) {
	// On GitHub a COMMENTED review leaves the previous verdict standing.
	// Treating it as the newest word would silently drop real approvals.
	provider := githubChange(t,
		`[{"number": 13, "user": {"login": "alice"}, "merged_at": "2026-08-19T10:00:00Z"}]`,
		`[{"state": "APPROVED", "user": {"login": "bob"}, "submitted_at": "2026-08-19T08:00:00Z"},
		  {"state": "COMMENTED", "user": {"login": "bob"}, "submitted_at": "2026-08-19T09:00:00Z"}]`)

	provenance, err := provider.CommitProvenance(context.Background(), "acme/shop", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if !provenance.Independent() {
		t.Error("a later comment dropped a standing approval")
	}
}

func TestADirectPushIsAnAnswerRatherThanAnError(t *testing.T) {
	// A commit nothing can be associated with is what a direct push looks
	// like. It has to be reportable as such: refusing to distinguish it from
	// an outage would make every outage look like a policy violation.
	provider := githubChange(t, `[]`, `[]`)

	provenance, err := provider.CommitProvenance(context.Background(), "acme/shop", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if provenance.PullRequest != 0 {
		t.Errorf("a direct push was attributed to request %d", provenance.PullRequest)
	}
	if provenance.Independent() || provenance.SelfApproved() {
		t.Error("a direct push carries approvals")
	}
	if provenance.Provider != "github" {
		t.Errorf("the answer is not attributed: %q", provenance.Provider)
	}
}

func TestAnUnmergedRequestIsNotTakenForTheOneTheCommitArrivedThrough(t *testing.T) {
	// A commit can be associated with several requests — a branch cut from
	// another branch does it routinely — and only the merged one put it on the
	// default branch.
	provider := githubChange(t,
		`[{"number": 20, "user": {"login": "alice"}, "merged_at": ""},
		  {"number": 21, "user": {"login": "dave"}, "merged_at": "2026-08-19T10:00:00Z"}]`,
		`[{"state": "APPROVED", "user": {"login": "erin"}, "submitted_at": "2026-08-19T09:00:00Z"}]`)

	provenance, err := provider.CommitProvenance(context.Background(), "acme/shop", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if provenance.PullRequest != 21 || provenance.Author != "dave" {
		t.Errorf("the wrong request was chosen: %+v", provenance)
	}
}

func TestAProviderThatCannotBeReachedIsAnError(t *testing.T) {
	// Distinct from "no pull request". The caller must be able to tell an
	// outage from a finding about the commit, because it refuses the build on
	// one and not on the other.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	provider := &GitHub{APIURL: server.URL, Token: "t", HTTPClient: server.Client()}

	if _, err := provider.CommitProvenance(context.Background(), "acme/shop", "deadbeef"); err == nil {
		t.Fatal("a provider that answered 500 was taken for a commit with no pull request")
	}
}

func TestGitHubIsAChangeReader(t *testing.T) {
	if _, ok := Change(&GitHub{}); !ok {
		t.Fatal("the GitHub provider does not offer change provenance")
	}
}

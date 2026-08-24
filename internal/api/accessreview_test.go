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

package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
)

// The recertification surface: opening a cycle, the snapshot it freezes,
// deciding grant by grant, the self-review that is recorded rather than
// refused, and the register.

const reviewsPath = "/api/v1/access/reviews"

// openCycle opens a cycle through the API and answers the view.
func openCycle(t *testing.T, h *harness, body string) accessReviewView {
	t.Helper()
	recorder := h.do(t, http.MethodPost, reviewsPath, body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("opening a cycle: %d %s", recorder.Code, recorder.Body.String())
	}
	return decode[accessReviewView](t, recorder)
}

// A cycle opens with a snapshot of every grant that stood at that instant —
// the platform's operators and every project's members, one row each.
func TestOpeningACycleFreezesEveryGrantThatStood(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.grantTo(t, "sub-anna", "anna@example.com", kitchenv1alpha1.AccessRoleDeveloper)

	review := openCycle(t, h, `{"reason":"the quarterly audit"}`)
	if review.Scope != string(kitchenv1alpha1.AccessReviewAll) {
		t.Errorf("the default scope is the whole install, got %q", review.Scope)
	}
	if review.Phase != string(kitchenv1alpha1.AccessReviewOpen) {
		t.Errorf("a freshly opened cycle is Open, got %q", review.Phase)
	}
	if review.SnapshotAt == nil {
		t.Error("a cycle without a snapshot instant is a review of nothing in particular")
	}

	grants := map[string]string{}
	for _, entry := range review.Entries {
		grants[entry.Grant+"/"+entry.Subject] = entry.Role
	}
	if grants[access.PlatformGrant+"/"+testSubject] != "operator" {
		t.Errorf("the caller's operator grant must be in the snapshot: %+v", grants)
	}
	if grants[feedProject+"/sub-anna"] != string(kitchenv1alpha1.AccessRoleDeveloper) {
		t.Errorf("anna's developer grant on %s must be in the snapshot: %+v", feedProject, grants)
	}
	if review.Pending != int32(len(review.Entries)) {
		t.Errorf("every grant starts undecided, got pending=%d of %d", review.Pending, len(review.Entries))
	}
}

// One cycle at a time over the same grants: two open cycles would be two
// reviewers deciding the same question, and a close that applied one set of
// revocations while the other still showed the grant.
func TestASecondOverlappingCycleIsRefused(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	openCycle(t, h, `{}`)

	recorder := h.do(t, http.MethodPost, reviewsPath, `{}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409 for a second overlapping cycle, got %d: %s",
			recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "already open") {
		t.Errorf("the refusal must name the cycle that is in the way: %s", recorder.Body.String())
	}
}

// A project-scoped cycle covers that project's grants and no others: a review
// of shop must not ask its reviewer about the platform's operators.
func TestAProjectScopedCycleCoversThatProjectAlone(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), blogFixtures()...)...)
	h.grantTo(t, "sub-anna", "anna@example.com", kitchenv1alpha1.AccessRoleDeveloper)

	review := openCycle(t, h, fmt.Sprintf(`{"scope":"project","project":%q}`, feedProject))
	if len(review.Entries) == 0 {
		t.Fatal("a project cycle over a project with grants must carry them")
	}
	for _, entry := range review.Entries {
		if entry.Grant != feedProject {
			t.Fatalf("a %s cycle carried a grant on %q: %+v", feedProject, entry.Grant, entry)
		}
	}
}

// The bad shapes, each answered with the sentence that says what to do
// instead rather than with a schema error.
func TestOpeningACycleRejectsUnusableRequests(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for _, testCase := range []struct{ name, body, wants string }{
		{"an unknown scope", `{"scope":"everything"}`, "not one of platform, project or all"},
		{"a project cycle naming none", `{"scope":"project"}`, "project is required"},
		{"a project that does not exist", `{"scope":"project","project":"nope"}`, "does not exist"},
		{"a project on an all-scoped cycle", `{"scope":"all","project":"shop"}`,
			"belongs to a project-scoped review only"},
		{"a deadline in the past",
			fmt.Sprintf(`{"dueBy":%q}`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)),
			"is not in the future"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, reviewsPath, testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), testCase.wants) {
				t.Fatalf("want a refusal mentioning %q, got %s", testCase.wants, recorder.Body.String())
			}
		})
	}
}

// Deciding is per grant, and the tallies are derived from the entries rather
// than incremented — a tally that disagreed with the list it summarizes would
// be the one number a reader trusts and should not.
func TestDecidingGrantByGrantMovesTheTallies(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.grantTo(t, "sub-anna", "anna@example.com", kitchenv1alpha1.AccessRoleDeveloper)
	review := openCycle(t, h, `{}`)

	body := fmt.Sprintf(`{"decisions":[{"subject":"sub-anna","grant":%q,"decision":"revoke",`+
		`"note":"left the team in June"}]}`, feedProject)
	recorder := h.do(t, http.MethodPatch, reviewsPath+"/"+review.Name, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	decided := decode[accessReviewView](t, recorder)
	if decided.Revoked != 1 {
		t.Errorf("one revocation was recorded, the tally says %d", decided.Revoked)
	}
	if decided.Pending != int32(len(decided.Entries))-1 {
		t.Errorf("one grant was decided, pending says %d of %d",
			decided.Pending, len(decided.Entries))
	}
	for _, entry := range decided.Entries {
		if entry.Subject != "sub-anna" {
			continue
		}
		if entry.Decision != string(kitchenv1alpha1.AccessRevoke) || entry.DecidedBy != testCaller {
			t.Fatalf("the decision must name what was decided and by whom: %+v", entry)
		}
		if entry.Note == "" {
			t.Error("the reviewer's words are part of the record")
		}
	}
}

// A decision about a grant made since the cycle opened is refused, with the
// sentence that explains why: a review decides about what was true when it
// opened, and a later grant belongs to the next cycle.
func TestADecisionAboutAGrantOutsideTheSnapshotIsRefused(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	review := openCycle(t, h, `{}`)

	body := fmt.Sprintf(`{"decisions":[{"subject":"joined-later","grant":%q,"decision":"confirm"}]}`,
		feedProject)
	recorder := h.do(t, http.MethodPatch, reviewsPath+"/"+review.Name, body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "next cycle") {
		t.Errorf("the refusal must say where that grant does belong: %s", recorder.Body.String())
	}
}

// Segregation of duties, answered the way §8.4 answers self-approval: the
// reviewer may be the reviewed, and it is recorded rather than refused. An
// installation with one operator has exactly one person who can review that
// operator's grant.
func TestAReviewerDecidingTheirOwnGrantIsRecordedAsASelfReview(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	review := openCycle(t, h, `{}`)

	body := fmt.Sprintf(`{"decisions":[{"subject":%q,"grant":%q,"decision":"confirm"}]}`,
		testSubject, access.PlatformGrant)
	recorder := h.do(t, http.MethodPatch, reviewsPath+"/"+review.Name, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("a self-review is recorded, never refused: %d %s", recorder.Code, recorder.Body.String())
	}
	decided := decode[accessReviewView](t, recorder)
	if decided.SelfReviewed != 1 {
		t.Fatalf("the self-review must be counted, got %d", decided.SelfReviewed)
	}
	for _, entry := range decided.Entries {
		if entry.Subject == testSubject && entry.Grant == access.PlatformGrant && !entry.SelfReview {
			t.Fatalf("the entry must be marked a self-review: %+v", entry)
		}
	}
}

// Closing is what the whole cycle is for, and it is one request with the last
// decisions in it: a close that raced them would produce an artefact missing
// the decision it was closed on.
func TestClosingACycleInTheSameRequestAsItsLastDecision(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	review := openCycle(t, h, `{}`)

	body := fmt.Sprintf(`{"decisions":[{"subject":%q,"grant":%q,"decision":"confirm"}],"close":true}`,
		testSubject, access.PlatformGrant)
	recorder := h.do(t, http.MethodPatch, reviewsPath+"/"+review.Name, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	closed := decode[accessReviewView](t, recorder)
	if closed.Phase != string(kitchenv1alpha1.AccessReviewClosed) {
		t.Fatalf("want the cycle closed, got %q", closed.Phase)
	}
	if closed.ClosedBy != testCaller || closed.ClosedAt == nil {
		t.Errorf("a close must name who and when: %+v", closed)
	}
	if closed.Confirmed != 1 {
		t.Errorf("the decision in the closing request must count, got %d", closed.Confirmed)
	}

	// And a closed cycle stays closed: its artefact is minted and its
	// decisions stand.
	again := h.do(t, http.MethodPatch, reviewsPath+"/"+review.Name, body)
	if again.Code != http.StatusConflict {
		t.Fatalf("want 409 on a closed cycle, got %d: %s", again.Code, again.Body.String())
	}
}

// The register is open cycles by default and the history with ?historical,
// because retaining a closed cycle is the point of retaining it.
func TestTheRegisterShowsOpenCyclesAndTheHistoryOnRequest(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	review := openCycle(t, h, `{}`)
	h.do(t, http.MethodPatch, reviewsPath+"/"+review.Name, `{"close":true}`)

	open := decode[listBody[accessReviewView]](t, h.do(t, http.MethodGet, reviewsPath, ""))
	if len(open.Items) != 0 {
		t.Fatalf("a closed cycle is not open, got %d", len(open.Items))
	}
	historical := decode[listBody[accessReviewView]](t,
		h.do(t, http.MethodGet, reviewsPath+"?historical=true", ""))
	if len(historical.Items) != 1 {
		t.Fatalf("the register keeps its history, got %d", len(historical.Items))
	}
}

// The decision record is privileged, classified `access`, and names every
// decision rather than a count of them — "who confirmed grace's operator role
// in the March review" is the question it exists to answer, and a tally
// cannot answer it.
func TestTheDecisionTransitionNamesEveryDecisionAndTheSelfReviews(t *testing.T) {
	review := &kitchenv1alpha1.AccessReview{}
	review.Name = "access-review-x"
	review.Spec.Scope = kitchenv1alpha1.AccessReviewAll
	review.Status.Confirmed = 2

	transition := accessDecisionTransition(review, reviewAccessRequest{
		Decisions: []accessDecisionRequest{
			{Subject: "sub-anna", Grant: feedProject, Decision: "confirm"},
			{Subject: testSubject, Grant: access.PlatformGrant, Decision: "confirm"},
		},
		Close: true,
	}, testCaller, []string{access.PlatformGrant + "/" + testSubject})

	if transition.Privileged != audit.PrivilegeAccess {
		t.Errorf("a recertification decision is a privileged access record, got %q", transition.Privileged)
	}
	decisions, ok := transition.Details["decisions"].([]map[string]any)
	if !ok || len(decisions) != 2 {
		t.Fatalf("every decision must be in the record, got %+v", transition.Details["decisions"])
	}
	if transition.Details["selfReviewed"] == nil {
		t.Error("a self-review is answered for on the record, not hidden")
	}
	if transition.To != string(kitchenv1alpha1.AccessReviewClosed) {
		t.Errorf("a closing request moves the cycle to Closed, got %q", transition.To)
	}
	if !strings.Contains(transition.Reason, "closed by "+testCaller) {
		t.Errorf("the one line a person reads must say who closed it: %q", transition.Reason)
	}
}

// The identity survey is the live answer, and it is the same materializer a
// cycle freezes — so the screen and the snapshot cannot disagree about what
// the platform's access is.
func TestTheIdentitySurveyAnswersWhoHoldsWhat(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.grantTo(t, "sub-anna", "anna@example.com", kitchenv1alpha1.AccessRoleDeveloper)
	h.withDirectory()
	h.logs.actorActivity = map[string]time.Time{
		testCaller: time.Now().Add(-time.Hour),
	}

	body := decode[identitiesView](t, h.do(t, http.MethodGet, "/api/v1/access/identities", ""))
	if len(body.Identities) != 2 {
		t.Fatalf("want the operator grant and anna's, got %d: %+v", len(body.Identities), body.Identities)
	}
	if !body.DirectoryConsulted {
		t.Error("the directory answered, and the survey must say so")
	}
	if body.Identities[0].Grant != access.PlatformGrant {
		t.Errorf("the platform's own grant leads: %+v", body.Identities[0])
	}
	if body.Identities[0].Inactive {
		t.Errorf("an hour ago is not dormant: %+v", body.Identities[0])
	}
}

// A member may not read any of it: the answer is the whole installation's
// access in one document.
func TestTheAccessSurfaceIsTheOperatorsAlone(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/access/identities"},
		{http.MethodGet, reviewsPath},
		{http.MethodPost, reviewsPath},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			recorder := h.do(t, route.method, route.path, `{}`)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("want 403 for a member, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

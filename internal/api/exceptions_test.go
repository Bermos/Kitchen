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
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
)

// The break-glass surface: what these tests pin is the two-person rule, the
// escalation ladder, the register's scoping, and resolution as an auditable
// act — the pieces a 3am emergency must be able to lean on and a lazy
// afternoon must not be able to abuse.

const (
	approverSubject = "audrey@example.com"
	operatorEmail   = "cto@example.com"
)

// exceptionBody is a well-formed creation request, hours from now, that a
// test then bends.
func exceptionBody(expiresIn time.Duration, approvedBy string) string {
	return fmt.Sprintf(`{"environment":%q,"ruleIDs":["max-severity"],"reason":"hotfix for INC-421",`+
		`"approvedBy":%q,"incidentRef":"INC-421","expiresAt":%q}`,
		testEnvironment, approvedBy, time.Now().Add(expiresIn).UTC().Format(time.RFC3339))
}

// storedException is an Exception fixture the register reads.
func storedException(name, project, environment string, expiresIn time.Duration,
	phase kitchenv1alpha1.ExceptionPhase) *kitchenv1alpha1.Exception {
	return &kitchenv1alpha1.Exception{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: kitchenv1alpha1.ExceptionSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: project},
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: environment},
			RuleIDs:        []string{"max-severity"},
			Reason:         "accepted finding",
			RequestedBy:    "grace@example.com",
			ApprovedBy:     approverSubject,
			ExpiresAt:      metav1.NewTime(time.Now().Add(expiresIn).UTC()),
		},
		Status: kitchenv1alpha1.ExceptionStatus{Phase: phase},
	}
}

func TestAnOnCallDeveloperGetsAShortExceptionWithAPeersApproval(t *testing.T) {
	// The caller holds developer — the on-call engineer — and the approver is
	// another developer: the default ladder's 24h rung asks for no more.
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	h.grantTo(t, approverSubject, approverSubject, kitchenv1alpha1.AccessRoleDeveloper)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions",
		exceptionBody(2*time.Hour, approverSubject))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[exceptionView](t, recorder)
	if view.Phase != string(kitchenv1alpha1.ExceptionActive) {
		t.Fatalf("a fresh grant is Active, got %q", view.Phase)
	}
	if view.RequestedBy != testCaller || view.ApprovedBy != approverSubject {
		t.Fatalf("both names go on the record: %+v", view)
	}
	if len(view.RuleIDs) != 1 || view.RuleIDs[0] != "max-severity" {
		t.Fatalf("the waiver is per-rule: %+v", view.RuleIDs)
	}

	stored := &kitchenv1alpha1.Exception{}
	if err := h.server.Client.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: view.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Annotations[requestedByAnnotation]; got != testCaller {
		t.Fatalf("the object carries who asked, got %q", got)
	}
}

func TestTheLadderEscalatesWithTheRequestedDuration(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	h.grantTo(t, approverSubject, approverSubject, kitchenv1alpha1.AccessRoleDeveloper)

	// Thirty days is past the 24h rung: a developer approver is not enough.
	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions",
		exceptionBody(719*time.Hour, approverSubject))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "admin") {
		t.Fatalf("the refusal names the rung it takes, got %q", got)
	}

	// The same ask with an admin approver is granted.
	h.grantTo(t, approverSubject, approverSubject, kitchenv1alpha1.AccessRoleAdmin)
	recorder = h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions",
		exceptionBody(719*time.Hour, approverSubject))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("an admin approver covers 30 days, got %d: %s", recorder.Code, recorder.Body.String())
	}

	// Beyond every rung — ninety days — only a platform operator will do.
	recorder = h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions",
		exceptionBody(90*24*time.Hour, approverSubject))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400 past every rung, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "operator") {
		t.Fatalf("the refusal names the operator rung, got %q", got)
	}
	h.updateKitchen(t, func(kitchen *kitchenv1alpha1.Kitchen) {
		kitchen.Spec.Access.Operators = append(kitchen.Spec.Access.Operators,
			kitchenv1alpha1.AccessSubject{Subject: operatorEmail})
	})
	recorder = h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions",
		exceptionBody(90*24*time.Hour, operatorEmail))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("an operator approver covers everything, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestTheLadderIsConfigurable(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	h.grantTo(t, approverSubject, approverSubject, kitchenv1alpha1.AccessRoleDeveloper)
	h.updateKitchen(t, func(kitchen *kitchenv1alpha1.Kitchen) {
		kitchen.Spec.Compliance.Exceptions = &kitchenv1alpha1.ExceptionPolicySpec{
			Ladder: []kitchenv1alpha1.ExceptionTier{{
				MaxDuration: metav1.Duration{Duration: time.Hour},
				Role:        kitchenv1alpha1.ExceptionApproverDeveloper,
			}},
		}
	})

	// Under the configured rung: a developer approver suffices.
	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions",
		exceptionBody(30*time.Minute, approverSubject))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201 under the configured rung, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// Two hours is beyond the only rung, and beyond every rung means operator
	// — the 24h developer default must not resurface under a configured
	// ladder.
	recorder = h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions",
		exceptionBody(2*time.Hour, approverSubject))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400 beyond the configured ladder, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAnExceptionMayCoverTheProductionTargetBeforeItExists(t *testing.T) {
	// Pre-first-build there is no production environment yet, and the
	// build-time break-glass is scoped to exactly its name — so the grant
	// must be possible before the build that needs it, or the refusal it
	// exists to waive could never be waived.
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	h.grantTo(t, approverSubject, approverSubject, kitchenv1alpha1.AccessRoleDeveloper)
	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvironment, Namespace: testNamespace},
	}
	if err := h.server.Client.Delete(context.Background(), env); err != nil {
		t.Fatal(err)
	}

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions",
		exceptionBody(2*time.Hour, approverSubject))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[exceptionView](t, recorder)
	if view.Environment != testEnvironment {
		t.Fatalf("the grant is scoped to the production target, got %q", view.Environment)
	}
}

func TestAStagedProjectsGrantableTargetIsItsLastStage(t *testing.T) {
	// Under a staged pipeline the production target is the LAST stage's
	// environment — the name the build-time break-glass reads — and it may
	// be granted against before its first build creates it.
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	h.grantTo(t, approverSubject, approverSubject, kitchenv1alpha1.AccessRoleDeveloper)
	project := &kitchenv1alpha1.Project{}
	if err := h.server.Client.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: feedProject}, project); err != nil {
		t.Fatal(err)
	}
	project.Spec.Promotion = &kitchenv1alpha1.PromotionPolicySpec{Stages: []kitchenv1alpha1.PromotionStage{
		{Name: "staging", Environment: "shop-staging"},
		{Name: "live", Environment: "shop-live"},
	}}
	if err := h.server.Client.Update(context.Background(), project); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"environment":"shop-live","ruleIDs":["require-pull-request"],`+
		`"reason":"hotfix for INC-421","approvedBy":%q,"expiresAt":%q}`,
		approverSubject, time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339))
	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[exceptionView](t, recorder); view.Environment != "shop-live" {
		t.Fatalf("the grant is scoped to the last stage, got %q", view.Environment)
	}
}

func TestAnUnknownEnvironmentThatIsNotTheTargetIsRefusedWithBothOptions(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	h.grantTo(t, approverSubject, approverSubject, kitchenv1alpha1.AccessRoleDeveloper)

	body := fmt.Sprintf(`{"environment":"shop-qa","ruleIDs":["max-severity"],`+
		`"reason":"hotfix","approvedBy":%q,"expiresAt":%q}`,
		approverSubject, time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339))
	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions", body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	got := errorOf(t, recorder.Body.String())
	if !strings.Contains(got, "does not exist") || !strings.Contains(got, "production target") ||
		!strings.Contains(got, testEnvironment) {
		t.Fatalf("the refusal explains both options — an existing environment, or the production target %q — got %q",
			testEnvironment, got)
	}
}

func TestAnExceptionTakesTwoPeople(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)

	// The approver is the caller, by address: refused before any role check.
	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions",
		exceptionBody(2*time.Hour, testCaller))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "two people") {
		t.Fatalf("the refusal states the rule, got %q", got)
	}

	// And by subject: the caller's `sub` matches the same way an access
	// entry's would.
	recorder = h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions",
		exceptionBody(2*time.Hour, testSubject))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("naming your own sub is the same refusal, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAnExceptionNeedsItsWords(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	h.grantTo(t, approverSubject, approverSubject, kitchenv1alpha1.AccessRoleDeveloper)

	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for name, body := range map[string]string{
		"no rules": `{"environment":"` + testEnvironment + `","ruleIDs":[],"reason":"x",` +
			`"approvedBy":"` + approverSubject + `","expiresAt":"` + expires + `"}`,
		"no reason": `{"environment":"` + testEnvironment + `","ruleIDs":["max-severity"],"reason":" ",` +
			`"approvedBy":"` + approverSubject + `","expiresAt":"` + expires + `"}`,
		"no expiry": `{"environment":"` + testEnvironment + `","ruleIDs":["max-severity"],"reason":"x",` +
			`"approvedBy":"` + approverSubject + `"}`,
		"born expired": `{"environment":"` + testEnvironment + `","ruleIDs":["max-severity"],"reason":"x",` +
			`"approvedBy":"` + approverSubject + `","expiresAt":"2020-01-01T00:00:00Z"}`,
	} {
		recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d: %s", name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestTheRegisterIsScopedAndKeepsItsHistory(t *testing.T) {
	extra := []runtime.Object{
		storedException("shop-exc-active", feedProject, testEnvironment, time.Hour,
			kitchenv1alpha1.ExceptionActive),
		storedException("shop-exc-expired", feedProject, testEnvironment, -time.Hour,
			kitchenv1alpha1.ExceptionExpired),
		storedException("blog-exc-active", otherProject, "blog-production", time.Hour,
			kitchenv1alpha1.ExceptionActive),
	}
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, append(blogFixtures(), extra...)...)

	// Active by default, and only the caller's projects.
	recorder := h.do(t, http.MethodGet, "/api/v1/exceptions", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	answer := decode[listBody[exceptionView]](t, recorder)
	if len(answer.Items) != 1 || answer.Items[0].Name != "shop-exc-active" {
		t.Fatalf("active, visible grants only: %+v", answer.Items)
	}

	// The history is a toggle away, because retaining it is the point.
	recorder = h.do(t, http.MethodGet, "/api/v1/exceptions?historical=true", "")
	answer = decode[listBody[exceptionView]](t, recorder)
	if len(answer.Items) != 2 {
		t.Fatalf("historical adds the expired, got %+v", answer.Items)
	}
	for _, item := range answer.Items {
		if item.Project == otherProject {
			t.Fatalf("another project's grants must not appear: %+v", item)
		}
	}

	// One that is past its expiry but never reconciled still reads Expired:
	// the phase is judged against the clock, not against the status row.
	stale := storedException("shop-exc-stale", feedProject, testEnvironment, -time.Minute, "")
	if err := h.server.Client.Create(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	recorder = h.do(t, http.MethodGet, "/api/v1/exceptions", "")
	answer = decode[listBody[exceptionView]](t, recorder)
	for _, item := range answer.Items {
		if item.Name == "shop-exc-stale" {
			t.Fatalf("a lapsed grant must not list as active: %+v", item)
		}
	}
}

func TestResolutionIsAnAdminsActWithAReason(t *testing.T) {
	active := storedException("shop-exc-1", feedProject, testEnvironment, time.Hour,
		kitchenv1alpha1.ExceptionActive)
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, active)

	// A developer requested it; a developer does not end it.
	recorder := h.do(t, http.MethodPatch, "/api/v1/exceptions/shop-exc-1",
		`{"resolved":true,"reason":"patched in 1.4.2"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("resolving needs admin, got %d: %s", recorder.Code, recorder.Body.String())
	}

	h.grant(t, feedProject, kitchenv1alpha1.AccessRoleAdmin)
	recorder = h.do(t, http.MethodPatch, "/api/v1/exceptions/shop-exc-1", `{"resolved":true,"reason":" "}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a resolution without a reason is refused, got %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = h.do(t, http.MethodPatch, "/api/v1/exceptions/shop-exc-1",
		`{"resolved":true,"reason":"patched in 1.4.2"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[exceptionView](t, recorder)
	if view.Phase != string(kitchenv1alpha1.ExceptionResolved) || view.ResolvedBy != testCaller {
		t.Fatalf("the resolution goes on the record: %+v", view)
	}

	// Resolved is terminal: a second resolution is a conflict, not a repeat.
	recorder = h.do(t, http.MethodPatch, "/api/v1/exceptions/shop-exc-1",
		`{"resolved":true,"reason":"again"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAnExceptionTheLogCannotRecordIsNotGranted(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.grantTo(t, approverSubject, approverSubject, kitchenv1alpha1.AccessRoleDeveloper)
	h.withUnreachableAuditLog(t)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/exceptions",
		exceptionBody(2*time.Hour, approverSubject))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
	exceptions := &kitchenv1alpha1.ExceptionList{}
	if err := h.server.Client.List(context.Background(), exceptions); err != nil {
		t.Fatal(err)
	}
	if len(exceptions.Items) != 0 {
		t.Fatalf("an unrecorded grant must not be made, got %d", len(exceptions.Items))
	}
}

// The audit record of the granting, held up to the light without a store.
func TestTheExceptionTransitionCarriesTheGrantWhole(t *testing.T) {
	exception := storedException("shop-exc-x", feedProject, testEnvironment, time.Hour, "")
	exception.Spec.IncidentRef = "INC-421"
	transition := exceptionTransition(exception, kitchenv1alpha1.ExceptionApproverDeveloper)
	if transition.Kind != "Exception" || transition.Project != feedProject {
		t.Fatalf("unexpected transition: %+v", transition)
	}
	if transition.Privileged != audit.PrivilegeBreakGlass {
		t.Fatalf("granting a waiver is a privileged break-glass record, got %q", transition.Privileged)
	}
	for _, key := range []string{"ruleIDs", "reason", "requestedBy", "approvedBy", "expiresAt",
		"requiredRole", "incidentRef", "environment"} {
		if _, ok := transition.Details[key]; !ok {
			t.Fatalf("the record must carry %q: %+v", key, transition.Details)
		}
	}
}

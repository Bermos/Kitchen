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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
)

// Asking to bind an offering, and being answered (#495).

// requestOffering is the default visibility spelled out: an offering nobody
// binds until the project that makes it says so.
func requestOffering() kitchenv1alpha1.ServiceOffering {
	return kitchenv1alpha1.ServiceOffering{
		Name:      openOfferingName,
		Process:   "api",
		VisibleTo: kitchenv1alpha1.OfferingRequest,
	}
}

// requestedClaim is the name of the consumer's claim in these tests, which is
// also what a decision addresses.
const requestedClaim = "prices"

// bindingClaim is the consumer's claim on the provider's offering, with the
// grant the reconciler would have written on it.
func bindingClaim(grant *kitchenv1alpha1.ClaimServiceGrant) runtime.Object {
	claim := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        requestedClaim,
			Namespace:   testNamespace,
			Annotations: map[string]string{requestedByAnnotation: "ada@example.com"},
		},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: feedProject},
			Type:       kitchenv1alpha1.ClaimTypeService,
			Config: &runtime.RawExtension{Raw: []byte(
				`{"service": {"project": "` + providerProject + `", "offering": "` + openOfferingName + `"}}`)},
		},
	}
	if grant != nil {
		claim.Status.Phase = kitchenv1alpha1.ClaimPendingApproval
		claim.Status.Service = &kitchenv1alpha1.ClaimServiceStatus{Grant: grant}
	}
	return claim
}

func pendingGrant() *kitchenv1alpha1.ClaimServiceGrant {
	return &kitchenv1alpha1.ClaimServiceGrant{
		State:       kitchenv1alpha1.ServiceGrantRequested,
		RequestedBy: "ada@example.com",
		RequestedAt: metav1.Now(),
	}
}

// The claim is the request, so writing one on an offering that admits by
// request is not a refusal any more: it is created, and it waits.
func TestABindingOnARequestOfferingIsWrittenAndWaits(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), pricingProject(requestOffering()))...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "prices", "project": "shop", "type": "service",
			"service": {"project": "pricing", "offering": "pricing-api"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), requestedClaim, claim); err != nil {
		t.Fatal(err)
	}
	if got := claim.Service(); got.Project != providerProject || got.Offering != openOfferingName {
		t.Fatalf("the claim names the offering it asks for: %+v", got)
	}
	// The API records who asked; the reconciler reads it back as the
	// request's author, since it knows nobody by name itself.
	if claim.Annotations[requestedByAnnotation] != testCaller {
		t.Errorf("the claim carries who asked, got %q", claim.Annotations[requestedByAnnotation])
	}
}

func TestTheProviderReadsWhatHasBeenAskedOfIt(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(),
		pricingProject(requestOffering()), bindingClaim(pendingGrant()))...)

	body := decode[listBody[bindingRequestView]](t,
		h.do(t, http.MethodGet, "/api/v1/projects/"+providerProject+"/requests", ""))
	if len(body.Items) != 1 {
		t.Fatalf("one request was made of this project: %+v", body.Items)
	}
	request := body.Items[0]
	if request.Claim != requestedClaim || request.Project != feedProject || request.Offering != openOfferingName {
		t.Errorf("the row says who asked for what: %+v", request)
	}
	if request.State != string(kitchenv1alpha1.ServiceGrantRequested) {
		t.Errorf("a request nobody has answered is %q, got %q", kitchenv1alpha1.ServiceGrantRequested,
			request.State)
	}
	if request.RequestedBy != "ada@example.com" || request.RequestedAt == nil {
		t.Errorf("the row says who asked and when: %+v", request)
	}
	if request.Visibility != string(kitchenv1alpha1.OfferingRequest) {
		t.Errorf("the row says why it is waiting: %+v", request)
	}

	// `?state=` is what the pane asks with, and an unknown one is a refusal
	// rather than an empty list.
	empty := decode[listBody[bindingRequestView]](t,
		h.do(t, http.MethodGet, "/api/v1/projects/"+providerProject+"/requests?state=approved", ""))
	if len(empty.Items) != 0 {
		t.Errorf("nothing here is approved: %+v", empty.Items)
	}
	if recorder := h.do(t, http.MethodGet,
		"/api/v1/projects/"+providerProject+"/requests?state=maybe", ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestApprovingABindingRecordsWhoDecidedIt(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(),
		pricingProject(requestOffering()), bindingClaim(pendingGrant()))...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+providerProject+"/requests/"+requestedClaim,
		`{"decision": "approved", "reason": "they are on the same team"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[bindingRequestView](t, recorder)
	if view.State != string(kitchenv1alpha1.ServiceGrantApproved) || view.DecidedBy != testCaller {
		t.Errorf("the answer says what was decided and by whom: %+v", view)
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), requestedClaim, claim); err != nil {
		t.Fatal(err)
	}
	grant := claim.ServiceGrant()
	if grant == nil || !grant.Admitted() || grant.DecidedAt == nil {
		t.Fatalf("the grant is written onto the claim: %+v", grant)
	}
	if grant.DecidedBy != testCaller || grant.Reason != "they are on the same team" {
		t.Errorf("the decision carries who and why: %+v", grant)
	}
	// The API writes the grant and nothing else: binding is the
	// reconciler's, through the one path an open offering binds by.
	if claim.Status.SecretName != "" || len(claim.Status.Service.Bindings) != 0 {
		t.Errorf("approving provisions nothing by itself: %+v", claim.Status)
	}
}

// The audit record of a decision, held up to the light without a store.
func TestTheBindingDecisionTransitionCarriesWhoAdmittedWhom(t *testing.T) {
	claim := bindingClaim(pendingGrant()).(*kitchenv1alpha1.ResourceClaim)
	provider := pricingProject(requestOffering())
	transition := bindingDecisionTransition(claim, provider, requestOffering(),
		string(kitchenv1alpha1.ServiceGrantRequested), kitchenv1alpha1.ServiceGrantApproved, "same team")

	// The *providing* project's record: it is that project's grant, and its
	// admins made it.
	if transition.Project != providerProject {
		t.Fatalf("the decision is the providing project's record, got %q", transition.Project)
	}
	if transition.Kind != audit.KindResourceClaim {
		t.Errorf("a decision is a transition of the claim, got %q", transition.Kind)
	}
	// A grant is who may do what, which is the class every other grant on
	// this platform is recorded under.
	if transition.Privileged != audit.PrivilegeAccess {
		t.Errorf("admitting a consumer is a privileged access record, got %q", transition.Privileged)
	}
	if transition.To != string(kitchenv1alpha1.ServiceGrantApproved) {
		t.Errorf("the record says what was decided, got %q", transition.To)
	}
	for _, key := range []string{"consumer", "claim", "offering", "decision", "reason"} {
		if _, ok := transition.Details[key]; !ok {
			t.Fatalf("the record must carry %q: %+v", key, transition.Details)
		}
	}
	if !strings.Contains(transition.Reason, feedProject) ||
		!strings.Contains(transition.Reason, openOfferingName) {
		t.Errorf("the record says who was admitted to what: %q", transition.Reason)
	}
}

func TestRefusingABindingSaysWhy(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(),
		pricingProject(requestOffering()), bindingClaim(pendingGrant()))...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+providerProject+"/requests/"+requestedClaim,
		`{"decision": "denied"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "reason is required") {
		t.Errorf("a refusal without words is refused: %s", recorder.Body.String())
	}

	recorder = h.do(t, http.MethodPatch, "/api/v1/projects/"+providerProject+"/requests/"+requestedClaim,
		`{"decision": "denied", "reason": "this offering is being retired"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), requestedClaim, claim); err != nil {
		t.Fatal(err)
	}
	grant := claim.ServiceGrant()
	if grant.State != kitchenv1alpha1.ServiceGrantDenied || grant.Reason != "this offering is being retired" {
		t.Fatalf("the refusal carries the provider's words: %+v", grant)
	}
	// The consumer reads them on its own claim, which is the only place it
	// can: it holds no role on the project that refused it.
	view := decode[claimView](t, h.do(t, http.MethodGet, "/api/v1/claims/"+requestedClaim, ""))
	if view.Service == nil || view.Service.Grant == nil {
		t.Fatal("the claim answers with the grant")
	}
	if view.Service.Grant.Reason != "this offering is being retired" {
		t.Errorf("the consumer reads the reason it was refused: %+v", view.Service.Grant)
	}

	// Refusing it twice is a conflict rather than a second refusal.
	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+providerProject+"/requests/"+requestedClaim,
		`{"decision": "denied", "reason": "still no"}`); recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAskingAgainAfterARefusal(t *testing.T) {
	denied := &kitchenv1alpha1.ClaimServiceGrant{
		State:     kitchenv1alpha1.ServiceGrantDenied,
		DecidedBy: testCaller,
		Reason:    "not yet",
	}
	h := newHarness(t, nil, append(fixtures(),
		pricingProject(requestOffering()), bindingClaim(denied))...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims/"+requestedClaim+"/request", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), requestedClaim, claim); err != nil {
		t.Fatal(err)
	}
	grant := claim.ServiceGrant()
	if grant.State != kitchenv1alpha1.ServiceGrantRequested || grant.RequestedBy != testCaller {
		t.Fatalf("asking again puts it back in front of the provider: %+v", grant)
	}
	if grant.DecidedBy != "" || grant.Reason != "" {
		t.Errorf("the refusal it replaces is not still standing: %+v", grant)
	}
	// The claim was not deleted and written afresh, which is the whole
	// point of the route.
	if claim.Name != requestedClaim {
		t.Errorf("the same claim carries the new request, got %q", claim.Name)
	}

	// Asking again for one that is already waiting is a conflict.
	if recorder := h.do(t, http.MethodPost, "/api/v1/claims/"+requestedClaim+"/request",
		""); recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAnOpenOfferingHasNothingToDecide(t *testing.T) {
	admitted := &kitchenv1alpha1.ClaimServiceGrant{
		State: kitchenv1alpha1.ServiceGrantApproved,
		Open:  true,
	}
	h := newHarness(t, nil, append(fixtures(),
		pricingProject(openOffering()), bindingClaim(admitted))...)

	// Nothing to refuse: the offering admits every project on the platform
	// by its own terms, and the way to withdraw one consumer is to close it
	// first.
	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+providerProject+"/requests/"+requestedClaim,
		`{"decision": "denied", "reason": "we would rather they did not"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "open to every project") {
		t.Errorf("the refusal says what to do instead: %s", recorder.Body.String())
	}
	// And nothing to ask for either.
	if recorder := h.do(t, http.MethodPost, "/api/v1/claims/"+requestedClaim+"/request",
		""); recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestADecisionIsRefusedOnSomebodyElsesRequest(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(),
		pricingProject(requestOffering()), bindingClaim(pendingGrant()))...)

	// The consuming project's own database claim, which is not a request of
	// anybody's offerings.

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+providerProject+"/requests/shop-db",
		`{"decision": "approved"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no binding request named") {
		t.Errorf("the refusal says what was not found: %s", recorder.Body.String())
	}
}

// The grant is the same grant an offering's visibility is, one consumer at a
// time, so it is the providing project's admins' — not its developers', not
// the consumer's admins' however much they would like it, and the *queue* is
// theirs too, because a row carries the name of the person who asked.
func TestDecidingABindingIsTheProvidingProjectsAdmins(t *testing.T) {
	decide := "/api/v1/projects/" + providerProject + "/requests/" + requestedClaim
	queue := "/api/v1/projects/" + providerProject + "/requests"

	for name, testCase := range map[string]struct {
		// on is the project the caller holds a role on, and role is which.
		on, role string
		status   int
		says     string
	}{
		// A developer of the providing project may deploy it and may not
		// decide who calls it.
		"a developer of the providing project": {
			providerProject, string(kitchenv1alpha1.AccessRoleDeveloper),
			http.StatusForbidden,
			"you have developer on " + providerProject + "; %s needs admin",
		},
		// And the consuming project's own admin, who would very much like to
		// admit themselves. They hold nothing on the providing project, so
		// they are not told there is anything here to be refused — the same
		// answer every other route gives somebody about a project they have
		// no role on, and the reason a queue naming who asked can be
		// answered at all.
		"an admin of the consuming project": {
			feedProject, string(kitchenv1alpha1.AccessRoleAdmin),
			http.StatusNotFound,
			`projects.kitchen.bermos.dev "` + providerProject + `" not found`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil, append(fixtures(),
				pricingProject(requestOffering()), bindingClaim(pendingGrant()))...)
			h.demoteCaller(t)
			h.grant(t, testCase.on, kitchenv1alpha1.AccessRole(testCase.role))

			for _, call := range []struct{ method, path, body, doing string }{
				{http.MethodPatch, decide, `{"decision": "approved"}`,
					"admitting another project to an offering"},
				{http.MethodGet, queue, "", "reading what other projects have asked to bind"},
			} {
				recorder := h.do(t, call.method, call.path, call.body)
				if recorder.Code != testCase.status {
					t.Fatalf("%s %s: want %d, got %d: %s", call.method, call.path, testCase.status,
						recorder.Code, recorder.Body.String())
				}
				// A refusal that names what was being done says so; a
				// not-found says only that there is no such project,
				// whichever call asked.
				want := testCase.says
				if strings.Contains(want, "%s") {
					want = fmt.Sprintf(want, call.doing)
				}
				if got := errorOf(t, recorder.Body.String()); got != want {
					t.Fatalf("%s %s: want %q, got %q", call.method, call.path, want, got)
				}
			}
			// And nothing was decided by either of them.
			claim := &kitchenv1alpha1.ResourceClaim{}
			if err := h.server.get(t.Context(), requestedClaim, claim); err != nil {
				t.Fatal(err)
			}
			if claim.ServiceGrant().State != kitchenv1alpha1.ServiceGrantRequested {
				t.Errorf("the request is still waiting: %+v", claim.ServiceGrant())
			}
		})
	}
}

// And the admin of the providing project, who is the whole of who this is
// for: the same two calls, answered.
func TestTheProvidingProjectsAdminReadsTheQueueAndDecides(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(),
		pricingProject(requestOffering()), bindingClaim(pendingGrant()))...)
	h.demoteCaller(t)
	h.grant(t, providerProject, kitchenv1alpha1.AccessRoleAdmin)

	body := decode[listBody[bindingRequestView]](t,
		h.do(t, http.MethodGet, "/api/v1/projects/"+providerProject+"/requests", ""))
	if len(body.Items) != 1 || body.Items[0].RequestedBy != "ada@example.com" {
		t.Fatalf("the queue answers who asked: %+v", body.Items)
	}
	if recorder := h.do(t, http.MethodPatch,
		"/api/v1/projects/"+providerProject+"/requests/"+requestedClaim,
		`{"decision": "approved"}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

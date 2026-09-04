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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Point-in-time recovery is two operations with two blast radii, and this is
// where the difference is proved: recovering is the developer's day job and
// promoting is the admin's, because one makes a sibling database and the
// other replaces the one every environment reads.

const (
	recoverableClaim = "shop-orders"
	// aLook is the recovery these tests make: a copy somebody takes to see
	// what was there, which is the whole of what recovering is for.
	aLook = "a-look"
)

// recoverable is a claim of `shop`'s whose provider has answered: a week of
// window, and one recovery already made.
func recoverable(recoveries ...kitchenv1alpha1.ClaimRecovery) runtime.Object {
	now := time.Now().UTC()
	claim := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: recoverableClaim, Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: feedProject},
			ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "neon"},
			Type:          kitchenv1alpha1.ClaimTypePostgres,
			DataClass:     kitchenv1alpha1.DataClassConfidential,
		},
		Status: kitchenv1alpha1.ResourceClaimStatus{
			Phase:      kitchenv1alpha1.ClaimBound,
			SecretName: recoverableClaim + "-binding",
			InstanceID: "proj-1",
			Recovery: &kitchenv1alpha1.ClaimRecoveryStatus{
				Available: true,
				Reason:    "neon can reconstruct this database to any moment inside the window",
				Window: &kitchenv1alpha1.ClaimRecoveryWindow{
					Earliest:   metav1.NewTime(now.Add(-7 * 24 * time.Hour)),
					Latest:     metav1.NewTime(now),
					ObservedAt: metav1.NewTime(now),
				},
				Recoveries: recoveries,
			},
		},
	}
	for _, recovery := range recoveries {
		claim.Spec.Recoveries = append(claim.Spec.Recoveries, kitchenv1alpha1.ClaimRecoveryRequest{
			Name: recovery.Name, At: recovery.At,
		})
	}
	return claim
}

// madeRecovery is a sibling the operator has finished making.
func madeRecovery(name string) kitchenv1alpha1.ClaimRecovery {
	return kitchenv1alpha1.ClaimRecovery{
		Name:       name,
		At:         metav1.NewTime(time.Now().UTC().Add(-3 * time.Hour)),
		ID:         "br-" + name,
		SecretName: recoverableClaim + "-recovery-" + name,
		Provenance: "production",
		DataClass:  kitchenv1alpha1.DataClassConfidential,
		Phase:      kitchenv1alpha1.ClaimRecoveryReady,
		CreatedAt:  metav1.NewTime(time.Now().UTC()),
	}
}

// recoveredClaim is the claim these tests act on, read back as the API server
// holds it: what the handler wrote is what the reconciler would read.
func recoveredClaim(t *testing.T, h *harness) *kitchenv1alpha1.ResourceClaim {
	t.Helper()
	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), recoverableClaim, claim); err != nil {
		t.Fatalf("reading claim %s: %v", recoverableClaim, err)
	}
	return claim
}

// The window is what the provider said, answered as it stands: a viewer
// reading the screen needs the span before anything else on it means
// anything.
func TestRecoveriesAnswerTheWindowAndTheSiblings(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, neonConnection(), recoverable(madeRecovery(aLook)))

	recorder := h.do(t, http.MethodGet, "/api/v1/claims/"+recoverableClaim+"/recoveries", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[recoveriesView](t, recorder)
	if !view.Available || view.Window == nil {
		t.Fatalf("the window the provider reported is the answer: %+v", view)
	}
	if !view.Window.Earliest.Before(view.Window.Latest) {
		t.Errorf("a window reaches back: %+v", view.Window)
	}
	if len(view.Recoveries) != 1 || view.Recoveries[0].Name != aLook {
		t.Fatalf("the siblings are answered too: %+v", view.Recoveries)
	}
	// A recovery is a new place the same data lives, so it carries the
	// claim's class and the provider's declaration.
	if view.Recoveries[0].DataClass != string(kitchenv1alpha1.DataClassConfidential) ||
		view.Recoveries[0].Provenance != "production" {
		t.Errorf("the recovery inherits the claim's class and the provider's provenance: %+v", view.Recoveries[0])
	}
}

// Recovering is cheap and reversible, so it is the developer's — and it
// answers 202, because the operator makes the database after the response.
func TestADeveloperRecoversToAMomentInTheWindow(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, neonConnection(), recoverable())

	at := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	recorder := h.do(t, http.MethodPost, "/api/v1/claims/"+recoverableClaim+"/recoveries",
		fmt.Sprintf(`{"at": %q, "name": "before-the-migration"}`, at))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}

	claim := recoveredClaim(t, h)
	if len(claim.Spec.Recoveries) != 1 || claim.Spec.Recoveries[0].Name != "before-the-migration" {
		t.Fatalf("the request is what the reconciler reads: %+v", claim.Spec.Recoveries)
	}
	if claim.Spec.PromotedRecovery != "" {
		t.Error("recovering binds nothing: the claim still reads its own database")
	}
}

// A date picker over a window that does not exist is worse than no feature,
// so the refusal names the window rather than failing at the provider.
func TestRecoveringOutsideTheWindowIsRefused(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, neonConnection(), recoverable())

	at := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	recorder := h.do(t, http.MethodPost, "/api/v1/claims/"+recoverableClaim+"/recoveries",
		fmt.Sprintf(`{"at": %q}`, at))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if refusal := errorOf(t, recorder.Body.String()); !strings.Contains(refusal, "can reach back to") {
		t.Errorf("the refusal says what the window is; got %q", refusal)
	}
}

// A claim whose provider cannot do this offers nothing, and says why in the
// provider's own words rather than through a disabled control.
func TestRecoveringWhereTheProviderCannotIsRefusedWithTheReason(t *testing.T) {
	claim := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-cnpg", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: feedProject},
			ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "neon"},
			Type:          kitchenv1alpha1.ClaimTypePostgres,
		},
		Status: kitchenv1alpha1.ResourceClaimStatus{
			Recovery: &kitchenv1alpha1.ClaimRecoveryStatus{
				Available: false,
				Reason:    "cnpg cannot recover a database to a point in time",
			},
		},
	}
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, neonConnection(), claim)

	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	recorder := h.do(t, http.MethodPost, "/api/v1/claims/shop-cnpg/recoveries", fmt.Sprintf(`{"at": %q}`, at))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if refusal := errorOf(t, recorder.Body.String()); !strings.Contains(refusal, "cnpg cannot recover") {
		t.Errorf("the provider's own words are the refusal; got %q", refusal)
	}
}

// Promote replaces the database every environment of the project reads, so it
// takes the role that may delete the project all of it belongs to.
func TestADeveloperMayNotPromoteARecovery(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, neonConnection(), recoverable(madeRecovery(aLook)))

	recorder := h.do(t, http.MethodPost,
		"/api/v1/claims/"+recoverableClaim+"/recoveries/"+aLook+"/promote", "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
	refusal := errorOf(t, recorder.Body.String())
	for _, want := range []string{"needs admin", "you have developer on " + feedProject, "displaces is kept"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal names the role it wants and what it does; got %q", refusal)
		}
	}
	if recoveredClaim(t, h).Spec.PromotedRecovery != "" {
		t.Error("the refused promote must not have cut anything over")
	}
}

func TestAnAdminPromotesARecovery(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin, neonConnection(), recoverable(madeRecovery(aLook)))

	recorder := h.do(t, http.MethodPost,
		"/api/v1/claims/"+recoverableClaim+"/recoveries/"+aLook+"/promote", "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if promoted := recoveredClaim(t, h).Spec.PromotedRecovery; promoted != aLook {
		t.Fatalf("the claim binds the recovery now: %q", promoted)
	}
}

// Promoting is a destructive write, so it goes through the audit log on the
// way: a log that cannot append is a promote that does not happen. That is
// what makes the record a record rather than a report.
func TestAPromoteThatCannotBeRecordedIsNotMade(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin, neonConnection(), recoverable(madeRecovery(aLook)))
	h.withUnreachableAuditLog(t)

	recorder := h.do(t, http.MethodPost,
		"/api/v1/claims/"+recoverableClaim+"/recoveries/"+aLook+"/promote", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if recoveredClaim(t, h).Spec.PromotedRecovery != "" {
		t.Error("an unrecorded promote is not one the platform makes")
	}
}

// A recovery that is not there yet cannot be promoted over a live database,
// and the refusal says which of the two it is.
func TestPromotingARecoveryThatIsNotReadyIsRefused(t *testing.T) {
	pending := madeRecovery("still-coming")
	pending.Phase = kitchenv1alpha1.ClaimRecoveryPending
	pending.SecretName = ""
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin, neonConnection(), recoverable(pending))

	recorder := h.do(t, http.MethodPost,
		"/api/v1/claims/"+recoverableClaim+"/recoveries/still-coming/promote", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestADeveloperDiscardsARecoveryNobodyPromoted(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, neonConnection(), recoverable(madeRecovery(aLook)))

	recorder := h.do(t, http.MethodDelete, "/api/v1/claims/"+recoverableClaim+"/recoveries/"+aLook, "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if requests := recoveredClaim(t, h).Spec.Recoveries; len(requests) != 0 {
		t.Fatalf("the request is gone, so the operator takes the database away: %+v", requests)
	}
}

// Discarding the database the application is reading is not a discard.
func TestDiscardingThePromotedRecoveryIsRefused(t *testing.T) {
	claim := recoverable(madeRecovery(aLook)).(*kitchenv1alpha1.ResourceClaim)
	claim.Spec.PromotedRecovery = aLook
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, neonConnection(), claim)

	recorder := h.do(t, http.MethodDelete, "/api/v1/claims/"+recoverableClaim+"/recoveries/"+aLook, "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if refusal := errorOf(t, recorder.Body.String()); !strings.Contains(refusal, "Promote another recovery first") {
		t.Errorf("the refusal says the way out; got %q", refusal)
	}
}

// A recovery this claim does not have is a 404 naming the ones it does.
func TestAnUnknownRecoveryNamesTheOnesThereAre(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin, neonConnection(), recoverable(madeRecovery(aLook)))

	recorder := h.do(t, http.MethodPost,
		"/api/v1/claims/"+recoverableClaim+"/recoveries/nowhere/promote", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if refusal := errorOf(t, recorder.Body.String()); !strings.Contains(refusal, "it has "+aLook) {
		t.Errorf("the refusal names what there is; got %q", refusal)
	}
}

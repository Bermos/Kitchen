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
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// `deletionPolicy: Delete` is the one escalation on the claims surface: the
// route's row in the table is the developer's floor, and destroying the
// provisioned data is the admin's. Both ends are checked here — asking for
// the policy, and acting on a claim that already carries it — because the
// condition is a field rather than a route, so no table can hold it.

// deletePolicyClaim is a claim of `shop`'s that destroys its database when it
// goes: the object a developer must not be able to delete.
func deletePolicyClaim() runtime.Object {
	return &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-analytics", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: feedProject},
			ConnectionRef:  &kitchenv1alpha1.LocalObjectReference{Name: "neon"},
			Type:           kitchenv1alpha1.ClaimTypePostgres,
			DeletionPolicy: kitchenv1alpha1.ClaimDelete,
		},
	}
}

func TestADeveloperMayNotClaimWithTheDeletePolicy(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, neonConnection())

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "orders-db", "project": "`+feedProject+`", "connection": "neon", "type": "postgres",
			"deletionPolicy": "Delete"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
	refusal := errorOf(t, recorder.Body.String())
	for _, want := range []string{"deletionPolicy Delete", "needs admin", "you have developer on " + feedProject} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal names the field and the role it wants; got %q", refusal)
		}
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "orders-db", claim); err == nil {
		t.Fatal("the refused claim must not have been created")
	}
}

func TestADeveloperStillClaimsWithTheRetainPolicy(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, neonConnection())

	for name, body := range map[string]string{
		"named outright": `{"name": "orders-db", "project": "` + feedProject + `", "connection": "neon",
			"type": "postgres", "deletionPolicy": "Retain"}`,
		"left to the default": `{"name": "reports-db", "project": "` + feedProject + `", "connection": "neon",
			"type": "postgres"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if recorder := h.do(t, http.MethodPost, "/api/v1/claims", body); recorder.Code != http.StatusCreated {
				t.Fatalf("a developer claims a retained resource: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestAnAdminClaimsWithTheDeletePolicy(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin, neonConnection())

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "orders-db", "project": "`+feedProject+`", "connection": "neon", "type": "postgres",
			"deletionPolicy": "Delete"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if created := decode[claimView](t, recorder); created.DeletionPolicy != string(kitchenv1alpha1.ClaimDelete) {
		t.Errorf("the admin's choice is what was stored: %q", created.DeletionPolicy)
	}
}

func TestADeveloperMayNotDeleteADeletePolicyClaim(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, neonConnection(), deletePolicyClaim())

	recorder := h.do(t, http.MethodDelete, "/api/v1/claims/shop-analytics", "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
	refusal := errorOf(t, recorder.Body.String())
	for _, want := range []string{"deletionPolicy Delete", "needs admin", "you have developer on " + feedProject} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal names the field and the role it wants; got %q", refusal)
		}
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "shop-analytics", claim); err != nil {
		t.Fatalf("the claim must still be there: %v", err)
	}
}

func TestADeveloperStillDeletesARetainedClaim(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)

	// `shop-db` carries no policy at all, which reads as Retain: the
	// database stays at the provider and only the binding goes.
	if recorder := h.do(t, http.MethodDelete, "/api/v1/claims/shop-db", ""); recorder.Code != http.StatusAccepted {
		t.Fatalf("a developer deletes a retained claim: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAnAdminDeletesADeletePolicyClaim(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin, neonConnection(), deletePolicyClaim())

	if recorder := h.do(t, http.MethodDelete, "/api/v1/claims/shop-analytics", ""); recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

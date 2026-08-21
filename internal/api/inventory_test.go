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
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The classification inventory: one request answers every environment and
// claim with its class, provenance and location — and answers the absences in
// words ("unclassified", "undeclared", "unknown") rather than blanks, because
// an export an auditor reads must not leave an empty cell open to a generous
// reading.

// inventoryClassConfidential keeps goconst quiet where the vocabulary is the point.
const inventoryClassConfidential = "confidential"

func classifiedFixtures() []runtime.Object {
	claim := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-warehouse", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: feedProject},
			ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "neon"},
			Type:          kitchenv1alpha1.ClaimTypePostgres,
			DataClass:     kitchenv1alpha1.DataClassConfidential,
		},
		Status: kitchenv1alpha1.ResourceClaimStatus{
			Phase:     kitchenv1alpha1.ClaimBound,
			Residency: "aws-eu-central-1",
		},
	}
	return append(blogFixtures(), claim)
}

func TestTheInventoryAnswersEveryEnvironmentAndClaimInOneRequest(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), classifiedFixtures()...)...)

	// Declare the platform's residency, so environments without one of their
	// own have something to inherit.
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Spec.Residency = "CH"
	if err := h.server.Client.Update(context.Background(), kitchen); err != nil {
		t.Fatal(err)
	}

	// Rate the shop environment so the answer carries a declared class too.
	env := h.environment(t)
	env.Spec.DataClass = kitchenv1alpha1.DataClassConfidential
	env.Spec.Residency = "CH"
	if err := h.server.Client.Update(context.Background(), env); err != nil {
		t.Fatal(err)
	}

	recorder := h.do(t, http.MethodGet, "/api/v1/compliance/inventory", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[inventoryBody](t, recorder)
	if body.DefaultResidency != "CH" {
		t.Fatalf("the platform's declared residency must be answered, got %q", body.DefaultResidency)
	}

	rows := map[string]inventoryItemView{}
	for _, item := range body.Items {
		rows[item.Kind+"/"+item.Name] = item
	}
	classified, ok := rows["environment/"+testEnvironment]
	if !ok || classified.DataClass != inventoryClassConfidential || classified.Residency != "CH" {
		t.Fatalf("the rated environment must carry its class and residency, got %+v", classified)
	}
	claim, ok := rows["claim/shop-warehouse"]
	if !ok || claim.DataClass != inventoryClassConfidential || claim.Residency != "aws-eu-central-1" {
		t.Fatalf("the claim must carry its class and its reported placement, got %+v", claim)
	}
	if claim.Provenance != "undeclared" {
		t.Fatalf("a claim without a declaration reads undeclared, not blank: %+v", claim)
	}

	// The blog environment is unclassified and undeclared — visible as such,
	// with the residency falling back to the platform's declared default.
	blog, ok := rows["environment/blog-production"]
	if !ok || blog.DataClass != "unclassified" {
		t.Fatalf("an unclassified environment must say so, got %+v", blog)
	}
	if blog.Residency != "CH" {
		t.Fatalf("an environment without a residency inherits the platform's declared one, got %+v", blog)
	}
}

func TestTheInventoryWithNoDeclarationsAnywhereSaysSoInWords(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/compliance/inventory", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[inventoryBody](t, recorder)
	if body.DefaultResidency != "" {
		t.Fatalf("nobody declared a platform residency, got %q", body.DefaultResidency)
	}
	for _, item := range body.Items {
		if item.DataClass != "unclassified" {
			t.Fatalf("nothing is classified, got %+v", item)
		}
		if item.Residency != "unknown" {
			t.Fatalf("nothing declares a residency, got %+v", item)
		}
	}
	if strings.Contains(recorder.Body.String(), `"dataClass":""`) {
		t.Fatalf("no blank cells: %s", recorder.Body.String())
	}
}

func TestTheInventoryIsFilteredToTheCallersProjects(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, classifiedFixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/compliance/inventory", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[inventoryBody](t, recorder)
	if len(body.Items) == 0 {
		t.Fatal("a member must still read their own projects' rows")
	}
	for _, item := range body.Items {
		if item.Project != feedProject {
			t.Fatalf("a member must not be answered about %s: %+v", item.Project, item)
		}
	}
}

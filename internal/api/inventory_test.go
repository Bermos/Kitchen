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

// The outsourcing half (#309): every image running here that somebody else
// built, with where it came from and who admitted it.
//
// It is keyed off the deployed release rather than off the project's
// declaration, because a declaration says what is wanted and a Release says
// what is running — and it is the second one the list is made against.
func TestTheInventoryNamesEveryVendoredImageRunningAndWhoAdmittedIt(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	build := &kitchenv1alpha1.Build{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: testBuild}, build); err != nil {
		t.Fatal(err)
	}
	build.Status.Artifact = &kitchenv1alpha1.ArtifactStatus{
		Repository: "ghcr.io/vendor/app",
		Digest:     "sha256:" + strings.Repeat("1", 64),
		SourceType: kitchenv1alpha1.ArtifactSourceVendored,
		Upstream: &kitchenv1alpha1.UpstreamArtifactStatus{
			Reference:  "ghcr.io/vendor/app:2026.9.1",
			Repository: "ghcr.io/vendor/app",
			AdmittedBy: "ana@example.com",
			Signature: kitchenv1alpha1.UpstreamSignatureStatus{
				Result: kitchenv1alpha1.UpstreamSignatureVerified,
			},
		},
	}
	// A sidecar this platform did build, in the same unit: a mixed unit is
	// not wholly outsourced and the answer must not read as though it were.
	build.Status.Workloads = []kitchenv1alpha1.WorkloadBuildStatus{{
		Name:       "api",
		Phase:      kitchenv1alpha1.BuildSucceeded,
		Repository: "registry.example.com/shop-api",
		Image:      "registry.example.com/shop-api@sha256:" + strings.Repeat("b", 64),
		Artifact: &kitchenv1alpha1.ArtifactStatus{
			Repository: "registry.example.com/shop-api",
			Digest:     "sha256:" + strings.Repeat("b", 64),
			SourceType: kitchenv1alpha1.ArtifactSourceBuilt,
		},
	}}
	if err := h.server.Client.Status().Update(context.Background(), build); err != nil {
		t.Fatal(err)
	}

	recorder := h.do(t, http.MethodGet, "/api/v1/compliance/inventory", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[inventoryBody](t, recorder)

	vendored := []inventoryItemView{}
	for _, item := range body.Items {
		if item.Kind == "vendoredImage" {
			vendored = append(vendored, item)
		}
	}
	if len(vendored) != 1 {
		t.Fatalf("the inventory lists %d outsourced images, want the one that is: %+v", len(vendored), vendored)
	}
	row := vendored[0]
	// The row names the environment and which of its images, because a unit
	// ships several and a row naming only the project would describe a mixed
	// unit as wholly outsourced.
	if row.Name != testEnvironment+"/web" {
		t.Errorf("the outsourced image is listed as %q", row.Name)
	}
	if row.Upstream != "ghcr.io/vendor/app:2026.9.1" {
		t.Errorf("the row says it came from %q", row.Upstream)
	}
	if row.Digest != "sha256:"+strings.Repeat("1", 64) {
		t.Errorf("the row resolves to %q", row.Digest)
	}
	if row.AdmittedBy != "ana@example.com" {
		t.Errorf("the row says %q admitted it", row.AdmittedBy)
	}
	if row.Signature != string(kitchenv1alpha1.UpstreamSignatureVerified) {
		t.Errorf("the row records the signature as %q", row.Signature)
	}
	// The environment's own facts, because an image has none of its own and
	// "unclassified" beside a component handling confidential data would be
	// answering a different question.
	if row.DataClass == "" || row.Residency == "" {
		t.Errorf("the row leaves a data fact blank: %+v", row)
	}
	// And an environment's own row is still there: this is a second kind of
	// row, not a replacement for the classification half.
	for _, item := range body.Items {
		if item.Kind == "environment" && item.Name == testEnvironment {
			return
		}
	}
	t.Error("the environment's own classification row was lost")
}

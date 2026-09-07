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

// One project offering something and another binding to it (#493), through
// the two doors that make it: the project's own settings, and the claim.

// The provider in these tests, and the offering the consumer binds.
const (
	providerProject  = "pricing"
	openOfferingName = "pricing-api"
)

// pricingProject is the provider in these tests: a second project, with a
// service workload of its own and whatever offerings the case needs.
func pricingProject(offers ...kitchenv1alpha1.ServiceOffering) *kitchenv1alpha1.Project {
	return &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: providerProject, Namespace: testNamespace},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
				Repo:          "acme/" + providerProject,
			}},
			Registry: &kitchenv1alpha1.RegistrySpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: testRegistry},
			},
			Processes: []kitchenv1alpha1.ProcessSpec{
				{Name: "api", Type: kitchenv1alpha1.ProcessService, Port: 8080},
				{Name: "mailer", Type: kitchenv1alpha1.ProcessWorker},
			},
			Offers: offers,
		},
	}
}

func openOffering() kitchenv1alpha1.ServiceOffering {
	return kitchenv1alpha1.ServiceOffering{
		Name:      openOfferingName,
		Process:   "api",
		VisibleTo: kitchenv1alpha1.OfferingOpen,
	}
}

func TestAnOfferingIsWrittenOnTheProjectSettings(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"offers": [{"name": "catalogue", "visibility": "open"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if len(view.Offers) != 1 {
		t.Fatalf("the project answers with what it offers: %+v", view.Offers)
	}
	offering := view.Offers[0]
	if offering.Name != "catalogue" || offering.Process != kitchenv1alpha1.WebProcessName ||
		offering.Protocol != string(kitchenv1alpha1.OfferingHTTP) ||
		offering.Auth != string(kitchenv1alpha1.OfferingAuthNone) ||
		offering.Visibility != string(kitchenv1alpha1.OfferingOpen) {
		t.Errorf("an offering answers with its defaults filled in: %+v", offering)
	}
	if offering.Environment != "shop-production" {
		t.Errorf("an offering with no environment serves the project's production one, got %q",
			offering.Environment)
	}

	project := &kitchenv1alpha1.Project{}
	if err := h.server.get(t.Context(), "shop", project); err != nil {
		t.Fatal(err)
	}
	if len(project.Spec.Offers) != 1 || project.Spec.Offers[0].Name != "catalogue" {
		t.Fatalf("spec.offers carries the offering: %+v", project.Spec.Offers)
	}
	// Written as it was asked for, with the defaults left absent so that a
	// later change of default reaches an offering that never chose.
	if project.Spec.Offers[0].Process != "" || project.Spec.Offers[0].Speaks != "" {
		t.Errorf("what the request did not say is not written: %+v", project.Spec.Offers[0])
	}
}

func TestTheShapeOfAnOfferingIsRefusedHere(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, testCase := range map[string]struct {
		body string
		says string
	}{
		"no name": {
			`{"offers": [{"visibility": "open"}]}`,
			"every offering has a name",
		},
		"a name that is not a label": {
			`{"offers": [{"name": "Pricing API"}]}`,
			"must work as a DNS label",
		},
		"twice": {
			`{"offers": [{"name": "a"}, {"name": "a"}]}`,
			"declared twice",
		},
		"a protocol nothing speaks": {
			`{"offers": [{"name": "a", "protocol": "grpc"}]}`,
			"speaks either http",
		},
		"a rung that is not built": {
			`{"offers": [{"name": "a", "auth": "gate"}]}`,
			"the only rung built is none",
		},
		"a visibility that is neither": {
			`{"offers": [{"name": "a", "visibility": "public"}]}`,
			"must be visible to consumers by request",
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), testCase.says) {
				t.Errorf("the refusal should say %q: %s", testCase.says, recorder.Body.String())
			}
		})
	}
}

// A repository's kitchen.json replaces the project's workload list at every
// build, so a project configured that way declares none of them here — and an
// offering of one has to be writable anyway. What is refused is the workload
// the project *does* declare and nothing addresses.
func TestAnOfferingMayNameAWorkloadTheRepositoryDeclares(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"offers": [{"name": "shop-api", "process": "api"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[projectView](t, recorder); len(view.Offers) != 1 || view.Offers[0].Process != "api" {
		t.Errorf("the offering keeps the workload it named: %+v", view.Offers)
	}
}

func TestAnOfferingCanNameAWorkloadTheSameRequestAdds(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"processes": [{"name": "api", "type": "service", "port": 8080}],
			"offers": [{"name": "shop-api", "process": "api"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("one request may add the workload and the offering that serves it: %d %s",
			recorder.Code, recorder.Body.String())
	}

	// And a workload nothing addresses is refused by name, because there is
	// no address to hand a consumer.
	recorder = h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"processes": [{"name": "mailer", "type": "worker"}],
			"offers": [{"name": "shop-api", "process": "mailer"}]}`)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "nothing addresses one") {
		t.Fatalf("want a refusal naming the workload: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestTheCatalogueListsWhatEveryProjectOffers(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), pricingProject(openOffering()))...)

	recorder := h.do(t, http.MethodGet, "/api/v1/offerings", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	offerings := decode[struct {
		Items []offeringView `json:"items"`
	}](t, recorder).Items
	if len(offerings) != 1 {
		t.Fatalf("the catalogue lists the platform's offerings: %+v", offerings)
	}
	if offerings[0].Project != providerProject || offerings[0].Name != openOfferingName ||
		offerings[0].Environment != providerProject+"-production" || offerings[0].Process != "api" {
		t.Errorf("an offering is listed with what a consumer needs to bind it: %+v", offerings[0])
	}
}

func TestAServiceClaimBindsAnOffering(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), pricingProject(openOffering()))...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "prices", "project": "shop", "type": "service",
			"service": {"project": "pricing", "offering": "pricing-api"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	created := decode[claimView](t, recorder)
	if created.Connection != "" {
		t.Errorf("a service claim names no connection: %+v", created)
	}
	if created.Service == nil || created.Service.Project != "pricing" ||
		created.Service.Offering != "pricing-api" {
		t.Fatalf("the answer says what it binds: %+v", created.Service)
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "prices", claim); err != nil {
		t.Fatal(err)
	}
	if got := claim.Service(); got.Project != "pricing" || got.Offering != "pricing-api" {
		t.Errorf("spec.config carries the service block as the reconciler reads it: %+v", got)
	}
}

func TestAServiceClaimIsRefusedWhatItCannotBind(t *testing.T) {
	// Two offerings, one of each visibility. Neither visibility is a
	// refusal at the door any more (#495): a claim on an offering that
	// admits by request is *created* and waits for the providing project to
	// answer it, which is what TestABindingOnARequestOfferingIsWrittenAndWaits
	// covers.
	provider := pricingProject(
		kitchenv1alpha1.ServiceOffering{Name: openOfferingName},
		kitchenv1alpha1.ServiceOffering{Name: "open-api", VisibleTo: kitchenv1alpha1.OfferingOpen},
	)
	h := newHarness(t, nil, append(fixtures(), provider)...)

	for name, testCase := range map[string]struct {
		body string
		says string
	}{
		"no block": {
			`{"name": "prices", "project": "shop", "type": "service"}`,
			"service is required",
		},
		"half a block": {
			`{"name": "prices", "project": "shop", "type": "service", "service": {"project": "pricing"}}`,
			"both required",
		},
		"a project that does not exist": {
			`{"name": "prices", "project": "shop", "type": "service",
				"service": {"project": "billing", "offering": "rates"}}`,
			"no project named",
		},
		"an offering that does not exist": {
			`{"name": "prices", "project": "shop", "type": "service",
				"service": {"project": "pricing", "offering": "rates"}}`,
			"makes no offering named",
		},
		"a connection": {
			`{"name": "prices", "project": "shop", "type": "service", "connection": "gh",
				"service": {"project": "pricing", "offering": "pricing-api"}}`,
			"takes no connection",
		},
		"a deletion policy, on an offering it could otherwise bind": {
			`{"name": "prices", "project": "shop", "type": "service", "deletionPolicy": "Delete",
				"service": {"project": "pricing", "offering": "open-api"}}`,
			"takes no deletionPolicy",
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/claims", testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), testCase.says) {
				t.Errorf("the refusal should say %q: %s", testCase.says, recorder.Body.String())
			}
		})
	}
}

// The collision the shared prefix costs: a binding and a workload of the
// consumer's own would arrive in one variable, so whichever is named second
// is refused.
func TestABindingAndAWorkloadCannotShareAName(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), pricingProject(openOffering()))...)

	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"processes": [{"name": "api", "type": "service", "port": 8080}]}`); recorder.Code != http.StatusOK {
		t.Fatalf("setting up the workload: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "api", "project": "shop", "type": "service",
			"service": {"project": "pricing", "offering": "pricing-api"}}`)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "KITCHEN_SERVICE_API") {
		t.Fatalf("want a refusal naming the variable: %d %s", recorder.Code, recorder.Body.String())
	}

	// And the other way round: the binding first, then a workload under its
	// name.
	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"processes": []}`); recorder.Code != http.StatusOK {
		t.Fatalf("clearing the workload: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "api", "project": "shop", "type": "service",
			"service": {"project": "pricing", "offering": "pricing-api"}}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"processes": [{"name": "api", "type": "service", "port": 8080}]}`)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "would collide with this project's binding") {
		t.Fatalf("want a refusal naming the binding: %d %s", recorder.Code, recorder.Body.String())
	}
}

// Deleting a project takes its offerings with it, which is the one part of a
// project's blast radius that belongs to somebody else.
func TestDeletingAProjectOtherProjectsBindToIsRefusedUnlessSaidTwice(t *testing.T) {
	binding := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "prices", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:       kitchenv1alpha1.ClaimTypeService,
			Config: &runtime.RawExtension{
				Raw: []byte(`{"service": {"project": "pricing", "offering": "pricing-api"}}`),
			},
		},
	}
	h := newHarness(t, nil, append(fixtures(), pricingProject(openOffering()), binding)...)

	recorder := h.do(t, http.MethodDelete, "/api/v1/projects/pricing", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "shop") {
		t.Errorf("the refusal names who is on the other end: %s", recorder.Body.String())
	}

	recorder = h.do(t, http.MethodDelete, "/api/v1/projects/pricing?breakBindings=true", "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("said twice, it goes: %d %s", recorder.Code, recorder.Body.String())
	}
}

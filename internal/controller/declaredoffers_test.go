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

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// kitchen.json declares the offerings this commit serves; the project says
// who may bind to them. The build is where the two meet (#493).
func TestTheCommitsOfferingsAreHeldToTheProjects(t *testing.T) {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "pricing"},
		Spec: kitchenv1alpha1.ProjectSpec{
			Offers: []kitchenv1alpha1.ServiceOffering{{
				Name:      "pricing-api",
				Process:   "api",
				VisibleTo: kitchenv1alpha1.OfferingOpen,
			}},
		},
	}

	for name, testCase := range map[string]struct {
		declared []kitchenv1alpha1.RepoOffering
		says     string
	}{
		"agreeing with the project": {
			declared: []kitchenv1alpha1.RepoOffering{{
				Name: "pricing-api", Process: "api", Speaks: kitchenv1alpha1.OfferingHTTP,
			}},
		},
		"holding no opinion at all": {
			declared: []kitchenv1alpha1.RepoOffering{{Name: "pricing-api"}},
		},
		"an offering the project does not make": {
			declared: []kitchenv1alpha1.RepoOffering{{Name: "rates"}},
			says:     "this project's offerings are pricing-api",
		},
		"a workload the project does not serve it from": {
			declared: []kitchenv1alpha1.RepoOffering{{Name: "pricing-api", Process: "web"}},
			says:     "answers with the wrong application",
		},
		"a protocol the project does not offer it as": {
			declared: []kitchenv1alpha1.RepoOffering{{
				Name: "pricing-api", Speaks: kitchenv1alpha1.OfferingTCP,
			}},
			says: "two different things to whoever is writing the client",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkDeclaredOffers(&kitchenv1alpha1.RepoConfig{Offers: testCase.declared}, project)
			if testCase.says == "" {
				if err != nil {
					t.Fatalf("the build should not fail here: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the build should fail on a declaration the project contradicts")
			}
			if !strings.Contains(err.Error(), testCase.says) {
				t.Errorf("message %q does not say what is wrong (%s)", err, testCase.says)
			}
		})
	}
}

// A project that offers nothing at all gets the sentence that says where an
// offering is made, since that is the state somebody adding the first one is
// in.
func TestAProjectThatOffersNothingSaysWhereToAddOne(t *testing.T) {
	err := checkDeclaredOffers(
		&kitchenv1alpha1.RepoConfig{Offers: []kitchenv1alpha1.RepoOffering{{Name: "pricing-api"}}},
		&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "pricing"}})
	if err == nil || !strings.Contains(err.Error(), "this project offers nothing") {
		t.Fatalf("err = %v, want the refusal that names the empty list", err)
	}
	if !strings.Contains(err.Error(), "PATCH /projects/pricing") {
		t.Errorf("the refusal does not say where an offering is added: %v", err)
	}
}

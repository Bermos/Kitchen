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

package policy

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The materializer is the one place typed objects become policy facts, so the
// data-classification fields have to reach the input here or the rules that
// judge them never see anything at all.

func TestMaterializeInputCarriesTheDataFacts(t *testing.T) {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kitchenv1alpha1.ProjectSpec{DataClass: kitchenv1alpha1.DataClassConfidential},
	}
	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-production"},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:       kitchenv1alpha1.EnvironmentProduction,
			DataClass:  kitchenv1alpha1.DataClassStrictlyConfidential,
			Residency:  "CH",
		},
	}
	release := &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-rel-1"},
		Spec:       kitchenv1alpha1.ReleaseSpec{Image: "registry.example.com/shop@sha256:1111"},
	}
	claims := []Claim{{Name: "shop-db", Type: "postgres", DataClass: "confidential"}}

	input := MaterializeInput(KindPromotion, time.Now().UTC(), project, env, release, nil, nil, claims)
	if input.Project.DataClass != "confidential" {
		t.Fatalf("the project's class must reach the input, got %+v", input.Project)
	}
	if input.Environment.DataClass != "strictlyConfidential" || input.Environment.Residency != "CH" {
		t.Fatalf("the environment's class and residency must reach the input, got %+v", input.Environment)
	}
	if len(input.Claims) != 1 || input.Claims[0].DataClass != "confidential" {
		t.Fatalf("the claims must reach the input, got %+v", input.Claims)
	}

	// A project that could not be read is judged unclassified — absent, not
	// defaulted.
	bare := MaterializeInput(KindPromotion, time.Now().UTC(), nil, env, release, nil, nil, nil)
	if bare.Project.DataClass != "" {
		t.Fatalf("no project means no class, got %+v", bare.Project)
	}
}

func TestClaimFactsAreTheEnvironmentsOwnProjects(t *testing.T) {
	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-pr-41"},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:       kitchenv1alpha1.EnvironmentPreview,
		},
	}
	claims := []kitchenv1alpha1.ResourceClaim{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "shop-db"},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
				Type:       "postgres",
				DataClass:  kitchenv1alpha1.DataClassInternal,
			},
			Status: kitchenv1alpha1.ResourceClaimStatus{Residency: "aws-eu-central-1"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "blog-db"},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "blog"},
				Type:       "postgres",
			},
		},
	}

	facts := ClaimFacts(env, claims)
	if len(facts) != 1 || facts[0].Name != "shop-db" {
		t.Fatalf("only the project's own claims are facts, got %+v", facts)
	}
	if facts[0].DataClass != "internal" || facts[0].Residency != "aws-eu-central-1" {
		t.Fatalf("the claim's class and placement must come through, got %+v", facts[0])
	}
}

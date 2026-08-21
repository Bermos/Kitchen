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

func TestProductionTargetIsTheLastStageOrTheDefaultName(t *testing.T) {
	project := &kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	if got := ProductionTargetEnvironmentName(project); got != "shop-production" {
		t.Fatalf("with no stages the target is <project>-production, got %q", got)
	}

	project.Spec.Promotion = &kitchenv1alpha1.PromotionPolicySpec{Stages: []kitchenv1alpha1.PromotionStage{
		{Name: "staging", Environment: "shop-staging"},
		{Name: "live", Environment: "shop-live"},
	}}
	if got := ProductionTargetEnvironmentName(project); got != "shop-live" {
		t.Fatalf("with stages the target is the last stage's environment, got %q", got)
	}
}

func TestDataClassRefusalNamesBothClassesAndTheFix(t *testing.T) {
	project := &kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	project.Spec.DataClass = kitchenv1alpha1.DataClassConfidential
	env := &kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: "shop-staging"}}
	env.Spec.DataClass = kitchenv1alpha1.DataClassInternal

	refusal := DataClassRefusal(project, env)
	for _, want := range []string{"confidential", "internal", "classify the environment"} {
		if !strings.Contains(refusal, want) {
			t.Fatalf("the refusal must carry %q, got %q", want, refusal)
		}
	}

	// An unrated environment refuses classified data too, and says why.
	env.Spec.DataClass = ""
	if refusal := DataClassRefusal(project, env); !strings.Contains(refusal, "unrated") {
		t.Fatalf("an unrated environment's refusal must say so, got %q", refusal)
	}

	// Equal classes fit, an environment rated above fits, and an
	// unclassified project raises no question at all.
	env.Spec.DataClass = kitchenv1alpha1.DataClassConfidential
	if refusal := DataClassRefusal(project, env); refusal != "" {
		t.Fatalf("an equal rating must be admissible, got %q", refusal)
	}
	env.Spec.DataClass = kitchenv1alpha1.DataClassStrictlyConfidential
	if refusal := DataClassRefusal(project, env); refusal != "" {
		t.Fatalf("a higher rating must be admissible, got %q", refusal)
	}
	project.Spec.DataClass = ""
	env.Spec.DataClass = ""
	if refusal := DataClassRefusal(project, env); refusal != "" {
		t.Fatalf("an unclassified project is refused nothing, got %q", refusal)
	}
}

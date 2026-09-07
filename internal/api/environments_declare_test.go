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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Declaring an environment before anything deploys into it (#491), and the
// two halves of the body: the name and the type, which the project's
// developers own, and the bar, which is nobody's but its owners' and the
// platform's.

// declarePath is the route under test, spelled once.
const declarePath = "/api/v1/projects/" + feedProject + "/environments"

// declaredStage is the environment these tests declare: `shop` has no
// promotion pipeline, so its production target is `shop-production` and every
// other durable environment of it is a stage.
const declaredStage = "shop-staging"

func TestDeclaringAnEnvironmentSetsItsBarBeforeTheFirstRelease(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, declarePath, `{
		"name": "`+declaredStage+`",
		"owners": ["risk-officer@example.com"],
		"requirements": {"bundleDigest": "sha256:`+strings.Repeat("a", 64)+`"},
		"dataClass": "confidential", "residency": "CH",
		"criticality": "critical", "rto": "15m", "rpo": "5m"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	view := decode[environmentView](t, recorder)
	if view.Type != string(kitchenv1alpha1.EnvironmentStage) {
		t.Fatalf("the type was not derived from the project's pipeline: %q", view.Type)
	}
	if view.Release != "" {
		t.Fatalf("a declared environment runs nothing yet, got release %q", view.Release)
	}

	stored := &kitchenv1alpha1.Environment{}
	if err := h.server.get(context.Background(), declaredStage, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.ReleaseRef.Name != "" {
		t.Fatalf("the declared environment names a release: %q", stored.Spec.ReleaseRef.Name)
	}
	if stored.Spec.ProjectRef.Name != feedProject {
		t.Fatalf("the environment belongs to %q, want %q", stored.Spec.ProjectRef.Name, feedProject)
	}
	if stored.Spec.Requirements == nil || stored.Spec.Requirements.BundleDigest == "" {
		t.Fatal("the bar was not stored: the whole point is that it is set before the first release")
	}
	if stored.Spec.DataClass != kitchenv1alpha1.DataClass("confidential") {
		t.Fatalf("dataClass is %q", stored.Spec.DataClass)
	}
	if stored.Spec.Residency != "CH" {
		t.Fatalf("residency is %q", stored.Spec.Residency)
	}
	if string(stored.Spec.Criticality) != "critical" || string(stored.Spec.RTO) != "15m" ||
		string(stored.Spec.RPO) != "5m" {
		t.Fatalf("the continuity declaration did not land: %q %q %q",
			stored.Spec.Criticality, stored.Spec.RTO, stored.Spec.RPO)
	}
	if len(stored.Spec.Owners) != 1 {
		t.Fatalf("owners are %v", stored.Spec.Owners)
	}

	// Every read the environment's own screen makes answers about it rather
	// than failing over the release it has not got: the processes list is the
	// one that would otherwise ask the cluster for a release named "".
	for _, path := range []string{
		"/api/v1/environments/" + declaredStage,
		"/api/v1/environments/" + declaredStage + "/processes",
	} {
		if recorder := h.do(t, http.MethodGet, path, ""); recorder.Code != http.StatusOK {
			t.Fatalf("GET %s answered %d: %s", path, recorder.Code, recorder.Body.String())
		}
	}
	// And the one that cannot: nothing is deployed, so there is nothing to
	// judge, and it says so rather than guessing.
	if recorder := h.do(t, http.MethodGet,
		"/api/v1/environments/"+declaredStage+"/eligibility", ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("eligibility of an environment running nothing answered %d: %s",
			recorder.Code, recorder.Body.String())
	}

	// It is on the project's list, which is where the dashboard reads it.
	list := decode[struct {
		Items []environmentView `json:"items"`
	}](t, h.do(t, http.MethodGet, declarePath, ""))
	var found bool
	for _, item := range list.Items {
		found = found || item.Name == declaredStage
	}
	if !found {
		t.Fatal("the declared environment is not in the project's environments")
	}

	// And the name is taken now, whoever asks next.
	if again := h.do(t, http.MethodPost, declarePath,
		`{"name": "`+declaredStage+`"}`); again.Code != http.StatusConflict {
		t.Fatalf("want 409 for a name already taken, got %d: %s", again.Code, again.Body.String())
	}
}

// The type is derived, and a type that disagrees with the project's pipeline
// is refused rather than accepted and corrected by the reconciler seconds
// later (#490's re-derivation).
func TestADeclaredEnvironmentsTypeMustAgreeWithThePipeline(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for _, testCase := range []struct{ name, body, says string }{
		{"a stage called production", `{"name": "` + declaredStage + `", "type": "production"}`, "stage"},
		{"a preview is never declared", `{"name": "` + declaredStage + `", "type": "preview"}`, "pull request"},
		{"and nothing else is a type", `{"name": "` + declaredStage + `", "type": "sideways"}`, "production or stage"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, declarePath, testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, testCase.says) {
				t.Fatalf("the refusal does not say why: %q", got)
			}
			if err := h.server.get(context.Background(), declaredStage,
				&kitchenv1alpha1.Environment{}); err == nil {
				t.Fatal("the environment was created anyway")
			}
		})
	}

	// The production target itself is `production`, sent or derived.
	if recorder := h.do(t, http.MethodPost, declarePath,
		`{"name": "shop-production-2", "type": "stage"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("a stage that agrees with the pipeline is a 201, got %d: %s",
			recorder.Code, recorder.Body.String())
	}
}

// A name that becomes a hostname is checked as one: the platform's own labels
// and the shape of another project's preview are refused at the name, the way
// a project's own name is (#423).
func TestADeclaredEnvironmentsNameIsCheckedAsAHostname(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for _, testCase := range []struct{ name, environment, says string }{
		{"not a DNS label", "Shop Staging", "DNS label"},
		{"empty", "", "name is required"},
		{"shaped like a preview", "shop-pr-7", "-pr-"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, declarePath, `{"name": "`+testCase.environment+`"}`)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, testCase.says) {
				t.Fatalf("the refusal does not say why: %q", got)
			}
		})
	}
}

// The split the feature stands on: a developer may declare that an
// environment exists, and may not declare what it demands. An environment
// that does not exist yet names no owners, so at creation that is the
// platform's operators alone — and the refusal says how to get there.
func TestOnlyAnOperatorDeclaresWhatANewEnvironmentDemands(t *testing.T) {
	for _, testCase := range []struct{ name, body string }{
		{"owners", `{"name": "` + declaredStage + `", "owners": ["someone@example.com"]}`},
		{"a bar", `{"name": "` + declaredStage + `", "requirements": {"bundleDigest": "sha256:` +
			strings.Repeat("b", 64) + `"}}`},
		{"a rating", `{"name": "` + declaredStage + `", "dataClass": "confidential"}`},
		{"a residency", `{"name": "` + declaredStage + `", "residency": "CH"}`},
		{"a designation", `{"name": "` + declaredStage + `", "criticality": "critical"}`},
		{"a tolerance", `{"name": "` + declaredStage + `", "rto": "15m"}`},
		{"who it serves", `{"name": "` + declaredStage + `", "serves": ["preview"]}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
			recorder := h.do(t, http.MethodPost, declarePath, testCase.body)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "operator") {
				t.Fatalf("the refusal does not say who may: %q", got)
			}
			if err := h.server.get(context.Background(), declaredStage,
				&kitchenv1alpha1.Environment{}); err == nil {
				t.Fatal("the environment was created anyway")
			}
		})
	}

	// The other half of the same rule: the name and the type are the
	// developer's, and a developer declaring only those is answered 201.
	t.Run("a developer declares the environment itself", func(t *testing.T) {
		h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
		recorder := h.do(t, http.MethodPost, declarePath, `{"name": "`+declaredStage+`"}`)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
		}
	})

	// And a viewer reaches none of it, which is the route's own row.
	t.Run("a viewer may not", func(t *testing.T) {
		h := asMember(t, kitchenv1alpha1.AccessRoleViewer)
		if recorder := h.do(t, http.MethodPost, declarePath,
			`{"name": "`+declaredStage+`"}`); recorder.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
		}
	})
}

// An environment inherits its project's class at creation the way one the
// build controller creates does (#137), so a classified project's declared
// environments can hold its data by construction.
func TestADeclaredEnvironmentInheritsItsProjectsClass(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+feedProject,
		`{"dataClass": "confidential"}`); recorder.Code != http.StatusOK {
		t.Fatalf("classifying the project failed: %d %s", recorder.Code, recorder.Body.String())
	}

	if recorder := h.do(t, http.MethodPost, declarePath,
		`{"name": "`+declaredStage+`"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stored := &kitchenv1alpha1.Environment{}
	if err := h.server.get(context.Background(), declaredStage, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.DataClass != kitchenv1alpha1.DataClass("confidential") {
		t.Fatalf("the declared environment did not inherit the project's class: %q", stored.Spec.DataClass)
	}
}

// Deleting one is defined by what is running in it: nothing, and it is a
// declaration somebody can take back; a release, and it is the project, torn
// down with it.
func TestDeletingADeclaredEnvironment(t *testing.T) {
	t.Run("nothing has deployed into it", func(t *testing.T) {
		h := newHarness(t, nil, fixtures()...)
		if recorder := h.do(t, http.MethodPost, declarePath,
			`{"name": "`+declaredStage+`"}`); recorder.Code != http.StatusCreated {
			t.Fatalf("declaring it failed: %d %s", recorder.Code, recorder.Body.String())
		}
		recorder := h.do(t, http.MethodDelete, "/api/v1/environments/"+declaredStage, "")
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
		}
		if err := h.server.get(context.Background(), declaredStage,
			&kitchenv1alpha1.Environment{}); err == nil {
			t.Fatal("the environment is still there")
		}
	})

	t.Run("a release is running in it", func(t *testing.T) {
		h := newHarness(t, nil, fixtures()...)
		recorder := h.do(t, http.MethodDelete, "/api/v1/environments/"+testEnvironment, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
		}
		if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, testRelease) {
			t.Fatalf("the refusal does not name what is running: %q", got)
		}
	})

	// Deleting an environment that declares a bar is the other way to remove
	// that bar, so it asks what changing the bar asks.
	t.Run("it declares a bar", func(t *testing.T) {
		declared := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: declaredStage, Namespace: testNamespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: feedProject},
				Type:       kitchenv1alpha1.EnvironmentStage,
				Owners:     []string{"risk-officer@example.com"},
			},
		}
		h := asMember(t, kitchenv1alpha1.AccessRoleAdmin, declared)

		recorder := h.do(t, http.MethodDelete, "/api/v1/environments/"+declaredStage, "")
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
		}
		if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "risk-officer@example.com") {
			t.Fatalf("the refusal does not say who may: %q", got)
		}
		if err := h.server.get(context.Background(), declaredStage,
			&kitchenv1alpha1.Environment{}); err != nil {
			t.Fatal("the environment was deleted anyway")
		}

		// An operator may, which is the other half of the same rule.
		operator := newHarness(t, nil, append(fixtures(), declared.DeepCopy())...)
		if recorder := operator.do(t, http.MethodDelete, "/api/v1/environments/"+declaredStage,
			""); recorder.Code != http.StatusAccepted {
			t.Fatalf("want 202 for an operator, got %d: %s", recorder.Code, recorder.Body.String())
		}
	})
}

// #494 at the moment an environment is declared: who it will answer is part
// of the owners' half of the body, so an operator can stand an environment up
// already open to other teams' previews — and an environment declared without
// it serves nobody, like every environment nobody has opened.
func TestADeclaredEnvironmentSaysWhoItServesOrNobody(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, declarePath,
		`{"name": "`+declaredStage+`", "serves": ["preview", "production"]}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[environmentView](t, recorder)
	if len(view.Serves) != 2 || view.Serves[0] != string(kitchenv1alpha1.EnvironmentProduction) {
		t.Fatalf("want the declaration in the platform's order, got %v", view.Serves)
	}

	// And one declared without it: the field is answered as an empty list
	// rather than left out, because "nobody" is the answer.
	h = newHarness(t, nil, fixtures()...)
	recorder = h.do(t, http.MethodPost, declarePath, `{"name": "`+declaredStage+`"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[environmentView](t, recorder); len(view.Serves) != 0 {
		t.Fatalf("an environment nobody opened serves nobody, got %v", view.Serves)
	}
}

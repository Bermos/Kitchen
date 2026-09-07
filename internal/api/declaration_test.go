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
	"fmt"
	"net/http"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What the institution declares about a project, declared *with* the project.
//
// The four fields were the settings route's alone, which meant every project
// began its life unclassified and undesignated and stayed that way until
// somebody went looking for a settings pane. A class nobody chose is the one
// thing a classification must never be, so the create takes them — and takes
// them the way the settings route does, with the same words and the same
// three states: absent says nothing, an empty string is the answer
// "unclassified" or "undesignated", and a word is the designation.

func TestCreatingAProjectCarriesTheDeclaration(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"ledger","repo":"acme/ledger","connection":"gh","registry":"registry",`+
			`"dataClass":"confidential","criticality":"important","rto":"4h","rpo":"30m"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "ledger", stored); err != nil {
		t.Fatal(err)
	}
	for _, field := range []struct {
		name string
		got  string
		want string
	}{
		{"dataClass", string(stored.Spec.DataClass), string(kitchenv1alpha1.DataClassConfidential)},
		{"criticality", string(stored.Spec.Criticality), string(kitchenv1alpha1.CriticalityImportant)},
		{"rto", string(stored.Spec.RTO), "4h"},
		{"rpo", string(stored.Spec.RPO), "30m"},
	} {
		if field.got != field.want {
			t.Errorf("%s is %q, want %q", field.name, field.got, field.want)
		}
	}
}

// The declaration a person makes when they decline to make one. An empty
// string is not the field being left out: it is somebody answering
// "unclassified", which reads back the same as an absent field on the object
// and differently in the record of what was asked.
func TestCreatingAProjectAcceptsADeliberatelyEmptyDeclaration(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"scratch","repo":"acme/scratch","connection":"gh","registry":"registry",`+
			`"dataClass":"","criticality":"","rto":"","rpo":""}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "scratch", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.DataClass.Classified() {
		t.Errorf("dataClass is %q, want unclassified", stored.Spec.DataClass)
	}
	if stored.Spec.Criticality.Designated() {
		t.Errorf("criticality is %q, want undesignated", stored.Spec.Criticality)
	}
}

// A project created without saying anything is exactly the project it was
// before this existed: unclassified, undesignated, and never defaulted to
// something the platform picked.
func TestCreatingAProjectWithoutADeclarationDefaultsNothing(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"quiet","repo":"acme/quiet","connection":"gh","registry":"registry"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "quiet", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.DataClass != "" || stored.Spec.Criticality != "" ||
		stored.Spec.RTO != "" || stored.Spec.RPO != "" {
		t.Fatalf("something was defaulted: %+v", stored.Spec)
	}
}

// The vocabulary is the settings route's, so a word neither of them knows is
// refused in the same sentence — before a project is written, since a project
// created and then corrected was misclassified in between.
func TestCreatingAProjectRefusesADeclarationItCannotRead(t *testing.T) {
	for _, testCase := range []struct{ name, body, says string }{
		{"a class nobody has", `"dataClass":"topSecret"`, "dataClass must be one of"},
		{"a designation nobody has", `"criticality":"quiteImportant"`, "criticality must be one of"},
		{"a tolerance in words", `"criticality":"critical","rto":"4 hours"`, "rto must be a duration"},
		{"and the same for the rpo", `"criticality":"critical","rpo":"a day"`, "rpo must be a duration"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, nil, fixtures()...)
			body := fmt.Sprintf(
				`{"name":"ledger","repo":"acme/ledger","connection":"gh","registry":"registry",%s}`,
				testCase.body)
			recorder := h.do(t, http.MethodPost, "/api/v1/projects", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), testCase.says) {
				t.Errorf("the refusal does not say %q: %s", testCase.says, recorder.Body.String())
			}
			if err := h.server.get(context.Background(), "ledger", &kitchenv1alpha1.Project{}); err == nil {
				t.Fatal("the project was created anyway")
			}
		})
	}
}

// The record of it, which is the half an auditor reads. A create that
// declared something is privileged and says what was declared; a create that
// declared nothing is the record it always was, with no keys for the four
// fields and no claim to privilege.
func TestTheCreateRecordCarriesWhatWasDeclared(t *testing.T) {
	project := &kitchenv1alpha1.Project{Spec: kitchenv1alpha1.ProjectSpec{
		Source:      kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{Repo: "acme/ledger"}},
		DataClass:   kitchenv1alpha1.DataClassConfidential,
		Criticality: kitchenv1alpha1.CriticalityImportant,
		RTO:         "4h",
	}}
	declared := createProjectRequest{
		Repo:        "acme/ledger",
		DataClass:   ptr.To(string(kitchenv1alpha1.DataClassConfidential)),
		Criticality: ptr.To(string(kitchenv1alpha1.CriticalityImportant)),
		RTO:         ptr.To("4h"),
	}

	details := projectCreationDetails(project, declared, "main", kitchenv1alpha1.ExposurePublic)
	if details["privileged"] != true {
		t.Fatalf("a declaration is a privileged record: %v", details)
	}
	if details["dataClass"] != string(kitchenv1alpha1.DataClassConfidential) ||
		details["criticality"] != string(kitchenv1alpha1.CriticalityImportant) ||
		details["rto"] != "4h" {
		t.Fatalf("the record does not carry the declaration: %v", details)
	}
	// A field the request did not carry is not in the record, and a create
	// replaces nothing — so there is no `previous…` key to read as a
	// designation that was taken away.
	for _, absent := range []string{"rpo", "previousCriticality", "previousRTO"} {
		if _, present := details[absent]; present {
			t.Errorf("%q is in the record of a create: %v", absent, details)
		}
	}

	quiet := projectCreationDetails(project, createProjectRequest{Repo: "acme/ledger"}, "main",
		kitchenv1alpha1.ExposurePublic)
	if _, present := quiet["privileged"]; present {
		t.Errorf("a create that declared nothing is not a privileged record: %v", quiet)
	}
	for _, absent := range []string{"dataClass", "criticality", "rto", "rpo"} {
		if _, present := quiet[absent]; present {
			t.Errorf("%q was recorded for a create that never mentioned it: %v", absent, quiet)
		}
	}
}

// A project with no repository declares the same four things: what an
// application handles is a fact about the application, not about who built it.
func TestCreatingAVendoredProjectCarriesTheDeclarationToo(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"assistant","image":{"repository":"ghcr.io/acme/thing","tag":"1.2.3"},`+
			`"dataClass":"internal","criticality":"nonCritical"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "assistant", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.DataClass != kitchenv1alpha1.DataClassInternal {
		t.Errorf("dataClass is %q, want %q", stored.Spec.DataClass, kitchenv1alpha1.DataClassInternal)
	}
	if stored.Spec.Criticality != kitchenv1alpha1.CriticalityNonCritical {
		t.Errorf("criticality is %q, want %q", stored.Spec.Criticality, kitchenv1alpha1.CriticalityNonCritical)
	}
}

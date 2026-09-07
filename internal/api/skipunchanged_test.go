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
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/controller"
)

// Skipping a build whose source tree did not change is a project's own answer
// (#500), so it rides the project write like every other build setting — and
// it has to read back, because a switch that answered "off" after being turned
// on would be a setting nobody could trust.
func TestSkipUnchangedIsOffUntilTheProjectAsksForIt(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/projects/shop", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// Spelled out rather than omitted: `false` is the default, and a client
	// that could not tell it from an absent field would draw the switch
	// wrong on every project that never touched it.
	if !strings.Contains(recorder.Body.String(), `"skipUnchanged":false`) {
		t.Fatalf("a project that never asked should answer false: %s", recorder.Body.String())
	}

	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"skipUnchanged":true}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Spec.Build.SkipUnchanged {
		t.Fatal("the setting did not stick")
	}

	recorder = h.do(t, http.MethodGet, "/api/v1/projects/shop", "")
	if !strings.Contains(recorder.Body.String(), `"skipUnchanged":true`) {
		t.Fatalf("the project does not answer its own setting: %s", recorder.Body.String())
	}

	// And off again, which a plain bool has to be able to express: the
	// project that turns it back on after moving shared code has to be able
	// to turn it off first.
	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"skipUnchanged":false}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Build.SkipUnchanged {
		t.Fatal("the setting could not be turned back off")
	}
}

// A rebuild is what somebody reaches for when the derivation is wrong, so it
// may not be answered by the derivation. The API's half of that is the
// annotation naming who asked; the operator reads it and never skips such a
// build.
func TestARebuildRecordsWhoAskedSoItIsNeverSkipped(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"skipUnchanged":true}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/shop/builds", "")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	requested := decode[buildView](t, recorder)

	stored := &kitchenv1alpha1.Build{}
	if err := h.server.get(context.Background(), requested.Name, stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Annotations[audit.RequestedByAnnotation]; got != testCaller {
		t.Fatalf("a rebuild must name who asked, got %q", got)
	}
}

// A skipped build is over. Cancelling one is the same 409 a succeeded build
// answers with, rather than a cancellation of something that never started.
func TestASkippedBuildCannotBeCancelled(t *testing.T) {
	objects := fixtures()
	h := newHarness(t, nil, objects...)

	build := &kitchenv1alpha1.Build{}
	if err := h.server.get(context.Background(), testBuild, build); err != nil {
		t.Fatal(err)
	}
	build.Status.Phase = kitchenv1alpha1.BuildSkipped
	build.Status.SourceTree = &kitchenv1alpha1.SourceTreeStatus{
		Object: "6f3c1a9d0b7e", Path: "services/checkout", MatchedBuild: "shop-bld-000000000000",
	}
	if err := h.server.Client.Status().Update(context.Background(), build); err != nil {
		t.Fatal(err)
	}

	recorder := h.do(t, http.MethodPost, "/api/v1/builds/"+testBuild+"/cancel", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}

	// And the build reads as what it is: the tree it matched and the build it
	// matched it against, which is what makes the skip a statement about the
	// source rather than a build that did nothing.
	recorder = h.do(t, http.MethodGet, "/api/v1/builds/"+testBuild, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[buildView](t, recorder)
	if view.Phase != string(kitchenv1alpha1.BuildSkipped) {
		t.Fatalf("phase %q, want Skipped", view.Phase)
	}
	if view.SourceTree == nil || view.SourceTree.MatchedBuild != "shop-bld-000000000000" {
		t.Fatalf("the build does not say what it matched: %s", recorder.Body.String())
	}
	if view.SourceTree.Path != "services/checkout" {
		t.Fatalf("the build does not say where: %+v", view.SourceTree)
	}
}

// The skip is not a fault, and the API is what says so — a dashboard that
// coloured the condition from its status alone would draw a project doing
// exactly what it was asked to do as a broken one (#436, #500).
func TestASkippedBuildsConditionIsNotAFault(t *testing.T) {
	severity := conditionSeverityOf(metav1.Condition{
		Type:   controller.ConditionReady,
		Status: metav1.ConditionFalse,
		Reason: controller.ReasonSourceUnchanged,
	})
	if severity != severityInfo {
		t.Fatalf("severity %q, want %q", severity, severityInfo)
	}
}

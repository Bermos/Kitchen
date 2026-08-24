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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/retention"
)

// The retention surface, against the acceptance criteria it carries:
//
//   - retention is configurable per class (here: through the route, one class
//     at a time, without disturbing the rest);
//   - the audit retention cannot be set below the documented minimum without
//     an explicit override — and the refusal has to name the way past it,
//     because a floor whose refusal says only "no" is a floor somebody
//     patches out of the code;
//   - using the override is itself an audit record.

// retentionOf reads the route's answer as a map of class to days, which is
// what almost every assertion here is about.
func retentionOf(t *testing.T, h *harness) (map[string]retentionClassView, retentionView) {
	t.Helper()
	recorder := h.do(t, http.MethodGet, "/api/v1/platform/retention", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /platform/retention: %d %s", recorder.Code, recorder.Body.String())
	}
	answer := decode[retentionView](t, recorder)
	byClass := map[string]retentionClassView{}
	for _, entry := range answer.Classes {
		byClass[entry.Class] = entry
	}
	return byClass, answer
}

func TestRetentionAnswersEveryClassWithWhereItsNumberCameFrom(t *testing.T) {
	h := newHarness(t, nil)
	classes, answer := retentionOf(t, h)

	if len(classes) != len(retention.Definitions()) {
		t.Fatalf("the route answers %d classes, want %d", len(classes), len(retention.Definitions()))
	}
	for _, definition := range retention.Definitions() {
		entry, ok := classes[string(definition.Class)]
		if !ok {
			t.Errorf("the class %s is missing from the answer", definition.Class)
			continue
		}
		if entry.Label == "" || entry.Description == "" {
			t.Errorf("%s is served as %q/%q, which a screen cannot render",
				entry.Class, entry.Label, entry.Description)
		}
		if entry.Source == "" {
			t.Errorf("%s does not say where its number came from, so nobody can tell "+
				"whether anybody chose it", entry.Class)
		}
	}
	if answer.AuditFloorDays != retention.AuditFloorDays {
		t.Errorf("the floor is served as %d, want %d — a client that hard-coded it would be a "+
			"second copy of this number", answer.AuditFloorDays, retention.AuditFloorDays)
	}
}

func TestSettingOneClassLeavesTheRestInheriting(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodPatch, "/api/v1/platform/retention", `{"buildLogs": 180}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", recorder.Code, recorder.Body.String())
	}

	classes, _ := retentionOf(t, h)
	if got := classes["buildLogs"]; got.Days != 180 || got.Source != retention.SourceModel {
		t.Errorf("build logs read back as %d days from %q, want 180 from %q",
			got.Days, got.Source, retention.SourceModel)
	}
	if got := classes["containerLogs"]; got.Source == retention.SourceModel {
		t.Error("setting build logs also pinned the container logs off their inherited value")
	}
}

func TestARetentionUnderADayIsRefusedRatherThanInterpreted(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodPatch, "/api/v1/platform/retention", `{"flows": 0}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a zero retention answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "keep nothing") {
		t.Errorf("the refusal does not say why zero is not a value: %s", recorder.Body.String())
	}
}

// TestTheAuditFloorIsRefusedAndTheRefusalNamesTheWayPast is the fourth
// acceptance criterion at the API.
func TestTheAuditFloorIsRefusedAndTheRefusalNamesTheWayPast(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodPatch, "/api/v1/platform/retention", `{"audit": 60}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("60 days of audit retention answered %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{"90", "auditFloorOverride", "reason", "approver"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not mention %q: %s", want, body)
		}
	}

	// And the object was not written: a refused write must leave nothing
	// behind, or the next reconcile enforces what the API said no to.
	classes, _ := retentionOf(t, h)
	if classes["audit"].Days < retention.AuditFloorDays {
		t.Errorf("a refused write landed anyway: audit reads back as %d days", classes["audit"].Days)
	}
}

func TestTheOverrideIsWhatMakesASmallerAuditRetentionAcceptable(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodPatch, "/api/v1/platform/retention",
		`{"audit": 60, "auditFloorOverride": {"reason": "demonstration cluster; no production data at all",`+
			`"approvedBy": "cto@example.com"}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("an explicit override answered %d: %s", recorder.Code, recorder.Body.String())
	}

	classes, answer := retentionOf(t, h)
	if classes["audit"].Days != 60 {
		t.Errorf("audit reads back as %d days, want 60", classes["audit"].Days)
	}
	if !answer.AuditFloorOverridden {
		t.Error("the answer does not say the floor is overridden, so a reader has to work it out")
	}
	if answer.AuditFloorOverride == nil || answer.AuditFloorOverride.ApprovedBy != "cto@example.com" {
		t.Errorf("the override is not read back: %+v — an override nobody can see is a setting",
			answer.AuditFloorOverride)
	}
}

// TestAThinReasonIsRefused. "n/a" is not an answer, and a field that accepts
// it is a field that will contain it.
func TestAThinReasonIsRefused(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodPatch, "/api/v1/platform/retention",
		`{"audit": 60, "auditFloorOverride": {"reason": "n/a", "approvedBy": "cto@example.com"}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a two-word reason answered %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = h.do(t, http.MethodPatch, "/api/v1/platform/retention",
		`{"audit": 60, "auditFloorOverride": {"reason": "demonstration cluster; no production data at all"}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an override with nobody's name on it answered %d: %s", recorder.Code, recorder.Body.String())
	}
}

// TestClearingTheOverrideUnderTheFloorIsRefused: the object would be left in a
// state admission will not accept, and answering here says which field to fix.
func TestClearingTheOverrideUnderTheFloorIsRefused(t *testing.T) {
	h := newHarness(t, nil)

	if recorder := h.do(t, http.MethodPatch, "/api/v1/platform/retention",
		`{"audit": 60, "auditFloorOverride": {"reason": "demonstration cluster; no production data at all",`+
			`"approvedBy": "cto@example.com"}}`); recorder.Code != http.StatusOK {
		t.Fatalf("setting up the override: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodPatch, "/api/v1/platform/retention",
		`{"clearAuditFloorOverride": true}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("clearing the override under the floor answered %d: %s", recorder.Code, recorder.Body.String())
	}
}

// TestTheStatusIsWhatTheMeasuredHalfComesFrom. The route answers the
// configured half from the spec and the measured half from the status the
// sweep publishes; a reader must be able to tell "not measured" from "empty".
func TestTheStatusIsWhatTheMeasuredHalfComesFrom(t *testing.T) {
	h := newHarness(t, nil)

	before, _ := retentionOf(t, h)
	if before["flows"].Enforced {
		t.Error("a class reads as enforced before any sweep has measured it")
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	oldest := metav1.NewTime(metav1.Now().Add(-72 * 60 * 60 * 1e9))
	kitchen.Status.Retention = &kitchenv1alpha1.RetentionStatus{
		LastSweep: ptr.To(metav1.Now()),
		Classes: []kitchenv1alpha1.RetentionClassStatus{{
			Class: "flows", Days: 30, Enforced: true, Rows: 1234, Oldest: &oldest, Expired: 7,
		}},
	}
	// A plain update rather than a status one: this harness's fake client
	// registers no status subresource for the Kitchen, and what is being
	// tested is the read.
	if err := h.server.Client.Update(context.Background(), kitchen); err != nil {
		t.Fatal(err)
	}

	after, answer := retentionOf(t, h)
	entry := after["flows"]
	if !entry.Enforced || entry.Rows != 1234 || entry.Expired != 7 || entry.Oldest == nil {
		t.Errorf("the measured half did not reach the answer: %+v", entry)
	}
	if answer.LastSweep == nil {
		t.Error("the answer is undated, so nothing says when the measurement was taken")
	}
}

// TestRetentionIsTheOperatorsAlone. It is platform configuration, like every
// other setting on the singleton.
func TestRetentionIsTheOperatorsAlone(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)

	for _, route := range []struct {
		method string
		body   string
	}{
		{http.MethodGet, ""},
		{http.MethodPatch, `{"flows": 7}`},
	} {
		recorder := h.do(t, route.method, "/api/v1/platform/retention", route.body)
		if recorder.Code != http.StatusForbidden && recorder.Code != http.StatusNotFound {
			t.Errorf("%s /platform/retention answered a project admin %d: %s",
				route.method, recorder.Code, recorder.Body.String())
		}
	}
}

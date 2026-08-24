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

package retention

import (
	"strings"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The retention model, against the first acceptance criterion: retention is
// configurable per class. The rest of that criterion — that it is *enforced* —
// is in internal/clickhouse and internal/controller, because this package
// deliberately touches no store.

// TestAnEmptyBlockReadsExactlyLikeTheInstallationThatPredatesIt is the
// upgrade contract. An installation that has never heard of spec.retention
// must keep the retentions it already had, or a minor version bump silently
// re-dates every table it holds.
func TestAnEmptyBlockReadsExactlyLikeTheInstallationThatPredatesIt(t *testing.T) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	kitchen.Spec.Observability.ClickHouse.RetentionDays = 14
	kitchen.Spec.Compliance.Audit.RetentionDays = 400

	model := Resolve(kitchen)
	for _, class := range model.Classes() {
		want, source := int32(14), SourceTelemetry
		if class == ClassAudit {
			want, source = 400, SourceAudit
		}
		if got := model.Days(class); got != want {
			t.Errorf("%s is kept %d days, want %d — an upgraded installation's retention moved", class, got, want)
		}
		if got := model.Source(class); got != source {
			t.Errorf("%s says it came from %q, want %q", class, got, source)
		}
	}
}

// TestAnUnsetSingletonFallsBackToTheCompiledInDefaults covers a Kitchen the
// API server never defaulted — created by a test, or by an older chart.
func TestAnUnsetSingletonFallsBackToTheCompiledInDefaults(t *testing.T) {
	model := Resolve(&kitchenv1alpha1.Kitchen{})
	if got := model.Days(ClassContainerLogs); got != DefaultTelemetryDays {
		t.Errorf("container logs default to %d days, want %d", got, DefaultTelemetryDays)
	}
	if got := model.Days(ClassAudit); got != DefaultAuditDays {
		t.Errorf("audit defaults to %d days, want %d", got, DefaultAuditDays)
	}
}

// TestEachClassIsConfigurableOnItsOwn is the criterion itself: nine classes,
// nine numbers, and setting one does not move another.
func TestEachClassIsConfigurableOnItsOwn(t *testing.T) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	kitchen.Spec.Observability.ClickHouse.RetentionDays = 30
	kitchen.Spec.Compliance.Audit.RetentionDays = 365

	for i, class := range Definitions() {
		days := int32(100 + i)
		spec := kitchenv1alpha1.RetentionSpec{}
		switch class.Class {
		case ClassContainerLogs:
			spec.ContainerLogs = ptr.To(days)
		case ClassBuildLogs:
			spec.BuildLogs = ptr.To(days)
		case ClassFlows:
			spec.Flows = ptr.To(days)
		case ClassMetrics:
			spec.Metrics = ptr.To(days)
		case ClassTraces:
			spec.Traces = ptr.To(days)
		case ClassRequests:
			spec.Requests = ptr.To(days)
		case ClassClusterEvents:
			spec.ClusterEvents = ptr.To(days)
		case ClassActivity:
			spec.Activity = ptr.To(days)
		case ClassAudit:
			spec.Audit = ptr.To(days)
		default:
			t.Fatalf("the class %s has no field in RetentionSpec, so nothing can configure it", class.Class)
		}

		kitchen.Spec.Retention = spec
		model := Resolve(kitchen)
		if got := model.Days(class.Class); got != days {
			t.Errorf("%s was set to %d and reads back %d", class.Class, days, got)
		}
		if got := model.Source(class.Class); got != SourceModel {
			t.Errorf("%s was set explicitly and reports source %q", class.Class, got)
		}
		for _, other := range model.Classes() {
			if other == class.Class {
				continue
			}
			if model.Source(other) == SourceModel {
				t.Errorf("setting %s also moved %s off its inherited value", class.Class, other)
			}
		}
	}
}

// TestTheAuditFloorIsNinetyDaysAndTheOverrideIsTheWayPast covers the fourth
// acceptance criterion at the level this package owns: the number, and the
// rule that an override is what makes a smaller one acceptable.
func TestTheAuditFloorIsNinetyDaysAndTheOverrideIsTheWayPast(t *testing.T) {
	if AuditFloorDays != 90 {
		t.Fatalf("the documented audit floor is %d days, and docs/COMPLIANCE.md says 90", AuditFloorDays)
	}
	if refusal := ValidateAudit(90, nil); refusal != "" {
		t.Errorf("90 days is the floor and was refused: %s", refusal)
	}
	if refusal := ValidateAudit(89, nil); refusal == "" {
		t.Error("89 days is under the floor and was accepted with no override")
	}
	override := &kitchenv1alpha1.RetentionOverrideSpec{
		Reason:     "this is a demonstration cluster and holds no production data at all",
		ApprovedBy: "cto@example.com",
	}
	if refusal := ValidateAudit(30, override); refusal != "" {
		t.Errorf("an explicit override was refused: %s", refusal)
	}
}

// TestTheRefusalSaysWhatToDoAboutIt: a floor that refuses without naming the
// way past it is a floor somebody patches out of the code.
func TestTheRefusalSaysWhatToDoAboutIt(t *testing.T) {
	refusal := ValidateAudit(30, nil)
	for _, want := range []string{"90", "auditFloorOverride", "reason", "approver"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal does not mention %q: %s", want, refusal)
		}
	}
}

// TestBelowTheFloorIsVisibleOnTheModel is what the status and the API read to
// answer "is this installation keeping less evidence than the platform
// recommends".
func TestBelowTheFloorIsVisibleOnTheModel(t *testing.T) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	kitchen.Spec.Retention.Audit = ptr.To(int32(30))
	if !Resolve(kitchen).AuditBelowFloor() {
		t.Error("30 days of audit retention does not read as below the floor")
	}
	kitchen.Spec.Retention.Audit = ptr.To(int32(365))
	if Resolve(kitchen).AuditBelowFloor() {
		t.Error("365 days of audit retention reads as below the floor")
	}
}

// TestTheHorizonIsTheRuleTheSweepEnforces. It is computed from a passed-in
// clock so that a sweep cannot report a horizon it did not measure against.
func TestTheHorizonIsTheRuleTheSweepEnforces(t *testing.T) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	kitchen.Spec.Retention.Flows = ptr.To(int32(7))
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	horizon := Resolve(kitchen).Horizon(ClassFlows, now)
	if want := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC); !horizon.Equal(want) {
		t.Errorf("a 7-day flow retention cuts at %s, want %s", horizon, want)
	}
}

// TestAnUnknownClassKeepsNothingAndDeletesNothing. A typo must never become a
// deletion, so the answer for a class nobody defined is zero days — which
// every caller reads as "do not enforce anything" — and a zero horizon.
func TestAnUnknownClassKeepsNothingAndDeletesNothing(t *testing.T) {
	model := Resolve(&kitchenv1alpha1.Kitchen{})
	if days := model.Days(Class("containerlogs")); days != 0 {
		t.Errorf("a misspelled class answered %d days", days)
	}
	if horizon := model.Horizon(Class("containerlogs"), time.Now()); !horizon.IsZero() {
		t.Errorf("a misspelled class produced the horizon %s, which something would sweep to", horizon)
	}
}

// TestEveryClassHasADefinition keeps the register and the resolver from
// drifting: a class the model resolves but nothing describes would reach the
// API with an empty label.
func TestEveryClassHasADefinition(t *testing.T) {
	model := Resolve(&kitchenv1alpha1.Kitchen{})
	for _, class := range model.Classes() {
		definition, ok := DefinitionFor(class)
		if !ok {
			t.Errorf("the class %s has no definition", class)
			continue
		}
		if definition.Label == "" || definition.Description == "" {
			t.Errorf("the class %s is described as %q/%q", class, definition.Label, definition.Description)
		}
	}
}

// TestTheAuditClassIsNeverSweepable is the rule that keeps the deletion
// evidence from being the thing that gets deleted: a sweeper that could drop
// audit partitions could drop the record of its own deletions.
func TestTheAuditClassIsNeverSweepable(t *testing.T) {
	definition, ok := DefinitionFor(ClassAudit)
	if !ok {
		t.Fatal("the audit class has no definition")
	}
	if definition.Sweepable {
		t.Error("the retention sweep is allowed to delete audit rows, so it can delete its own evidence")
	}
}

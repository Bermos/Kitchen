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

package v1alpha1

import (
	"reflect"
	"testing"
)

// The posture is per project *and* per workload (#399): the unit declares one
// and a workload writes over as much of it as it needs to.
//
// The case these exist for is #393's own application — three `node:22-slim`
// images ending `USER node` and one distroless ending `USER nonroot`, where
// one uid across the four is luck rather than design.

func TestAWorkloadThatDeclaresNothingRunsUnderTheUnits(t *testing.T) {
	unit := &SecuritySpec{RunAsNonRoot: true, RunAsUser: 1000, DropCapabilities: []string{"ALL"}}

	resolved := ResolveSecurity(unit, nil)
	if !reflect.DeepEqual(resolved, unit) {
		t.Fatalf("a workload with no declaration should run under the unit's: %+v", resolved)
	}
	if overrides := SecurityOverrides(nil); overrides != nil {
		t.Fatalf("nothing was overridden, so nothing should be named: %v", overrides)
	}
}

func TestAWorkloadsOwnFieldsWinAndTheRestAreInherited(t *testing.T) {
	unit := &SecuritySpec{
		RunAsNonRoot:           true,
		RunAsUser:              1000,
		RunAsGroup:             1000,
		ReadOnlyRootFilesystem: true,
		DropCapabilities:       []string{"ALL"},
	}
	// The distroless workload: its own user, and nothing else to say.
	workload := &SecuritySpec{RunAsUser: 65532}

	resolved := ResolveSecurity(unit, workload)
	if resolved.RunAsUser != 65532 {
		t.Fatalf("the workload's own uid did not win: %+v", resolved)
	}
	for _, inherited := range []struct {
		what string
		got  bool
	}{
		{"runAsNonRoot", resolved.RunAsNonRoot},
		{"readOnlyRootFilesystem", resolved.ReadOnlyRootFilesystem},
		{"dropCapabilities", len(resolved.DropCapabilities) == 1},
		{"runAsGroup", resolved.RunAsGroup == 1000},
	} {
		if !inherited.got {
			t.Fatalf("%s should have been inherited from the unit: %+v", inherited.what, resolved)
		}
	}
	if overrides := SecurityOverrides(workload); !reflect.DeepEqual(overrides, []string{"runAsUser"}) {
		t.Fatalf("the marker should name the one field the workload declared, got %v", overrides)
	}

	// The unit is not written to. Two workloads resolving in one reconcile
	// would otherwise accumulate each other's answers.
	if unit.RunAsUser != 1000 {
		t.Fatalf("resolving a workload changed the unit: %+v", unit)
	}
}

// A withdrawn override leaves the workload the way a withdrawn posture does:
// back to the unit's, never to yesterday's resolution. The block is written
// whole every reconcile, so this is the whole of what "withdrawn" costs.
func TestAWithdrawnOverrideLeavesTheUnitsPosture(t *testing.T) {
	unit := &SecuritySpec{RunAsNonRoot: true, RunAsUser: 1000}
	if resolved := ResolveSecurity(unit, &SecuritySpec{RunAsUser: 65532}); resolved.RunAsUser != 65532 {
		t.Fatalf("the override did not take: %+v", resolved)
	}
	resolved := ResolveSecurity(unit, nil)
	if resolved.RunAsUser != 1000 || !resolved.RunAsNonRoot {
		t.Fatalf("withdrawing the override should restore the unit's posture: %+v", resolved)
	}
}

// A unit that declares nothing and a workload that declares something: the
// workload's is the whole posture, which is what a project hardening one of
// four workloads writes.
func TestAWorkloadCanDeclareAPostureTheUnitDoesNot(t *testing.T) {
	workload := &SecuritySpec{ReadOnlyRootFilesystem: true, FSGroup: 2000}
	resolved := ResolveSecurity(nil, workload)
	if !resolved.ReadOnlyRootFilesystem || resolved.FSGroup != 2000 {
		t.Fatalf("the workload's own posture should stand alone: %+v", resolved)
	}
	if overrides := SecurityOverrides(workload); !reflect.DeepEqual(
		overrides, []string{"fsGroup", "readOnlyRootFilesystem"}) {
		t.Fatalf("both declared fields should be marked, got %v", overrides)
	}
}

// Every field merges, and the markers name every one of them — the check that
// the two halves of the answer are computed from one list rather than two.
func TestEveryFieldOfThePostureMergesAndIsMarked(t *testing.T) {
	workload := &SecuritySpec{
		RunAsNonRoot:             true,
		RunAsUser:                65532,
		RunAsGroup:               65532,
		FSGroup:                  2000,
		FSGroupChangePolicy:      FSGroupChangeOnRootMismatch,
		ReadOnlyRootFilesystem:   true,
		AllowPrivilegeEscalation: true,
		DropCapabilities:         []string{"NET_RAW"},
	}
	resolved := ResolveSecurity(&SecuritySpec{RunAsUser: 1000}, workload)
	if !reflect.DeepEqual(resolved, workload) {
		t.Fatalf("a workload that declares everything should inherit nothing: %+v", resolved)
	}
	want := []string{
		"runAsNonRoot", "runAsUser", "runAsGroup", "fsGroup",
		"fsGroupChangePolicy", "readOnlyRootFilesystem", "allowPrivilegeEscalation", "dropCapabilities",
	}
	if overrides := SecurityOverrides(workload); !reflect.DeepEqual(overrides, want) {
		t.Fatalf("every field should be marked as the workload's, got %v", overrides)
	}
	if len(want) != reflect.TypeFor[SecuritySpec]().NumField() {
		t.Fatalf("the posture has %d fields and the merge walks %d — a field added to SecuritySpec and not "+
			"to securityFields is a field a workload silently cannot override",
			reflect.TypeFor[SecuritySpec]().NumField(), len(want))
	}
}

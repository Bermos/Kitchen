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
	"slices"
	"testing"
	"time"
)

// The ordering, the duration spelling, and the resolution rule. Everything
// above them — the API's refusals, the forward map's filter, the signal's
// threshold — is one of these three read somewhere else, so they are pinned
// here.

func TestUndesignatedIsNeverAtLeastAnything(t *testing.T) {
	// The distinction the whole field turns on: nobody has said is not the
	// same answer as somebody said no.
	if Criticality("").Designated() {
		t.Fatal("an empty criticality reports itself as designated")
	}
	if !CriticalityNonCritical.Designated() {
		t.Fatal("nonCritical is a designation somebody made, not an absence")
	}
	if Criticality("").AtLeast(CriticalityNonCritical) {
		t.Fatal("an undesignated project answers a nonCritical-and-worse filter")
	}
	if !CriticalityNonCritical.AtLeast("") {
		t.Fatal("a designated project must answer an unfiltered query")
	}
}

func TestCriticalityOrdersNonCriticalBelowCritical(t *testing.T) {
	for _, tc := range []struct {
		have, want Criticality
		atLeast    bool
	}{
		{CriticalityCritical, CriticalityImportant, true},
		{CriticalityCritical, CriticalityCritical, true},
		{CriticalityImportant, CriticalityCritical, false},
		{CriticalityImportant, CriticalityNonCritical, true},
		{CriticalityNonCritical, CriticalityImportant, false},
	} {
		if got := tc.have.AtLeast(tc.want); got != tc.atLeast {
			t.Errorf("%s.AtLeast(%s) = %v, want %v", tc.have, tc.want, got, tc.atLeast)
		}
	}
	if got := Criticalities(); !slices.Equal(got,
		[]Criticality{CriticalityNonCritical, CriticalityImportant, CriticalityCritical}) {
		t.Errorf("the vocabulary is not in ascending order: %v", got)
	}
}

func TestAToleranceIsAWholeNumberOfHoursAndMinutes(t *testing.T) {
	for spelling, want := range map[Tolerance]time.Duration{
		"4h":      4 * time.Hour,
		"30m":     30 * time.Minute,
		"1h30m":   90 * time.Minute,
		"0m":      0,
		"72h":     72 * time.Hour,
		"1h0m":    time.Hour,
		"":        -1, // undeclared
		"250ms":   -1, // refused at admission; unreadable here
		"-5m":     -1,
		"4 hours": -1,
	} {
		got, ok := spelling.Duration()
		if want < 0 {
			if ok {
				t.Errorf("%q parsed as %s and should not have", spelling, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%q parsed as (%s, %v), want %s", spelling, got, ok, want)
		}
	}
	if Tolerance("").Declared() {
		t.Fatal("an empty tolerance reports itself as declared")
	}
	if !Tolerance("0m").Declared() {
		t.Fatal("0m is a tolerance somebody set — no downtime at all — not an absence")
	}
}

func TestAPreviewNeverInheritsItsProjectsCriticality(t *testing.T) {
	// The rule that separates criticality from a data class: consequence is
	// not contained, so a critical project's preview is not a critical
	// function and must not alert like one.
	project := &Project{Spec: ProjectSpec{
		Criticality: CriticalityCritical, RTO: "1h", RPO: "5m",
	}}
	preview := &Environment{Spec: EnvironmentSpec{Type: EnvironmentPreview}}

	resolved := EffectiveContinuity(project, preview)
	if resolved.Designated() {
		t.Fatalf("a preview inherited a designation: %+v", resolved)
	}
}

func TestAProductionEnvironmentInheritsAndSaysSo(t *testing.T) {
	project := &Project{Spec: ProjectSpec{
		Criticality: CriticalityCritical, RTO: "1h", RPO: "5m",
	}}
	production := &Environment{Spec: EnvironmentSpec{Type: EnvironmentProduction}}

	resolved := EffectiveContinuity(project, production)
	if resolved.Criticality != CriticalityCritical || resolved.RTO != "1h" || resolved.RPO != "5m" {
		t.Fatalf("production did not read its project's designation: %+v", resolved)
	}
	if !slices.Equal(resolved.Inherited, []string{"criticality", "rto", "rpo"}) {
		t.Fatalf("an inherited value was shown as a declared one: %v", resolved.Inherited)
	}
}

func TestAnEnvironmentsOwnDesignationWinsAndIsNotCapped(t *testing.T) {
	// No ceiling, in either direction: the institution designates the
	// environment, and a project's designation is not a bound on it.
	project := &Project{Spec: ProjectSpec{Criticality: CriticalityNonCritical, RTO: "72h"}}
	production := &Environment{Spec: EnvironmentSpec{
		Type: EnvironmentProduction, Criticality: CriticalityCritical, RTO: "15m",
	}}

	resolved := EffectiveContinuity(project, production)
	if resolved.Criticality != CriticalityCritical || resolved.RTO != "15m" {
		t.Fatalf("the environment's own designation was overridden: %+v", resolved)
	}
	if len(resolved.Inherited) != 0 {
		t.Fatalf("nothing was inherited, but the answer says %v", resolved.Inherited)
	}

	// A field the environment leaves out still falls back, and only that one.
	production.Spec.RTO = ""
	resolved = EffectiveContinuity(project, production)
	if resolved.RTO != "72h" || !slices.Equal(resolved.Inherited, []string{"rto"}) {
		t.Fatalf("per-field fallback is wrong: %+v", resolved)
	}
}

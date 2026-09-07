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

package signals

import (
	"strings"
	"testing"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The load-bearing test of this file. `balanced` is not a preset among three:
// it is what every installation that predates the policy screen is already
// running, so a value that drifts from the constant beside it changes what
// those installations see without anybody asking them.
func TestBalancedIsTheConstantsExactly(t *testing.T) {
	balanced := DefaultPolicy()
	if balanced.CorrelatedProjects != CorrelatedProjects {
		t.Errorf("correlatedProjects = %d, want the constant %d",
			balanced.CorrelatedProjects, CorrelatedProjects)
	}
	if balanced.CorrelationWindow != RecentWindow {
		t.Errorf("correlationWindow = %s, want the window the detectors reused, %s",
			balanced.CorrelationWindow, RecentWindow)
	}
	if balanced.EscalationWindow != EscalationWindow {
		t.Errorf("escalationWindow = %s, want the constant %s",
			balanced.EscalationWindow, EscalationWindow)
	}
	if balanced.UntendedMultiple != UntendedMultiple {
		t.Errorf("untendedMultiple = %d, want the constant %d",
			balanced.UntendedMultiple, UntendedMultiple)
	}
	if balanced.MaxSilence != MaxSilence {
		t.Errorf("maxSilence = %s, want the constant %s", balanced.MaxSilence, MaxSilence)
	}
	if balanced.UntendedAfter() != UntendedAfter {
		t.Errorf("untendedAfter = %s, want the constant %s", balanced.UntendedAfter(), UntendedAfter)
	}
	if !balanced.Paging {
		t.Error("the balanced preset does not page, which is not what installations have today")
	}
}

// An installation that has configured nothing is on `balanced`, whatever the
// singleton says about anything else.
func TestAnUnconfiguredInstallationIsBalanced(t *testing.T) {
	if policy := PolicyFrom(nil); policy.Preset != PresetBalanced {
		t.Fatalf("a nil singleton resolves to %q", policy.Preset)
	}
	kitchen := &kitchenv1alpha1.Kitchen{}
	if policy := PolicyFrom(kitchen); policy != DefaultPolicy() {
		t.Fatalf("an empty spec resolves to %+v, want the default", policy)
	}
}

// A preset supplies the base and a set field overrides that one number. Both
// halves matter: without the first the screen's presets do nothing, and
// without the second the numbers underneath them are decoration.
func TestAPresetIsABaseAndAFieldOverridesOneNumber(t *testing.T) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	kitchen.Spec.Observability.Signals.Policy = kitchenv1alpha1.SignalPolicySpec{
		Preset:             string(PresetHomelab),
		CorrelatedProjects: ptr(int32(4)),
	}
	policy := PolicyFrom(kitchen)

	homelab, _ := Preset(PresetHomelab)
	if policy.CorrelatedProjects != 4 {
		t.Errorf("the override did not take: correlatedProjects = %d", policy.CorrelatedProjects)
	}
	if policy.EscalationWindow != homelab.EscalationWindow {
		t.Errorf("the base did not take: escalationWindow = %s, want %s",
			policy.EscalationWindow, homelab.EscalationWindow)
	}
	if policy.Matches(PresetHomelab) {
		t.Error("a policy with a number moved still claims to be the preset exactly")
	}
}

// Homelab turns paging off for the whole installation, and the description
// says only that.
//
// #472's decision 2 argues it should be "off *by default*", with a project
// free to turn it back on for its own rows — but that override is #519 and is
// not built, so a description promising it would send the reader looking for a
// switch this platform does not have. The test is here to keep the copy honest
// in that direction: it fails if the served sentence starts offering the
// project-scoped control again before there is one.
func TestHomelabTurnsPagingOffForTheWholeInstallation(t *testing.T) {
	homelab, ok := Preset(PresetHomelab)
	if !ok {
		t.Fatal("there is no homelab preset")
	}
	if homelab.Paging {
		t.Error("the homelab preset pages")
	}
	if homelab.CorrelatedProjects >= CorrelatedProjects {
		t.Errorf("homelab keeps a correlation threshold of %d, which a small estate never reaches",
			homelab.CorrelatedProjects)
	}
	description := PresetDescriptions[PresetHomelab]
	for _, promise := range []string{"by default", "turn it back on", "tighten"} {
		if strings.Contains(description, promise) {
			t.Errorf("the homelab description offers a per-project override that does not exist "+
				"(#519): %q", description)
		}
	}
	if !strings.Contains(description, "pages") {
		t.Errorf("the homelab description does not say what it does to paging: %q", description)
	}
}

// Paging off holds a page down to a ticket, and does nothing else. A policy
// that could raise a tier, or lower one past ticket, would be the
// configurable-rules door #471's decision 4 keeps shut.
func TestPagingOffLowersOnlyPages(t *testing.T) {
	homelab, _ := Preset(PresetHomelab)
	for _, test := range []struct {
		tier Tier
		want Tier
	}{
		{TierPage, TierTicket},
		{TierTicket, TierTicket},
		{TierLog, TierLog},
	} {
		if got := homelab.Deliver(test.tier); got != test.want {
			t.Errorf("with paging off, %s is delivered as %s, want %s", test.tier, got, test.want)
		}
	}
	balanced := DefaultPolicy()
	if got := balanced.Deliver(TierPage); got != TierPage {
		t.Errorf("with paging on, a page is delivered as %s", got)
	}
}

// A hand-built Policy — every test in this package, and any caller that
// assembled one — is repaired rather than obeyed, because a correlation
// threshold of zero would call every pair of failures an incident.
func TestAnUnresolvedPolicyIsTheDefault(t *testing.T) {
	if got := (Policy{}).Normalised(); got != DefaultPolicy() {
		t.Fatalf("a zero policy normalised to %+v", got)
	}
	// A resolved policy keeps the false somebody chose. The preset is what
	// distinguishes the two, which is the whole reason it is on the value.
	homelab, _ := Preset(PresetHomelab)
	if got := homelab.Normalised(); got.Paging {
		t.Error("normalising a resolved homelab policy turned paging back on")
	}
}

// The provenance is what a finding carries and an audit pack quotes, so it has
// to name every configurable number and be stable.
func TestProvenanceNamesEveryThreshold(t *testing.T) {
	provenance := DefaultPolicy().Provenance()
	for _, field := range []string{
		"preset=balanced", "correlatedProjects=3", "correlationWindow=", "escalationWindow=",
		"untendedMultiple=4", "maxSilence=", "paging=on",
	} {
		if !strings.Contains(provenance, field) {
			t.Errorf("the provenance does not carry %q: %s", field, provenance)
		}
	}
	// Spelled out rather than compared to itself. This string is written into
	// findings and read years later, so its exact shape is the contract: a
	// reordered field or a seconds-instead-of-durations change would make
	// every old record read differently from every new one.
	const want = "preset=balanced correlatedProjects=3 correlationWindow=15m0s " +
		"escalationWindow=1h0m0s untendedMultiple=4 maxSilence=720h0m0s paging=on"
	if provenance != want {
		t.Errorf("the provenance reads\n got %q\nwant %q", provenance, want)
	}
	homelab, _ := Preset(PresetHomelab)
	if homelab.Provenance() == DefaultPolicy().Provenance() {
		t.Error("two different policies produce the same provenance, so a finding cannot be reproduced")
	}
}

// A write that changed nothing is recorded as nothing.
func TestPolicyDifferenceNamesOnlyWhatMoved(t *testing.T) {
	was := DefaultPolicy()
	if changed := PolicyDifference(was, was); len(changed) != 0 {
		t.Fatalf("an unchanged policy reports %v as changed", changed)
	}
	now := was
	now.CorrelatedProjects = 2
	now.MaxSilence = 48 * time.Hour
	changed := PolicyDifference(was, now)
	if len(changed) != 2 {
		t.Fatalf("two moved numbers reported as %v", changed)
	}
	if !strings.Contains(strings.Join(changed, " "), "correlatedProjects 3→2") {
		t.Errorf("the difference does not read as a move: %v", changed)
	}
}

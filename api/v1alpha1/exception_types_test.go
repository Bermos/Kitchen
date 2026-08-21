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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The ladder and the clock: pure functions everything else leans on, pinned
// here so the API's enforcement and the reconciler's expiry cannot drift.

func TestTheDefaultLadderEscalatesWithDuration(t *testing.T) {
	var none *ExceptionPolicySpec // nil spec = the compiled-in default
	for duration, want := range map[time.Duration]ExceptionApproverRole{
		time.Hour:            ExceptionApproverDeveloper,
		24 * time.Hour:       ExceptionApproverDeveloper,
		25 * time.Hour:       ExceptionApproverAdmin,
		720 * time.Hour:      ExceptionApproverAdmin,
		721 * time.Hour:      ExceptionApproverOperator,
		90 * 24 * time.Hour:  ExceptionApproverOperator,
		365 * 24 * time.Hour: ExceptionApproverOperator,
	} {
		if got := none.RequiredApproverRole(duration); got != want {
			t.Errorf("%s: want %s, got %s", duration, want, got)
		}
	}
}

func TestAConfiguredLadderReplacesTheDefaultWhole(t *testing.T) {
	spec := &ExceptionPolicySpec{Ladder: []ExceptionTier{
		{MaxDuration: metav1.Duration{Duration: time.Hour}, Role: ExceptionApproverAdmin},
	}}
	if got := spec.RequiredApproverRole(30 * time.Minute); got != ExceptionApproverAdmin {
		t.Errorf("under the rung: want admin, got %s", got)
	}
	// Beyond every configured rung is always the operator's — the 24h
	// developer default must not resurface underneath a stricter ladder.
	if got := spec.RequiredApproverRole(2 * time.Hour); got != ExceptionApproverOperator {
		t.Errorf("beyond the ladder: want operator, got %s", got)
	}
}

func TestTheLowestCoveringRungDecides(t *testing.T) {
	// Rungs deliberately out of order: the answer is the tightest one that
	// covers, not the first one written down.
	spec := &ExceptionPolicySpec{Ladder: []ExceptionTier{
		{MaxDuration: metav1.Duration{Duration: 720 * time.Hour}, Role: ExceptionApproverAdmin},
		{MaxDuration: metav1.Duration{Duration: 24 * time.Hour}, Role: ExceptionApproverDeveloper},
	}}
	if got := spec.RequiredApproverRole(time.Hour); got != ExceptionApproverDeveloper {
		t.Errorf("want the 24h rung, got %s", got)
	}
}

func TestEffectivePhaseJudgesAgainstTheClock(t *testing.T) {
	now := time.Now()
	exception := &Exception{Spec: ExceptionSpec{ExpiresAt: metav1.NewTime(now.Add(time.Hour))}}
	if got := exception.EffectivePhase(now); got != ExceptionActive {
		t.Errorf("unexpired and unresolved is Active, got %s", got)
	}
	if got := exception.EffectivePhase(now.Add(2 * time.Hour)); got != ExceptionExpired {
		t.Errorf("past expiry is Expired whatever the status row says, got %s", got)
	}
	exception.Status.Phase = ExceptionResolved
	if got := exception.EffectivePhase(now.Add(2 * time.Hour)); got != ExceptionResolved {
		t.Errorf("resolved is terminal and wins over the clock, got %s", got)
	}
}

func TestCoversScopesByTripleAndWaivesByRule(t *testing.T) {
	exception := &Exception{Spec: ExceptionSpec{
		ProjectRef:     LocalObjectReference{Name: "shop"},
		EnvironmentRef: LocalObjectReference{Name: "shop-production"},
		RuleIDs:        []string{"max-severity"},
	}}
	if !exception.Covers("shop", "shop-production", "shop-rel-1") {
		t.Error("an environment-wide grant covers every release")
	}
	if exception.Covers("shop", "shop-staging", "shop-rel-1") {
		t.Error("a grant for production says nothing about staging")
	}
	exception.Spec.ReleaseRef = &LocalObjectReference{Name: "shop-rel-9"}
	if exception.Covers("shop", "shop-production", "shop-rel-1") {
		t.Error("a release-scoped grant covers that release alone")
	}
	if !exception.Covers("shop", "shop-production", "shop-rel-9") {
		t.Error("the named release is covered")
	}
	if !exception.WaivesRule("max-severity") || exception.WaivesRule("require-sbom") {
		t.Error("the waiver is per-rule")
	}
}

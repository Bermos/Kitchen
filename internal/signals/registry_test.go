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
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// catalogueV1 is every row of every table in docs/OBSERVABILITY.md §7,
// transcribed. It is a literal list rather than something derived from the
// catalogue, because the thing worth testing is that the code matches the
// design — a check computed from the code would pass whatever the code said.
var catalogueV1 = []ID{
	// Workloads.
	SignalCrashLoop, SignalOOMKilled, SignalNearMemoryLimit, SignalAtCPULimit,
	SignalImagePull, SignalUnschedulable, SignalAdmissionRefused, SignalNotReady,
	SignalErrorRate, SignalLatencyRegressed, SignalTrafficVanished, SignalNoBackend,
	// Nodes and capacity.
	SignalNodeNotReady, SignalNodePressure, SignalNodeSaturated, SignalNodeDiskFilling,
	SignalNodeSilent, SignalOvercommitted,
	// Storage.
	SignalPVCPending, SignalPVCFilling, SignalAttachFailed, SignalStoreDisk,
	SignalIngestStalled, SignalFlowsLost,
	// Edge and certificates.
	SignalGatewayUnprogrammed, SignalRouteRejected, SignalDNSMismatch, SignalCertExpiring,
	SignalTunnelDown, SignalUnroutedHosts,
	// Builds.
	SignalBuildQueueBackedUp, SignalBuildPodPending, SignalBuildFailingRepeated,
	// Cross-project.
	SignalLatencyCorrelated, SignalErrorCorrelated, SignalComponentUnhealthy,
	// Continuity: the one rule whose threshold the institution sets (#141).
	SignalRTOAtRisk,
}

func TestCatalogueHoldsEveryRowOfTheDesign(t *testing.T) {
	registry := Catalogue()
	for _, id := range catalogueV1 {
		if _, ok := registry.Lookup(id); !ok {
			t.Errorf("the catalogue is missing %q", id)
		}
	}
	if got := len(registry.Signals()); got != len(catalogueV1) {
		t.Errorf("the catalogue holds %d signals, the design names %d", got, len(catalogueV1))
	}
}

func TestCatalogueRefusesADuplicateID(t *testing.T) {
	rule := Signal{
		ID: SignalCrashLoop, Version: 1, Audience: AudienceOperator, Summary: "x",
		Evaluate: func(*Snapshot) []Finding { return nil },
	}
	if _, err := NewRegistry(rule, rule); err == nil {
		t.Fatal("two signals under one id were accepted; their fingerprints would collide")
	}
}

func TestCatalogueRefusesAnIncompleteSignal(t *testing.T) {
	for name, rule := range map[string]Signal{
		"no id":       {Version: 1, Audience: AudienceOperator, Summary: "x", Evaluate: nilRule},
		"no version":  {ID: "a.b", Audience: AudienceOperator, Summary: "x", Evaluate: nilRule},
		"no summary":  {ID: "a.b", Version: 1, Audience: AudienceOperator, Evaluate: nilRule},
		"no rule":     {ID: "a.b", Version: 1, Audience: AudienceOperator, Summary: "x"},
		"no audience": {ID: "a.b", Version: 1, Summary: "x", Evaluate: nilRule},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRegistry(rule); err == nil {
				t.Fatal("an incomplete signal was accepted")
			}
		})
	}
}

func nilRule(*Snapshot) []Finding { return nil }

// A rule that cannot read its input must say so rather than report health it
// did not measure. This is the whole degradation contract, tested through the
// registry because that is where it is implemented once for all of them.
func TestAnUnreadableInputReportsThatTheRuleCouldNotRun(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.MarkUnreadable(InputResources, "dial tcp 10.0.0.5:8123: connection refused")

	finding := expectOne(t, evaluate(t, SignalNearMemoryLimit, snapshot))
	if finding.Severity != SeverityUnknown {
		t.Fatalf("severity = %q, want unknown", finding.Severity)
	}
	expectDetail(t, finding, "connection refused")
	expectDetail(t, finding, string(InputResources))
	if finding.Title != "cannot be evaluated" {
		t.Fatalf("title = %q", finding.Title)
	}
}

// An input that does not apply to this installation is not a gap in what the
// platform knows, and a permanent row saying otherwise would train the reader
// to ignore the list.
func TestANotApplicableInputProducesNothing(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.MarkNotApplicable(InputDNS, "cloudflared is enabled")
	expectNone(t, evaluate(t, SignalDNSMismatch, snapshot))
}

// A rule that merely prefers an input still runs: workload.crashloop fires
// from pod status alone, and is only richer with the restart counts beside it.
func TestAnOptionalInputDoesNotStopARule(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Pods = []corev1.Pod{waitingPod(reasonCrashLoopBackOff, "back-off")}
	snapshot.MarkUnreadable(InputResources, "the store is down")

	finding := expectOne(t, evaluate(t, SignalCrashLoop, snapshot))
	if finding.Severity == SeverityUnknown {
		t.Fatal("a rule that reads pod status was stopped by a store failure")
	}
	// Without the rollup it falls back to the pod's own cumulative count,
	// which is worse but is not nothing.
	expectDetail(t, finding, "so far")
}

// Unreadable inputs are also reportable once, above a list that would
// otherwise repeat them per rule.
func TestUnreadableListsWhatCouldNotBeRead(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.MarkUnreadable(InputRequests, "connection refused")
	snapshot.MarkNotApplicable(InputDNS, "cloudflared is enabled")

	failures := snapshot.Unreadable()
	if len(failures) != 1 || failures[0].Input != InputRequests {
		t.Fatalf("unreadable = %+v, want the requests input alone", failures)
	}
}

// A snapshot nobody marked is a snapshot of a world where everything was
// readable, which is what keeps every test in this package free of ceremony.
func TestAnUnmarkedInputCountsAsAvailable(t *testing.T) {
	if !newSnapshot().Available(InputPods) {
		t.Fatal("an unmarked input was not available")
	}
}

// Two evaluations of an unchanged snapshot must produce byte-identical rounds,
// or the diffing loop stage 5 adds would see churn that is only map ordering.
func TestEvaluationIsDeterministic(t *testing.T) {
	snapshot := brokenSnapshot()
	first := Catalogue().Evaluate(snapshot)
	second := Catalogue().Evaluate(snapshot)

	if len(first) != len(second) {
		t.Fatalf("rounds differ in size: %d then %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("position %d differs: %+v then %+v", i, first[i], second[i])
		}
	}
}

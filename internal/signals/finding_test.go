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
)

// The fingerprint is the one thing in this package that a later release
// depends on being exactly right: a background loop diffs rounds by it, and a
// fingerprint that moves for the same condition would resolve and reopen a
// problem that never changed. So it gets the most direct tests here, and the
// end-to-end version in fingerprint_test.go.

func TestFingerprintMatchesTheDesignsExample(t *testing.T) {
	scope := Scope{
		Kind:        ScopeEnvironment,
		Project:     testProject,
		Environment: testEnvironment,
		Name:        testContainer,
	}
	const want = "workload.crashloop/shop/pr-41/web"
	if got := Fingerprint(SignalCrashLoop, scope); got != want {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}
}

func TestFingerprintOfAScopelessSignalIsTheIdAlone(t *testing.T) {
	if got := Fingerprint(SignalOvercommitted, Scope{Kind: ScopePlatform}); got != string(SignalOvercommitted) {
		t.Fatalf("fingerprint = %q, want the bare id", got)
	}
}

func TestScopePathJoinsOnlyTheFieldsThatAreSet(t *testing.T) {
	for name, testCase := range map[string]struct {
		scope Scope
		want  string
	}{
		"platform workload": {
			scope: Scope{Kind: ScopeWorkload, Namespace: "kitchen-system", Name: "logs"},
			want:  "kitchen-system/logs",
		},
		"node dimension": {
			scope: Scope{Kind: ScopeNode, Node: testNode, Name: "CPU"},
			want:  testNode + "/CPU",
		},
		"project": {
			scope: Scope{Kind: ScopeProject, Project: testProject},
			want:  testProject,
		},
		"empty": {
			scope: Scope{Kind: ScopePlatform},
			want:  "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := testCase.scope.Path(); got != testCase.want {
				t.Fatalf("path = %q, want %q", got, testCase.want)
			}
		})
	}
}

// An unevaluable rule and the same rule firing must never share a fingerprint:
// a transition log that conflated them would resolve one by opening the other.
func TestUnevaluableFingerprintCannotCollideWithAFiringOne(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.MarkUnreadable(InputRequests, "the store refused the connection")

	finding := expectOne(t, evaluate(t, SignalLatencyCorrelated, snapshot))
	if finding.Severity != SeverityUnknown {
		t.Fatalf("severity = %q, want unknown", finding.Severity)
	}
	firing := Fingerprint(SignalLatencyCorrelated, Scope{Kind: ScopePlatform, Name: "latency"})
	if finding.Fingerprint == firing {
		t.Fatalf("the unevaluable fingerprint collides with the firing one: %q", finding.Fingerprint)
	}
	if !strings.HasSuffix(finding.Fingerprint, unevaluableMarker) {
		t.Fatalf("fingerprint %q is not marked unevaluable", finding.Fingerprint)
	}
}

func TestFindingsSortWorstFirstAndStably(t *testing.T) {
	findings := Findings{
		{Signal: SignalNotReady, Severity: SeverityInfo, Fingerprint: "b"},
		{Signal: SignalCrashLoop, Severity: SeverityCritical, Fingerprint: "z"},
		{Signal: SignalCrashLoop, Severity: SeverityCritical, Fingerprint: "a"},
		{Signal: SignalNotReady, Severity: SeverityUnknown, Fingerprint: "c"},
		{Signal: SignalOOMKilled, Severity: SeverityWarning, Fingerprint: "d"},
	}
	findings.Sort()

	want := []string{"a", "z", "d", "c", "b"}
	for i, fingerprint := range want {
		if findings[i].Fingerprint != fingerprint {
			t.Fatalf("position %d = %q, want %q (%s)", i, findings[i].Fingerprint, fingerprint,
				describe(findings))
		}
	}
}

func TestForEnvironmentKeepsTheProjectsOwnAndItsEnvironments(t *testing.T) {
	findings := Findings{
		{Fingerprint: "env", Scope: Scope{Project: testProject, Environment: testEnvironment}},
		{Fingerprint: "project", Scope: Scope{Project: testProject}},
		{Fingerprint: "other-env", Scope: Scope{Project: testProject, Environment: "production"}},
		{Fingerprint: "other-project", Scope: Scope{Project: "blog"}},
		{Fingerprint: "platform", Scope: Scope{Kind: ScopePlatform}},
	}

	kept := findings.ForEnvironment(testProject, testEnvironment)
	if len(kept) != 2 {
		t.Fatalf("kept %d findings, want 2: %s", len(kept), describe(kept))
	}
	if kept[0].Fingerprint != "env" || kept[1].Fingerprint != "project" {
		t.Fatalf("kept the wrong findings: %s", describe(kept))
	}
}

func TestFiringDropsTheRulesThatCouldNotBeEvaluated(t *testing.T) {
	findings := Findings{
		{Fingerprint: "known", Severity: SeverityWarning},
		{Fingerprint: "dark", Severity: SeverityUnknown},
	}
	firing := findings.Firing()
	if len(firing) != 1 || firing[0].Fingerprint != "known" {
		t.Fatalf("firing = %s, want the known one alone", describe(firing))
	}
}

func TestDetailIsBounded(t *testing.T) {
	finding := fire(SignalCrashLoop, SeverityWarning, Scope{Kind: ScopePlatform}, testNow,
		"title", strings.Repeat("x", MaxDetailLength*2), EvidencePlatform)
	if len([]rune(finding.Detail)) > MaxDetailLength+1 {
		t.Fatalf("detail is %d runes, want it bounded at %d", len([]rune(finding.Detail)), MaxDetailLength)
	}
}

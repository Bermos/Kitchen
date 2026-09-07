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
	"time"
)

// The differ is the whole of what turns a list of findings into a history, and
// every case below is one a wrong answer would put into the record permanently.

var (
	transitionRoundOne = time.Date(2026, 9, 1, 3, 14, 0, 0, time.UTC)
	transitionRoundTwo = transitionRoundOne.Add(time.Minute)
)

// trackerOver builds a tracker over a catalogue of exactly the rules a test
// needs, so that a version is a number the test chose rather than whatever the
// real catalogue happens to carry today.
func trackerOver(t *testing.T, entries ...Signal) *Tracker {
	t.Helper()
	registry, err := NewRegistry(entries...)
	if err != nil {
		t.Fatalf("the test catalogue is malformed: %v", err)
	}
	return NewTracker(registry)
}

func testSignal(id ID, audience Audience, version int) Signal {
	return testSignalAt(id, audience, version, testTiers(audience))
}

// testSignalAt is testSignal with the tier declaration chosen, for the tests
// that are about what a tier does rather than about the diff.
func testSignalAt(id ID, audience Audience, version int, tiers Tiers) Signal {
	return Signal{
		ID:       id,
		Version:  version,
		Audience: audience,
		Tiers:    tiers,
		Summary:  "a rule that exists to be diffed",
		Evaluate: func(*Snapshot) []Finding { return nil },
	}
}

// testTiers is a declaration covering exactly the audiences a rule reaches, so
// that a test about the diff does not have to decide a tier to get a valid
// catalogue.
func testTiers(audience Audience) Tiers {
	if audience == AudienceDeveloper {
		return Tiers{Developer: TierPage, Operator: TierTicket}
	}
	return Tiers{Operator: TierPage}
}

func crashLoop(scope Scope) Finding {
	finding := fire("workload.crashloop", SeverityCritical, scope, transitionRoundOne,
		"crash-looping", "12 restarts in 30m", "/environments/shop-production")
	finding.Audience = AudienceDeveloper
	return finding
}

func nodeSilent() Finding {
	finding := fire("node.silent", SeverityWarning, Scope{Kind: ScopeNode, Node: "node-b"},
		transitionRoundOne, "no telemetry", "nothing received for 34m", "/platform/nodes?node=node-b")
	finding.Audience = AudienceOperator
	return finding
}

// A condition nobody has seen before opens, and it opens once per delivery.
func TestAConditionOpens(t *testing.T) {
	tracker := trackerOver(t, testSignal("node.silent", AudienceOperator, 2))

	transitions := tracker.Observe(Findings{nodeSilent()}, transitionRoundOne)
	if len(transitions) != 1 {
		t.Fatalf("one operator condition is one transition: %+v", transitions)
	}
	transition := transitions[0]
	switch {
	case transition.State != StateOpen:
		t.Errorf("a condition nobody was seeing opened: %+v", transition)
	case transition.Audience != AudienceOperator:
		t.Errorf("an operator signal is delivered to the operator alone: %+v", transition)
	case transition.Fingerprint != "node.silent/node-b":
		t.Errorf("the transition is keyed on the finding's fingerprint: %+v", transition)
	case !transition.OpenedAt.Equal(transitionRoundOne):
		t.Errorf("the platform first saw it in this round: %+v", transition)
	case transition.Version != 2:
		t.Errorf("the rule's version travels with the transition: %+v", transition)
	}
}

// The same condition in the next round is not news. This is the case that
// decides whether a history is a history or a sampling of the same problem
// every interval forever.
func TestAConditionThatIsStillTrueIsNotATransition(t *testing.T) {
	tracker := trackerOver(t, testSignal("node.silent", AudienceOperator, 1))
	tracker.Observe(Findings{nodeSilent()}, transitionRoundOne)

	again := nodeSilent()
	again.Detail = "nothing received for 35m"
	if transitions := tracker.Observe(Findings{again}, transitionRoundTwo); len(transitions) != 0 {
		t.Fatalf("the same condition a minute later is not a transition: %+v", transitions)
	}
	if tracker.Open() != 1 {
		t.Errorf("it is still open: %d", tracker.Open())
	}
}

// A condition that stops being found resolves, and the resolving row carries
// the whole episode: when it opened, and what it last said.
func TestAConditionResolves(t *testing.T) {
	tracker := trackerOver(t, testSignal("node.silent", AudienceOperator, 1))
	opening := nodeSilent()
	opening.Detail = "nothing received for 34m"
	tracker.Observe(Findings{opening}, transitionRoundOne)

	transitions := tracker.Observe(nil, transitionRoundTwo)
	if len(transitions) != 1 {
		t.Fatalf("a condition that is gone is one transition: %+v", transitions)
	}
	transition := transitions[0]
	switch {
	case transition.State != StateResolved:
		t.Errorf("it resolved: %+v", transition)
	case !transition.At.Equal(transitionRoundTwo):
		t.Errorf("it resolved in this round: %+v", transition)
	case !transition.OpenedAt.Equal(transitionRoundOne):
		t.Errorf("how long it lasted is one row, not a join: %+v", transition)
	case transition.Detail != "nothing received for 34m":
		t.Errorf("the resolving row says what the condition was: %+v", transition)
	}
	if tracker.Open() != 0 {
		t.Errorf("nothing is open now: %d", tracker.Open())
	}
}

// One fingerprint, two audiences. A developer signal is additive — it is on the
// project's strip and on the operator's list — and recorded that is two
// deliveries, because #471 acknowledges and silences them separately.
func TestOneConditionReachesBothAudiences(t *testing.T) {
	tracker := trackerOver(t, testSignal("workload.crashloop", AudienceDeveloper, 3))
	scope := Scope{Kind: ScopeEnvironment, Project: "shop", Environment: "shop-production", Name: "web"}

	transitions := tracker.Observe(Findings{crashLoop(scope)}, transitionRoundOne)
	if len(transitions) != 2 {
		t.Fatalf("a developer condition is delivered twice: %+v", transitions)
	}
	audiences := map[Audience]Transition{}
	for _, transition := range transitions {
		if transition.Fingerprint != "workload.crashloop/shop/shop-production/web" {
			t.Errorf("both rows are about one condition: %+v", transition)
		}
		if transition.Version != 3 {
			t.Errorf("both rows carry the rule's version: %+v", transition)
		}
		audiences[transition.Audience] = transition
	}
	if len(audiences) != 2 || audiences[AudienceDeveloper].State == "" || audiences[AudienceOperator].State == "" {
		t.Fatalf("the two rows are the two audiences: %+v", transitions)
	}
	if tracker.Open() != 2 {
		t.Errorf("two deliveries are open: %d", tracker.Open())
	}

	// And they resolve as two, so an ack on one of them is never an ack on
	// the other's condition either.
	resolved := tracker.Observe(nil, transitionRoundTwo)
	if len(resolved) != 2 {
		t.Fatalf("two deliveries resolve as two: %+v", resolved)
	}
}

// The case a store outage would otherwise put into the record: a rule that
// could not be evaluated has not stopped finding its condition, and recording
// thirty resolutions at the moment ClickHouse restarted would be a history
// that lies about the one thing it exists to be right about.
func TestARuleThatCouldNotBeEvaluatedResolvesNothing(t *testing.T) {
	tracker := trackerOver(t, testSignal("node.silent", AudienceOperator, 1))
	tracker.Observe(Findings{nodeSilent()}, transitionRoundOne)

	blind := fire("node.silent", SeverityUnknown, Scope{Kind: ScopePlatform}, transitionRoundTwo,
		"cannot be evaluated", "telemetry_freshness could not be read", EvidencePlatform)
	blind.Audience = AudienceOperator
	if transitions := tracker.Observe(Findings{blind}, transitionRoundTwo); len(transitions) != 0 {
		t.Fatalf("a rule that could not see resolves nothing: %+v", transitions)
	}
	if tracker.Open() != 1 {
		t.Errorf("the condition is carried forward, not forgotten: %d", tracker.Open())
	}

	// And when the input comes back and the condition is genuinely gone, it
	// resolves then — with the opening time it had all along.
	transitions := tracker.Observe(nil, transitionRoundTwo.Add(time.Minute))
	if len(transitions) != 1 || transitions[0].State != StateResolved {
		t.Fatalf("it resolves once the rule can see again: %+v", transitions)
	}
	if !transitions[0].OpenedAt.Equal(transitionRoundOne) {
		t.Errorf("the blind round did not restart the clock: %+v", transitions[0])
	}
}

// A restarted operator — or a replica that has just won the lease — must not
// re-announce what the previous leader already recorded.
func TestARestoredTrackerReAnnouncesNothing(t *testing.T) {
	tracker := trackerOver(t, testSignal("node.silent", AudienceOperator, 1))
	tracker.Restore([]Transition{{
		At:          transitionRoundOne,
		State:       StateOpen,
		Signal:      "node.silent",
		Fingerprint: "node.silent/node-b",
		Audience:    AudienceOperator,
		Severity:    SeverityWarning,
		Scope:       Scope{Kind: ScopeNode, Node: "node-b"},
		OpenedAt:    transitionRoundOne,
	}})

	if transitions := tracker.Observe(Findings{nodeSilent()}, transitionRoundTwo); len(transitions) != 0 {
		t.Fatalf("a condition the store already holds open is not news: %+v", transitions)
	}
	transitions := tracker.Observe(nil, transitionRoundTwo.Add(time.Minute))
	if len(transitions) != 1 || !transitions[0].OpenedAt.Equal(transitionRoundOne) {
		t.Fatalf("it resolves with the opening time the store carried: %+v", transitions)
	}
}

// A round with everything in it: one opening, one resolving, one unchanged,
// in a fixed order.
func TestARoundOpensAndResolvesTogether(t *testing.T) {
	tracker := trackerOver(t,
		testSignal("node.silent", AudienceOperator, 1),
		testSignal("workload.crashloop", AudienceDeveloper, 1))
	scope := Scope{Kind: ScopeEnvironment, Project: "shop", Environment: "shop-production", Name: "web"}
	tracker.Observe(Findings{nodeSilent()}, transitionRoundOne)

	transitions := tracker.Observe(Findings{crashLoop(scope)}, transitionRoundTwo)
	if len(transitions) != 3 {
		t.Fatalf("two openings and one resolution: %+v", transitions)
	}
	// Sorted by fingerprint, then audience: the crash loop's two deliveries
	// first, then the node.
	states := map[string]TransitionState{}
	for _, transition := range transitions {
		states[transition.Fingerprint+"/"+string(transition.Audience)] = transition.State
	}
	if states["workload.crashloop/shop/shop-production/web/developer"] != StateOpen ||
		states["workload.crashloop/shop/shop-production/web/operator"] != StateOpen ||
		states["node.silent/node-b/operator"] != StateResolved {
		t.Fatalf("the round is not what changed: %+v", transitions)
	}
	if transitions[0].Fingerprint > transitions[2].Fingerprint {
		t.Errorf("a round is written in a fixed order: %+v", transitions)
	}
}

// Recorded rows read back are the findings the screens render, worst first,
// and the audience they carry is the delivery's — which is what lets the
// operator's copy of a developer condition be selected on its own.
func TestRecordedTransitionsReadBackAsFindings(t *testing.T) {
	findings := TransitionsFindings([]Transition{
		{
			Signal: "workload.crashloop", Fingerprint: "workload.crashloop/shop/pr-41/web",
			Audience: AudienceOperator, Severity: SeverityWarning,
			Scope: Scope{Kind: ScopeEnvironment, Project: "shop", Environment: "pr-41", Name: "web"},
			Title: "crash-looping",
		},
		{
			Signal: "node.silent", Fingerprint: "node.silent/node-b",
			Audience: AudienceOperator, Severity: SeverityCritical,
			Scope: Scope{Kind: ScopeNode, Node: "node-b"}, Title: "no telemetry",
		},
	})
	if len(findings) != 2 {
		t.Fatalf("both rows are findings: %+v", findings)
	}
	if findings[0].Severity != SeverityCritical {
		t.Errorf("a read-back round is ordered worst first: %+v", findings)
	}
	if findings[0].Audience != AudienceOperator || findings[0].Scope.Node != "node-b" {
		t.Errorf("a finding read back is the finding that was recorded: %+v", findings[0])
	}
}

// The two mappings are one round trip, because a column that means one thing
// on the way in and another on the way out is a history nobody can read.
func TestTransitionRowsRoundTrip(t *testing.T) {
	transition := Transition{
		At:          transitionRoundTwo,
		State:       StateResolved,
		Signal:      "workload.crashloop",
		Fingerprint: "workload.crashloop/shop/pr-41/web",
		Audience:    AudienceDeveloper,
		Version:     4,
		Severity:    SeverityCritical,
		Scope: Scope{
			Kind: ScopeEnvironment, Project: "shop", Environment: "pr-41",
			Namespace: "kitchen-shop", Node: "node-b", Name: "web",
		},
		Title:    "crash-looping",
		Detail:   "12 restarts in 30m",
		Evidence: "/environments/pr-41",
		Since:    transitionRoundOne,
		OpenedAt: transitionRoundOne,
	}
	back := TransitionsFrom(TransitionRows([]Transition{transition}))
	if len(back) != 1 {
		t.Fatalf("one row is one transition: %+v", back)
	}
	if back[0] != transition {
		t.Errorf("the round trip changed it:\n got %+v\nwant %+v", back[0], transition)
	}
}

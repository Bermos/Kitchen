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

	corev1 "k8s.io/api/core/v1"
)

var alertNow = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

// openPair is one developer condition as the loop records it: two rows, two
// tiers, one fingerprint.
func openPair(openedAt time.Time) []Transition {
	scope := Scope{Kind: ScopeEnvironment, Project: "shop", Environment: "shop-production", Name: "web"}
	rows := make([]Transition, 0, 2)
	for audience, tier := range map[Audience]Tier{AudienceDeveloper: TierPage, AudienceOperator: TierTicket} {
		rows = append(rows, Transition{
			At: openedAt, State: StateOpen, Signal: SignalCrashLoop,
			Fingerprint: "workload.crashloop/shop/shop-production/web",
			Audience:    audience, Tier: tier, Version: 2,
			Severity: SeverityCritical, Scope: scope,
			Title: "crash-looping", Detail: "12 restarts in 30m",
			Since: openedAt, OpenedAt: openedAt,
		})
	}
	return rows
}

func alertFor(alerts []Alert, audience Audience) (Alert, bool) {
	for _, alert := range alerts {
		if alert.Finding.Audience == audience && !alert.Symptom {
			return alert, true
		}
	}
	return Alert{}, false
}

// Nothing has happened yet: both readers see the tier the rule declared.
func TestAFreshConditionReadsAtItsDeclaredTier(t *testing.T) {
	alerts := Assess(openPair(alertNow.Add(-5*time.Minute)), nil, DefaultPolicy(), alertNow)

	developer, ok := alertFor(alerts, AudienceDeveloper)
	if !ok {
		t.Fatal("the project's own row is missing")
	}
	operator, _ := alertFor(alerts, AudienceOperator)
	if developer.Tier != TierPage {
		t.Errorf("the owner's row is the page it was declared as: %q", developer.Tier)
	}
	if operator.Tier != TierTicket {
		t.Errorf("the operator's row is a ticket: %q", operator.Tier)
	}
	if developer.Escalated || operator.Escalated {
		t.Error("five minutes is not past the escalation window")
	}
}

// The audience matrix's second row: something is already mitigating it, so a
// fix is still owed — a ticket for the owner — and nobody else needs waking.
func TestAnAcknowledgedConditionIsATicketForItsOwnerAndALogForEverybodyElse(t *testing.T) {
	open := openPair(alertNow.Add(-10 * time.Minute))
	state := map[TransitionKey]MitigationState{
		{Fingerprint: open[0].Fingerprint, Audience: AudienceDeveloper}: {
			Acknowledged: true, AcknowledgedBy: "ana@example.com", AcknowledgedAt: alertNow,
		},
	}

	alerts := Assess(open, state, DefaultPolicy(), alertNow)
	developer, _ := alertFor(alerts, AudienceDeveloper)
	operator, _ := alertFor(alerts, AudienceOperator)
	if developer.Tier != TierTicket {
		t.Errorf("a fix is still owed, so the owner keeps a ticket: %q", developer.Tier)
	}
	if operator.Tier != TierLog {
		t.Errorf("the system is doing its job; nobody else needs waking: %q", operator.Tier)
	}
}

// The escalation rule, in the issue's own words: past the window an
// unacknowledged owner-tier condition repeats and *adds the operator as a
// ticket*. Nothing becomes a page, because nothing is more broken at hour four
// than at hour one.
func TestAnUnacknowledgedConditionAddsTheOperatorAsATicket(t *testing.T) {
	open := openPair(alertNow.Add(-(EscalationWindow + 12*time.Minute)))

	alerts := Assess(open, nil, DefaultPolicy(), alertNow)
	developer, _ := alertFor(alerts, AudienceDeveloper)
	operator, _ := alertFor(alerts, AudienceOperator)

	if !developer.Escalated || !operator.Escalated {
		t.Fatalf("past the window both rows say so: developer=%+v operator=%+v", developer, operator)
	}
	if developer.Tier != TierPage {
		t.Errorf("escalation changes the audience, not the colour: %q", developer.Tier)
	}
	if operator.Tier != TierTicket {
		t.Errorf("the operator is added as a ticket: %q", operator.Tier)
	}
	for _, part := range []string{"shop / shop-production", "unmitigated 1h 12m", "nobody has acknowledged"} {
		if !strings.Contains(operator.Note, part) {
			t.Errorf("the escalated row says %q; note is %q", part, operator.Note)
		}
	}
	if developer.Untended || operator.Untended {
		t.Error("one window past is escalated, not untended")
	}
}

// A condition an operator-audience rule produced has nobody to escalate *to*:
// the operator already owns it, and adding them to their own list would be a
// repetition rather than an escalation.
func TestAnOperatorsOwnConditionEscalatesToNobody(t *testing.T) {
	open := []Transition{{
		At: alertNow.Add(-3 * EscalationWindow), State: StateOpen, Signal: SignalNodeNotReady,
		Fingerprint: "node.notready/node-b", Audience: AudienceOperator, Tier: TierPage,
		Severity: SeverityCritical, Scope: Scope{Kind: ScopeNode, Node: "node-b"},
		Title: "not ready", OpenedAt: alertNow.Add(-3 * EscalationWindow),
	}}

	alerts := Assess(open, nil, DefaultPolicy(), alertNow)
	if len(alerts) != 1 {
		t.Fatalf("an operator condition is one delivery: %+v", alerts)
	}
	if alerts[0].Tier != TierPage {
		t.Errorf("its tier does not move: %q", alerts[0].Tier)
	}
	if !alerts[0].Escalated {
		t.Error("it is still an unacknowledged condition past the window, and says so")
	}
}

// Past the multiple, the condition stops being an alert and becomes a finding
// about the institution.
func TestAnUnacknowledgedConditionBecomesUntended(t *testing.T) {
	open := openPair(alertNow.Add(-(UntendedAfter + time.Minute)))

	alerts := Assess(open, nil, DefaultPolicy(), alertNow)
	developer, _ := alertFor(alerts, AudienceDeveloper)
	operator, _ := alertFor(alerts, AudienceOperator)
	if !developer.Untended || !operator.Untended {
		t.Fatalf("past %s with nobody acknowledging is untended: %+v", UntendedAfter, alerts)
	}
}

// The acks are keyed per (fingerprint, audience), which is the decision the
// whole escalation clock rests on: a member acking their project's row must
// not satisfy the operator's row about the same condition.
func TestAMembersAckDoesNotSatisfyTheOperatorsRow(t *testing.T) {
	open := openPair(alertNow.Add(-(EscalationWindow + time.Minute)))
	fingerprint := open[0].Fingerprint

	// Acked on the *operator's* row alone. The owner's row is still
	// unacknowledged, so it still escalates — and the operator's own ack is
	// not what the escalation is asking for.
	operatorAcked := Assess(open, map[TransitionKey]MitigationState{
		{Fingerprint: fingerprint, Audience: AudienceOperator}: {
			Acknowledged: true, AcknowledgedBy: "ops@example.com", AcknowledgedAt: alertNow,
		},
	}, DefaultPolicy(), alertNow)
	developer, _ := alertFor(operatorAcked, AudienceDeveloper)
	if !developer.Escalated {
		t.Error("an operator's ack does not acknowledge the project's row")
	}

	// And the other way: the member acks theirs, and the operator's row is
	// not escalated because somebody is on it.
	memberAcked := Assess(open, map[TransitionKey]MitigationState{
		{Fingerprint: fingerprint, Audience: AudienceDeveloper}: {
			Acknowledged: true, AcknowledgedBy: "ana@example.com", AcknowledgedAt: alertNow,
		},
	}, DefaultPolicy(), alertNow)
	operator, _ := alertFor(memberAcked, AudienceOperator)
	if operator.Escalated {
		t.Error("somebody has acknowledged, so nothing escalates")
	}
	if operator.Tier != TierLog {
		t.Errorf("and the operator's row falls to a log: %q", operator.Tier)
	}
}

// A silence is keyed on the pair too, which is what "never reaching across
// projects" means once one fingerprint spans both audiences.
func TestAMembersSilenceDoesNotSilenceTheOperatorsRow(t *testing.T) {
	open := openPair(alertNow.Add(-10 * time.Minute))
	alerts := Assess(open, map[TransitionKey]MitigationState{
		{Fingerprint: open[0].Fingerprint, Audience: AudienceDeveloper}: {
			SilencedBy: "ana@example.com", SilenceReason: "known, fix in flight",
			SilencedUntil: alertNow.Add(time.Hour),
		},
	}, DefaultPolicy(), alertNow)

	developer, _ := alertFor(alerts, AudienceDeveloper)
	operator, _ := alertFor(alerts, AudienceOperator)
	if developer.Tier != TierLog {
		t.Errorf("the silenced row is quiet: %q", developer.Tier)
	}
	if operator.Tier != TierTicket {
		t.Errorf("the operator's row is untouched by a project's silence: %q", operator.Tier)
	}
}

// An expired silence is not a silence. The expiry is the whole reason a
// silence is safe to grant.
func TestAnExpiredSilenceStopsSilencing(t *testing.T) {
	open := openPair(alertNow.Add(-10 * time.Minute))
	alerts := Assess(open, map[TransitionKey]MitigationState{
		{Fingerprint: open[0].Fingerprint, Audience: AudienceDeveloper}: {
			SilencedBy: "ana@example.com", SilencedUntil: alertNow.Add(-time.Minute),
		},
	}, DefaultPolicy(), alertNow)

	developer, _ := alertFor(alerts, AudienceDeveloper)
	if developer.Tier != TierPage {
		t.Errorf("an expired silence quietens nothing: %q", developer.Tier)
	}
}

// A claimed ticket is mitigated: an operator has taken it, which is the whole
// of decision 1 — no rota, one ticket anybody can take.
func TestAClaimIsMitigation(t *testing.T) {
	open := []Transition{{
		At: alertNow.Add(-2 * EscalationWindow), State: StateOpen, Signal: SignalNodeNotReady,
		Fingerprint: "node.notready/node-b", Audience: AudienceOperator, Tier: TierPage,
		Severity: SeverityCritical, Scope: Scope{Kind: ScopeNode, Node: "node-b"},
		OpenedAt: alertNow.Add(-2 * EscalationWindow),
	}}
	alerts := Assess(open, map[TransitionKey]MitigationState{
		{Fingerprint: "node.notready/node-b", Audience: AudienceOperator}: {
			ClaimedBy: "ops@example.com", ClaimedAt: alertNow,
		},
	}, DefaultPolicy(), alertNow)

	if alerts[0].Escalated {
		t.Error("somebody has taken it, so it is not untended")
	}
	if alerts[0].Tier != TierTicket {
		t.Errorf("a claimed page is a ticket — a fix is still owed: %q", alerts[0].Tier)
	}
}

// The developer is never told nothing. A platform condition scoped to their
// project reaches them symptom-shaped: no Kubernetes noun, no evidence link,
// no silence, and a fingerprint nothing can be written against.
func TestAPlatformConditionSurfacesToTheProjectAsASymptom(t *testing.T) {
	open := []Transition{{
		At: alertNow.Add(-4 * time.Minute), State: StateOpen, Signal: SignalAttachFailed,
		Fingerprint: "volume.attach-failed/shop/shop-production/data",
		Audience:    AudienceOperator, Tier: TierTicket, Severity: SeverityCritical,
		Scope: Scope{
			Kind: ScopeVolume, Project: "shop", Environment: "shop-production",
			Namespace: "kitchen-shop", Node: "node-b", Name: "data",
		},
		Title: "volume will not attach", Detail: "AttachVolume.Attach failed for volume pvc-9c1",
		Evidence: "/platform/storage", OpenedAt: alertNow.Add(-4 * time.Minute),
	}}

	symptoms := Symptoms(open, "shop", alertNow)
	if len(symptoms) != 1 {
		t.Fatalf("one platform condition about this project is one symptom: %+v", symptoms)
	}
	symptom := symptoms[0]
	switch {
	case !symptom.Symptom || symptom.Actionable:
		t.Errorf("a symptom is not actionable: %+v", symptom)
	case symptom.Tier != TierLog:
		t.Errorf("it notifies nobody: %q", symptom.Tier)
	case symptom.Finding.Title != "shop-production degraded":
		t.Errorf("it is shaped as a symptom of their environment: %q", symptom.Finding.Title)
	case symptom.Finding.Detail != "a platform issue, the operator has been notified":
		t.Errorf("and says who is acting: %q", symptom.Finding.Detail)
	case symptom.Finding.Evidence != "":
		t.Errorf("no button they cannot press: %q", symptom.Finding.Evidence)
	case symptom.Finding.Scope.Namespace != "" || symptom.Finding.Scope.Node != "" ||
		symptom.Finding.Scope.Name != "":
		t.Errorf("no Kubernetes noun: %+v", symptom.Finding.Scope)
	case !strings.HasSuffix(symptom.Finding.Fingerprint, symptomMarker):
		t.Errorf("a derived row cannot collide with a recorded key: %q", symptom.Finding.Fingerprint)
	}
}

// A platform condition about nothing in particular is nobody's symptom: a node
// going NotReady is not a word a developer has, and inventing a project for it
// would be the correlation this design refuses to guess at.
func TestAPlatformConditionWithNoProjectIsNobodysSymptom(t *testing.T) {
	open := []Transition{{
		State: StateOpen, Signal: SignalNodeNotReady, Fingerprint: "node.notready/node-b",
		Audience: AudienceOperator, Tier: TierPage, Scope: Scope{Kind: ScopeNode, Node: "node-b"},
		OpenedAt: alertNow,
	}}
	if symptoms := Symptoms(open, "shop", alertNow); len(symptoms) != 0 {
		t.Errorf("nothing links this to a project: %+v", symptoms)
	}
}

func TestSilenceValidation(t *testing.T) {
	for name, test := range map[string]struct {
		reason string
		until  time.Time
		ok     bool
	}{
		"no reason":     {"", alertNow.Add(time.Hour), false},
		"no expiry":     {"known", time.Time{}, false},
		"already over":  {"known", alertNow.Add(-time.Minute), false},
		"past the cap":  {"known", alertNow.Add(MaxSilence + time.Hour), false},
		"reason and an": {"known, fix in flight", alertNow.Add(4 * time.Hour), true},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateSilence(test.reason, test.until, DefaultPolicy(), alertNow)
			if test.ok && err != nil {
				t.Fatalf("a silence with a reason and an expiry is allowed: %v", err)
			}
			if !test.ok && err == nil {
				t.Fatal("a silence that cannot do its job was accepted")
			}
		})
	}
}

// The fold is what turns an append-only set of records into "what stands now",
// and the unsilence is the one record that clears rather than sets.
func TestMitigationRecordsFold(t *testing.T) {
	key := TransitionKey{Fingerprint: "a.b/shop", Audience: AudienceDeveloper}
	states := FoldMitigations([]Mitigation{
		{At: alertNow.Add(-3 * time.Hour), Kind: MitigationSilence, Fingerprint: key.Fingerprint,
			Audience: key.Audience, Actor: "ana", Reason: "known", Until: alertNow.Add(time.Hour)},
		{At: alertNow.Add(-2 * time.Hour), Kind: MitigationAck, Fingerprint: key.Fingerprint,
			Audience: key.Audience, Actor: "bo", Source: SourceAction},
		{At: alertNow.Add(-time.Hour), Kind: MitigationUnsilence, Fingerprint: key.Fingerprint,
			Audience: key.Audience, Actor: "ana"},
	})

	state := states[key]
	switch {
	case !state.Acknowledged || state.AcknowledgedBy != "bo":
		t.Errorf("the newest ack stands: %+v", state)
	case state.AcknowledgedBySource != SourceAction:
		t.Errorf("an implicit ack says it was an action: %+v", state)
	case state.Silenced(alertNow):
		t.Errorf("the unsilence lifted the silence: %+v", state)
	}

	// A silence laid on top of an unsilence wins again, whatever order the
	// rows arrive in.
	states = FoldMitigations([]Mitigation{
		{At: alertNow, Kind: MitigationSilence, Fingerprint: key.Fingerprint, Audience: key.Audience,
			Actor: "ana", Reason: "still known", Until: alertNow.Add(time.Hour)},
		{At: alertNow.Add(-time.Hour), Kind: MitigationUnsilence, Fingerprint: key.Fingerprint,
			Audience: key.Audience, Actor: "ana"},
	})
	if !states[key].Silenced(alertNow) {
		t.Errorf("the newest silence stands whatever order it arrived in: %+v", states[key])
	}
}

// A recorded transition carries the tier the *rule* declared, and what this
// installation does with it is applied when the row is read.
//
// The failure this pins: the policy used to be applied where a finding was
// stamped, so a condition that opened while the homelab preset was in force
// recorded `ticket` and stayed a ticket after somebody moved the installation
// to `balanced` — until it happened to resolve and reopen. A policy that only
// governs what breaks next is not a policy.
func TestTheRecordedTierIsTheRulesAndThePolicyIsAppliedOnReading(t *testing.T) {
	homelab, _ := Preset(PresetHomelab)
	snapshot := newSnapshot()
	snapshot.Policy = homelab
	snapshot.Pods = []corev1.Pod{waitingPod("CrashLoopBackOff", "back-off 5m0s")}

	round := Catalogue().Evaluate(snapshot)
	recorded := NewTracker(Catalogue()).Observe(round, snapshot.Now)

	var opened *Transition
	for i := range recorded {
		if recorded[i].Signal == SignalCrashLoop && recorded[i].Audience == AudienceDeveloper {
			opened = &recorded[i]
		}
	}
	if opened == nil {
		t.Fatalf("no crash loop was recorded: %+v", recorded)
	}
	if opened.Tier != TierPage {
		t.Fatalf("the history recorded %q, not the tier the rule declares — so the row remembers "+
			"a setting rather than a condition", opened.Tier)
	}

	// Read under each policy: the same recorded row, two answers.
	quiet := Assess(recorded, nil, homelab, snapshot.Now)
	loud := Assess(recorded, nil, DefaultPolicy(), snapshot.Now)
	for _, alert := range quiet {
		if alert.Finding.Signal == SignalCrashLoop && alert.Tier == TierPage {
			t.Error("a page reached a reader on an installation with paging off")
		}
	}
	paged := false
	for _, alert := range loud {
		if alert.Finding.Signal == SignalCrashLoop && alert.Tier == TierPage {
			paged = true
		}
	}
	if !paged {
		t.Error("moving off homelab left a condition that was already open at the lower tier")
	}
}

// A rule that lowers its own tier lowers it on both rows of the condition: an
// instance is not milder for one reader and louder for the other.
func TestALoweredTierReachesBothDeliveries(t *testing.T) {
	scope := Scope{Kind: ScopeEnvironment, Project: testProject, Environment: testEnvironment}
	finding := fire(SignalPVCFilling, SeverityWarning, scope, alertNow.Add(-time.Hour),
		"a volume is filling", "88% of 10Gi used", "")
	finding.Audience = AudienceDeveloper
	finding.Tier = TierLog

	for _, transition := range NewTracker(Catalogue()).Observe(Findings{finding}, alertNow) {
		if transition.Tier != TierLog {
			t.Errorf("the %s delivery recorded %q, not the tier the rule answered with",
				transition.Audience, transition.Tier)
		}
	}
}

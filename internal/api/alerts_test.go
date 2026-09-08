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
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/signals"
)

const (
	alertsPath    = "/api/v1/alerts"
	ackPath       = "/api/v1/alerts/ack"
	silencePath   = "/api/v1/alerts/silence"
	unsilencePath = "/api/v1/alerts/unsilence"
	claimPath     = "/api/v1/alerts/claim"
)

// recordedBoth is the fixtures' crash loop as the loop wrote it: one condition,
// two deliveries, opened long enough ago to have escalated if nobody had
// looked.
func recordedBoth(opened time.Time) []clickhouse.SignalTransition {
	developer := recordedCrashLoop("developer", opened)
	developer.Tier = string(signals.TierPage)
	operator := recordedCrashLoop("operator", opened)
	operator.Tier = string(signals.TierTicket)
	return []clickhouse.SignalTransition{developer, operator}
}

// recordedNodeNotReady is a platform condition scoped to a project's
// namespace: the operator's to act on, and the project's to be told about as a
// symptom.
func recordedNodeNotReady(opened time.Time) clickhouse.SignalTransition {
	const node = "node-1"
	transition := recordedCrashLoop(string(signals.AudienceOperator), opened)
	transition.Signal = "node.notready"
	transition.Fingerprint = "node.notready/" + node
	transition.Node = node
	transition.Title = node + " is not ready"
	return transition
}

// mitigationBody is a write's request body.
func mitigationBody(audience signals.Audience, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"fingerprint": %q, "audience": %q%s}`, recordedFingerprint, audience, extra)
}

// A condition nobody has acknowledged reads at the tier its rule declared for
// each audience, which is the whole model in one assertion: the same
// fingerprint, two rows, two tiers.
func TestAlertsCarryATierPerAudience(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = recordedBoth(time.Now().Add(-2 * time.Minute))

	body := decode[alertsBody](t, h.do(t, http.MethodGet, alertsPath, ""))
	if body.Source != sourceRecorded {
		t.Fatalf("the recorded history is what an alert can be acted on from: %+v", body)
	}
	tiers := map[signals.Audience]signals.Tier{}
	for _, item := range body.Items {
		tiers[item.Audience] = item.Tier
	}
	if tiers[signals.AudienceDeveloper] != signals.TierPage {
		t.Errorf("the developer owns this condition and reads it at the top tier: %+v", body.Items)
	}
	if tiers[signals.AudienceOperator] != signals.TierTicket {
		t.Errorf("the operator wants to know a tenant is down without owning the fix: %+v", body.Items)
	}
	if body.Counts.Page != 1 || body.Counts.Ticket != 1 {
		t.Errorf("the headline counts by the tier each row is read at: %+v", body.Counts)
	}
}

// The pair is the key, and this is what it is for. A member acknowledging
// their project's row must not acknowledge the operator's row about the same
// condition — otherwise escalation, which is explicitly about *nobody* having
// acknowledged, is satisfied by the wrong person.
func TestAnAcknowledgementIsPerDeliveryAndNotPerCondition(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = recordedBoth(time.Now().Add(-2 * time.Minute))

	res := h.do(t, http.MethodPost, ackPath, mitigationBody(signals.AudienceDeveloper, ""))
	if res.Code != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", ackPath, res.Code, res.Body.String())
	}
	acked := decode[alertView](t, res)
	if acked.Mitigation == nil || !acked.Mitigation.Acknowledged {
		t.Fatalf("the answer is the delivery as it now reads: %+v", acked)
	}
	if acked.Tier != signals.TierTicket {
		t.Errorf("somebody is on it, so a page becomes a ticket — a fix is still owed: %+v", acked)
	}

	if len(h.logs.mitigationWritten) != 1 {
		t.Fatalf("one record, about one delivery: %+v", h.logs.mitigationWritten)
	}
	written := h.logs.mitigationWritten[0]
	if written.Audience != string(signals.AudienceDeveloper) {
		t.Errorf("the record names the delivery it is about: %+v", written)
	}
	if written.Source != signals.SourceExplicit {
		t.Errorf("somebody pressed the button, and the record says so: %+v", written)
	}

	// And the operator's row is untouched — it is still theirs to read at the
	// tier the rule declared.
	body := decode[alertsBody](t, h.do(t, http.MethodGet, alertsPath, ""))
	for _, item := range body.Items {
		if item.Audience != signals.AudienceOperator {
			continue
		}
		if item.Mitigation != nil && item.Mitigation.Acknowledged {
			t.Errorf("a member acked their own row and not the operator's: %+v", item)
		}
	}
}

// The other half of the same rule, and the one the project-scoping sentence is
// protecting: a member may silence their own project's signal, and it must
// never reach the operator's row about the same condition.
func TestASilenceIsProjectScopedAndBounded(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = recordedBoth(time.Now().Add(-2 * time.Minute))

	until := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	silence := mitigationBody(signals.AudienceDeveloper,
		fmt.Sprintf(`"reason": "waiting on the upstream fix", "until": %q`, until))
	res := h.do(t, http.MethodPost, silencePath, silence)
	if res.Code != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", silencePath, res.Code, res.Body.String())
	}
	if quiet := decode[alertView](t, res); quiet.Tier != signals.TierLog {
		t.Errorf("a silence lowers the delivery it names to a log: %+v", quiet)
	}

	// The operator's row is where it was: loud, at the tier its rule declared.
	body := decode[alertsBody](t, h.do(t, http.MethodGet, alertsPath, ""))
	for _, item := range body.Items {
		if item.Audience == signals.AudienceOperator && item.Tier != signals.TierTicket {
			t.Errorf("a member's silence reached across to the operator's row: %+v", item)
		}
	}

	// A silence with no reason and no end is a rule nobody remembers making.
	for name, request := range map[string]string{
		"no reason": mitigationBody(signals.AudienceDeveloper, fmt.Sprintf(`"until": %q`, until)),
		"no expiry": mitigationBody(signals.AudienceDeveloper, `"reason": "later"`),
		"an expiry in the past": mitigationBody(signals.AudienceDeveloper, fmt.Sprintf(
			`"reason": "later", "until": %q`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))),
		"an expiry past the bound": mitigationBody(signals.AudienceDeveloper, fmt.Sprintf(
			`"reason": "later", "until": %q`, time.Now().Add(90*24*time.Hour).UTC().Format(time.RFC3339))),
	} {
		t.Run(name, func(t *testing.T) {
			if res := h.do(t, http.MethodPost, silencePath, request); res.Code != http.StatusBadRequest {
				t.Errorf("POST %s with %s = %d, want 400: %s", silencePath, name, res.Code, res.Body.String())
			}
		})
	}
}

// The audience decides who may write, and that is the whole authorization
// rule. A member acting on the operator's delivery is the case it exists for.
func TestTheOperatorsDeliveryIsTheOperatorsToActOn(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = recordedBoth(time.Now().Add(-2 * time.Minute))

	res := h.do(t, http.MethodPost, ackPath, mitigationBody(signals.AudienceOperator, ""))
	if res.Code != http.StatusForbidden {
		t.Fatalf("POST %s as a member = %d, want 403: %s", ackPath, res.Code, res.Body.String())
	}
	// Claiming is the operators' as a group, and refused at the route rather
	// than in the handler.
	if res := h.do(t, http.MethodPost, claimPath, `{"fingerprint": "x"}`); res.Code != http.StatusForbidden {
		t.Errorf("POST %s as a member = %d, want 403: %s", claimPath, res.Code, res.Body.String())
	}
}

// A member may not act on another project's condition, and is told it does not
// exist rather than that they may not — the same answer every other read gives.
func TestAnotherProjectsAlertsAreNotVisible(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = recordedBoth(time.Now().Add(-2 * time.Minute))

	body := decode[alertsBody](t, h.do(t, http.MethodGet, alertsPath+"?project="+otherProject, ""))
	if len(body.Items) != 0 {
		t.Errorf("a project this caller cannot see answers with nothing: %+v", body.Items)
	}
}

// Any operator claims the escalated ticket. There is no rota: escalation
// addresses one ticket to the operators as a group, and this is one of them
// taking it.
func TestAnyOperatorClaimsTheEscalatedTicket(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = recordedBoth(time.Now().Add(-2 * time.Hour))

	res := h.do(t, http.MethodPost, claimPath, fmt.Sprintf(`{"fingerprint": %q}`, recordedFingerprint))
	if res.Code != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", claimPath, res.Code, res.Body.String())
	}
	claimed := decode[alertView](t, res)
	if claimed.Mitigation == nil || claimed.Mitigation.ClaimedBy != testCaller {
		t.Fatalf("the claim names who took it: %+v", claimed)
	}
	if claimed.Audience != signals.AudienceOperator {
		t.Errorf("a claim is about the operator's delivery and no other: %+v", claimed)
	}
}

// Past the escalation window with nobody acknowledging, the condition does not
// become louder for its owner — nothing is more broken at hour four than at
// hour one — it repeats, and adds the operator as a ticket with the sentence
// saying why.
func TestAnUnacknowledgedConditionAddsTheOperatorAsATicket(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = recordedBoth(time.Now().Add(-2 * signals.EscalationWindow))

	body := decode[alertsBody](t, h.do(t, http.MethodGet, alertsPath, ""))
	for _, item := range body.Items {
		if !item.Escalated {
			t.Errorf("both rows of an unattended condition are marked: %+v", item)
		}
		if item.Note == "" {
			t.Errorf("the escalated row carries the sentence saying why: %+v", item)
		}
	}
	// And past a multiple of the same window it stops being an alert and
	// becomes a line on the compliance posture.
	h.logs.openTransitions = recordedBoth(time.Now().Add(-2 * signals.UntendedAfter))
	posture := decode[complianceBody](t, h.do(t, http.MethodGet, "/api/v1/compliance", ""))
	if len(posture.Incidents.Untended) == 0 {
		t.Fatalf("an untended incident is a fact about the institution: %+v", posture.Incidents)
	}
	if posture.Incidents.Untended[0].Project != feedProject {
		t.Errorf("the line has names on it: %+v", posture.Incidents.Untended[0])
	}
}

// A platform condition degrading a project reaches that project
// symptom-shaped: no Kubernetes noun, nothing to press, and nothing to silence.
func TestAPlatformConditionReachesTheProjectAsASymptom(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	recordRound(t, h, time.Now().Add(-30*time.Second))

	platform := recordedNodeNotReady(time.Now().Add(-4 * time.Minute))
	platform.Tier = string(signals.TierPage)
	h.logs.openTransitions = []clickhouse.SignalTransition{platform}

	body := decode[alertsBody](t, h.do(t, http.MethodGet, alertsPath, ""))
	if len(body.Items) != 1 {
		t.Fatalf("the project is told something rather than nothing: %+v", body.Items)
	}
	symptom := body.Items[0]
	switch {
	case !symptom.Symptom || symptom.Actionable:
		t.Errorf("a symptom row is derived and carries no controls: %+v", symptom)
	case symptom.Scope.Node != "":
		t.Errorf("no Kubernetes noun reaches this row: %+v", symptom)
	case symptom.Detail != "a platform issue, the operator has been notified":
		t.Errorf("the row is the symptom, in the project's own words: %+v", symptom)
	case symptom.Evidence != "":
		t.Errorf("a link to a screen this reader cannot open is a button they cannot press: %+v", symptom)
	}

	// And nothing about it can be recorded: its fingerprint is derived and was
	// never a delivery.
	res := h.do(t, http.MethodPost, ackPath, fmt.Sprintf(
		`{"fingerprint": %q, "audience": "developer"}`, symptom.Fingerprint))
	if res.Code != http.StatusNotFound {
		t.Errorf("POST %s for a symptom = %d, want 404: %s", ackPath, res.Code, res.Body.String())
	}
}

// With nothing recording, the screen still answers — an empty alerts list
// because detection is off would be the strongest claim this platform makes,
// made about nothing — but nothing on it can be acted on, and it says so.
func TestAlertsFallBackToAnEvaluatedRoundAndRefuseWrites(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), crashLoopingPod("shop-production-7d9f4"))...)

	body := decode[alertsBody](t, h.do(t, http.MethodGet, alertsPath, ""))
	if body.Source != sourceEvaluated {
		t.Fatalf("a platform with no history evaluates a round: %+v", body)
	}
	if body.Message == "" {
		t.Errorf("and says why nothing on it can be acknowledged: %+v", body)
	}
	if len(body.Items) == 0 {
		t.Errorf("a crash-looping container is still a condition: %+v", body)
	}

	res := h.do(t, http.MethodPost, ackPath, mitigationBody(signals.AudienceOperator, ""))
	if res.Code != http.StatusConflict {
		t.Errorf("POST %s with nothing recording = %d, want 409: %s", ackPath, res.Code, res.Body.String())
	}
}

// Starting the resolving action records an acknowledgement, because the
// alternative is a condition somebody is actively fixing going on counting as
// untended and escalating underneath them.
func TestRollingBackAcknowledgesTheEnvironmentsOwnConditions(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = recordedBoth(time.Now().Add(-2 * time.Minute))

	res := h.do(t, http.MethodPatch, "/api/v1/environments/"+testEnvironment,
		`{"release":"`+testPreviousRelease+`"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("PATCH environment = %d: %s", res.Code, res.Body.String())
	}
	if len(h.logs.mitigationWritten) != 1 {
		t.Fatalf("the project's own delivery is acknowledged, and only it: %+v", h.logs.mitigationWritten)
	}
	written := h.logs.mitigationWritten[0]
	switch {
	case written.Audience != string(signals.AudienceDeveloper):
		t.Errorf("the operator's row is theirs to acknowledge: %+v", written)
	case written.Source != signals.SourceAction:
		t.Errorf("an implicit acknowledgement says it was one: %+v", written)
	case written.Kind != string(signals.MitigationAck):
		t.Errorf("starting the fix acknowledges rather than silences: %+v", written)
	}
}

// The lift is a record rather than a deletion: the log is append-only, and
// "who decided this should be loud again" is as much a question as who
// quietened it.
func TestASilenceCanBeLiftedBeforeItExpires(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = recordedBoth(time.Now().Add(-2 * time.Minute))

	until := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	silence := mitigationBody(signals.AudienceDeveloper,
		fmt.Sprintf(`"reason": "waiting on the upstream fix", "until": %q`, until))
	if res := h.do(t, http.MethodPost, silencePath, silence); res.Code != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", silencePath, res.Code, res.Body.String())
	}
	res := h.do(t, http.MethodPost, unsilencePath, mitigationBody(signals.AudienceDeveloper, ""))
	if res.Code != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", unsilencePath, res.Code, res.Body.String())
	}
	loud := decode[alertView](t, res)
	if loud.Mitigation != nil && !loud.Mitigation.SilencedUntil.IsZero() {
		t.Errorf("the silence is off: %+v", loud.Mitigation)
	}
	if len(h.logs.mitigationWritten) != 2 {
		t.Errorf("both decisions are on the record, not one overwriting the other: %+v",
			h.logs.mitigationWritten)
	}
}

// The reading on an open row, which is not the row the history keeps (#532).
//
// `pvc.filling` is the case the issue is about and the shape of every rule
// whose whole content is a moving number: the volume was 85% full when the
// condition opened and 89% full ten hours later, and the durable transition —
// written once, at the instant it fired — says 85% for as long as it stays
// open.
const (
	correlatedFingerprint = "platform.correlated/2"
	fillingFingerprint    = "pvc.filling/" + feedProject + "/data-" + feedProject
	fillingWhenOpened     = "volume 85% full"
	fillingNow            = "volume 89% full"
	fillingDetailThen     = "17Gi of 20Gi used on claim data-" + feedProject
	fillingDetailNow      = "17.8Gi of 20Gi used on claim data-" + feedProject
)

// recordedFilling is that condition as the loop wrote it when it fired.
func recordedFilling(opened time.Time) clickhouse.SignalTransition {
	return clickhouse.SignalTransition{
		At:          opened,
		State:       "open",
		Signal:      "pvc.filling",
		Fingerprint: fillingFingerprint,
		Audience:    string(signals.AudienceOperator),
		Tier:        string(signals.TierTicket),
		Version:     1,
		Severity:    string(signals.SeverityWarning),
		Scope:       string(signals.ScopeVolume),
		Project:     feedProject,
		Name:        "data-" + feedProject,
		Title:       fillingWhenOpened,
		Detail:      fillingDetailThen,
		Evidence:    "/platform/storage",
		Since:       opened,
		OpenedAt:    opened,
	}
}

// currentReading is what the tracker would answer for that condition now.
func currentReading(at time.Time) signals.CurrentReading {
	return signals.CurrentReading{
		Finding: signals.Finding{
			Signal:      "pvc.filling",
			Severity:    signals.SeverityWarning,
			Scope:       signals.Scope{Kind: signals.ScopeVolume, Project: feedProject, Name: "data-" + feedProject},
			Audience:    signals.AudienceOperator,
			Tier:        signals.TierTicket,
			Fingerprint: fillingFingerprint,
			Title:       fillingNow,
			Detail:      fillingDetailNow,
		},
		At: at,
	}
}

// trackerReadings is a detection loop this process holds, for the one read the
// alerts list makes of it. `readings` being nil is an ordinary answer and not
// an error — a replica that is not the leader runs no loop at all.
type trackerReadings struct {
	readings map[signals.TransitionKey]signals.CurrentReading
	err      error
	reads    int
}

func (t *trackerReadings) CurrentReadings(
	context.Context,
) (map[signals.TransitionKey]signals.CurrentReading, error) {
	t.reads++
	return t.readings, t.err
}

// The bug, and the fix. The stored row says 85% because that is what the
// volume held when the condition opened; the platform's most recent round says
// 89%, which is what /platform/storage was showing on the next screen. The
// list carries the round's words, and the condition's own identity — when it
// opened, and therefore how long it has been open — is untouched by that.
func TestAnOpenAlertCarriesTheCurrentRoundsReading(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	opened := time.Now().Add(-10 * time.Hour).UTC().Truncate(time.Second)
	round := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	recordRound(t, h, round)
	h.logs.openTransitions = []clickhouse.SignalTransition{recordedFilling(opened)}
	loop := &trackerReadings{readings: map[signals.TransitionKey]signals.CurrentReading{
		{Fingerprint: fillingFingerprint, Audience: signals.AudienceOperator}: currentReading(round),
	}}
	h.server.Detection = loop

	body := decode[alertsBody](t, h.do(t, http.MethodGet, alertsPath, ""))
	if len(body.Items) != 1 {
		t.Fatalf("one open delivery: %+v", body.Items)
	}
	item := body.Items[0]
	if item.Title != fillingNow || item.Detail != fillingDetailNow {
		t.Errorf("the row says what the volume holds now, not what it held when this opened: %+v", item)
	}
	if !item.OpenedAt.Equal(opened) {
		t.Errorf("the reading moved and the condition did not: %v, opened %v", item.OpenedAt, opened)
	}
	if item.Reading != readingRound || !item.ReadingAt.Equal(round) {
		t.Errorf("the row says when its figures are from, so the age beside them is not read as theirs: %+v", item)
	}
	if item.Severity != signals.SeverityWarning || item.Tier != signals.TierTicket {
		t.Errorf("only the words move: severity and tier are the history's, and decide what escalates: %+v", item)
	}
	// The durable row is the record of a moment and is not rewritten for
	// being read: nothing here writes, and what the history holds still says
	// what the condition looked like when it fired.
	if h.logs.openTransitions[0].Title != fillingWhenOpened ||
		h.logs.openTransitions[0].Detail != fillingDetailThen {
		t.Errorf("the transition row is the historical record: %+v", h.logs.openTransitions[0])
	}
	if loop.reads != 1 {
		t.Errorf("one read of the round per answer: %d", loop.reads)
	}
}

// A delivery the loop no longer holds — it resolved between the round that
// recorded it and this read, or this process is not the one running the loop —
// keeps the words the history has. That is the honest answer and it is
// labelled as one: the row says the figures are the ones it opened with, and
// dates them at the opening rather than at now.
func TestAnAlertTheLoopHoldsNoReadingOfKeepsTheOpeningsWords(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	opened := time.Now().Add(-10 * time.Hour).UTC().Truncate(time.Second)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = []clickhouse.SignalTransition{recordedFilling(opened)}
	// A loop that is running and holds some other condition, which is the
	// resolved case: the map answers, and this key is not in it.
	h.server.Detection = &trackerReadings{readings: map[signals.TransitionKey]signals.CurrentReading{
		{Fingerprint: "node.silent/node-b", Audience: signals.AudienceOperator}: currentReading(time.Now()),
	}}

	body := decode[alertsBody](t, h.do(t, http.MethodGet, alertsPath, ""))
	if len(body.Items) != 1 {
		t.Fatalf("one open delivery: %+v", body.Items)
	}
	item := body.Items[0]
	if item.Title != fillingWhenOpened || item.Detail != fillingDetailThen {
		t.Errorf("nothing holds a newer reading, so the row is the history's: %+v", item)
	}
	if item.Reading != readingOpened || !item.ReadingAt.Equal(opened) {
		t.Errorf("and the row says so rather than dating the opening's figures at now: %+v", item)
	}
}

// The same answer with no loop in this process at all: a replica that does not
// hold the lease, or a leader that has restarted and not yet evaluated
// anything. Both are ordinary states and neither is an error — the list is
// what it always was, with the row saying which reading it is.
func TestAnAlertOnAProcessRunningNoRoundSaysWhereItsWordsCameFrom(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	opened := time.Now().Add(-10 * time.Hour).UTC().Truncate(time.Second)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = []clickhouse.SignalTransition{recordedFilling(opened)}
	h.server.Detection = nil

	res := h.do(t, http.MethodGet, alertsPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", alertsPath, res.Code, res.Body.String())
	}
	body := decode[alertsBody](t, res)
	if len(body.Items) != 1 {
		t.Fatalf("one open delivery: %+v", body.Items)
	}
	if body.Items[0].Title != fillingWhenOpened || body.Items[0].Reading != readingOpened {
		t.Errorf("no round of its own is answered with the opening's words, marked as such: %+v", body.Items[0])
	}
}

// A write answers with the delivery as it now reads, and "now" has to mean the
// same thing it means in the list — otherwise a row would change its figures
// under somebody for pressing a button on it.
func TestAnAcknowledgementAnswersWithTheReadingTheListShowed(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	opened := time.Now().Add(-10 * time.Hour).UTC().Truncate(time.Second)
	round := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	recordRound(t, h, round)
	h.logs.openTransitions = []clickhouse.SignalTransition{recordedFilling(opened)}
	h.server.Detection = &trackerReadings{readings: map[signals.TransitionKey]signals.CurrentReading{
		{Fingerprint: fillingFingerprint, Audience: signals.AudienceOperator}: currentReading(round),
	}}

	body := fmt.Sprintf(`{"fingerprint": %q, "audience": %q}`, fillingFingerprint, signals.AudienceOperator)
	res := h.do(t, http.MethodPost, ackPath, body)
	if res.Code != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", ackPath, res.Code, res.Body.String())
	}
	acked := decode[alertView](t, res)
	if acked.Title != fillingNow || acked.Reading != readingRound || !acked.ReadingAt.Equal(round) {
		t.Errorf("the row that comes back is the row that was pressed: %+v", acked)
	}
	if !acked.OpenedAt.Equal(opened) {
		t.Errorf("acknowledging a condition does not restart it: %+v", acked)
	}
}

// The affected set travels with the sentence that describes it.
//
// `platform.correlated` is raised at a rung the round decides — its
// confidence, its projects and the rules it stands in front of are all
// recomputed every evaluation — so a row whose words said "four projects" over
// the two the history recorded would be the same disagreement one field
// further in.
func TestACorrelationsAffectedSetMovesWithItsSentence(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	opened := time.Now().Add(-40 * time.Minute).UTC().Truncate(time.Second)
	round := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	recordRound(t, h, round)

	recorded := recordedFilling(opened)
	recorded.Signal = "platform.correlated"
	recorded.Fingerprint = correlatedFingerprint
	recorded.Scope = string(signals.ScopePlatform)
	recorded.Project = ""
	recorded.Name = ""
	recorded.Title = "2 projects failing together"
	recorded.Detail = "no shared node, no shared dependency, and no change of ours"
	recorded.Confidence = string(signals.ConfidenceCoincidence)
	recorded.Projects = feedProject + ",billing"
	recorded.Correlates = "workload.crashloop"
	h.logs.openTransitions = []clickhouse.SignalTransition{recorded}

	current := currentReading(round)
	current.Finding.Signal = "platform.correlated"
	current.Finding.Scope = signals.Scope{Kind: signals.ScopePlatform}
	current.Finding.Fingerprint = correlatedFingerprint
	current.Finding.Title = "4 projects failing together"
	current.Finding.Detail = "all four depend on the same claim"
	current.Finding.Confidence = signals.ConfidenceDependency
	current.Finding.Projects = []string{feedProject, "billing", "docs", "search"}
	current.Finding.Correlates = []signals.ID{"workload.crashloop", "pvc.filling"}
	h.server.Detection = &trackerReadings{readings: map[signals.TransitionKey]signals.CurrentReading{
		{Fingerprint: correlatedFingerprint, Audience: signals.AudienceOperator}: current,
	}}

	body := decode[alertsBody](t, h.do(t, http.MethodGet, alertsPath, ""))
	if len(body.Items) != 1 {
		t.Fatalf("one open delivery: %+v", body.Items)
	}
	item := body.Items[0]
	if item.Title != "4 projects failing together" {
		t.Fatalf("the sentence is the round's: %+v", item)
	}
	if len(item.Projects) != 4 {
		t.Errorf("and it is a sentence about the round's affected set: %+v", item.Projects)
	}
	if item.Confidence != signals.ConfidenceDependency {
		t.Errorf("the rung the round climbed to, not the one it opened at: %+v", item)
	}
	if len(item.Correlates) != 2 {
		t.Errorf("the rules the correlation stands in front of move with it: %+v", item.Correlates)
	}
	if !item.OpenedAt.Equal(opened) {
		t.Errorf("a wider correlation is the same condition, still open since it opened: %+v", item)
	}
}

// A symptom row is the platform's own sentence about a project rather than a
// reading of anything, so it carries neither field — and `readingAt` has to be
// absent rather than the zero instant, which is what `omitempty` on a struct
// would have served.
func TestASymptomRowCarriesNoReading(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	recordRound(t, h, time.Now().Add(-30*time.Second))
	h.logs.openTransitions = []clickhouse.SignalTransition{
		recordedNodeNotReady(time.Now().Add(-4 * time.Minute)),
	}

	res := h.do(t, http.MethodGet, alertsPath, "")
	raw := res.Body.String()
	body := decode[alertsBody](t, res)
	if len(body.Items) != 1 || !body.Items[0].Symptom {
		t.Fatalf("a member sees the symptom row and not the condition: %+v", body.Items)
	}
	if body.Items[0].Reading != "" {
		t.Errorf("the platform's own words are not a reading of anything: %+v", body.Items[0])
	}
	if strings.Contains(raw, "readingAt") || strings.Contains(raw, "0001-01-01") {
		t.Errorf("an absent instant is absent, not the zero one: %s", raw)
	}
}

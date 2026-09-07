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
	"fmt"
	"net/http"
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

	platform := recordedCrashLoop("operator", time.Now().Add(-4*time.Minute))
	platform.Signal = "node.notready"
	platform.Fingerprint = "node.notready/node-1"
	platform.Tier = string(signals.TierPage)
	platform.Node = "node-1"
	platform.Title = "node-1 is not ready"
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

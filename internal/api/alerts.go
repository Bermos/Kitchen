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
	"sort"
	"strings"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/signals"
)

// The alerts surface: one open delivery per row, at the tier its reader is
// meant to read it at, with what anybody has done about it and the controls
// that do something about it.
//
// # Why this is not GET /platform/signals with more fields
//
// A finding is one row per (signal, scope). A *delivery* is one row per
// (fingerprint, audience), and the two are not the same object: a
// developer-audience condition is delivered twice, at two tiers, to two
// readers, in two vocabularies, and is acknowledged and silenced separately.
// The two signals endpoints answer "what is wrong" and are unchanged; this one
// answers "and what am I meant to do about it", which is a question only the
// pair has an answer to.
//
// # What a caller sees
//
// A member sees their projects' developer deliveries, plus the symptom rows —
// a platform condition degrading one of their projects, said in their own
// vocabulary with no Kubernetes noun and nothing to press. An operator sees
// every delivery of both audiences, and therefore no symptoms: they have the
// condition itself, and a derived restatement of a row already in the list
// would be the same fact twice.
//
// # Why the writes go against the recorded history alone
//
// An acknowledgement is a record about a delivery, and a delivery the platform
// never recorded has nothing for the record to be about: nothing would read it
// back, and the escalation clock it is meant to stop is measured from an
// `openedAt` only the history has. So the four writes resolve their subject in
// `signal_transitions` and refuse otherwise, saying which of the two reasons it
// is — nothing is recording, or that delivery is not open.

// listAlerts answers both alerts screens: the fleet's, and a project's.
func (s *Server) listAlerts(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	project := strings.TrimSpace(req.URL.Query().Get("project"))

	round, err := s.alertRound(ctx)
	if err != nil {
		s.writeStoreError(w, err, "the alerts read")
		return
	}
	if project != "" && !scopeFrom(ctx).allows(project) {
		// The same answer a project nobody may see gets everywhere else: not
		// a refusal, because "you may not know whether billing exists" is the
		// answer on a platform where developers do not see each other's work.
		writeJSON(w, http.StatusOK, alertsBody{Items: []alertView{}, Source: round.source, EvaluatedAt: round.at})
		return
	}

	body := alertsBody{
		Items:       []alertView{},
		Source:      round.source,
		EvaluatedAt: round.at,
		Message:     round.message,
		Project:     project,
	}
	for _, alert := range s.visibleAlerts(ctx, round, project) {
		body.Items = append(body.Items, alertViewOf(alert))
		switch alert.Tier {
		case signals.TierPage:
			body.Counts.Page++
		case signals.TierTicket:
			body.Counts.Ticket++
		case signals.TierLog:
			body.Counts.Log++
		}
		if alert.Untended {
			body.Counts.Untended++
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// alertsBody is one answer.
type alertsBody struct {
	Items []alertView `json:"items"`
	// Counts is the headline, by the tier each row is *read* at rather than
	// by the one its rule declared: a screen ordering itself by urgency has
	// to count the same way it sorts.
	Counts alertCounts `json:"counts"`

	// Source is `recorded` for the background loop's history and `evaluated`
	// for a round taken to answer this request. It is the difference between
	// a list whose rows can be acknowledged and one whose rows cannot, so it
	// is served rather than inferred.
	Source string `json:"source"`
	// EvaluatedAt is when the round this answers from was taken.
	EvaluatedAt time.Time `json:"evaluatedAt"`
	// Message explains an answer that is thinner than it should be: nothing
	// is recording, or the mitigation records could not be read.
	Message string `json:"message,omitempty"`

	Project string `json:"project,omitempty"`
}

// alertCounts is a round by tier, plus the one number that is not a tier.
type alertCounts struct {
	Page   int `json:"page"`
	Ticket int `json:"ticket"`
	Log    int `json:"log"`
	// Untended is how many have gone unacknowledged past the point where
	// they stop being an alert and become a finding about the institution.
	Untended int `json:"untended"`
}

// alertView is one delivery on the wire: the finding, at the tier this reader
// reads it at, with what stands about it.
//
// The embedded finding's `tier` is the *effective* one — after a silence, an
// acknowledgement and the clock — because that is what the row is rendered and
// sorted by. `baseTier` is what the rule declared, kept beside it so a screen
// can say "a page, held down because somebody is on it" rather than only the
// answer.
type alertView struct {
	signals.Finding

	Base signals.Tier `json:"baseTier,omitempty"`

	// OpenedAt is when the platform first saw the condition, and
	// UnmitigatedSeconds how long it has been open with nobody
	// acknowledging it — zero once anybody has.
	OpenedAt           time.Time `json:"openedAt,omitempty"`
	UnmitigatedSeconds int64     `json:"unmitigatedSeconds,omitempty"`

	Escalated bool `json:"escalated,omitempty"`
	Untended  bool `json:"untended,omitempty"`
	// Note is the escalation sentence, in the issue's words.
	Note string `json:"note,omitempty"`

	// Symptom marks a derived row: a platform condition degrading this
	// project, said in the project's own vocabulary. Nothing about it is
	// recorded and nothing on it is actionable.
	Symptom bool `json:"symptom,omitempty"`
	// Actionable is whether the controls apply to this row at all.
	Actionable bool `json:"actionable"`

	Mitigation *mitigationView `json:"mitigation,omitempty"`
}

// mitigationView is what stands about one delivery.
type mitigationView struct {
	Acknowledged   bool      `json:"acknowledged,omitempty"`
	AcknowledgedBy string    `json:"acknowledgedBy,omitempty"`
	AcknowledgedAt time.Time `json:"acknowledgedAt,omitempty"`
	// AcknowledgedVia is `explicit` when somebody pressed the button and
	// `action` when starting the fix recorded it for them, so a screen can
	// say "rolled back" where that is what happened.
	AcknowledgedVia string `json:"acknowledgedVia,omitempty"`

	SilencedBy    string    `json:"silencedBy,omitempty"`
	SilencedAt    time.Time `json:"silencedAt,omitempty"`
	SilenceReason string    `json:"silenceReason,omitempty"`
	SilencedUntil time.Time `json:"silencedUntil,omitempty"`

	ClaimedBy string    `json:"claimedBy,omitempty"`
	ClaimedAt time.Time `json:"claimedAt,omitempty"`
}

func alertViewOf(alert signals.Alert) alertView {
	finding := alert.Finding
	finding.Tier = alert.Tier
	view := alertView{
		Finding:    finding,
		Base:       alert.Base,
		OpenedAt:   alert.OpenedAt,
		Escalated:  alert.Escalated,
		Untended:   alert.Untended,
		Note:       alert.Note,
		Symptom:    alert.Symptom,
		Actionable: alert.Actionable,
	}
	if alert.Unmitigated > 0 {
		view.UnmitigatedSeconds = int64(alert.Unmitigated / time.Second)
	}
	if state := alert.Mitigation; state != (signals.MitigationState{}) {
		view.Mitigation = &mitigationView{
			Acknowledged:    state.Acknowledged,
			AcknowledgedBy:  state.AcknowledgedBy,
			AcknowledgedAt:  state.AcknowledgedAt,
			AcknowledgedVia: state.AcknowledgedBySource,
			SilencedBy:      state.SilencedBy,
			SilencedAt:      state.SilencedAt,
			SilenceReason:   state.SilenceReason,
			SilencedUntil:   state.SilencedUntil,
			ClaimedBy:       state.ClaimedBy,
			ClaimedAt:       state.ClaimedAt,
		}
	}
	return view
}

// alertRound is every open delivery on the platform, assessed — before any
// question of who is allowed to read which of them.
type alertRound struct {
	open    []signals.Transition
	alerts  []signals.Alert
	at      time.Time
	source  string
	message string
	now     time.Time
}

// alertRound reads the open deliveries and folds in what people have done.
//
// The recorded history first, exactly as the signals endpoints resolve it, so
// that the two screens cannot disagree about what is open. Where there is no
// current history it evaluates a round instead — which is honest and thin: the
// conditions are right, and nothing carries an age, because how long something
// has been true is the one thing an evaluator that runs when somebody looks
// cannot know. The rows are then not actionable, and `message` says so.
func (s *Server) alertRound(ctx context.Context) (alertRound, error) {
	now := time.Now().UTC()
	round := alertRound{now: now}

	store, storeErr := s.logStore(ctx)
	if status, ok := s.currentRound(ctx); ok && storeErr == nil {
		rows, err := store.OpenSignalTransitions(ctx)
		if err != nil {
			return alertRound{}, err
		}
		round.open = signals.TransitionsFrom(rows)
		round.at = status.LastEvaluated.Time
		round.source = sourceRecorded
	} else {
		snapshot := signals.Gather(ctx, s.signalSources(ctx), signals.Options{})
		findings := signals.Catalogue().Evaluate(snapshot).Firing()
		// A fresh tracker reports every condition as newly opened, which is
		// exactly what this round knows: these are open, and this platform
		// has no record of when they became so.
		round.open = signals.NewTracker(signals.Catalogue()).Observe(findings, snapshot.Now)
		round.at = snapshot.Now
		round.source = sourceEvaluated
		round.message = "background evaluation is not recording, so these conditions carry no history: " +
			"nothing can be acknowledged or silenced until it does"
	}

	states := map[signals.TransitionKey]signals.MitigationState{}
	if storeErr == nil && round.source == sourceRecorded {
		records, err := store.SignalMitigations(ctx)
		if err != nil {
			// A list that silently forgot every silence would be worse than
			// one that says it could not read them: a condition somebody
			// quietened would come back loud with no explanation.
			round.message = "what people have done about these conditions could not be read, " +
				"so every row is shown as if nothing were mitigating it"
		} else {
			states = signals.FoldMitigations(signals.MitigationsFrom(records))
		}
	}

	round.alerts = signals.Assess(round.open, states, s.signalPolicy(ctx), now)
	if round.source != sourceRecorded {
		// Nothing on an evaluated round can be acted on, and the rows say so
		// themselves rather than only in the message above them: a client
		// that offered a button here would be offering one the write refuses.
		for i := range round.alerts {
			round.alerts[i].Actionable = false
		}
	}
	return round, nil
}

// visibleAlerts narrows a round to what one caller may read, and adds the rows
// that exist only for them.
//
// The symptom rows are the second half. They are derived from the operator's
// own deliveries, so a caller who can already see those gets the condition
// rather than a restatement of it — which is why the operator's list has none.
func (s *Server) visibleAlerts(ctx context.Context, round alertRound, project string) []signals.Alert {
	scope := scopeFrom(ctx)
	operator := platformRoleFrom(ctx).AtLeast(access.PlatformOperator)

	visible := make([]signals.Alert, 0, len(round.alerts))
	for _, alert := range round.alerts {
		if project != "" && alert.Finding.Scope.Project != project {
			continue
		}
		switch alert.Finding.Audience {
		case signals.AudienceOperator:
			if !operator {
				continue
			}
		case signals.AudienceDeveloper:
			if !scope.allows(alert.Finding.Scope.Project) {
				continue
			}
		}
		visible = append(visible, alert)
	}

	if !operator {
		for _, symptom := range signals.Symptoms(round.open, project, round.now) {
			if scope.allows(symptom.Finding.Scope.Project) {
				visible = append(visible, symptom)
			}
		}
	}

	signals.SortAlerts(visible)
	return visible
}

// The four writes. Each of them is a record about a delivery and never a
// lifecycle: the tier is a classification, and ack, silence and claim are
// things people did about a condition rather than a ticket's status field.

// mitigationRequest is the body all four take.
type mitigationRequest struct {
	// Fingerprint and Audience name the delivery. Both are required, because
	// half of the key is the half that lets a member acknowledge a row that
	// is not theirs.
	Fingerprint string `json:"fingerprint"`
	Audience    string `json:"audience"`

	// Reason is why. A silence must carry one.
	Reason string `json:"reason,omitempty"`
	// Until is when a silence expires, RFC 3339. Silences only.
	Until string `json:"until,omitempty"`
}

// ackAlert records that somebody has seen a condition.
func (s *Server) ackAlert(w http.ResponseWriter, req *http.Request) {
	s.writeMitigation(w, req, signals.MitigationAck)
}

// silenceAlert quietens one delivery, with a reason and an end.
func (s *Server) silenceAlert(w http.ResponseWriter, req *http.Request) {
	s.writeMitigation(w, req, signals.MitigationSilence)
}

// unsilenceAlert lifts a silence before its expiry. It is a record rather than
// a deletion: the log is append-only, and "who decided this should be loud
// again" is as much a question as who quietened it.
func (s *Server) unsilenceAlert(w http.ResponseWriter, req *http.Request) {
	s.writeMitigation(w, req, signals.MitigationUnsilence)
}

// claimAlert is an operator taking the escalated ticket.
//
// Any operator, and there is no rota: escalation addresses one ticket to the
// operators as a group, and a claim is the record of one of them taking it.
// Deliberately not restricted to a delivery that has already escalated —
// refusing an early claim would be refusing somebody saying "I have this"
// before the clock agreed with them.
func (s *Server) claimAlert(w http.ResponseWriter, req *http.Request) {
	s.writeMitigation(w, req, signals.MitigationClaim)
}

func (s *Server) writeMitigation(w http.ResponseWriter, req *http.Request, kind signals.MitigationKind) {
	ctx := req.Context()

	body := mitigationRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Fingerprint = strings.TrimSpace(body.Fingerprint)
	body.Audience = strings.TrimSpace(body.Audience)
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Fingerprint == "" {
		badRequest(w, "which condition: `fingerprint` is required")
		return
	}
	if kind == signals.MitigationClaim {
		// A claim is the operators' own ticket being taken. There is no
		// other row it could be about, so the field is not asked for.
		if body.Audience != "" && body.Audience != string(signals.AudienceOperator) {
			badRequest(w, "a claim is the operator's ticket being taken; it has no %s row to claim",
				body.Audience)
			return
		}
		body.Audience = string(signals.AudienceOperator)
	}
	audience := signals.Audience(body.Audience)
	if audience != signals.AudienceDeveloper && audience != signals.AudienceOperator {
		badRequest(w, "which delivery: `audience` must be %q or %q, and one condition's two "+
			"deliveries are acknowledged and silenced separately",
			signals.AudienceDeveloper, signals.AudienceOperator)
		return
	}

	now := time.Now().UTC()
	until := time.Time{}
	if kind == signals.MitigationSilence {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(body.Until))
		if body.Until != "" && err != nil {
			badRequest(w, "`until` must be an RFC 3339 instant: %s", err.Error())
			return
		}
		until = parsed.UTC()
		if err := signals.ValidateSilence(body.Reason, until, s.signalPolicy(ctx), now); err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
	}

	delivery, ok := s.openDelivery(w, req, body.Fingerprint, audience)
	if !ok {
		return
	}
	if !s.mayMitigate(w, req, delivery, kind) {
		return
	}

	caller, _ := CallerFrom(ctx)
	record := signals.Mitigation{
		At:          now,
		Kind:        kind,
		Fingerprint: delivery.Fingerprint,
		Audience:    delivery.Audience,
		Project:     delivery.Scope.Project,
		Actor:       callerName(caller),
		Reason:      body.Reason,
		Until:       until,
		Source:      signals.SourceExplicit,
	}
	if !s.recordMitigation(w, req, record, delivery) {
		return
	}

	store, err := s.logStore(ctx)
	if err != nil {
		s.writeStoreError(w, err, "recording what was done about this condition")
		return
	}
	if err := store.InsertSignalMitigation(ctx, signals.MitigationRow(record)); err != nil {
		s.writeStoreError(w, err, "recording what was done about this condition")
		return
	}

	// The delivery as it now reads, so a screen does not have to guess what
	// its own write did to the tier.
	states := map[signals.TransitionKey]signals.MitigationState{}
	if records, err := store.SignalMitigations(ctx); err == nil {
		states = signals.FoldMitigations(signals.MitigationsFrom(records))
	}
	for _, alert := range signals.Assess([]signals.Transition{delivery}, states, s.signalPolicy(ctx), now) {
		if alert.Key() == delivery.Key() {
			writeJSON(w, http.StatusOK, alertViewOf(alert))
			return
		}
	}
	writeJSON(w, http.StatusOK, alertViewOf(signals.Alert{Finding: delivery.Finding()}))
}

// openDelivery resolves the recorded delivery a write names, or answers the
// request itself with why it could not.
func (s *Server) openDelivery(
	w http.ResponseWriter, req *http.Request, fingerprint string, audience signals.Audience,
) (signals.Transition, bool) {
	ctx := req.Context()

	if _, current := s.currentRound(ctx); !current {
		writeJSON(w, http.StatusConflict, errorBody{
			Error: "nothing is recording what the catalogue finds, so there is no delivery to " +
				"record this against: switch background evaluation on " +
				"(spec.observability.signals) and give the platform a telemetry store",
		})
		return signals.Transition{}, false
	}
	store, err := s.logStore(ctx)
	if err != nil {
		s.writeStoreError(w, err, "the alerts read")
		return signals.Transition{}, false
	}
	rows, err := store.OpenSignalTransitions(ctx)
	if err != nil {
		s.writeStoreError(w, err, "the alerts read")
		return signals.Transition{}, false
	}
	for _, transition := range signals.TransitionsFrom(rows) {
		if transition.Fingerprint == fingerprint && transition.Audience == audience {
			return transition, true
		}
	}
	writeJSON(w, http.StatusNotFound, errorBody{
		Error: fmt.Sprintf("no %s delivery of %q is open — a condition that has resolved, "+
			"or a row this platform never recorded", audience, fingerprint),
	})
	return signals.Transition{}, false
}

// mayMitigate decides whether this caller may write this record, and refuses
// with the reason when they may not.
//
// The audience decides, and that is the whole rule: a project's member writes
// about their project's delivery, an operator about the operator's. It is the
// same sentence the key exists to make sayable — a member acking their
// project's row must not acknowledge the operator's row about the same
// condition, and a member silencing their own must not silence the operator's.
func (s *Server) mayMitigate(
	w http.ResponseWriter, req *http.Request, delivery signals.Transition, kind signals.MitigationKind,
) bool {
	ctx := req.Context()
	caller, _ := CallerFrom(ctx)

	if delivery.Audience == signals.AudienceOperator {
		if !platformRoleFrom(ctx).AtLeast(access.PlatformOperator) {
			forbidden(w, fmt.Sprintf(
				"this is the operator's delivery of that condition, and %s it needs the operator role; "+
					"your project's own row is the one to act on", verbFor(kind)))
			return false
		}
		return true
	}

	// A developer delivery always names the project it is about; one that
	// somehow does not is nobody's to act on rather than everybody's.
	if delivery.Scope.Project == "" {
		forbidden(w, "that delivery names no project, so there is no membership that would "+
			"authorise acting on it")
		return false
	}
	var project *kitchenv1alpha1.Project
	found := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, delivery.Scope.Project, found); err == nil {
		project = found
	}
	role := access.ProjectRoleFor(caller.access(), kitchenFrom(ctx), project)
	if !role.AtLeast(access.ProjectViewer) {
		forbidden(w, fmt.Sprintf("%s a condition of %s needs membership of it; you have %s",
			verbFor(kind), delivery.Scope.Project, role))
		return false
	}
	return true
}

// verbFor completes the refusal sentences above.
func verbFor(kind signals.MitigationKind) string {
	switch kind {
	case signals.MitigationSilence:
		return "silencing"
	case signals.MitigationUnsilence:
		return "lifting the silence on"
	case signals.MitigationClaim:
		return "claiming"
	default:
		return "acknowledging"
	}
}

// recordMitigation appends the record to the audit log, and reports whether
// the caller may go ahead.
//
// It goes in the log for the reason every other decision here does: an
// acknowledgement is somebody saying they have seen an outage, a silence is
// somebody deciding an alert should stop being said, and a claim is somebody
// taking responsibility for one. All three are questions asked months later,
// and the store copy beside them answers "what stands now" rather than "who
// decided what, and when".
func (s *Server) recordMitigation(
	w http.ResponseWriter, req *http.Request, record signals.Mitigation, delivery signals.Transition,
) bool {
	object := s.mitigationSubject(req.Context(), record.Project)
	return s.recorded(w, req, audit.Transition{
		Object:    object,
		Kind:      audit.KindSignalMitigation,
		Operation: clickhouse.AuditUpdate,
		To:        string(record.Kind),
		Project:   record.Project,
		// The delivery, not the condition: one fingerprint is two rows, and a
		// record that named only the fingerprint would read as a decision
		// about both.
		Correlation: record.Fingerprint + "#" + string(record.Audience),
		Reason: fmt.Sprintf("%s %s the %s delivery of %s: %s",
			record.Actor, pastTenseOf(record.Kind), record.Audience, delivery.Signal, delivery.Title),
		Details: mitigationDetails(record, delivery),
	})
}

func mitigationDetails(record signals.Mitigation, delivery signals.Transition) map[string]any {
	details := map[string]any{
		"kind":        string(record.Kind),
		"signal":      string(delivery.Signal),
		"fingerprint": record.Fingerprint,
		"audience":    string(record.Audience),
		"tier":        string(delivery.Tier),
		"source":      record.Source,
	}
	if record.Reason != "" {
		details["reason"] = record.Reason
	}
	if !record.Until.IsZero() {
		details["until"] = record.Until.UTC().Format(time.RFC3339)
	}
	if delivery.Scope.Environment != "" {
		details["environment"] = delivery.Scope.Environment
	}
	return details
}

func pastTenseOf(kind signals.MitigationKind) string {
	switch kind {
	case signals.MitigationSilence:
		return "silenced"
	case signals.MitigationUnsilence:
		return "lifted the silence on"
	case signals.MitigationClaim:
		return "claimed"
	default:
		return "acknowledged"
	}
}

// mitigationSubject is the object the audit record hangs off: the project the
// condition is about, and the platform singleton for a condition that is about
// nobody's project.
//
// The record's own name comes off the object, which is why this is a lookup
// rather than a synthetic object: a record naming a project somebody can open
// is a record that leads somewhere.
func (s *Server) mitigationSubject(ctx context.Context, project string) *kitchenv1alpha1.Project {
	found := &kitchenv1alpha1.Project{}
	if project != "" {
		if err := s.get(ctx, project, found); err == nil {
			return found
		}
	}
	// A stand-in rather than the Kitchen singleton, so the log's `kind` and
	// its object cannot disagree: this record is about a condition, and the
	// object is only what it is attributed to.
	found.Name = project
	if found.Name == "" {
		// The same word every other scope-less record here uses, for the same
		// reason: a condition about nobody's project is the platform's.
		found.Name = subscriptionScopePlatform
	}
	return found
}

// The implicit acknowledgement.
//
// Starting the resolving action — retrying the build, redeploying, rolling
// back — is a stronger statement than pressing Ack, and the alternative is a
// condition somebody is actively fixing going on counting as untended and
// escalating underneath them. So the action records one on their behalf, at
// `source: action`, which is what lets a screen say "rolled back" rather than
// "acknowledged".
//
// It is best-effort and never fails the action it follows: the action itself
// is already recorded, and a redeploy refused because a note about it could not
// be written would be the tail wagging the dog. What it must not do is claim
// to have been written when it was not, which is why the failure is logged.
func (s *Server) ackByAction(ctx context.Context, project, environment, doing string) {
	if project == "" {
		return
	}
	if _, current := s.currentRound(ctx); !current {
		return
	}
	store, err := s.logStore(ctx)
	if err != nil {
		return
	}
	rows, err := store.OpenSignalTransitions(ctx)
	if err != nil {
		s.log().V(1).Info("a resolving action's implicit acknowledgement was not recorded",
			"project", project, "reason", err.Error())
		return
	}

	caller, _ := CallerFrom(ctx)
	actor := callerName(caller)
	now := time.Now().UTC()
	for _, delivery := range signals.TransitionsFrom(rows) {
		// The project's own deliveries alone. The operator's row about the
		// same condition is theirs to acknowledge — that is the whole of the
		// (fingerprint, audience) key — and an environment named in the
		// request narrows it further, so redeploying one preview does not
		// speak for the project's production.
		if delivery.Audience != signals.AudienceDeveloper || delivery.Scope.Project != project {
			continue
		}
		if environment != "" && delivery.Scope.Environment != "" && delivery.Scope.Environment != environment {
			continue
		}
		record := signals.Mitigation{
			At:          now,
			Kind:        signals.MitigationAck,
			Fingerprint: delivery.Fingerprint,
			Audience:    delivery.Audience,
			Project:     delivery.Scope.Project,
			Actor:       actor,
			Reason:      doing,
			Source:      signals.SourceAction,
		}
		if err := store.InsertSignalMitigation(ctx, signals.MitigationRow(record)); err != nil {
			s.log().V(1).Info("a resolving action's implicit acknowledgement was not recorded",
				"fingerprint", delivery.Fingerprint, "reason", err.Error())
			continue
		}
		if err := s.Audit.Record(ctx, audit.Transition{
			Object:      s.mitigationSubject(ctx, project),
			Kind:        audit.KindSignalMitigation,
			Operation:   clickhouse.AuditUpdate,
			Actor:       actor,
			To:          string(signals.MitigationAck),
			Project:     project,
			Correlation: delivery.Fingerprint + "#" + string(delivery.Audience),
			Reason: fmt.Sprintf("%s acknowledged the %s delivery of %s by %s",
				actor, delivery.Audience, delivery.Signal, doing),
			Details: mitigationDetails(record, delivery),
		}); err != nil {
			s.log().V(1).Info("a resolving action's implicit acknowledgement was not recorded in the log",
				"fingerprint", delivery.Fingerprint, "reason", err.Error())
		}
	}
}

// untendedIncidents is the compliance posture's line: the conditions that have
// gone unacknowledged long enough to stop being alerts.
//
// It is on the posture rather than only on the alerts screen because that is
// what the escalation ladder's last rung is *for*. An outage nobody looked at
// for four hours is not a louder alert — nothing is more broken at hour four
// than at hour one — it is a fact about the institution, and the place facts
// about the institution are reported is the posture, with names on them.
func untendedIncidents(alerts []signals.Alert) []untendedIncident {
	incidents := make([]untendedIncident, 0, 4)
	for _, alert := range alerts {
		// The owner's row alone. A condition past the window marks both of
		// its deliveries — that is what "adds the operator as a ticket"
		// means — and an untended *incident* is one thing that happened,
		// not two rows about it.
		if !alert.Untended || alert.Symptom || !alert.Owner {
			continue
		}
		incidents = append(incidents, untendedIncident{
			Signal:      string(alert.Finding.Signal),
			Fingerprint: alert.Finding.Fingerprint,
			Audience:    string(alert.Finding.Audience),
			Project:     alert.Finding.Scope.Project,
			Environment: alert.Finding.Scope.Environment,
			Title:       alert.Finding.Title,
			Note:        alert.Note,
			OpenedAt:    alert.OpenedAt,
			Unmitigated: int64(alert.Unmitigated / time.Second),
		})
	}
	sort.SliceStable(incidents, func(i, j int) bool {
		return incidents[i].Unmitigated > incidents[j].Unmitigated
	})
	return incidents
}

// untendedFromHistory is the posture's own read: the untended incidents, and
// the sentence for a platform that cannot answer the question.
//
// The recorded history alone, deliberately. An untended incident is a claim
// about how long nobody has looked, which is a fact only the history holds —
// an installation with nothing recording has no untended incidents rather than
// none it can see, and the message says which.
func (s *Server) untendedFromHistory(ctx context.Context) ([]untendedIncident, string) {
	if _, current := s.currentRound(ctx); !current {
		return nil, "background evaluation is not recording, so how long a condition has gone " +
			"unacknowledged is not a question this installation can answer"
	}
	store, err := s.logStore(ctx)
	if err != nil {
		return nil, "the signal history could not be read"
	}
	rows, err := store.OpenSignalTransitions(ctx)
	if err != nil {
		return nil, "the signal history could not be read"
	}
	records, err := store.SignalMitigations(ctx)
	if err != nil {
		return nil, "what people have done about these conditions could not be read, so nothing " +
			"here can be said to be untended"
	}
	states := signals.FoldMitigations(signals.MitigationsFrom(records))
	return untendedIncidents(signals.Assess(signals.TransitionsFrom(rows), states,
		s.signalPolicy(ctx), time.Now().UTC())), ""
}

// untendedIncident is one such line.
type untendedIncident struct {
	Signal      string `json:"signal"`
	Fingerprint string `json:"fingerprint"`
	Audience    string `json:"audience"`
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Title       string `json:"title"`
	// Note is the sentence the escalated row carries, which is the one a
	// person reads: `shop / production · unmitigated 4h 12m · nobody has
	// acknowledged`.
	Note        string    `json:"note,omitempty"`
	OpenedAt    time.Time `json:"openedAt,omitempty"`
	Unmitigated int64     `json:"unmitigatedSeconds,omitempty"`
}

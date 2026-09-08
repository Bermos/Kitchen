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
		body.Items = append(body.Items, alertViewOf(alert, round.readingOf(alert)))
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

	// Reading and ReadingAt are where this row's words came from — the
	// title, the detail and, on a correlation, the affected set they
	// describe — and they are here because the row already carries an age
	// that is not theirs. `openedAt` is the condition's, and a volume that opened at 85%
	// and is now at 98% was showing the opening's number beside ten hours of
	// the condition's age with nothing distinguishing the two — which read as
	// the storage screen and the alerts screen disagreeing.
	//
	// `round` is the current round's reading of the condition, dated at the
	// round that took it. `opened` is the one recorded when it fired, which
	// is what a reader gets when this process holds no round of its own: a
	// replica that is not the leader, an operator that has just restarted, or
	// a condition that resolved between the round and this read.
	Reading string `json:"reading,omitempty"`
	// `omitzero` rather than `omitempty`: a time.Time is a struct and a
	// struct is never empty, so omitempty would serve `0001-01-01T00:00:00Z`
	// on every symptom row — which is the one kind of row documented as
	// carrying neither field.
	ReadingAt time.Time `json:"readingAt,omitzero"`

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

func alertViewOf(alert signals.Alert, read reading) alertView {
	finding := alert.Finding
	finding.Tier = alert.Tier
	view := alertView{
		Finding:    finding,
		Base:       alert.Base,
		OpenedAt:   alert.OpenedAt,
		Reading:    read.from,
		ReadingAt:  read.at,
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

// Where a row's title and detail come from, which is the one thing the alerts
// list was not saying.
const (
	// readingRound is the current round's reading of the condition.
	readingRound = "round"
	// readingOpened is the reading recorded when the condition fired, shown
	// when this process has no round of its own to overlay.
	readingOpened = "opened"
)

// reading is that answer for one row: which of the two it is, and when it was
// taken.
type reading struct {
	from string
	at   time.Time
}

// alertRound is every open delivery on the platform, assessed — before any
// question of who is allowed to read which of them.
type alertRound struct {
	open   []signals.Transition
	alerts []signals.Alert
	// readings says, per delivery, where the title and detail on it came
	// from. A key with no entry — a symptom row, whose words are the
	// platform's own and not a reading of anything — carries neither field.
	readings map[signals.TransitionKey]reading
	at       time.Time
	source   string
	message  string
	now      time.Time
}

// readingOf is what to say about where one alert's words came from. Symptom
// rows answer with nothing: their sentence is written by the platform rather
// than read off the cluster, so dating it would be dating a constant.
func (r alertRound) readingOf(alert signals.Alert) reading {
	if alert.Symptom {
		return reading{}
	}
	return r.readings[alert.Key()]
}

// alertRound reads the open deliveries and folds in what people have done.
//
// The recorded history first, exactly as the signals endpoints resolve it, so
// that the two screens cannot disagree about what is open. Where there is no
// current history it evaluates a round instead — which is honest and thin: the
// conditions are right, and nothing carries an age, because how long something
// has been true is the one thing an evaluator that runs when somebody looks
// cannot know. The rows are then not actionable, and `message` says so.
//
// The recorded rows are then given the current round's words. See
// [Server.refreshOpen] for why the history is read for what is open and asked
// again for what it says.
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
		round.readings = s.refreshOpen(ctx, round.open)
	} else {
		snapshot := signals.Gather(ctx, s.signalSources(ctx), signals.Options{})
		findings := signals.Catalogue().Evaluate(snapshot).Firing()
		// A fresh tracker reports every condition as newly opened, which is
		// exactly what this round knows: these are open, and this platform
		// has no record of when they became so.
		round.open = signals.NewTracker(signals.Catalogue()).Observe(findings, snapshot.Now)
		round.at = snapshot.Now
		round.source = sourceEvaluated
		// Every row here was read by the round that is answering the request,
		// so there is no older copy for any of them to be confused with.
		round.readings = make(map[signals.TransitionKey]reading, len(round.open))
		for _, transition := range round.open {
			round.readings[transition.Key()] = reading{from: readingRound, at: snapshot.Now}
		}
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

// refreshOpen gives each recorded open delivery the words of the round the
// platform is in now, and says which reading every row ended up with.
//
// The transitions are the history's, and the history is right to be what it
// is: a row is written once, at the instant the condition opened, and it is
// the record of that moment. But a rule whose whole content is a moving number
// — a volume filling, a node saturating — then shows the least alarming value
// the condition ever had for as long as it stays open, next to an age that
// belongs to the condition and reads as the number's. That is what made the
// alerts screen and the storage screen appear to disagree (#532).
//
// So the durable row is left alone and the reading is overlaid on the way out,
// out of the detection loop's own memory: [signals.Tracker] already refreshes
// every open episode each round, so this costs a map lookup rather than a
// write per round per open finding.
//
// **The sentence and what it is a sentence about, and nothing else.** Title
// and detail move, and so do `confidence`, `projects` and `correlates`: a
// cross-project correlation raised at the second rung names a set that grows
// and shrinks with the round, and a row whose words said *four projects* over
// a list of two would be a worse answer than the stale one it replaced. All
// five are descriptive — none of them is read by [signals.Assess], the
// escalation clock or the sort.
//
// Severity, tier, scope, since, the fingerprint and `openedAt` stay the
// history's. Those are the row's identity and its urgency, and they decide
// what escalates, what pages and what sorts first; a row that quietly
// re-tiered itself between two reads would be a different feature, and one
// whose blast radius is the paging policy. A condition whose severity has
// genuinely moved opens a delivery of its own.
//
// A delivery this process holds no reading of keeps the opening's words and is
// marked as carrying them: a replica that is not the leader runs no loop, a
// leader that has just restarted has seeded itself from the history and
// evaluated nothing yet, and a condition that resolved since the round is gone
// from memory before it is gone from the history. All three are honestly
// answered by "this is what it said when it opened", and never by a guess.
// It writes the words into the transitions it is handed, which are this
// request's own copies of the recorded rows and go no further than the
// response.
func (s *Server) refreshOpen(ctx context.Context, open []signals.Transition) map[signals.TransitionKey]reading {
	readings := make(map[signals.TransitionKey]reading, len(open))
	current := s.currentReadings(ctx)
	for i := range open {
		key := open[i].Key()
		latest, held := current[key]
		if !held {
			readings[key] = reading{from: readingOpened, at: openedInstant(open[i])}
			continue
		}
		open[i].Title = latest.Finding.Title
		open[i].Detail = latest.Finding.Detail
		// The affected set travels with the sentence that describes it. A
		// correlation's rung, its projects and the rules it stands in front
		// of are what the sentence is *about*, and leaving them behind would
		// re-create the disagreement one field further in.
		open[i].Confidence = latest.Finding.Confidence
		open[i].Projects = latest.Finding.Projects
		open[i].Correlates = latest.Finding.Correlates
		readings[key] = reading{from: readingRound, at: latest.At}
	}
	return readings
}

// refreshDelivery is the same overlay for the one delivery a write answers
// with. It is here so that the row a screen gets back from pressing Ack says
// what the row it pressed the button on said — the same rule that makes both
// alerts screens one component.
func (s *Server) refreshDelivery(
	ctx context.Context, delivery signals.Transition,
) (signals.Transition, reading) {
	one := []signals.Transition{delivery}
	readings := s.refreshOpen(ctx, one)
	return one[0], readings[one[0].Key()]
}

// currentReadings is what the detection loop in this process last saw, or
// nothing.
//
// Nothing is an ordinary answer and never an error: this replica may not be
// the one running the loop, and the read is an improvement to a row that is
// already correct. A failure to obtain it degrades to the opening's words with
// the row saying so, which is exactly what the platform knew before it asked.
func (s *Server) currentReadings(ctx context.Context) map[signals.TransitionKey]signals.CurrentReading {
	if s.Detection == nil {
		return nil
	}
	readings, err := s.Detection.CurrentReadings(ctx)
	if err != nil {
		s.log().V(1).Info("the current round could not be read, so open alerts carry the "+
			"reading they opened with", "reason", err.Error())
		return nil
	}
	return readings
}

// openedInstant is when a recorded row's words were read, which for an open
// transition is when it opened. `At` is the fallback for a history written
// before the column existed.
func openedInstant(transition signals.Transition) time.Time {
	if !transition.OpenedAt.IsZero() {
		return transition.OpenedAt.UTC()
	}
	return transition.At.UTC()
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
	// its own write did to the tier — and in the words the list showed it,
	// because a row that read differently for having been acted on would be
	// the disagreement this endpoint exists to prevent, in one component.
	//
	// The answer alone. Everything above this line — the audit entry's
	// sentence, the mitigation row — is about the delivery *the history
	// holds*, and carries what the condition said when it opened for the same
	// reason the transition itself does: it is the record of a moment, and it
	// is what a later reader correlates against.
	delivery, read := s.refreshDelivery(ctx, delivery)
	states := map[signals.TransitionKey]signals.MitigationState{}
	if records, err := store.SignalMitigations(ctx); err == nil {
		states = signals.FoldMitigations(signals.MitigationsFrom(records))
	}
	for _, alert := range signals.Assess([]signals.Transition{delivery}, states, s.signalPolicy(ctx), now) {
		if alert.Key() == delivery.Key() {
			writeJSON(w, http.StatusOK, alertViewOf(alert, read))
			return
		}
	}
	writeJSON(w, http.StatusOK, alertViewOf(signals.Alert{Finding: delivery.Finding()}, read))
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

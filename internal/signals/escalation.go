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
	"fmt"
	"sort"
	"strings"
	"time"
)

// The clock half of the tier model: what an hour of nobody looking does.
//
// **Escalation changes the audience, not the colour.** Nothing is more broken
// at hour four than at hour one, so an unacknowledged condition does not
// become a page — it repeats and *adds the operator as a ticket*, in those
// words. Past a multiple of the same window it becomes an untended-incident
// line on the compliance posture, with names on it.
//
// # Why these are constants, and what happened to that
//
// The base tier is code and the clock is policy (#471, decision 4), and the
// policy half was #472's — an installation that runs one estate at business
// hours and another at 24/7 will want two windows. It exists now: these three
// are the `balanced` preset's values in [Policy], nothing here reads them
// directly any more, and the number a round is actually judged against comes
// off [Snapshot.Policy]. They stay as constants because a preset has to be
// *something*, and because an installation that has configured nothing must
// keep behaving exactly as it did.

const (
	// EscalationWindow is how long an owner-tier condition may sit
	// unacknowledged before the operator is added to it.
	//
	// An hour, because it is the shortest window that is not a restatement
	// of the condition itself: a build that fails and is retried, a preview
	// that comes up on the second attempt and a rollout that finishes are
	// all inside it, and a condition still open an hour later is one nobody
	// has looked at rather than one that is mid-flight.
	EscalationWindow = time.Hour

	// UntendedMultiple is how many escalation windows an unacknowledged
	// condition survives before it stops being an alert and becomes a
	// finding about the institution: an untended incident on the compliance
	// posture, with the project and the elapsed time on it.
	//
	// Four, so that a condition that opened at the end of a working day is
	// untended by the following morning rather than during the night —
	// which is the shape of the failure the line exists to report. Nobody is
	// woken either way; this is a record, not a page.
	UntendedMultiple = 4

	// MaxSilence bounds how long one silence may last. Thirty days, because
	// a silence is a decision about a condition somebody has looked at, and
	// a decision nobody revisits within a month is one whose reason has
	// stopped being true without anybody noticing. Asking again is cheap.
	MaxSilence = 30 * 24 * time.Hour
)

// UntendedAfter is the `balanced` preset's second clock. What a round is
// judged against is [Policy.UntendedAfter], which multiplies the configured
// pair; this is the default that pair falls back to.
const UntendedAfter = UntendedMultiple * EscalationWindow

// symptomMarker separates a fingerprint from the derived, symptom-shaped copy
// of it a project reads. It is the same device as [unevaluableMarker] and for
// the same reason: the derived row must never collide with a recorded key, so
// that an ack or a silence naming it is refused rather than quietly written
// against a delivery that does not exist.
const symptomMarker = "#symptom"

// Alert is one delivery as a reader sees it: the condition, what is being done
// about it, and the tier that follows from both.
//
// It is the shape the alerts screens and the delivery filter both read, and it
// is computed rather than stored: the recorded [Transition] carries the *base*
// tier, and everything the clock and the mitigation records do to it is
// arithmetic over the same two inputs, done here once.
type Alert struct {
	// Finding is the condition, in the audience's own vocabulary.
	Finding Finding

	// Tier is the tier this reader actually sees it at, after mitigation
	// and escalation. Base is the rule's declaration for this audience,
	// kept beside it so a screen can say "a page, held down to a ticket
	// because somebody is on it" rather than only the answer.
	Tier Tier
	Base Tier

	// Mitigation is what stands: the ack, the silence, the claim.
	Mitigation MitigationState

	// OpenedAt is when the platform first saw the condition, and Unmitigated
	// how long it has been open with nobody acknowledging it. The second is
	// zero once anybody has.
	OpenedAt    time.Time
	Unmitigated time.Duration

	// Escalated is an owner-tier condition past [EscalationWindow] with
	// nobody acknowledging it. On the owner's own row it means "this is
	// being repeated"; on the operator's it means "this was added to your
	// list because of it".
	Escalated bool

	// Untended is the same condition past [UntendedAfter]. It is what puts
	// a line on the compliance posture.
	Untended bool

	// Note is the escalation sentence, in the issue's words:
	// `shop / production · unmitigated 4h 12m · nobody has acknowledged`.
	Note string

	// Owner marks the row belonging to whoever has to fix the condition, as
	// opposed to the copy somebody else is being kept informed by.
	//
	// It is on the row because two rows of one condition can both be
	// [Alert.Untended] — the bystander's is marked from the owner's clock —
	// and a list of untended *incidents* is one line per condition rather
	// than one per delivery.
	Owner bool

	// Symptom marks the derived row a project reads about a platform
	// condition degrading it. Nothing on it is actionable and nothing about
	// it is recorded — see [Symptoms].
	Symptom bool

	// Actionable is whether the controls apply to this row at all: a
	// recorded delivery takes an ack and (for a project's own row) a
	// silence, and a symptom takes neither.
	Actionable bool
}

// Key is the delivery this alert is about.
func (a Alert) Key() TransitionKey {
	return TransitionKey{Fingerprint: a.Finding.Fingerprint, Audience: a.Finding.Audience}
}

// Assess turns the recorded open deliveries and what people have done about
// them into what each reader sees.
//
// The four rules, in the order they are applied — the order matters, because
// each reads the answer of the one before:
//
//  1. **The owner is the developer where there is a developer delivery**,
//     and the operator otherwise. That is [Deliveries] read backwards: a
//     developer-audience rule is additive, so a developer row exists exactly
//     when the condition is a project's to fix.
//  2. **Mitigation lowers.** A condition somebody is acting on is a ticket
//     for its owner — a fix is still owed — and a log for everybody else.
//     That is the audience matrix's second and third rows, and it is why
//     the tier cannot be a stored column.
//  3. **A silence lowers to log, for that delivery alone.** It is keyed on
//     the pair, so a member silencing their project's row leaves the
//     operator's exactly where it was — loud, at the tier the rule
//     declared. What a silence *does* reach across is the clock: somebody
//     wrote down a reason and an expiry, which is a stronger statement than
//     an ack, so nothing escalates underneath it.
//  4. **Escalation raises the operator, never the owner.** An unacknowledged
//     owner-tier condition past the window repeats — the owner's row is
//     marked, not moved, because nothing is more broken at hour four — and
//     the operator's row is raised to at least a ticket and carries the
//     sentence saying why.
func Assess(
	open []Transition,
	mitigations map[TransitionKey]MitigationState,
	policy Policy,
	now time.Time,
) []Alert {
	policy = policy.Normalised()
	owners := ownerAudiences(open)

	alerts := make([]Alert, 0, len(open))
	// The owner's standing is decided first because rules 2 and 4 both read
	// it: the operator's row is lowered or raised by what the project did or
	// did not do, and a single pass would answer with whichever row it
	// happened to see first.
	escalated := map[string]Alert{}
	for _, transition := range open {
		if owners[transition.Fingerprint] != transition.Audience {
			continue
		}
		alert := assessOwner(transition, mitigations[transition.Key()], policy, now)
		escalated[transition.Fingerprint] = alert
		alerts = append(alerts, alert)
	}

	for _, transition := range open {
		if owners[transition.Fingerprint] == transition.Audience {
			continue
		}
		alerts = append(alerts, assessBystander(transition, mitigations[transition.Key()],
			escalated[transition.Fingerprint], policy, now))
	}

	SortAlerts(alerts)
	return alerts
}

// ownerAudiences answers, per fingerprint, whose condition it is to fix.
func ownerAudiences(open []Transition) map[string]Audience {
	owners := make(map[string]Audience, len(open))
	for _, transition := range open {
		if transition.Audience == AudienceDeveloper {
			owners[transition.Fingerprint] = AudienceDeveloper
			continue
		}
		if _, already := owners[transition.Fingerprint]; !already {
			owners[transition.Fingerprint] = AudienceOperator
		}
	}
	return owners
}

// assessOwner is the row belonging to whoever has to fix it.
func assessOwner(transition Transition, state MitigationState, policy Policy, now time.Time) Alert {
	// The recorded row carries the tier the rule declared. What this
	// installation does with it is the policy's — paging off holds a page
	// down to a ticket — and it is applied to the base rather than only to
	// the answer, so that a screen saying "a page, held down because somebody
	// is on it" never says "page" on an installation that does not page.
	base := policy.Deliver(transition.Tier)
	alert := Alert{
		Finding:    transition.Finding(),
		Base:       base,
		Tier:       base,
		Mitigation: state,
		OpenedAt:   transition.OpenedAt,
		Owner:      true,
		Actionable: true,
	}

	switch {
	case state.Silenced(now):
		// Quietened by the people it belongs to, with a reason and an end.
		alert.Tier = TierLog
	case state.Mitigated(now):
		// Somebody is on it. A fix is still owed, so it does not fall all
		// the way to a log — see the matrix's second row.
		alert.Tier = alert.Tier.atMost(TierTicket)
	default:
		alert.Unmitigated = unmitigatedFor(transition, now)
		if ownerTier(alert.Base) && alert.Unmitigated > policy.EscalationWindow {
			alert.Escalated = true
			alert.Untended = alert.Unmitigated > policy.UntendedAfter()
			alert.Note = escalationNote(transition.Scope, alert.Unmitigated)
		}
	}
	return alert
}

// assessBystander is the row belonging to whoever is only being kept informed
// — in practice the operator's copy of a project's condition.
func assessBystander(
	transition Transition,
	state MitigationState,
	owner Alert,
	policy Policy,
	now time.Time,
) Alert {
	base := policy.Deliver(transition.Tier)
	alert := Alert{
		Finding:    transition.Finding(),
		Base:       base,
		Tier:       base,
		Mitigation: state,
		OpenedAt:   transition.OpenedAt,
		Actionable: true,
	}

	switch {
	case state.Silenced(now):
		// This reader's own silence, on their own row.
		alert.Tier = TierLog
	case owner.Mitigation.Acknowledged || owner.Mitigation.ClaimedBy != "":
		// The owner is acting, so this reader has nothing to do: a log.
		//
		// A silence deliberately does *not* count here, though it stops the
		// escalation clock below. "A member silencing their project's signal
		// must not silence the operator's row" is the rule the whole
		// (fingerprint, audience) key exists to make statable, and a silence
		// that quietened the operator's copy would be that rule broken
		// through the side door.
		alert.Tier = TierLog
	case owner.Escalated:
		// "It repeats and adds the operator as a ticket, in those words."
		alert.Tier = alert.Tier.AtLeast(TierTicket)
		alert.Escalated = true
		alert.Untended = owner.Untended
		alert.Unmitigated = owner.Unmitigated
		alert.Note = owner.Note
	default:
		alert.Unmitigated = unmitigatedFor(transition, now)
	}
	return alert
}

// atMost lowers a tier to a ceiling, and never raises it.
func (t Tier) atMost(ceiling Tier) Tier {
	if t.Rank() > ceiling.Rank() {
		return ceiling
	}
	return t
}

// ownerTier is whether a condition is one its owner is meant to act on at all.
// A condition declared a log for the person who owns it is a data point, and a
// data point nobody acknowledged is not an incident.
func ownerTier(tier Tier) bool { return tier.Rank() >= TierTicket.Rank() }

func unmitigatedFor(transition Transition, now time.Time) time.Duration {
	opened := transition.OpenedAt
	if opened.IsZero() {
		opened = transition.At
	}
	if now.Before(opened) {
		return 0
	}
	return now.Sub(opened)
}

// escalationNote is the sentence the escalated row carries, in the words the
// issue writes it in: `shop / production · unmitigated 4h 12m · nobody has
// acknowledged`.
func escalationNote(scope Scope, unmitigated time.Duration) string {
	where := strings.Join(nonEmpty(scope.Project, scope.Environment), " / ")
	if where == "" {
		where = string(scope.Kind)
	}
	return fmt.Sprintf("%s · unmitigated %s · nobody has acknowledged", where, elapsed(unmitigated))
}

func nonEmpty(parts ...string) []string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return kept
}

// elapsed is `4h 12m`, `12m`, `45s` — the sentence's own spelling of a
// duration, which is deliberately not the dashboard's: this string is written
// into an audit record and read years later, so it cannot be a rendering
// choice made on a screen.
func elapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if hours == 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dh %dm", hours, minutes)
}

// Symptoms is what a project is told about the platform conditions degrading
// it, and it is the fourth thing #471 asks for.
//
// A node going NotReady is not a developer's alert — they do not have the word
// and they cannot act on it — but their production being degraded *is* theirs
// to know. So an operator-audience condition whose scope names a project
// surfaces to that project symptom-shaped and un-actioned: **no Kubernetes
// noun, no button they cannot press, and no silence either.**
//
// Which conditions those are is read off the data rather than guessed. An
// operator-audience finding that carries a project in its scope is exactly the
// set [Findings.ForEnvironment] drops on purpose — `pvc.pending` and
// `volume.attach-failed` are its own examples, both scoped to a claim in the
// project's namespace and both the operator's problem. That set is the
// symptom, and there is no correlation to invent.
//
// The rows are derived and never recorded: their fingerprints carry
// [symptomMarker], so nothing can ack or silence one, which is the same
// sentence the mockup ends on.
func Symptoms(open []Transition, project string, now time.Time) []Alert {
	symptoms := make([]Alert, 0, 4)
	for _, transition := range open {
		if transition.Audience != AudienceOperator || transition.Scope.Project == "" {
			continue
		}
		if project != "" && transition.Scope.Project != project {
			continue
		}
		symptoms = append(symptoms, Alert{
			Finding:     symptomFinding(transition),
			Base:        TierLog,
			Tier:        TierLog,
			OpenedAt:    transition.OpenedAt,
			Unmitigated: unmitigatedFor(transition, now),
			Symptom:     true,
		})
	}
	SortAlerts(symptoms)
	return symptoms
}

// symptomFinding is one platform condition in the project's vocabulary: what
// of theirs is degraded, and that somebody who can act on it has been told.
func symptomFinding(transition Transition) Finding {
	where := transition.Scope.Environment
	if where == "" {
		where = transition.Scope.Project
	}
	return Finding{
		Signal:   transition.Signal,
		Severity: transition.Severity,
		// The scope keeps the project and the environment and drops
		// everything else. Namespace, node and claim names are the
		// operator's nouns, and the whole rule here is that none of them
		// reach this row.
		Scope: Scope{
			Kind:        ScopeEnvironment,
			Project:     transition.Scope.Project,
			Environment: transition.Scope.Environment,
		},
		Audience:    AudienceDeveloper,
		Tier:        TierLog,
		Fingerprint: transition.Fingerprint + symptomMarker,
		Title:       where + " degraded",
		Detail:      "a platform issue, the operator has been notified",
		Since:       transition.Since,
		// Deliberately empty: evidence is a link to a screen this reader
		// cannot open, and a button they cannot press is the thing the
		// symptom row exists to avoid.
		Evidence: "",
	}
}

// SortAlerts puts a list in the order the screens render it: the tier the
// reader sees first, then severity, then the condition's own order — so that
// two evaluations of an unchanged platform produce byte-identical output.
func SortAlerts(alerts []Alert) {
	sort.SliceStable(alerts, func(i, j int) bool {
		left, right := alerts[i], alerts[j]
		if l, r := left.Tier.Rank(), right.Tier.Rank(); l != r {
			return l > r
		}
		if l, r := left.Finding.Severity.Rank(), right.Finding.Severity.Rank(); l != r {
			return l > r
		}
		if left.Finding.Signal != right.Finding.Signal {
			return left.Finding.Signal < right.Finding.Signal
		}
		if left.Finding.Fingerprint != right.Finding.Fingerprint {
			return left.Finding.Fingerprint < right.Finding.Fingerprint
		}
		return left.Finding.Audience < right.Finding.Audience
	})
}

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
	"time"
)

// What somebody has done about a condition, and what that does to its tier.
//
// Everything here is as pure as the rules and the tracker are: a set of
// records in, a standing out, no clock beyond the instant passed in and no
// I/O. Where the records are stored is internal/clickhouse's, who may write
// them is internal/api's, and the arithmetic that turns them into "what does
// this reader see" is here — one implementation, read by the API that renders
// the screens and by the loop that decides what to deliver.
//
// # The one constraint the whole model rests on
//
// **A mitigation is a record about a condition, not a ticket's status field.**
// The tier is a classification — what kind of thing is this, and what is this
// reader meant to do about it — and it stays true whether the ticket that
// comes out of it lives in Kitchen, in GitLab or nowhere (#471, second
// comment). So there is no lifecycle here: nothing transitions, nothing is
// closed, and an ack does not "own" anything. The condition resolves when the
// condition stops being true, which is [Tracker]'s business alone.

// MitigationKind is what one record says was done.
type MitigationKind string

const (
	// MitigationAck is somebody saying they have seen it. It commits to no
	// fix — that is the point of shipping it, since "somebody is looking"
	// needs to be sayable without promising anything — and it is what stops
	// the escalation clock.
	MitigationAck MitigationKind = "ack"
	// MitigationSilence is a project's member quietening a condition they
	// have decided about, with a reason and an expiry. It is project-scoped
	// and never reaches the operator's row for the same condition.
	MitigationSilence MitigationKind = "silence"
	// MitigationUnsilence lifts a silence before its expiry. It is a record
	// rather than a deletion, because the log is append-only and "who
	// decided this should be loud again" is as much a question as who
	// quietened it.
	MitigationUnsilence MitigationKind = "unsilence"
	// MitigationClaim is an operator taking the escalated ticket. There is
	// no rota (#471, decision 1): escalation addresses one ticket to the
	// operators as a group and any of them claims it, which turns "three
	// tickets nobody owns" into "one ticket anybody can take".
	MitigationClaim MitigationKind = "claim"
)

// MitigationSource says how a record came to be written.
const (
	// SourceExplicit is somebody pressing the button or calling the route.
	SourceExplicit = "explicit"
	// SourceAction is the implicit ack a resolving action records. Starting
	// the fix — retrying the build, rolling back — is a stronger statement
	// than pressing Ack, and a condition somebody is actively fixing must
	// not go on counting as untended and escalate underneath them (#471,
	// decision 2).
	SourceAction = "action"
)

// Mitigation is one record: what was done, to which delivery, by whom.
//
// The key is (Fingerprint, Audience) and never the fingerprint alone, for the
// reason [TransitionKey] is: a member acking their project's row must not
// acknowledge the operator's row about the same condition — escalation is
// explicitly about *nobody* having acknowledged — and a member silencing their
// project's signal must not silence the operator's, which is what the
// project-scoping rule protects.
type Mitigation struct {
	At   time.Time
	Kind MitigationKind

	Fingerprint string
	Audience    Audience

	// Project is the project the condition is about, carried so that the
	// store can answer "everything anybody did about this project" without
	// joining back to the transition.
	Project string

	// Actor is who did it, as the API knew them.
	Actor string

	// Reason is why. A silence must carry one; an ack need not.
	Reason string

	// Until is when a silence expires. Every silence has one — a silence
	// with no end is a rule somebody deleted and did not say so.
	Until time.Time

	// Source is [SourceExplicit] or [SourceAction].
	Source string
}

// Key is the delivery this record is about.
func (m Mitigation) Key() TransitionKey {
	return TransitionKey{Fingerprint: m.Fingerprint, Audience: m.Audience}
}

// MitigationState is every record about one delivery, folded.
//
// It is a fold rather than a list because the questions asked of it are all
// "what stands now": is this acknowledged, is it silenced, who has it. The
// list is the audit log's, which is where the history of a decision belongs.
type MitigationState struct {
	Acknowledged   bool
	AcknowledgedBy string
	AcknowledgedAt time.Time
	// AcknowledgedBySource is [SourceExplicit] or [SourceAction], so a
	// screen can say "rolled back" rather than "acknowledged" where that is
	// what happened.
	AcknowledgedBySource string

	SilencedBy    string
	SilencedAt    time.Time
	SilenceReason string
	SilencedUntil time.Time

	ClaimedBy string
	ClaimedAt time.Time
}

// Silenced reports whether a silence stands at an instant. An expired silence
// is not a silence: the expiry is the whole reason a silence is safe to grant.
func (s MitigationState) Silenced(now time.Time) bool {
	return !s.SilencedUntil.IsZero() && now.Before(s.SilencedUntil)
}

// Mitigated reports whether anything at all is being done about the delivery:
// somebody has acknowledged it, or an operator has claimed it. It is what the
// audience matrix means by "something is already mitigating it" — the row that
// makes a condition a ticket for its owner and a log for everybody else.
func (s MitigationState) Mitigated(now time.Time) bool {
	return s.Acknowledged || s.ClaimedBy != "" || s.Silenced(now)
}

// FoldMitigations turns a set of records into the state of each delivery.
//
// Records may arrive in any order and the newest of each kind wins, which is
// what makes the store's read a plain scan rather than an ordered one. An
// unsilence is the one record that clears rather than sets: it takes the
// silence off when it is newer than the silence it lifts, and does nothing
// when an even newer silence has been laid on top.
func FoldMitigations(records []Mitigation) map[TransitionKey]MitigationState {
	states := map[TransitionKey]MitigationState{}
	// The silence half is folded through its own timestamp so that a
	// silence and the unsilence that lifts it can be compared however they
	// arrive.
	silencedAt := map[TransitionKey]time.Time{}
	for _, record := range records {
		key := record.Key()
		state := states[key]
		switch record.Kind {
		case MitigationAck:
			if record.At.After(state.AcknowledgedAt) {
				state.Acknowledged = true
				state.AcknowledgedBy = record.Actor
				state.AcknowledgedAt = record.At
				state.AcknowledgedBySource = record.Source
			}
		case MitigationSilence:
			if record.At.After(silencedAt[key]) {
				silencedAt[key] = record.At
				state.SilencedBy = record.Actor
				state.SilencedAt = record.At
				state.SilenceReason = record.Reason
				state.SilencedUntil = record.Until
			}
		case MitigationUnsilence:
			if record.At.After(silencedAt[key]) {
				silencedAt[key] = record.At
				state.SilencedBy = ""
				state.SilencedAt = time.Time{}
				state.SilenceReason = ""
				state.SilencedUntil = time.Time{}
			}
		case MitigationClaim:
			if record.At.After(state.ClaimedAt) {
				state.ClaimedBy = record.Actor
				state.ClaimedAt = record.At
			}
		}
		states[key] = state
	}
	return states
}

// ValidateSilence refuses a silence that cannot do its job, at the route
// rather than at the read that later cannot explain it.
//
// Both rules are the same rule twice: a silence is a decision somebody made,
// and a decision with no reason and no end is a rule that quietly outlives
// whoever made it.
func ValidateSilence(reason string, until time.Time, now time.Time) error {
	switch {
	case reason == "":
		return fmt.Errorf("a silence must say why: it is a decision somebody made, " +
			"and a reader months later has only this sentence to go on")
	case until.IsZero() || !until.After(now):
		return fmt.Errorf("a silence must expire in the future: a silence with no end " +
			"is a rule nobody remembers making")
	case until.Sub(now) > MaxSilence:
		return fmt.Errorf("a silence may last at most %s; ask for it again if it is still true then",
			MaxSilence)
	}
	return nil
}

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
	"sort"
	"time"
)

// Diffing one round against the last one, which is the whole of what turns a
// list of findings into a history.
//
// Everything here is as pure as the rules are: a [Tracker] holds the previous
// round in memory, is handed the next one, and answers with what changed. It
// performs no I/O and reads no clock — the round's instant is passed in — so
// the interesting cases are struct literals in a test rather than a cluster
// somebody has to break.

// TransitionState is what happened to a condition.
type TransitionState string

const (
	// StateOpen is the platform seeing a condition it was not seeing before.
	StateOpen TransitionState = "open"
	// StateResolved is a condition the platform was seeing and no longer is.
	StateResolved TransitionState = "resolved"
)

// TransitionKey is what a transition is recorded under, and it is the one
// design decision in this file that cannot be changed cheaply afterwards.
//
// It is (fingerprint, audience) rather than the fingerprint alone. The
// fingerprint is stable for the same underlying condition across evaluations —
// that is what makes rounds diffable at all — but a condition reaches up to two
// audiences, and an audience is a delivery: a tier, a vocabulary and, once
// there is an inbox, an acknowledgement. A member acking their project's row
// must not acknowledge the operator's row about the same condition, and an
// operator silencing the platform's row must not silence the project's. Keyed
// on the fingerprint alone, both of those are the same row and neither rule can
// be stated.
type TransitionKey struct {
	Fingerprint string
	Audience    Audience
}

// Transition is one change, in the shape it is recorded in.
//
// It carries the finding rather than pointing at it, because the point of
// writing it down is that the finding does not survive the round. A reader
// months later must be able to say what the condition was without re-evaluating
// a catalogue that has moved on — which is also why [Transition.Version] is
// here.
type Transition struct {
	// At is when the round that saw the change was evaluated.
	At time.Time

	// State is what happened.
	State TransitionState

	// Signal, Fingerprint and Audience are the identity: the rule, the
	// condition, and which of the two deliveries this row is.
	Signal      ID
	Fingerprint string
	Audience    Audience

	// Tier is what this delivery's audience is meant to do about the
	// condition, as the rule declared it for that audience. It is on the row
	// rather than looked up from the catalogue when the row is read, for the
	// reason Version is: the catalogue moves, and a history that
	// reinterpreted an old row against today's declaration would be a
	// history that changes what it said.
	Tier Tier

	// Version is the rule's own version at the moment this row was written.
	// It travels with the transition so that a catalogue change is visible in
	// the history — a condition that opened under version 1 and resolved
	// under version 2 resolved because the rule changed, and a history that
	// did not say so would be silently reinterpreting itself.
	Version int

	// Severity and Scope are the finding's, at the moment of the transition.
	Severity Severity
	Scope    Scope

	// Title, Detail and Evidence are what a person reads.
	Title    string
	Detail   string
	Evidence string

	// Since is what the snapshot could prove about the condition's age, and
	// OpenedAt is when this platform first saw it. They are both here because
	// they answer different questions: a pod's last restart is Since, and
	// "nobody has touched this in four hours" is measured from OpenedAt.
	Since    time.Time
	OpenedAt time.Time
}

// Key is the transition's identity.
func (t Transition) Key() TransitionKey {
	return TransitionKey{Fingerprint: t.Fingerprint, Audience: t.Audience}
}

// Finding is the transition read back as what it was about, which is what the
// screens above it render. The audience is the row's — the delivery — rather
// than the producing rule's, which is what makes the operator's copy of a
// developer condition selectable on its own.
func (t Transition) Finding() Finding {
	return Finding{
		Signal:      t.Signal,
		Severity:    t.Severity,
		Scope:       t.Scope,
		Audience:    t.Audience,
		Tier:        t.Tier,
		Fingerprint: t.Fingerprint,
		Title:       t.Title,
		Detail:      t.Detail,
		Since:       t.Since,
		Evidence:    t.Evidence,
	}
}

// Deliveries is who one finding reaches, and it is the shape of [Audience]
// written out.
//
// A developer signal is additive: it appears on the environment's diagnostics
// strip *and* on the operator's problems list, because a project failing is a
// thing the operator wants to know. Recorded, that is two deliveries of one
// condition, and they are two rows.
func Deliveries(audience Audience) []Audience {
	if audience == AudienceDeveloper {
		return []Audience{AudienceDeveloper, AudienceOperator}
	}
	return []Audience{AudienceOperator}
}

// episode is a condition the tracker currently believes is open.
type episode struct {
	finding  Finding
	openedAt time.Time
}

// Tracker turns a sequence of rounds into a sequence of transitions.
//
// It holds the previous round's open set in memory, which is process state on
// purpose: the store holds the durable copy, and a leader taking over seeds
// itself from it (see [Tracker.Restore]) rather than treating a fresh process
// as a platform where nothing is wrong.
type Tracker struct {
	versions map[ID]int
	tiers    map[ID]Tiers
	open     map[TransitionKey]episode
}

// NewTracker builds a tracker over a catalogue. The catalogue is read for two
// things only — each rule's version and its tier declaration, both of which
// every row it writes carries.
//
// The tier has to come from here rather than from the finding, because a
// finding carries one tier and a developer condition is written as two rows:
// the delivery's audience decides which half of the declaration the row gets.
func NewTracker(catalogue *Registry) *Tracker {
	versions := map[ID]int{}
	tiers := map[ID]Tiers{}
	if catalogue != nil {
		for _, signal := range catalogue.Signals() {
			versions[signal.ID] = signal.Version
			tiers[signal.ID] = signal.Tiers
		}
	}
	return &Tracker{versions: versions, tiers: tiers, open: map[TransitionKey]episode{}}
}

// Restore seeds the tracker with the conditions the store says are already
// open, so that a restarted operator — or a replica that has just won the lease
// — records what changed while it was not looking rather than re-announcing
// everything the previous leader had already announced.
func (t *Tracker) Restore(open []Transition) {
	t.open = make(map[TransitionKey]episode, len(open))
	for _, transition := range open {
		openedAt := transition.OpenedAt
		if openedAt.IsZero() {
			openedAt = transition.At
		}
		t.open[transition.Key()] = episode{finding: transition.Finding(), openedAt: openedAt}
	}
}

// Open is how many deliveries the tracker currently holds open.
func (t *Tracker) Open() int { return len(t.open) }

// Observe diffs one round against the last and returns what changed, in a
// stable order.
//
// Three rules decide what comes back, and the third is the one that is easy to
// get wrong:
//
//   - A key in this round that was not in the last opened.
//   - A key in the last round that is not in this one resolved.
//   - A key whose *rule could not be evaluated this round* did neither. A
//     store that went down does not resolve every condition it was the input
//     to; it makes them unknown, and a history that recorded thirty
//     resolutions at the moment ClickHouse restarted would be a history that
//     lies about the one thing it exists to be right about. The round says so
//     itself: an unevaluable rule answers with a [SeverityUnknown] finding,
//     which is not a condition and is carried forward instead.
func (t *Tracker) Observe(round Findings, now time.Time) []Transition {
	blind := map[ID]bool{}
	current := make(map[TransitionKey]Finding, len(round))
	for _, finding := range round {
		if finding.Severity == SeverityUnknown {
			blind[finding.Signal] = true
			continue
		}
		for _, audience := range Deliveries(finding.Audience) {
			current[TransitionKey{Fingerprint: finding.Fingerprint, Audience: audience}] = finding
		}
	}

	transitions := make([]Transition, 0, 8)
	next := make(map[TransitionKey]episode, len(current))

	for key, finding := range current {
		if was, already := t.open[key]; already {
			// Still true: no transition, and the opening time is kept. The
			// finding itself is refreshed, because a detail that has moved —
			// twelve restarts rather than four — is what the resolving row
			// should carry.
			next[key] = episode{finding: finding, openedAt: was.openedAt}
			continue
		}
		next[key] = episode{finding: finding, openedAt: now}
		transitions = append(transitions, t.transition(StateOpen, key, finding, now, now))
	}

	for key, was := range t.open {
		if _, still := current[key]; still {
			continue
		}
		if blind[was.finding.Signal] {
			next[key] = was
			continue
		}
		transitions = append(transitions, t.transition(StateResolved, key, was.finding, now, was.openedAt))
	}

	t.open = next
	sortTransitions(transitions)
	return transitions
}

// transition builds one row. The audience is the key's — the delivery — and
// never the finding's, which is what keeps the operator's copy of a developer
// condition a row of its own.
func (t *Tracker) transition(
	state TransitionState,
	key TransitionKey,
	finding Finding,
	now, openedAt time.Time,
) Transition {
	// The declaration's half for *this* delivery's audience, which is what
	// makes the operator's copy of a developer condition a ticket while the
	// developer's own copy is a page.
	tier, _ := t.tiers[finding.Signal].For(key.Audience)
	return Transition{
		At:          now,
		State:       state,
		Signal:      finding.Signal,
		Fingerprint: key.Fingerprint,
		Audience:    key.Audience,
		Tier:        tier,
		Version:     t.versions[finding.Signal],
		Severity:    finding.Severity,
		Scope:       finding.Scope,
		Title:       finding.Title,
		Detail:      finding.Detail,
		Evidence:    finding.Evidence,
		Since:       finding.Since,
		OpenedAt:    openedAt,
	}
}

// sortTransitions puts a round's writes in a fixed order, so that two rounds
// over an unchanged cluster produce byte-identical batches and a test can
// assert on one.
func sortTransitions(transitions []Transition) {
	sort.SliceStable(transitions, func(i, j int) bool {
		if transitions[i].Fingerprint != transitions[j].Fingerprint {
			return transitions[i].Fingerprint < transitions[j].Fingerprint
		}
		return transitions[i].Audience < transitions[j].Audience
	})
}

// TransitionsFindings is a round read back out of recorded transitions: the
// open ones, in the order the problems list renders.
func TransitionsFindings(open []Transition) Findings {
	findings := make(Findings, 0, len(open))
	for _, transition := range open {
		findings = append(findings, transition.Finding())
	}
	findings.Sort()
	return findings
}

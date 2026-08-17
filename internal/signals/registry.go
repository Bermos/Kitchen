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
	"sync"
	"time"
)

// Registry is a catalogue of rules. It exists as a value rather than as a
// package-level slice so that a test can evaluate one rule in isolation
// against a hand-built snapshot, which is most of what the tests in this
// package do.
type Registry struct {
	signals []Signal
	byID    map[ID]Signal
}

// NewRegistry builds a catalogue, refusing one that cannot work. A duplicate
// id is the failure worth catching here: two rules under one name would write
// colliding fingerprints, and a transition log cannot tell colliding
// fingerprints apart after the fact.
func NewRegistry(signals ...Signal) (*Registry, error) {
	registry := &Registry{
		signals: make([]Signal, 0, len(signals)),
		byID:    make(map[ID]Signal, len(signals)),
	}
	for _, signal := range signals {
		if err := signal.validate(); err != nil {
			return nil, err
		}
		if _, taken := registry.byID[signal.ID]; taken {
			return nil, fmt.Errorf("two signals share the id %q", signal.ID)
		}
		registry.byID[signal.ID] = signal
		registry.signals = append(registry.signals, signal)
	}
	sort.SliceStable(registry.signals, func(i, j int) bool {
		return registry.signals[i].ID < registry.signals[j].ID
	})
	return registry, nil
}

var (
	catalogueOnce sync.Once
	catalogue     *Registry
)

// Catalogue is v1 of the catalogue: every rule docs/OBSERVABILITY.md §7 names.
//
// It panics on a malformed entry, and that is deliberate. The catalogue is
// compiled-in data with no runtime input, so a duplicate id or a missing rule
// is a programming error that a test catches long before a cluster does —
// returning an error here would only push the same failure into every caller.
func Catalogue() *Registry {
	catalogueOnce.Do(func() {
		registry, err := NewRegistry(all()...)
		if err != nil {
			panic("signals: the catalogue is malformed: " + err.Error())
		}
		catalogue = registry
	})
	return catalogue
}

// all is the catalogue's contents, one function per table of §7.
func all() []Signal {
	groups := [][]Signal{
		workloadSignals(),
		nodeSignals(),
		storageSignals(),
		edgeSignals(),
		buildSignals(),
		platformSignals(),
	}
	var signals []Signal
	for _, group := range groups {
		signals = append(signals, group...)
	}
	return signals
}

// Signals lists the catalogue in id order.
func (r *Registry) Signals() []Signal {
	listed := make([]Signal, len(r.signals))
	copy(listed, r.signals)
	return listed
}

// Lookup finds one rule by id.
func (r *Registry) Lookup(id ID) (Signal, bool) {
	signal, ok := r.byID[id]
	return signal, ok
}

// Evaluate runs the whole catalogue over one snapshot and returns the round,
// sorted worst first.
//
// The availability check lives here rather than in the rules because there is
// exactly one right answer to "my input is missing" and thirty-six chances to
// get it subtly wrong: a rule whose store query failed must say so, not return
// an empty slice that every screen above it will render as health. So a rule
// with an unreadable requirement is not called at all, and answers with one
// [SeverityUnknown] finding naming the input and the reason.
//
// An input marked not-applicable is different and produces nothing: DNS
// probing behind cloudflared is not a gap in the platform's knowledge, it is a
// question that does not arise, and a permanent row saying otherwise would
// train the reader to ignore the list.
func (r *Registry) Evaluate(snapshot *Snapshot) Findings {
	findings := make(Findings, 0, len(r.signals))
	for _, signal := range r.signals {
		if finding, ok := unevaluable(signal, snapshot); ok {
			if finding != nil {
				findings = append(findings, *finding)
			}
			continue
		}
		findings = append(findings, stamped(signal.Evaluate(snapshot), signal, snapshot.Now)...)
	}
	findings.Sort()
	return findings
}

// stamped applies the two things every finding carries that the rule producing
// it cannot know, in one place rather than in thirty-six.
//
// [Finding.Since] is the first. Most rules date a finding from something the
// snapshot can prove — a condition's last transition, an event's first
// occurrence, the start of the run that made it sustained — but some conditions
// have nothing to date them by, and an object whose creation timestamp is
// missing would otherwise be reported as having been broken since the year one.
//
// [Finding.Audience] is the second, and it is here because the registry is the
// only place that holds both a rule and what the rule produced. It is what
// [Findings.ForEnvironment] filters on, so a finding that arrived without it
// would be an operator's problem rendered on a developer's diagnostics strip.
func stamped(findings []Finding, signal Signal, now time.Time) []Finding {
	for i := range findings {
		if findings[i].Since.IsZero() {
			findings[i].Since = now
		}
		findings[i].Audience = signal.Audience
	}
	return findings
}

// unevaluable decides whether a rule can run, and what to say when it cannot.
// The second return is true when the rule must be skipped; the first is the
// finding to report in its place, or nil when there is nothing worth saying.
func unevaluable(signal Signal, snapshot *Snapshot) (*Finding, bool) {
	for _, input := range signal.Requires {
		state, reason := snapshot.inputState(input)
		switch state {
		case inputAvailable:
			continue
		case inputNotApplicable:
			return nil, true
		default:
			scope := Scope{Kind: ScopePlatform}
			finding := fire(signal.ID, SeverityUnknown, scope, snapshot.Now,
				"cannot be evaluated",
				fmt.Sprintf("%s could not be read, so this rule reports nothing rather than health: %s",
					input, reason),
				// The overview, because that is where the health strip explains
				// an input failure once for every rule it darkened.
				EvidencePlatform)
			// The scope of a rule that never ran is not the scope it would
			// have fired at — it has no subject yet — so the fingerprint is
			// the rule's own, marked. See unevaluableMarker for why it cannot
			// simply be the bare id.
			finding.Fingerprint = string(signal.ID) + unevaluableMarker
			finding.Audience = signal.Audience
			return &finding, true
		}
	}
	return nil, false
}

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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The clock, as opposed to the catalogue.
//
// docs/OBSERVABILITY.md §7 said the thresholds were constants and §9 recorded
// the trade-off — "signals as versioned code vs. user-configurable rules —
// code, until the alerting era forces the question with real requirements".
// The homelab installation is that requirement: a threshold of three
// correlated projects is a threshold a three-project estate never reaches, and
// a detector that can never fire is worse than one that fires too often,
// because it reads as health.
//
// So a bounded, named set of numbers becomes configuration, and nothing else
// does. Which signals exist, what they compute and their base tier stay
// compiled in, for the reason [Signal.Version] exists: two installations on
// catalogue v1 that disagreed about what a rule *is* would make a version
// number meaningless. What they may disagree about is how impatient they are,
// and every finding records the numbers it was evaluated against so that
// `v1 @ correlatedProjects=2` and `v1 @ correlatedProjects=3` are
// distinguishable years later. That is [Policy.Provenance], and an audit pack
// that could not say what the floor was at the time would not be evidence.

// Policy is the bounded set of thresholds an installation may set.
//
// It is a plain value with no pointers and no absent fields: every caller gets
// a whole answer, and the resolution from a possibly-empty spec happens once,
// in [PolicyFrom]. A zero Policy is not "the defaults" — it is a Policy nobody
// resolved, which [Policy.Normalised] repairs and which the rules never see,
// because [Gather] always resolves one.
type Policy struct {
	// Preset is the named base the numbers came from, kept so that a screen
	// can say "balanced, with the correlation threshold moved" rather than
	// only showing six numbers.
	Preset PresetName

	// CorrelatedProjects is how many projects must be degrading together
	// before it is one platform problem rather than several application
	// problems. It replaces the [CorrelatedProjects] constant.
	CorrelatedProjects int

	// CorrelationWindow is how far apart two conditions may have started and
	// still count as the same moment.
	//
	// This is a knob the code did not have: the two cross-project detectors
	// reused [RecentWindow], which is what "now" means for a series and is
	// not the same question. The `balanced` preset is that number exactly, so
	// nothing moves for an installation that sets nothing.
	CorrelationWindow time.Duration

	// EscalationWindow is how long an owner-tier condition may sit
	// unacknowledged before the operator is added to it, and UntendedMultiple
	// how many of those windows it survives before it becomes a line on the
	// compliance posture. They replace the constants of the same names.
	EscalationWindow time.Duration
	UntendedMultiple int

	// MaxSilence is the longest silence a member may set on their own row.
	MaxSilence time.Duration

	// Paging is whether [TierPage] is delivered as a page at all. False holds
	// every paging condition down to a ticket — the homelab reading, where
	// nobody is on call.
	//
	// It is installation-wide, like every number here. #472 argues a project
	// should be able to tighten its own rows back up; #519 is that, and until
	// it lands this setting is the whole answer for every project.
	Paging bool
}

// PresetName is one of the three named bases.
type PresetName string

const (
	// PresetStrict wants to hear about it early.
	PresetStrict PresetName = "strict"
	// PresetBalanced is the platform's own judgement, and is the compiled-in
	// constants exactly.
	PresetBalanced PresetName = "balanced"
	// PresetHomelab is one host and a handful of projects.
	PresetHomelab PresetName = "homelab"
)

// presets are the three, and `balanced` is load-bearing: it is the numbers
// this package compiled in before any of this was configurable, so an
// installation that has never opened the screen behaves exactly as it did.
// A change to a value here changes what an existing installation sees.
var presets = map[PresetName]Policy{
	PresetStrict: {
		Preset:             PresetStrict,
		CorrelatedProjects: 2,
		CorrelationWindow:  30 * time.Minute,
		EscalationWindow:   30 * time.Minute,
		UntendedMultiple:   2,
		MaxSilence:         7 * 24 * time.Hour,
		Paging:             true,
	},
	PresetBalanced: {
		Preset:             PresetBalanced,
		CorrelatedProjects: CorrelatedProjects,
		CorrelationWindow:  RecentWindow,
		EscalationWindow:   EscalationWindow,
		UntendedMultiple:   UntendedMultiple,
		MaxSilence:         MaxSilence,
		Paging:             true,
	},
	PresetHomelab: {
		Preset:             PresetHomelab,
		CorrelatedProjects: 2,
		CorrelationWindow:  time.Hour,
		EscalationWindow:   4 * time.Hour,
		UntendedMultiple:   6,
		MaxSilence:         30 * 24 * time.Hour,
		Paging:             false,
	},
}

// PresetDescriptions is what each preset is for, in one sentence, served to
// the screen rather than written on it — a dashboard that spelled these out
// itself would be a second copy of a decision this package owns.
//
// The homelab sentence says what the preset does and stops there. #472 argues
// that a project should be able to tighten what *it* hears — turning paging
// back on for its own rows costs the operator nothing — and that is true and
// not built: the policy is installation-wide, and #519 is the project-scoped
// override. A description promising a control the platform does not have would
// be the worst kind of copy, since the person reading it is the one who would
// go looking for the switch.
var PresetDescriptions = map[PresetName]string{
	PresetStrict: "Two projects are a correlation, half an hour unmitigated escalates to the operator, " +
		"and a silence lasts a week.",
	PresetBalanced: "The platform's own judgement, and what every installation has had until now: three " +
		"projects correlate, an hour unmitigated escalates, a silence lasts a month.",
	PresetHomelab: "One host and a handful of projects: two projects are a correlation, nothing escalates " +
		"before the afternoon, and nothing pages — everything that would say “act now” is a ticket.",
}

// Presets lists the three in the order the screen offers them: loudest first,
// which is also the order they read in.
func Presets() []Policy {
	ordered := make([]Policy, 0, len(presets))
	for _, name := range []PresetName{PresetStrict, PresetBalanced, PresetHomelab} {
		ordered = append(ordered, presets[name])
	}
	return ordered
}

// Preset is one named base.
func Preset(name PresetName) (Policy, bool) {
	policy, ok := presets[name]
	return policy, ok
}

// DefaultPolicy is what an installation that has configured nothing runs
// under, and it is `balanced` — which is to say, the constants.
func DefaultPolicy() Policy { return presets[PresetBalanced] }

// PolicyFrom resolves the singleton's spec into the whole value the rules
// read: the named preset, with each field somebody set overriding that one
// number.
//
// A nil Kitchen, an empty spec and an unknown preset all resolve to
// [DefaultPolicy]. That is deliberate rather than defensive: the gatherer
// resolves a policy even when the singleton could not be read, and a round
// evaluated against no policy at all would be a round with no thresholds.
func PolicyFrom(kitchen *kitchenv1alpha1.Kitchen) Policy {
	if kitchen == nil {
		return DefaultPolicy()
	}
	return PolicyFromSpec(kitchen.Spec.Observability.Signals.Policy)
}

// PolicyFromSpec is [PolicyFrom]'s half that a caller holding only the spec
// can use — the API's PATCH handler, which resolves what a request would mean
// before it writes it.
func PolicyFromSpec(spec kitchenv1alpha1.SignalPolicySpec) Policy {
	policy, ok := presets[PresetName(spec.Preset)]
	if !ok {
		policy = DefaultPolicy()
	}
	if spec.CorrelatedProjects != nil {
		policy.CorrelatedProjects = int(*spec.CorrelatedProjects)
	}
	if spec.CorrelationWindowMinutes != nil {
		policy.CorrelationWindow = time.Duration(*spec.CorrelationWindowMinutes) * time.Minute
	}
	if spec.EscalationWindowMinutes != nil {
		policy.EscalationWindow = time.Duration(*spec.EscalationWindowMinutes) * time.Minute
	}
	if spec.UntendedMultiple != nil {
		policy.UntendedMultiple = int(*spec.UntendedMultiple)
	}
	if spec.MaxSilenceHours != nil {
		policy.MaxSilence = time.Duration(*spec.MaxSilenceHours) * time.Hour
	}
	if spec.Paging != nil {
		policy.Paging = *spec.Paging
	}
	return policy.Normalised()
}

// Normalised replaces anything unset or nonsensical with the default's answer
// for that one field.
//
// It exists because a Policy reaches the rules from three directions — the
// spec, a test's struct literal, and a caller that built one by hand — and a
// correlation threshold of zero would make every pair of failures an incident.
// Repairing one field at a time rather than rejecting the whole value is the
// right failure mode here: this is an observability capability, and refusing to
// evaluate the catalogue because somebody wrote a nonsense number would be the
// platform going quiet over a setting.
//
// [Policy.Preset] is what says whether a value was resolved at all, and it is
// the only way [Policy.Paging] can be repaired: a `false` bool cannot be told
// from an unset one, so an unresolved policy — no preset named — takes the
// default's paging along with every other zero field, and a resolved one keeps
// the false somebody chose. Every resolver here names a preset; nothing else
// does.
func (p Policy) Normalised() Policy {
	base := presets[PresetBalanced]
	if _, known := presets[p.Preset]; !known {
		p.Preset = PresetBalanced
		p.Paging = base.Paging
	}
	if p.CorrelatedProjects < 2 {
		p.CorrelatedProjects = base.CorrelatedProjects
	}
	if p.CorrelationWindow <= 0 {
		p.CorrelationWindow = base.CorrelationWindow
	}
	if p.EscalationWindow <= 0 {
		p.EscalationWindow = base.EscalationWindow
	}
	if p.UntendedMultiple < 1 {
		p.UntendedMultiple = base.UntendedMultiple
	}
	if p.MaxSilence <= 0 {
		p.MaxSilence = base.MaxSilence
	}
	return p
}

// UntendedAfter is how long a condition runs unacknowledged before it becomes
// a line on the compliance posture. It is the whole of the second clock, so
// that nothing multiplies the two numbers together in more than one place.
func (p Policy) UntendedAfter() time.Duration {
	return time.Duration(p.UntendedMultiple) * p.EscalationWindow
}

// Deliver is what this installation actually does with a rule's declared tier.
//
// The only thing a policy may do to a tier is hold a page down to a ticket,
// and only when paging is off. It never raises one and never lowers past
// ticket: the base tier is catalogue knowledge (#471, decision 4), and a
// policy that could rewrite it would be the configurable-rules door this
// package spent §9 keeping shut.
func (p Policy) Deliver(tier Tier) Tier {
	if tier == TierPage && !p.Paging {
		return TierTicket
	}
	return tier
}

// Matches reports whether a policy is one of the presets exactly, which is how
// a screen tells "balanced" from "balanced, with two numbers moved".
func (p Policy) Matches(name PresetName) bool {
	preset, ok := presets[name]
	if !ok {
		return false
	}
	preset.Preset = p.Preset
	return preset == p
}

// Provenance is the finding's record of what it was evaluated against: one
// short, ordered, stable string.
//
// It is one string rather than six columns because of what it is for. Nobody
// queries on the escalation window; somebody reads a finding from eight months
// ago and asks what the floor was when it fired, and an audit pack quotes the
// answer. A single field also means a threshold added later widens the string
// rather than migrating the table.
//
// The order is fixed and the spelling is durations, not seconds, because this
// is read by people.
func (p Policy) Provenance() string {
	paging := "off"
	if p.Paging {
		paging = "on"
	}
	return strings.Join([]string{
		"preset=" + string(p.Preset),
		fmt.Sprintf("correlatedProjects=%d", p.CorrelatedProjects),
		"correlationWindow=" + p.CorrelationWindow.String(),
		"escalationWindow=" + p.EscalationWindow.String(),
		fmt.Sprintf("untendedMultiple=%d", p.UntendedMultiple),
		"maxSilence=" + p.MaxSilence.String(),
		"paging=" + paging,
	}, " ")
}

// PolicyDifference names what a change moved, for the audit record and for the
// sentence the API answers with. It is empty for a write that changed nothing,
// which is what stops a no-op PATCH appending a line to the audit log.
func PolicyDifference(was, now Policy) []string {
	changed := make([]string, 0, 7)
	for _, field := range []struct {
		name       string
		was, becam string
	}{
		{"preset", string(was.Preset), string(now.Preset)},
		{"correlatedProjects", fmt.Sprint(was.CorrelatedProjects), fmt.Sprint(now.CorrelatedProjects)},
		{"correlationWindow", was.CorrelationWindow.String(), now.CorrelationWindow.String()},
		{"escalationWindow", was.EscalationWindow.String(), now.EscalationWindow.String()},
		{"untendedMultiple", fmt.Sprint(was.UntendedMultiple), fmt.Sprint(now.UntendedMultiple)},
		{"maxSilence", was.MaxSilence.String(), now.MaxSilence.String()},
		{"paging", onOff(was.Paging), onOff(now.Paging)},
	} {
		if field.was != field.becam {
			changed = append(changed, fmt.Sprintf("%s %s→%s", field.name, field.was, field.becam))
		}
	}
	sort.SliceStable(changed, func(i, j int) bool { return changed[i] < changed[j] })
	return changed
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

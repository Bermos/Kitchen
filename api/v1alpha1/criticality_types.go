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

package v1alpha1

import (
	"regexp"
	"time"
)

// Criticality is how much it matters that a thing keeps working, in the order
// the platform compares it: nonCritical < important < critical.
//
// **Kitchen does not decide what is critical.** That is a board's judgement
// about the institution's functions and it is explicitly out of scope: what
// the platform does is carry the designation once somebody has made it, map it
// onto the resources that actually serve the function, and alert against the
// tolerances that come with it. Absence is a state of its own —
// *undesignated* — surfaced as such everywhere and never defaulted, because a
// silent default is a designation nobody made.
//
// Three rungs rather than four, and rather than the two the supervisory
// question actually asks. The question a regulator puts is close to binary —
// is this a critical or important function? — but "not critical" has to be a
// designation somebody made rather than the absence of one, which is what
// separates nonCritical from empty, and "important but survivable for a day"
// is a real answer institutions give and would otherwise be rounded up.
// +kubebuilder:validation:Enum=nonCritical;important;critical
type Criticality string

const (
	// CriticalityNonCritical is a designation, not an absence: somebody
	// looked and decided this supports no critical or important function.
	CriticalityNonCritical Criticality = "nonCritical"
	// CriticalityImportant supports an important function — disruption is
	// material and is not existential.
	CriticalityImportant Criticality = "important"
	// CriticalityCritical supports a critical function.
	CriticalityCritical Criticality = "critical"
)

// criticalityRank is the one ordering every comparison reads.
var criticalityRank = map[Criticality]int{
	CriticalityNonCritical: 0,
	CriticalityImportant:   1,
	CriticalityCritical:    2,
}

// Rank is the designation's position in the ordering, and -1 for anything that
// is not a designation — the empty string above all, which is how
// "undesignated" stays distinguishable from "nonCritical" in every comparison.
func (c Criticality) Rank() int {
	rank, ok := criticalityRank[c]
	if !ok {
		return -1
	}
	return rank
}

// Designated reports whether a criticality was actually declared.
func (c Criticality) Designated() bool { return c.Rank() >= 0 }

// AtLeast reports whether this designation is `other` or worse. It is the
// filter the forward mapping is asked with — "everything supporting an
// important-or-critical function" — and an undesignated thing is never at
// least anything, because nobody has said.
func (c Criticality) AtLeast(other Criticality) bool {
	if !c.Designated() {
		return false
	}
	if !other.Designated() {
		return true
	}
	return c.Rank() >= other.Rank()
}

// Criticalities lists the designations in ascending order, for refusals that
// name the vocabulary.
func Criticalities() []Criticality {
	return []Criticality{CriticalityNonCritical, CriticalityImportant, CriticalityCritical}
}

// Tolerance is a disruption tolerance — an RTO or an RPO — written as a Go
// duration of whole hours and minutes: "4h", "30m", "1h30m", "0m" for none at
// all.
//
// One spelling, enforced at admission, is the whole point. A number of minutes
// would be unambiguous and unreadable; ISO 8601 would be readable to a
// standards body and to nobody in a terminal. A Go duration is what every
// other duration in these CRDs already is, and the pattern below is what stops
// it from being the *other* Go durations: an RTO of 300ms is not a tolerance
// anybody set, it is a unit somebody guessed. Because it is a string and not a
// metav1.Duration it round-trips exactly — "4h" comes back "4h", not "4h0m0s"
// — through the API, the CLI and the dashboard alike.
// +kubebuilder:validation:Pattern=`^([0-9]+h)?([0-9]+m)?$`
// +kubebuilder:validation:MinLength=2
// +kubebuilder:validation:MaxLength=16
type Tolerance string

// tolerancePattern is the CRD's own rule, compiled. It is spelled twice —
// here and in the marker above — and the two must stay the same string: the
// marker is what the API server refuses on, and this is what every reader
// enforces, so a value the marker admits and this rejects would be a
// tolerance the platform stores and never acts on.
var tolerancePattern = regexp.MustCompile(`^([0-9]+h)?([0-9]+m)?$`)

// Declared reports whether a tolerance was set at all.
func (t Tolerance) Declared() bool { return t != "" }

// Valid reports whether a tolerance is one the platform will act on. The API
// checks it before writing, so the refusal names the spelling rather than
// echoing an admission error.
func (t Tolerance) Valid() bool {
	_, ok := t.Duration()
	return ok
}

// Duration parses the tolerance. The second return is false for an absent
// value and for anything outside the pattern — admission refuses those, so a
// reader that meets one is reading an object written before the field existed,
// and treating it as undeclared is the safe reading.
func (t Tolerance) Duration() (time.Duration, bool) {
	if t == "" || !tolerancePattern.MatchString(string(t)) {
		return 0, false
	}
	parsed, err := time.ParseDuration(string(t))
	if err != nil || parsed < 0 {
		return 0, false
	}
	return parsed, true
}

// Continuity is the designation that applies to one environment: how much its
// disruption matters, and how long a disruption may last (RTO) and how much
// data may be lost (RPO) before the institution's own tolerance is breached.
//
// It is a derived value and not a schema field. Nothing writes it: it is what
// [EffectiveContinuity] resolves from the Project's declaration and the
// Environment's own, which is why the fields alongside it say where each one
// came from.
type Continuity struct {
	Criticality Criticality `json:"criticality,omitempty"`
	RTO         Tolerance   `json:"rto,omitempty"`
	RPO         Tolerance   `json:"rpo,omitempty"`

	// Inherited names the fields that came from the Project rather than from
	// the Environment, in the order criticality, rto, rpo. It is here so that
	// no screen and no export ever shows an inherited value as a declared
	// one — the same reason an absent class is the word "unclassified"
	// rather than a blank cell.
	// +optional
	Inherited []string `json:"inherited,omitempty"`
}

// Designated reports whether anything at all applies here.
func (c Continuity) Designated() bool {
	return c.Criticality.Designated() || c.RTO.Declared() || c.RPO.Declared()
}

// EffectiveContinuity resolves the designation that applies to one
// environment.
//
// **Criticality does not inherit the way a data class does, and that is
// deliberate rather than an omission.** A data class is a containment
// property: whatever holds classified data must be rated to hold it, so a
// child narrows its parent and never widens it. Criticality is a property of
// *consequence* — what breaks in the outside world when this stops working —
// and consequence is not contained by anything. A preview environment of a
// payments service is not a critical function: nobody's payment fails while it
// is down, and a platform that woke somebody at 03:00 for a pull request's
// preview would have taught them to ignore the pager by the end of the month.
// So there is no ceiling either: a nonCritical project may perfectly well own
// an environment half the institution depends on, and the institution says so
// by designating that environment.
//
// What does happen is a fallback, and only for production. A production
// environment that declares nothing reads its project's designation, because
// production is where the project's function actually runs and declaring it
// twice would only be a second place to forget. A preview inherits nothing.
// The fallback is *derived*, never written back to the object, and every
// answer that carries it also carries [Continuity.Inherited] saying so.
func EffectiveContinuity(project *Project, env *Environment) Continuity {
	if env == nil {
		if project == nil {
			return Continuity{}
		}
		return Continuity{
			Criticality: project.Spec.Criticality,
			RTO:         project.Spec.RTO,
			RPO:         project.Spec.RPO,
		}
	}

	resolved := Continuity{
		Criticality: env.Spec.Criticality,
		RTO:         env.Spec.RTO,
		RPO:         env.Spec.RPO,
	}
	if project == nil || env.Spec.Type != EnvironmentProduction {
		return resolved
	}
	if !resolved.Criticality.Designated() && project.Spec.Criticality.Designated() {
		resolved.Criticality = project.Spec.Criticality
		resolved.Inherited = append(resolved.Inherited, "criticality")
	}
	if !resolved.RTO.Declared() && project.Spec.RTO.Declared() {
		resolved.RTO = project.Spec.RTO
		resolved.Inherited = append(resolved.Inherited, "rto")
	}
	if !resolved.RPO.Declared() && project.Spec.RPO.Declared() {
		resolved.RPO = project.Spec.RPO
		resolved.Inherited = append(resolved.Inherited, "rpo")
	}
	return resolved
}

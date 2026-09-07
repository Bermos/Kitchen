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

import "fmt"

// What a reader is meant to *do* about a condition, which is a different
// question from how bad it is.
//
// [Severity] answers "how much of a hurry is the reader in", and it is a
// property of the condition alone. A tier answers "and what is this particular
// reader meant to do about it", which is a property of the pair (condition,
// audience) — a node going NotReady is the operator's to act on now and not
// even a word a developer has, and a failed dependency install is the
// developer's to fix and a data point about queue load for the operator. One
// flag with two values cannot say that, which is the whole of #471.
//
// The tier lives on the [Signal] rather than on a subscription because it is
// catalogue knowledge — *what kind of thing is this, and what is this reader
// meant to do about it* — versioned with the catalogue per
// docs/OBSERVABILITY.md §9. What an installation configures is different and
// does not conflict: the thresholds that move a condition between tiers (#472).
// The base tier is code; the clock is policy.

// Tier is one of three deliveries.
type Tier string

const (
	// TierPage is act now: urgent, actionable by this reader, and
	// user-visible.
	//
	// Nothing in this platform wakes anybody yet, so the dashboard does not
	// use this word — see ui/src/lib/alerts.ts, which names it "Act now"
	// until something behind it actually pages (#471, decision 5). The code
	// keeps the word because the model is the industry's and renaming it
	// here would make every comparison to that model a translation.
	TierPage Tier = "page"
	// TierTicket is act in hours: a fix is owed and nobody needs waking.
	TierTicket Tier = "ticket"
	// TierLog is a data point. It appears on the list it belongs to and
	// notifies nobody — which is what makes it a tier rather than an
	// absence: the operator still wants to be able to read that a tenant's
	// build failed, without hearing about it.
	TierLog Tier = "log"
)

// tierRank orders the three against each other, highest first. It is the
// ordering of the screens, which is where decision 5 says the value of the
// three tiers is.
var tierRank = map[Tier]int{
	TierPage:   2,
	TierTicket: 1,
	TierLog:    0,
}

// Rank orders tiers, highest first. An unknown tier ranks below log, which is
// where a value nobody declared belongs.
func (t Tier) Rank() int {
	if rank, ok := tierRank[t]; ok {
		return rank
	}
	return -1
}

// Valid reports whether a tier is one of the three.
func (t Tier) Valid() bool {
	_, ok := tierRank[t]
	return ok
}

// AtLeast raises a tier to another, and never lowers it. It is what escalation
// does to the operator's row: an unacknowledged condition adds the operator as
// a ticket, and a row already above ticket is left where it is.
func (t Tier) AtLeast(floor Tier) Tier {
	if floor.Rank() > t.Rank() {
		return floor
	}
	return t
}

// Notifies reports whether a tier is one an outbound subscription may carry.
// Log does not, by definition: it is the tier that notifies nobody, and a
// subscription filter offering it would be offering to undo what the tier
// means. See NotificationSubscriptionSpec.MinTier.
func (t Tier) Notifies() bool { return t.Rank() >= TierTicket.Rank() }

// Tiers is a rule's declaration: what each of the two audiences is meant to do
// about the condition it fires on.
//
// Both halves are declared, and [Signal.validate] refuses a rule that leaves
// out an audience it is delivered to. That is the point of the type: the
// audience matrix (docs/ux/mockups/4a-audience-matrix.png) is a table with two
// columns, and a rule that filled in one of them would be a rule whose second
// reader got a row nobody had decided the meaning of.
//
// An audience the rule does not reach carries no tier and is left empty. That
// follows from [Deliveries]: an operator-audience signal reaches only the
// operator, so its Developer half is not a decision anybody declined to make —
// there is no developer delivery for it to describe.
type Tiers struct {
	// Developer is the tier for the project's members, where the rule
	// reaches them at all.
	Developer Tier
	// Operator is the tier for the platform's operators, which every rule
	// reaches — [Deliveries] is additive.
	Operator Tier
}

// For is the tier one audience reads this condition at, and whether the rule
// declared one at all.
func (t Tiers) For(audience Audience) (Tier, bool) {
	switch audience {
	case AudienceDeveloper:
		return t.Developer, t.Developer != ""
	case AudienceOperator:
		return t.Operator, t.Operator != ""
	default:
		return "", false
	}
}

// validate refuses a declaration that does not cover exactly the audiences the
// rule is delivered to: an audience with no tier is a reader nobody decided
// about, and a tier for an audience the rule never reaches is a decision about
// a delivery that does not exist.
func (t Tiers) validate(id ID, audience Audience) error {
	delivered := map[Audience]bool{}
	for _, to := range Deliveries(audience) {
		delivered[to] = true
	}
	for _, to := range []Audience{AudienceDeveloper, AudienceOperator} {
		tier, declared := t.For(to)
		switch {
		case delivered[to] && !declared:
			return fmt.Errorf("signal %q is delivered to the %s and declares no tier for them", id, to)
		case delivered[to] && !tier.Valid():
			return fmt.Errorf("signal %q declares an unknown %s tier %q", id, to, tier)
		case !delivered[to] && declared:
			return fmt.Errorf("signal %q declares a %s tier but is not delivered to them", id, to)
		}
	}
	return nil
}

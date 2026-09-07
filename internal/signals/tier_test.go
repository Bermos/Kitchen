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

import "testing"

// The one test that makes the tier model a property of the catalogue rather
// than of the rules somebody remembered: every rule, every audience it
// reaches, has a tier — and no rule has one for an audience it does not reach.
//
// It walks the real catalogue rather than a fixture, so a rule added next year
// without a tier fails here rather than reaching a screen that renders it as
// blank.
func TestEveryCatalogueRuleDeclaresATierPerAudience(t *testing.T) {
	for _, signal := range Catalogue().Signals() {
		t.Run(string(signal.ID), func(t *testing.T) {
			delivered := map[Audience]bool{}
			for _, audience := range Deliveries(signal.Audience) {
				delivered[audience] = true
			}
			for _, audience := range []Audience{AudienceDeveloper, AudienceOperator} {
				tier, declared := signal.Tiers.For(audience)
				switch {
				case delivered[audience] && !declared:
					t.Errorf("%s reaches the %s and tells them nothing about what to do", signal.ID, audience)
				case delivered[audience] && !tier.Valid():
					t.Errorf("%s declares %q for the %s, which is not a tier", signal.ID, tier, audience)
				case !delivered[audience] && declared:
					t.Errorf("%s declares a %s tier and is never delivered to them", signal.ID, audience)
				}
			}
		})
	}
}

// A developer-audience rule reaches two readers, and the whole point of the
// pair is that it may say two different things to them. This asserts the
// catalogue actually uses that — a catalogue where every rule said the same
// thing twice would be a catalogue that had gained a column and no model.
func TestTheCatalogueSaysDifferentThingsToTheTwoAudiences(t *testing.T) {
	differing := 0
	for _, signal := range Catalogue().Signals() {
		if signal.Audience != AudienceDeveloper {
			continue
		}
		if signal.Tiers.Developer != signal.Tiers.Operator {
			differing++
		}
	}
	if differing == 0 {
		t.Error("no rule in the catalogue tiers its two audiences differently, " +
			"which is the only reason the tier is a pair rather than a field")
	}
}

// A finding carries the tier of the audience it was stamped with, which is
// what makes the tier readable on a round evaluated for a screen rather than
// only on a recorded one.
func TestAFindingCarriesItsAudiencesTier(t *testing.T) {
	rule := testSignalAt("a.b", AudienceDeveloper, 1, Tiers{Developer: TierPage, Operator: TierLog})
	rule.Evaluate = func(snapshot *Snapshot) []Finding {
		return []Finding{fire("a.b", SeverityCritical, Scope{Kind: ScopeEnvironment, Project: "shop"},
			snapshot.Now, "broken", "very", "/x")}
	}
	registry, err := NewRegistry(rule)
	if err != nil {
		t.Fatalf("building a one-signal registry: %v", err)
	}

	findings := registry.Evaluate(newSnapshot())
	if len(findings) != 1 {
		t.Fatalf("one rule that fired is one finding: %+v", findings)
	}
	if findings[0].Tier != TierPage {
		t.Errorf("a developer-audience finding carries the developer's tier, got %q", findings[0].Tier)
	}
}

// A rule that could not be evaluated keeps its tier. "I cannot see whether
// production is serving" is the same claim on the reader's attention as the
// rule it stands in for, and quietly demoting it would be the one thing this
// package exists to avoid, done to the tier instead of to the list.
func TestAnUnevaluableRuleKeepsItsTier(t *testing.T) {
	rule := testSignalAt("a.b", AudienceOperator, 1, Tiers{Operator: TierPage})
	rule.Requires = []Input{InputRequests}
	registry, err := NewRegistry(rule)
	if err != nil {
		t.Fatalf("building a one-signal registry: %v", err)
	}

	snapshot := newSnapshot()
	snapshot.MarkUnreadable(InputRequests, "the store refused the query")

	findings := registry.Evaluate(snapshot)
	if len(findings) != 1 || findings[0].Severity != SeverityUnknown {
		t.Fatalf("an unreadable input is one unknown finding: %+v", findings)
	}
	if findings[0].Tier != TierPage {
		t.Errorf("a rule that could not be read keeps its tier, got %q", findings[0].Tier)
	}
}

// The tier a transition carries is the *delivery's*, not the rule's. One
// developer condition is two rows and they must be able to disagree, which is
// the whole reason the row key is (fingerprint, audience).
func TestATransitionCarriesTheDeliverysTier(t *testing.T) {
	tracker := trackerOver(t, testSignalAt("workload.crashloop", AudienceDeveloper, 1,
		Tiers{Developer: TierPage, Operator: TierTicket}))

	transitions := tracker.Observe(Findings{crashLoop(Scope{
		Kind: ScopeEnvironment, Project: "shop", Environment: "shop-production", Name: "web",
	})}, transitionRoundOne)
	if len(transitions) != 2 {
		t.Fatalf("a developer condition is two deliveries: %+v", transitions)
	}

	byAudience := map[Audience]Tier{}
	for _, transition := range transitions {
		byAudience[transition.Audience] = transition.Tier
	}
	if byAudience[AudienceDeveloper] != TierPage {
		t.Errorf("the developer's row is the developer's tier: %+v", byAudience)
	}
	if byAudience[AudienceOperator] != TierTicket {
		t.Errorf("the operator's row is the operator's tier: %+v", byAudience)
	}
}

func TestTierOrdering(t *testing.T) {
	if !(TierPage.Rank() > TierTicket.Rank() && TierTicket.Rank() > TierLog.Rank()) {
		t.Error("the three tiers order page, ticket, log")
	}
	if got := TierLog.AtLeast(TierTicket); got != TierTicket {
		t.Errorf("AtLeast raises: %q", got)
	}
	if got := TierPage.AtLeast(TierTicket); got != TierPage {
		t.Errorf("AtLeast never lowers: %q", got)
	}
	if TierLog.Notifies() {
		t.Error("the log tier notifies nobody — that is what the tier is")
	}
	if !TierTicket.Notifies() || !TierPage.Notifies() {
		t.Error("the two tiers above log are the ones a subscription can carry")
	}
}

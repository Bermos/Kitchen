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

package cli

import (
	"strings"
	"testing"
)

// The criticality commands, run the way somebody would run them: argv in,
// HTTP out, one JSON document back.

func criticalityFixture(h *harness) {
	h.platform.criticality = &criticalityMap{
		Minimum: "critical",
		Functions: []criticalityFunction{{
			Project:     "shop",
			Criticality: "critical",
			RTO:         "1h",
			RPO:         "5m",
			Environments: []criticalityEnvironment{{
				Name: "shop-production", Type: "production", Criticality: "critical",
				RTO: "1h", Inherited: []string{"criticality", "rto"},
				Release: "shop-rel-9", Domains: []string{"shop.example.com"},
			}},
			Claims: []criticalityClaim{{
				Name: "shop-db", Type: "postgres", Connection: "neon", Provider: "neon",
				DataClass: "confidential", Residency: "aws-eu-central-1",
			}},
			ThirdParties: []string{"github", "neon"},
		}},
		Undesignated: 4,
		Depth:        "the graph the platform reconciles",
	}
	h.platform.dependents = &dependents{
		Subject: criticalitySubject{Kind: "provider", Name: "neon", Provider: "neon",
			Connections: []string{"neon"}},
		Affected: []criticalityDependent{{
			Project: "shop", Environment: "shop-production", Type: "production",
			Criticality: "critical", RTO: "1h", Through: []string{"claim shop-db"},
		}},
		Counts:      map[string]int{"critical": 1},
		TightestRTO: "1h",
	}
}

func TestCriticalitySendsTheFilterAndAnswersTheWholeMap(t *testing.T) {
	h := newHarness(t)
	criticalityFixture(h)

	if code := h.run("criticality", "--json",
		"--project", "shop", "--criticality", "critical"); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, h.stderr.String())
	}
	answer := criticalityMap{}
	h.answer(&answer)
	if len(answer.Functions) != 1 || answer.Functions[0].Project != "shop" {
		t.Fatalf("answered %+v", answer.Functions)
	}
	// The whole tree survives the trip, which is the point of the command:
	// one call, and the register is on stdout.
	function := answer.Functions[0]
	if len(function.Environments) != 1 || function.Environments[0].Release != "shop-rel-9" {
		t.Fatalf("the environments must come through whole, got %+v", function.Environments)
	}
	if len(function.Claims) != 1 || function.Claims[0].Provider != "neon" {
		t.Fatalf("the third party behind the claim must come through, got %+v", function.Claims)
	}
	if answer.Undesignated != 4 {
		t.Fatalf("the undesignated count must come through, got %d", answer.Undesignated)
	}

	sent := h.platform.sent("GET", "/compliance/criticality")
	if len(sent) != 1 {
		t.Fatalf("want one call, got %v", h.platform.requests)
	}
	for _, fragment := range []string{"project=shop", "criticality=critical"} {
		if !strings.Contains(sent[0].Query, fragment) {
			t.Errorf("the query must carry %s, got %q", fragment, sent[0].Query)
		}
	}
}

func TestDependentsInsistsOnOneSubjectBeforeItCallsAnything(t *testing.T) {
	h := newHarness(t)
	criticalityFixture(h)

	for name, args := range map[string][]string{
		"nothing": {"criticality", "dependents", "--json"},
		"both": {"criticality", "dependents", "--json",
			"--connection", "neon", "--provider", "neon"},
	} {
		t.Run(name, func(t *testing.T) {
			if code := h.run(args...); code != exitUsage {
				t.Fatalf("want a usage failure, got %d\nstderr: %s", code, h.stderr.String())
			}
			if len(h.platform.sent("GET", "/compliance/dependents")) != 0 {
				t.Fatal("a usage failure must not reach the platform")
			}
		})
	}
}

func TestDependentsAnswersWhatBreaks(t *testing.T) {
	h := newHarness(t)
	criticalityFixture(h)

	if code := h.run("criticality", "dependents", "--provider", "neon", "--json"); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, h.stderr.String())
	}
	answer := dependents{}
	h.answer(&answer)
	if len(answer.Affected) != 1 || answer.Affected[0].Environment != "shop-production" {
		t.Fatalf("answered %+v", answer.Affected)
	}
	if answer.TightestRTO != "1h" {
		t.Fatalf("the tightest objective is the headline, got %q", answer.TightestRTO)
	}
	if len(answer.Subject.Connections) != 1 {
		t.Fatalf("a provider query must say which connections it resolved to, got %+v", answer.Subject)
	}

	sent := h.platform.sent("GET", "/compliance/dependents")
	if len(sent) != 1 || !strings.Contains(sent[0].Query, "provider=neon") {
		t.Fatalf("the query must carry the subject, got %v", h.platform.requests)
	}
}

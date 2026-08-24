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

package policy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The engine's contract: same bundle + same input = same verdict, no way out
// of the sandbox, and the three verdicts mean exactly what they say.

// evaluationTime keeps every test's input digest deterministic.
var evaluationTime = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

func minimalInput(kind string) Input {
	return Input{
		Kind:        kind,
		At:          evaluationTime,
		Project:     ProjectFacts{Name: "shop"},
		Environment: EnvironmentFacts{Name: "shop-production", Type: "production"},
		Release:     ReleaseFacts{Name: "shop-rel-1", Digest: "sha256:" + strings.Repeat("a", 64)},
	}
}

func evaluate(t *testing.T, input Input) Result {
	t.Helper()
	result, err := Evaluate(context.Background(), DefaultBundle(), input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTheDefaultBundleDemandsNothingUnparameterized(t *testing.T) {
	// A bare input against an unparameterized bundle: every rule is either
	// opted in through parameters or inert until its facts exist, so this is
	// allowed — the bar is whatever the owners turned on, and they turned on
	// nothing.
	result := evaluate(t, minimalInput(KindPromotion))
	if result.Verdict != VerdictAllowed || len(result.Fired) != 0 {
		t.Fatalf("an unparameterized bundle must demand nothing, got %+v", result)
	}
}

func TestAbsentInputFieldsDoNotError(t *testing.T) {
	// The emptiest possible input, with every opt-in rule turned on: rules
	// must fire for missing evidence, never error on missing fields.
	input := Input{
		Kind: KindPromotion,
		At:   evaluationTime,
		Parameters: map[string]string{
			"require-provenance":         "true",
			"require-sbom":               "true",
			"requiredGates":              "trivy, gitleaks",
			"require-independent-review": "true",
			"no-self-approval":           "true",
			"maxSeverity":                "high",
		},
	}
	result := evaluate(t, input)
	if result.Verdict != VerdictBlocked {
		t.Fatalf("want blocked, got %+v", result)
	}
	fired := map[string]int{}
	for _, rule := range result.Fired {
		fired[rule.Rule]++
	}
	for _, want := range []string{"require-provenance", "require-sbom", "require-independent-review"} {
		if fired[want] != 1 {
			t.Errorf("rule %s fired %d times, want 1: %+v", want, fired[want], result.Fired)
		}
	}
	// One firing per missing gate, naming the gate.
	if fired["require-gate"] != 2 {
		t.Errorf("require-gate fired %d times for two missing gates, want 2", fired["require-gate"])
	}
	// max-severity has no findings to object to, and no-self-approval no
	// recorded review: both stay quiet.
	if fired["max-severity"] != 0 || fired["no-self-approval"] != 0 {
		t.Errorf("rules without facts must stay quiet, got %+v", result.Fired)
	}
}

func TestProvenanceAndSBOMAndGatesAreSatisfiedByEvidence(t *testing.T) {
	input := minimalInput(KindPromotion)
	input.Parameters = map[string]string{
		"require-provenance": "true",
		"require-sbom":       "true",
		"requiredGates":      "trivy",
	}
	input.Evidence = []Evidence{
		{PredicateType: "https://slsa.dev/provenance/v1", Verified: true},
		{PredicateType: "https://spdx.dev/Document", Verified: true},
		{
			PredicateType: "https://kitchen.bermos.dev/attestation/quality-gate/v1",
			Verified:      true,
			Predicate:     json.RawMessage(`{"gate":"trivy","findings":[]}`),
		},
	}
	result := evaluate(t, input)
	if result.Verdict != VerdictAllowed {
		t.Fatalf("want allowed, got %+v", result)
	}
}

func TestReviewRulesReadTheApprovalPredicate(t *testing.T) {
	input := minimalInput(KindPromotion)
	input.Parameters = map[string]string{
		"require-independent-review": "true",
		"no-self-approval":           "true",
	}
	input.Evidence = []Evidence{{
		PredicateType: "https://kitchen.bermos.dev/attestation/pull-request-approval/v1",
		Verified:      true,
		Predicate:     json.RawMessage(`{"independentlyApproved":false,"selfApproved":true}`),
	}}
	result := evaluate(t, input)
	if result.Verdict != VerdictBlocked || len(result.Fired) != 2 {
		t.Fatalf("both review rules must fire, got %+v", result)
	}

	input.Evidence[0].Predicate = json.RawMessage(`{"independentlyApproved":true,"selfApproved":false}`)
	if result := evaluate(t, input); result.Verdict != VerdictAllowed {
		t.Fatalf("an independently reviewed change must pass, got %+v", result)
	}
}

func TestMaxSeverityConsultsVEX(t *testing.T) {
	input := minimalInput(KindPromotion)
	input.Parameters = map[string]string{"maxSeverity": "high"}
	input.Evidence = []Evidence{{
		PredicateType: "https://kitchen.bermos.dev/attestation/vulnerability-scan/v1",
		Verified:      true,
		Predicate: json.RawMessage(`{"findings":[
			{"vulnerability":"CVE-2026-1","severity":"critical"},
			{"vulnerability":"CVE-2026-2","severity":"high"},
			{"vulnerability":"CVE-2026-3","severity":"CRITICAL"}
		]}`),
	}}

	result := evaluate(t, input)
	if result.Verdict != VerdictBlocked || len(result.Fired) != 2 {
		t.Fatalf("both critical findings must fire, case-insensitively: %+v", result)
	}

	// A not_affected VEX statement, justified from the enumeration and signed
	// by a key the platform holds, suppresses exactly its vulnerability.
	input.VEX = []VEXStatement{
		{
			Vulnerability: "CVE-2026-1",
			Status:        "not_affected",
			Justification: "vulnerable_code_not_present",
			Author:        vexAuthor,
			Verified:      true,
		},
		{Vulnerability: "CVE-2026-3", Status: "affected", Author: vexAuthor, Verified: true},
	}
	result = evaluate(t, input)
	if len(result.Fired) != 1 || !strings.Contains(result.Fired[0].Message, "CVE-2026-3") {
		t.Fatalf("only the unsuppressed finding may fire, got %+v", result.Fired)
	}
}

// vexAuthor keeps the same name out of a dozen literals; goconst objects
// otherwise and the vocabulary is the point.
const vexAuthor = "security@shop.example"

// vexScanEvidence is one vulnerability-scan attestation carrying one critical
// finding, which is the smallest thing max-severity will object to.
func vexScanEvidence() []Evidence {
	return []Evidence{{
		PredicateType: "https://kitchen.bermos.dev/attestation/vulnerability-scan/v1",
		Verified:      true,
		Predicate:     json.RawMessage(`{"findings":[{"vulnerability":"CVE-2026-1","severity":"critical"}]}`),
	}}
}

// vexInput is that finding, a ceiling it exceeds, and one statement about it.
func vexInput(statement VEXStatement) Input {
	input := minimalInput(KindPromotion)
	input.Parameters = map[string]string{"maxSeverity": "high"}
	input.Evidence = vexScanEvidence()
	input.VEX = []VEXStatement{statement}
	return input
}

func suppressed(t *testing.T, input Input) bool {
	t.Helper()
	return len(evaluate(t, input).Fired) == 0
}

func TestNotAffectedNeedsAJustificationFromTheEnumeration(t *testing.T) {
	// Free text is not a justification. The API refuses one at ingest; this
	// is the half that also covers a document some other tool attached, and
	// the reason the check exists in both places.
	for _, justification := range []string{"", "we looked at it and it is fine", "not_exploitable"} {
		input := vexInput(VEXStatement{
			Vulnerability: "CVE-2026-1",
			Status:        "not_affected",
			Justification: justification,
			Author:        vexAuthor,
			Verified:      true,
		})
		if suppressed(t, input) {
			t.Errorf("justification %q must not suppress anything", justification)
		}
	}

	// Every one of OpenVEX's five does.
	for _, justification := range []string{
		"component_not_present",
		"vulnerable_code_not_present",
		"vulnerable_code_not_in_execute_path",
		"vulnerable_code_cannot_be_controlled_by_adversary",
		"inline_mitigations_already_exist",
	} {
		input := vexInput(VEXStatement{
			Vulnerability: "CVE-2026-1",
			Status:        "not_affected",
			Justification: justification,
			Author:        vexAuthor,
			Verified:      true,
		})
		if !suppressed(t, input) {
			t.Errorf("justification %q is in the enumeration and must suppress", justification)
		}
	}
}

func TestOnlyNotAffectedSuppresses(t *testing.T) {
	for _, status := range []string{"affected", "fixed", "under_investigation"} {
		input := vexInput(VEXStatement{
			Vulnerability: "CVE-2026-1",
			Status:        status,
			Justification: "vulnerable_code_not_present",
			Author:        vexAuthor,
			Verified:      true,
		})
		if suppressed(t, input) {
			t.Errorf("status %q must not suppress a finding", status)
		}
	}
}

func TestVEXFromAnUntrustedSignerIsRefusedByDefault(t *testing.T) {
	unverified := VEXStatement{
		Vulnerability: "CVE-2026-1",
		Status:        "not_affected",
		Justification: "vulnerable_code_not_present",
		Author:        vexAuthor,
	}

	// A statement whose envelope no key the platform holds verified — a VEX
	// document somebody pushed with cosign under their own key — is listed in
	// the input and believed by nothing. That is the default.
	if suppressed(t, vexInput(unverified)) {
		t.Fatal("an unverified VEX statement must not suppress a finding by default")
	}

	// An installation that wants the looser reading says so, and gets it.
	loose := vexInput(unverified)
	loose.Parameters["vexRequireVerified"] = "false"
	if !suppressed(t, loose) {
		t.Fatal("vexRequireVerified=false must honour an unverified statement")
	}
}

func TestVEXTrustedAuthorsNarrowsWhoIsBelieved(t *testing.T) {
	statement := VEXStatement{
		Vulnerability: "CVE-2026-1",
		Status:        "not_affected",
		Justification: "vulnerable_code_not_present",
		Author:        vexAuthor,
		Verified:      true,
	}

	named := vexInput(statement)
	named.Parameters["vexTrustedAuthors"] = "vendor@upstream.example, " + vexAuthor
	if !suppressed(t, named) {
		t.Fatal("a named author's statement must suppress")
	}

	// Case-insensitively, because the platform's own trustedAuthors list is
	// (VEXSpec.AdmitsAuthor) and §10.5 presents the two as one idea at two
	// levels. An operator copying an address off the singleton into an
	// environment's parameters must not get a different answer for it.
	shouted := vexInput(statement)
	shouted.Parameters["vexTrustedAuthors"] = " Security@Shop.Example "
	if !suppressed(t, shouted) {
		t.Fatal("the same author spelled in another case was silently not believed")
	}

	others := vexInput(statement)
	others.Parameters["vexTrustedAuthors"] = "vendor@upstream.example"
	if suppressed(t, others) {
		t.Fatal("an author this environment does not name must not suppress")
	}
}

func TestVEXMaxAgeDaysBoundsHowLongAStatementIsCurrent(t *testing.T) {
	statement := VEXStatement{
		Vulnerability: "CVE-2026-1",
		Status:        "not_affected",
		Justification: "vulnerable_code_not_present",
		Author:        vexAuthor,
		Verified:      true,
		Timestamp:     evaluationTime.Add(-10 * 24 * time.Hour).Format(time.RFC3339),
	}

	within := vexInput(statement)
	within.Parameters["vexMaxAgeDays"] = "30"
	if !suppressed(t, within) {
		t.Fatal("a statement inside the bound must suppress")
	}

	beyond := vexInput(statement)
	beyond.Parameters["vexMaxAgeDays"] = "7"
	if suppressed(t, beyond) {
		t.Fatal("a statement older than the bound must stop suppressing")
	}

	// The age is judged against input.at, not against the reader's clock:
	// replaying an old decision must suppress exactly what it suppressed.
	replayed := beyond
	replayed.At = evaluationTime.Add(-9 * 24 * time.Hour)
	if !suppressed(t, replayed) {
		t.Fatal("the bound must be judged against the evaluation's own clock")
	}

	// Under a bound, a statement nobody dated cannot be shown to be current.
	undated := statement
	undated.Timestamp = ""
	input := vexInput(undated)
	input.Parameters["vexMaxAgeDays"] = "30"
	if suppressed(t, input) {
		t.Fatal("an undated statement must not suppress while an age bound is set")
	}

	// A bound nobody can read bounds everything out.
	nonsense := vexInput(statement)
	nonsense.Parameters["vexMaxAgeDays"] = "a fortnight"
	if suppressed(t, nonsense) {
		t.Fatal("an unreadable vexMaxAgeDays must suppress nothing")
	}
}

func TestAnUnknownMaxSeverityIsAFiringNotAnError(t *testing.T) {
	input := minimalInput(KindPromotion)
	input.Parameters = map[string]string{"maxSeverity": "terrifying"}
	result := evaluate(t, input)
	if result.Verdict != VerdictBlocked || len(result.Fired) != 1 {
		t.Fatalf("a misspelt ceiling must block, not silently allow: %+v", result)
	}
	if !strings.Contains(result.Fired[0].Message, "terrifying") {
		t.Fatalf("the firing must name the bad value, got %q", result.Fired[0].Message)
	}
}

// classConfidential keeps goconst quiet where the vocabulary is the point.
const classConfidential = "confidential"

func TestDataClassMustNotExceedTheEnvironments(t *testing.T) {
	input := minimalInput(KindPromotion)

	// Both sides unclassified: nothing asserted, nothing to exceed — inert.
	if result := evaluate(t, input); len(result.Fired) != 0 {
		t.Fatalf("two unclassified sides must stay inert, got %+v", result.Fired)
	}

	// A classified project into an unclassified environment fires: the data
	// has a class and the target has no rating.
	input.Project.DataClass = classConfidential
	result := evaluate(t, input)
	if result.Verdict != VerdictBlocked || result.Fired[0].Rule != "dataclass-le-environment" {
		t.Fatalf("classified into unclassified must block, got %+v", result)
	}
	if !strings.Contains(result.Fired[0].Message, "unclassified") {
		t.Fatalf("the firing must say the environment is unclassified, got %q", result.Fired[0].Message)
	}

	// The other way round is narrowing's degenerate case: an unclassified
	// project asserts nothing to exceed with.
	input.Project.DataClass = ""
	input.Environment.DataClass = "internal"
	if result := evaluate(t, input); len(result.Fired) != 0 {
		t.Fatalf("an unclassified project must stay inert, got %+v", result.Fired)
	}

	input.Project.DataClass = classConfidential
	result = evaluate(t, input)
	if result.Verdict != VerdictBlocked || result.Fired[0].Rule != "dataclass-le-environment" {
		t.Fatalf("confidential into internal must block, got %+v", result)
	}

	input.Environment.DataClass = "strictlyConfidential"
	if result := evaluate(t, input); result.Verdict != VerdictAllowed {
		t.Fatalf("confidential into strictlyConfidential is narrowing, got %+v", result)
	}
}

func TestPreviewsRefuseProductionDerivedData(t *testing.T) {
	input := minimalInput(KindPromotion)
	input.Environment.Type = "preview"
	input.Claims = []Claim{
		{Name: "db", Type: "postgres", Provenance: "production"},
		{Name: "cache", Type: "postgres"}, // undeclared: the worst case, not clean
		{Name: "auth", Type: "oidcClient", Provenance: "synthetic"},
	}
	result := evaluate(t, input)
	if result.Verdict != VerdictBlocked || len(result.Fired) != 2 {
		t.Fatalf("the production claim and the undeclared one must both fire, got %+v", result)
	}
	fired := map[string]string{}
	for _, rule := range result.Fired {
		if rule.Rule != "data-provenance-preview" {
			t.Fatalf("only the provenance rule may fire, got %+v", result.Fired)
		}
		fired[rule.Message] = rule.Rule
	}
	messages := strings.Join(mapKeys(fired), "\n")
	if !strings.Contains(messages, `"db"`) || !strings.Contains(messages, `"cache"`) {
		t.Fatalf("the firings must name both claims, got %q", messages)
	}
	if strings.Contains(messages, `"auth"`) {
		t.Fatalf("a synthetic claim must not fire, got %q", messages)
	}

	// A production environment takes production data.
	input.Environment.Type = "production"
	if result := evaluate(t, input); len(result.Fired) != 0 {
		t.Fatalf("the rule is about previews, got %+v", result.Fired)
	}

	// The parameter can widen what a preview accepts — and a policy that
	// accepts production accepts the undeclared with it, because tolerating
	// the known worst leaves nothing to protect from the unknown.
	input.Environment.Type = "preview"
	input.Parameters = map[string]string{"preview-data-provenance": "masked,synthetic,production"}
	if result := evaluate(t, input); len(result.Fired) != 0 {
		t.Fatalf("a widened allowance must pass, got %+v", result.Fired)
	}
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func TestExceptionsWaiveButNeverSilence(t *testing.T) {
	input := minimalInput(KindPromotion)
	input.Parameters = map[string]string{"require-provenance": "true", "require-sbom": "true"}
	input.Exceptions = []Exception{{
		Name:      "incident-441",
		RuleIDs:   []string{"require-provenance"},
		ExpiresAt: evaluationTime.Add(time.Hour),
	}}

	result := evaluate(t, input)
	if result.Verdict != VerdictBlocked {
		t.Fatalf("an exception for one of two rules leaves the other standing, got %+v", result)
	}

	input.Exceptions[0].RuleIDs = []string{"require-provenance", "require-sbom"}
	result = evaluate(t, input)
	if result.Verdict != VerdictAllowedWithException {
		t.Fatalf("all rules waived is allowed-with-exception, got %+v", result)
	}
	if len(result.Fired) != 2 {
		t.Fatalf("waived rules still report, got %+v", result.Fired)
	}
	for _, rule := range result.Fired {
		if !rule.Waived || rule.Exception != "incident-441" {
			t.Fatalf("every fired rule must name its waiver, got %+v", rule)
		}
	}

	// Expiry is judged against the input's own clock, which is what makes a
	// replay reproduce the original waiving.
	input.Exceptions[0].ExpiresAt = evaluationTime.Add(-time.Minute)
	if result := evaluate(t, input); result.Verdict != VerdictBlocked {
		t.Fatalf("an expired exception waives nothing, got %+v", result)
	}
}

func TestABundleCallingHTTPSendIsRefusedAtCompileTime(t *testing.T) {
	// The property that makes decisions replayable: a policy cannot fetch, so
	// its inputs are exactly what was stored. Each of the removed builtins is
	// refused at compile time, before any rule runs.
	for name, module := range map[string]string{
		"http.send": `package kitchen.promotion
deny contains {"rule": "phone-home", "message": "no"} if {
	http.send({"method": "get", "url": "https://example.com"})
}`,
		"net.lookup_ip_addr": `package kitchen.promotion
deny contains {"rule": "resolve", "message": "no"} if {
	net.lookup_ip_addr("example.com")
}`,
		"opa.runtime": `package kitchen.promotion
deny contains {"rule": "peek", "message": "no"} if {
	opa.runtime().env
}`,
	} {
		_, err := Evaluate(context.Background(), Bundle{"evil.rego": module}, minimalInput(KindPromotion))
		if err == nil {
			t.Fatalf("a bundle calling %s must be refused at compile time", name)
		}
		if !strings.Contains(err.Error(), strings.Split(name, "(")[0]) {
			t.Fatalf("the refusal for %s must name the builtin, got: %v", name, err)
		}
	}
}

func TestABundleWithoutTheEntryPointIsAnErrorNotAnAllow(t *testing.T) {
	bundle := Bundle{"other.rego": `package kitchen.other

deny contains {"rule": "x", "message": "y"} if { true }`}
	_, err := Evaluate(context.Background(), bundle, minimalInput(KindPromotion))
	if err == nil || !strings.Contains(err.Error(), "data.kitchen.promotion.deny") {
		t.Fatalf("a bundle that never defines the entry point must error, got %v", err)
	}
}

func TestADenyEntryWithoutARuleIDIsRefused(t *testing.T) {
	bundle := Bundle{"anonymous.rego": `package kitchen.promotion

deny contains {"message": "who fired this"} if { true }`}
	_, err := Evaluate(context.Background(), bundle, minimalInput(KindPromotion))
	if err == nil || !strings.Contains(err.Error(), "rule id") {
		t.Fatalf("an anonymous violation must be refused, got %v", err)
	}
}

func TestBundleDataIsServedToTheRules(t *testing.T) {
	bundle := Bundle{
		"rules.rego": `package kitchen.promotion

deny contains {"rule": "blocked-branch", "message": "this branch never promotes"} if {
	object.get(object.get(object.get(input, "release", {}), "build", {}), "branch", "") in data.blocked_branches
}`,
		"data.json": `{"blocked_branches": ["experiments"]}`,
	}
	input := minimalInput(KindPromotion)
	input.Release.Build = BuildFacts{Branch: "experiments"}
	result, err := Evaluate(context.Background(), bundle, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != VerdictBlocked {
		t.Fatalf("the bundle's data.json must reach the rules, got %+v", result)
	}
}

func TestEvaluationIsDeterministic(t *testing.T) {
	input := minimalInput(KindRescan)
	input.Parameters = map[string]string{
		"require-provenance": "true",
		"require-sbom":       "true",
		"requiredGates":      "trivy,gitleaks,licences",
	}
	first := evaluate(t, input)
	for range 5 {
		if again := evaluate(t, input); !equalResults(first, again) {
			t.Fatalf("two evaluations of one input disagree:\n%+v\n%+v", first, again)
		}
	}
}

func equalResults(a, b Result) bool {
	if a.Verdict != b.Verdict || len(a.Fired) != len(b.Fired) {
		return false
	}
	for i := range a.Fired {
		if a.Fired[i] != b.Fired[i] {
			return false
		}
	}
	return true
}

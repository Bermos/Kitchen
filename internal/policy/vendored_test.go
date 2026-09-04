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
	"encoding/json"
	"strings"
	"testing"
)

// The default bundle's answer to software this platform did not build (#309).
//
// Two things are being pinned here and they pull in opposite directions. The
// three commit-shaped rules must **refuse** a vendored artifact and must never
// be satisfied by a substitute claim dressed up to look like a review. The two
// vendored rules must actually work — a control that could only ever refuse
// would be one nobody turns on.

// The two identities the four-eyes rule is about: whoever admitted the digest
// onto the platform, and whoever is asking to move it. They are constants
// because the whole of digest-approved-by-someone-else is whether they are the
// same person.
const (
	admitter  = "ana@example.com"
	requester = "bob@example.com"
)

// vendoredInput is a release of one image somebody else published.
func vendoredInput(artifacts ...VendoredArtifact) Input {
	input := minimalInput(KindPromotion)
	if len(artifacts) == 0 {
		artifacts = []VendoredArtifact{{
			Reference:  "ghcr.io/vendor/app:2026.9.1",
			Digest:     "sha256:" + strings.Repeat("a", 64),
			AdmittedBy: admitter,
			Signature:  "verified",
		}}
	}
	input.Release.Vendored = artifacts
	return input
}

// firedBy counts the rules a result fired, by id.
func firedBy(result Result) map[string]int {
	fired := map[string]int{}
	for _, rule := range result.Fired {
		fired[rule.Rule]++
	}
	return fired
}

// messageFor is the first message a named rule fired with.
func messageFor(result Result, rule string) string {
	for _, entry := range result.Fired {
		if entry.Rule == rule {
			return entry.Message
		}
	}
	return ""
}

func TestTheCommitShapedRulesRefuseAVendoredArtifactAndSayWhy(t *testing.T) {
	input := vendoredInput()
	input.Parameters = map[string]string{
		"require-pull-request":       "true",
		"require-independent-review": "true",
		"no-self-approval":           "true",
	}
	result := evaluate(t, input)
	if result.Verdict != VerdictBlocked {
		t.Fatalf("an environment requiring a review admitted an artifact with no commit: %+v", result)
	}

	fired := firedBy(result)
	for _, rule := range []string{"require-pull-request", "require-independent-review", "no-self-approval"} {
		if fired[rule] != 1 {
			t.Errorf("rule %s fired %d times, want exactly one refusal: %+v", rule, fired[rule], result.Fired)
			continue
		}
		message := messageFor(result, rule)
		// The refusal has to read as a deliberate outcome rather than as a
		// bug, which means naming the artifact and saying what cannot be
		// satisfied — not "no attestation asserts…", which sends somebody
		// looking for an attestation that can never exist.
		if !strings.Contains(message, "published by somebody else") {
			t.Errorf("rule %s refused with %q, which does not say why it cannot be satisfied", rule, message)
		}
		if !strings.Contains(message, "web") {
			t.Errorf("rule %s refused with %q, which does not name the image", rule, message)
		}
	}
}

func TestNothingSubstitutesForAReviewOnAVendoredArtifact(t *testing.T) {
	// An adoption record with two people in it, and a signature that
	// verified. Neither is a review, and the review rules must not be
	// satisfied by either — which is the whole of "nothing fakes a commit".
	input := vendoredInput()
	input.RequestedBy = requester
	input.Parameters = map[string]string{"require-independent-review": "true"}
	input.Evidence = []Evidence{{
		PredicateType: "https://kitchen.bermos.dev/attestation/artifact-adoption/v1",
		Source:        "platform-observed",
		Verified:      true,
		Predicate: json.RawMessage(
			`{"admittedBy":"ana@example.com","signature":{"result":"verified"},"independentlyApproved":true}`),
	}}
	result := evaluate(t, input)
	if result.Verdict != VerdictBlocked {
		t.Fatalf("an adoption record satisfied a review requirement: %+v", result)
	}
	if firedBy(result)["require-independent-review"] != 1 {
		t.Errorf("want the review rule to refuse once, got %+v", result.Fired)
	}
}

func TestTheReviewRulesAreUnchangedForABuiltArtifact(t *testing.T) {
	// The vendored clauses must not leak into the built path: the same
	// input with no vendored artifacts still fires the ordinary refusal,
	// with the ordinary message.
	input := minimalInput(KindPromotion)
	input.Parameters = map[string]string{"require-independent-review": "true"}
	result := evaluate(t, input)
	if message := messageFor(result, "require-independent-review"); message !=
		"no attestation asserts this change was independently reviewed" {
		t.Errorf("a built artifact's refusal reads %q, and it has not changed", message)
	}
	// require-pull-request is the build-time control's id and has no
	// engine-side rule for a built artifact — a second implementation of one
	// requirement is how two answers to one question come about.
	input.Parameters["require-pull-request"] = "true"
	if fired := firedBy(evaluate(t, input))["require-pull-request"]; fired != 0 {
		t.Errorf("require-pull-request fired %d times for a built artifact, want none", fired)
	}
}

func TestUpstreamSignatureVerifiedTellsTheThreeFactsApart(t *testing.T) {
	parameters := map[string]string{"upstream-signature-verified": "true"}

	verified := vendoredInput()
	verified.Parameters = parameters
	if result := evaluate(t, verified); result.Verdict != VerdictAllowed {
		t.Fatalf("a verified upstream signature was refused: %+v", result)
	}

	unsigned := vendoredInput(VendoredArtifact{
		Workload: "cache", Reference: "docker.io/library/redis:7.4", Signature: "none",
	})
	unsigned.Parameters = parameters
	message := messageFor(evaluate(t, unsigned), "upstream-signature-verified")
	if !strings.Contains(message, "without a signature") || !strings.Contains(message, "cache") {
		t.Errorf("an unsigned image refused with %q, which does not say what is missing", message)
	}

	unverifiable := vendoredInput(VendoredArtifact{Signature: "unverifiable"})
	unverifiable.Parameters = parameters
	message = messageFor(evaluate(t, unverifiable), "upstream-signature-verified")
	// The two findings are different and an operator sent to look at the
	// wrong one wastes an afternoon.
	if !strings.Contains(message, "did not verify") {
		t.Errorf("an unverifiable signature refused with %q, want the other finding", message)
	}
}

func TestUpstreamSignatureVerifiedChecksTheIdentityTheEnvironmentNames(t *testing.T) {
	input := vendoredInput(VendoredArtifact{
		Reference: "ghcr.io/vendor/app:1", Signature: "verified", SignatureIdentity: "somebody@else.example",
	})
	input.Parameters = map[string]string{
		"upstream-signature-verified": "true",
		"upstreamSignatureIdentity":   "releases@vendor.example",
	}
	message := messageFor(evaluate(t, input), "upstream-signature-verified")
	if !strings.Contains(message, "somebody@else.example") ||
		!strings.Contains(message, "releases@vendor.example") {
		t.Errorf("refused with %q, want both identities named", message)
	}
}

func TestUpstreamSignatureVerifiedIsInertForABuiltArtifact(t *testing.T) {
	// A built artifact has no upstream. The questions asked of one are
	// require-provenance and the review rules.
	input := minimalInput(KindPromotion)
	input.Parameters = map[string]string{"upstream-signature-verified": "true"}
	if result := evaluate(t, input); result.Verdict != VerdictAllowed {
		t.Fatalf("the upstream signature rule fired over an artifact with no upstream: %+v", result)
	}
}

func TestDigestApprovedBySomeoneElseIsTheFourEyesControlForVendoredSoftware(t *testing.T) {
	parameters := map[string]string{"digest-approved-by-someone-else": "true"}

	// Two people: the one who admitted the digest and the one moving it.
	elsewhere := vendoredInput()
	elsewhere.Parameters = parameters
	elsewhere.RequestedBy = requester
	if result := evaluate(t, elsewhere); result.Verdict != VerdictAllowed {
		t.Fatalf("two different people were refused: %+v", result)
	}

	// One person, twice. Case is not a second identity.
	same := vendoredInput()
	same.Parameters = parameters
	same.RequestedBy = "Ana@Example.com"
	message := messageFor(evaluate(t, same), "digest-approved-by-someone-else")
	if !strings.Contains(message, "also requesting this move") {
		t.Errorf("a self-approved digest refused with %q", message)
	}

	// Nobody recorded. A four-eyes rule whose first eye is nobody has to
	// fire rather than pass.
	unattributed := vendoredInput(VendoredArtifact{Reference: "ghcr.io/vendor/app:1"})
	unattributed.Parameters = parameters
	unattributed.RequestedBy = requester
	message = messageFor(evaluate(t, unattributed), "digest-approved-by-someone-else")
	if !strings.Contains(message, "no record of who admitted") {
		t.Errorf("an unattributed digest refused with %q", message)
	}
}

func TestDigestApprovedBySomeoneElseAsksNothingOnARescan(t *testing.T) {
	// A scheduled re-evaluation of what is already deployed is not a request
	// by anyone, and a four-eyes rule fired against nobody would report every
	// vendored environment as drifting every hour.
	input := vendoredInput()
	input.Kind = KindRescan
	input.RequestedBy = ""
	input.Parameters = map[string]string{"digest-approved-by-someone-else": "true"}
	if result := evaluate(t, input); result.Verdict != VerdictAllowed {
		t.Fatalf("the four-eyes rule fired on a rescan, where nobody is asking: %+v", result)
	}
}

func TestAMixedUnitNamesTheWorkloadThatIsVendored(t *testing.T) {
	input := vendoredInput(VendoredArtifact{
		Workload: "cache", Reference: "docker.io/library/redis:7.4", Signature: "none",
	})
	input.Parameters = map[string]string{"require-independent-review": "true"}
	message := messageFor(evaluate(t, input), "require-independent-review")
	if !strings.Contains(message, "cache") {
		t.Errorf("a half-vendored unit was refused with %q, which does not say which half", message)
	}
}

func TestABuiltReleaseCarriesNoVendoredFieldAtAll(t *testing.T) {
	// The canonical encoding is the reproduction contract every stored
	// decision cites. A field added for vendored software must not appear in
	// the encoding of an input that has none, or every decision recorded
	// before this existed would replay to a different digest.
	canonical, err := minimalInput(KindPromotion).Canonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, added := range []string{"vendored", "requestedBy"} {
		if strings.Contains(string(canonical), added) {
			t.Errorf("a built release's canonical input mentions %q: %s", added, canonical)
		}
	}
}

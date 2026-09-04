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

	"github.com/Bermos/Kitchen/internal/attestation"
)

// The newest scan is the artifact's current claim about its findings; the
// older ones are history the registry keeps. What that has to buy, and what
// it must not cost:
//
//   - a finding the newest scan no longer reports stops counting;
//   - the order two scans are collapsed in is total, so one artifact's
//     evidence has one input digest however the registry listed it;
//   - and an artifact carrying exactly one scan materializes byte for byte as
//     it did before any of this existed, or every decision stored until now
//     stops replaying.

func scanEvidence(digest, scannedAt, findings string) attestation.Evidence {
	predicate := `{"scannedAt":"` + scannedAt + `","findings":` + findings + `}`
	if scannedAt == "" {
		predicate = `{"findings":` + findings + `}`
	}
	return attestation.Evidence{
		PredicateType: attestation.PredicateVulnerabilityScan,
		Verified:      true,
		Digest:        digest,
		Statement:     attestation.Statement{Predicate: json.RawMessage(predicate)},
	}
}

func criticalFinding(identifier string) string {
	return `[{"vulnerability":"` + identifier + `","severity":"critical"}]`
}

func TestOnlyTheNewestScanReachesThePolicy(t *testing.T) {
	// Day one found a critical; day forty's scan of the same artifact does
	// not report it — withdrawn, re-rated, or matched by a database that has
	// since been fixed. Feeding both to the rules would leave max-severity
	// firing on a finding no scanner reports any more, and the only way out
	// would be a VEX statement about something that does not exist.
	set := attestation.EvidenceSet{Attestations: []attestation.Evidence{
		scanEvidence("sha256:"+strings.Repeat("1", 64), "2026-07-01T09:00:00Z", criticalFinding("CVE-2026-1")),
		scanEvidence("sha256:"+strings.Repeat("2", 64), "2026-08-09T09:00:00Z", `[]`),
	}}

	evidence := EvidenceFrom("", set, nil)
	if len(evidence) != 1 {
		t.Fatalf("two scans of one artifact are one claim, got %d entries", len(evidence))
	}

	input := minimalInput(KindRescan)
	input.Parameters = map[string]string{"maxSeverity": "high"}
	input.Evidence = evidence
	if result := evaluate(t, input); result.Verdict != VerdictAllowed {
		t.Fatalf("a finding the newest scan dropped still fired: %+v", result.Fired)
	}

	// And the reverse, so the test is not passing because nothing is read: the
	// finding the *newest* scan reports fires.
	set.Attestations[1] = scanEvidence(
		"sha256:"+strings.Repeat("2", 64), "2026-08-09T09:00:00Z", criticalFinding("CVE-2026-9"))
	input.Evidence = EvidenceFrom("", set, nil)
	result := evaluate(t, input)
	if len(result.Fired) != 1 || !strings.Contains(result.Fired[0].Message, "CVE-2026-9") {
		t.Fatalf("the newest scan's own finding did not fire: %+v", result.Fired)
	}
}

func TestNothingButTheVulnerabilityScanIsCollapsed(t *testing.T) {
	// A gate result, a provenance and an SBOM are each a distinct claim made
	// once, so two of them are two facts rather than a restatement. Only the
	// scan is a restatement, and only the scan is collapsed.
	gate := func(name string) attestation.Evidence {
		return attestation.Evidence{
			PredicateType: attestation.PredicateQualityGate,
			Verified:      true,
			Digest:        "sha256:" + strings.Repeat(name, 64),
			Statement:     attestation.Statement{Predicate: json.RawMessage(`{"gate":"` + name + `"}`)},
		}
	}
	set := attestation.EvidenceSet{Attestations: []attestation.Evidence{gate("a"), gate("b")}}
	if evidence := EvidenceFrom("", set, nil); len(evidence) != 2 {
		t.Fatalf("two gate results are two claims, got %d entries", len(evidence))
	}
}

func TestOneScanMaterializesExactlyAsItAlwaysDid(t *testing.T) {
	// The input digest is the reproduction contract every stored decision
	// cites. An artifact carrying one scan has to materialize to the same
	// bytes it did before the collapse existed — which is to say, to the
	// straight mapping of the evidence set in the registry's own order.
	set := attestation.EvidenceSet{Attestations: []attestation.Evidence{
		{
			PredicateType: attestation.PredicateSPDX,
			Verified:      true,
			Digest:        "sha256:" + strings.Repeat("a", 64),
			Statement:     attestation.Statement{Predicate: json.RawMessage(`{"spdxVersion":"SPDX-2.3"}`)},
		},
		scanEvidence("sha256:"+strings.Repeat("b", 64), "2026-08-09T09:00:00Z", criticalFinding("CVE-2026-1")),
		{
			PredicateType: attestation.PredicateBuildRecord,
			Verified:      true,
			Digest:        "sha256:" + strings.Repeat("c", 64),
			Statement:     attestation.Statement{Predicate: json.RawMessage(`{"project":"shop"}`)},
		},
	}}

	// The straight mapping is what EvidenceFrom was before this change.
	unchanged := make([]Evidence, 0, len(set.Attestations))
	for _, entry := range set.Attestations {
		unchanged = append(unchanged, Evidence{
			PredicateType: entry.PredicateType,
			Verified:      entry.Verified,
			Predicate:     entry.Statement.Predicate,
		})
	}

	before := minimalInput(KindPromotion)
	before.Evidence = unchanged
	after := minimalInput(KindPromotion)
	after.Evidence = EvidenceFrom("", set, nil)

	wanted, err := before.Digest()
	if err != nil {
		t.Fatal(err)
	}
	got, err := after.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if got != wanted {
		t.Fatalf("a single-scan artifact's input digest moved: %s, was %s", got, wanted)
	}
}

func TestTheNewestScanIsPickedTheSameWayHoweverTheRegistryListsThem(t *testing.T) {
	first := scanEvidence("sha256:"+strings.Repeat("1", 64), "2026-08-09T09:00:00Z", `[]`)
	second := scanEvidence("sha256:"+strings.Repeat("2", 64), "2026-07-01T09:00:00Z", `[]`)

	forwards, found := NewestVulnerabilityScan([]attestation.Evidence{first, second})
	if !found || forwards != 0 {
		t.Fatalf("the newer scan is the one dated later, got index %d (found %v)", forwards, found)
	}
	backwards, found := NewestVulnerabilityScan([]attestation.Evidence{second, first})
	if !found || backwards != 1 {
		t.Fatalf("listing order changed which scan is current, got index %d (found %v)", backwards, found)
	}

	// Two scans stamped the same second: the envelope digest decides, so the
	// answer cannot depend on which the registry happened to hand back first.
	tie := scanEvidence("sha256:"+strings.Repeat("3", 64), "2026-08-09T09:00:00Z", `[]`)
	if index, _ := NewestVulnerabilityScan([]attestation.Evidence{first, tie}); index != 1 {
		t.Errorf("a tie is not broken on the envelope digest, got index %d", index)
	}
	if index, _ := NewestVulnerabilityScan([]attestation.Evidence{tie, first}); index != 0 {
		t.Errorf("the tie-break is not stable under listing order, got index %d", index)
	}
}

func TestAScanThatWillNotSayWhenItRanLosesToOneThatDoes(t *testing.T) {
	// Kitchen's own scans always stamp `scannedAt`, so this is a scan
	// something else attested under Kitchen's predicate type. It is treated
	// as infinitely old rather than as current: an undated scan that won
	// would shadow every dated rescan for as long as it stayed attached.
	undated := scanEvidence("sha256:"+strings.Repeat("f", 64), "", criticalFinding("CVE-2026-1"))
	dated := scanEvidence("sha256:"+strings.Repeat("0", 64), "2026-08-09T09:00:00Z", `[]`)

	if index, _ := NewestVulnerabilityScan([]attestation.Evidence{undated, dated}); index != 1 {
		t.Errorf("an undated scan beat a dated one, got index %d", index)
	}
	// Among nothing but undated scans the digest tie-break still answers, so
	// there is always exactly one current claim.
	other := scanEvidence("sha256:"+strings.Repeat("e", 64), "", `[]`)
	if index, found := NewestVulnerabilityScan([]attestation.Evidence{other, undated}); !found || index != 1 {
		t.Errorf("undated scans left no current claim at all, got index %d (found %v)", index, found)
	}
}

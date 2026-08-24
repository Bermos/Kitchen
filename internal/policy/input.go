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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/Bermos/Kitchen/internal/attestation"
)

// The evaluation kinds: why the engine was asked. One input shape serves all
// of them, and the kind is part of the input so a stored decision says which
// question it answered.
const (
	// KindPromotion is the decision that moves an artifact: may this release
	// land on this environment now.
	KindPromotion = "promotion"
	// KindRescan is the same question asked again on a schedule about what is
	// already running, so drift has a paper trail.
	KindRescan = "rescan"
	// KindReplay is a stored decision re-evaluated from its stored inputs, to
	// confirm the original verdict reproduces.
	KindReplay = "replay"
	// KindEligibility is the read-only preview a screen asks for. It is never
	// stored — it decides nothing.
	KindEligibility = "eligibility"
)

// Input is everything a policy may look at, fully materialized before
// evaluation. There is deliberately no way to reach past it: the engine
// removes every network builtin, so what is not in here does not exist as far
// as a rule is concerned.
//
// Canonical form: the canonical encoding of an Input IS this struct's JSON
// encoding — field order is fixed by the struct, map keys are sorted by
// encoding/json, and evidence predicates are embedded verbatim as the bytes
// they were signed over. Do not reorder these fields: the order is part of
// the digest every stored decision carries.
type Input struct {
	// Kind is why the engine was asked: one of the Kind constants above.
	Kind string `json:"kind"`
	// At is when the question was asked. Exception expiry is judged against
	// it — against the evaluation's own clock, not the reader's — which is
	// what makes a replayed decision waive exactly what the original waived.
	At time.Time `json:"at"`
	// Parameters are the environment's tuning of the bundle's rules, reaching
	// the rules as input.parameters.
	Parameters map[string]string `json:"parameters,omitempty"`

	Project     ProjectFacts     `json:"project"`
	Environment EnvironmentFacts `json:"environment"`
	Release     ReleaseFacts     `json:"release"`

	// Evidence is what the artifact carries: the attested statements read
	// back from the registry, with their verification state.
	Evidence []Evidence `json:"evidence,omitempty"`
	// Claims are the environment's provisioned resources, with the data
	// facts issues #137/#138 record about them: class, provenance, residency.
	Claims []Claim `json:"claims,omitempty"`
	// Exceptions are the active break-glass grants in scope for this pair
	// (#136). They do not stop rules firing — they waive fired rules, and the
	// waiving is visible on the result.
	Exceptions []Exception `json:"exceptions,omitempty"`
	// VEX is the artifact's ingested OpenVEX statements (#135), which is what
	// lets max-severity ignore a finding the vendor says does not apply.
	VEX []VEXStatement `json:"vex,omitempty"`
	// DataSnapshot identifies the dataset the evidence was produced against —
	// a scanner's vulnerability database identifier — so a decision can say
	// what the world knew when it was made.
	DataSnapshot string `json:"dataSnapshot,omitempty"`
}

// ProjectFacts is what the engine knows about the project.
type ProjectFacts struct {
	Name string `json:"name"`
	// DataClass is the project's declared classification (#137) and
	// Criticality its declared designation (#141). Rules treat absence as
	// unclassified and undesignated respectively, never as a default.
	DataClass   string `json:"dataClass,omitempty"`
	Criticality string `json:"criticality,omitempty"`
	// RTO and RPO are the project's declared disruption tolerances, verbatim
	// ("4h", "30m"). They are here so a bundle can require of a critical
	// project what it does not require of the rest — evidence, a reviewer,
	// an environment that declares an RTO at all — rather than every such
	// rule having to be a second copy of the designation.
	RTO string `json:"rto,omitempty"`
	RPO string `json:"rpo,omitempty"`
}

// EnvironmentFacts is what the engine knows about the target environment.
type EnvironmentFacts struct {
	Name string `json:"name"`
	// Type is production or preview, which is what data-provenance-preview
	// pivots on.
	Type      string `json:"type,omitempty"`
	DataClass string `json:"dataClass,omitempty"`
	Residency string `json:"residency,omitempty"`
	// Criticality, RTO and RPO are the designation that *applies here*, which
	// for a production environment declaring none is its project's — resolved
	// by v1alpha1.EffectiveContinuity, the one implementation, so a rule and
	// a screen never disagree about whether a preview is critical. It is not.
	Criticality string `json:"criticality,omitempty"`
	RTO         string `json:"rto,omitempty"`
	RPO         string `json:"rpo,omitempty"`
}

// ReleaseFacts is what the engine knows about the artifact being judged.
type ReleaseFacts struct {
	Name string `json:"name"`
	// Image is the full deployable reference (repository@digest) and Digest
	// the artifact digest alone — the subject every piece of evidence names.
	Image  string     `json:"image,omitempty"`
	Digest string     `json:"digest,omitempty"`
	Build  BuildFacts `json:"build,omitempty"`
	// Source is a summary of how the change was reviewed, as recorded before
	// the build. The signed pull-request-approval attestation in Evidence is
	// the claim rules judge; this is the index of it.
	Source map[string]any `json:"source,omitempty"`
}

// BuildFacts identifies the build that produced the artifact.
type BuildFacts struct {
	Name     string `json:"name,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Strategy string `json:"strategy,omitempty"`
}

// Evidence is one attested statement, materialized: the predicate itself is
// embedded, verbatim, as the bytes the platform's key verified — never
// re-encoded, so the input digest is stable across reads.
type Evidence struct {
	PredicateType string `json:"predicateType"`
	// Source is platform or builder for evidence the platform indexed, empty
	// for evidence found attached by something else.
	Source   string `json:"source,omitempty"`
	Verified bool   `json:"verified"`
	// Predicate is the statement's predicate, verbatim.
	Predicate json.RawMessage `json:"predicate,omitempty"`
}

// Claim is one provisioned resource as the data rules see it.
type Claim struct {
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`
	DataClass string `json:"dataClass,omitempty"`
	// Provenance is what the provisioned data derives from: production,
	// masked or synthetic; empty means the provider declared nothing.
	Provenance string `json:"provenance,omitempty"`
	Residency  string `json:"residency,omitempty"`
}

// Exception is one break-glass grant, as ApplyExceptions consumes it.
type Exception struct {
	Name    string   `json:"name"`
	RuleIDs []string `json:"ruleIDs"`
	// ExpiresAt bounds the grant; an exception is consulted only while
	// Input.At is before it.
	ExpiresAt  time.Time `json:"expiresAt"`
	ApprovedBy string    `json:"approvedBy,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

// VEXStatement is one OpenVEX statement, flattened to what the rules match
// on: which vulnerability, which status, and whose word it is.
type VEXStatement struct {
	Vulnerability string   `json:"vulnerability"`
	Products      []string `json:"products,omitempty"`
	Status        string   `json:"status"`
	Justification string   `json:"justification,omitempty"`
	Author        string   `json:"author,omitempty"`
	Timestamp     string   `json:"timestamp,omitempty"`
	Verified      bool     `json:"verified,omitempty"`
}

// Canonical is the input's canonical JSON encoding — see the note on Input
// for what makes it canonical.
func (in Input) Canonical() ([]byte, error) {
	return json.Marshal(in)
}

// Digest content-addresses the input: sha256 over the canonical encoding,
// rendered `sha256:<hex>`. It is what a stored decision cites, and what
// replay compares against.
func (in Input) Digest() (string, error) {
	canonical, err := in.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// EvidenceFrom materializes an artifact's evidence set into the input's
// shape: predicate bytes verbatim, verification state carried through, and
// the source looked up from the build's own index by predicate type — the
// registry knows what is attached, the index knows whose claim it was.
//
// One predicate type is collapsed to its newest entry, and only one: the
// vulnerability scan. A scan is a **restatement of the same claim about the
// same artifact** — "these are the findings against today's database" — and
// the continuous re-evaluation pass (§9) attaches a fresh one every interval,
// so an artifact that has been up for a year carries three hundred and
// sixty-five of them. A quality gate, a provenance, an SBOM and a VEX
// document are each a distinct claim made once, so nothing is collapsed for
// them and nothing should be: dropping the older of two gate results would
// be dropping a fact, where dropping the older of two scans is dropping a
// superseded reading.
//
// Feeding all of them to the rules is not merely wasteful; it is wrong. A CVE
// reported on day one and absent from day forty's scan — withdrawn, re-rated,
// or matched by a database that has since been fixed — is still in day one's
// predicate, so max-severity fires on a finding no scanner reports any more
// and the only escape is a VEX statement about something that does not exist.
// The evidence index already names the newest scan alone (§9.9); this is the
// same rule applied where the decision is actually made.
//
// The registry keeps every scan regardless — history is retained, which is an
// acceptance criterion of #134. What is collapsed is the evaluation's view of
// it, not the evidence.
func EvidenceFrom(set attestation.EvidenceSet, sourceByPredicateType map[string]string) []Evidence {
	newest, scanned := NewestVulnerabilityScan(set.Attestations)
	out := make([]Evidence, 0, len(set.Attestations))
	for index, entry := range set.Attestations {
		if scanned && index != newest && entry.PredicateType == attestation.PredicateVulnerabilityScan {
			continue
		}
		out = append(out, Evidence{
			PredicateType: entry.PredicateType,
			Source:        sourceByPredicateType[entry.PredicateType],
			Verified:      entry.Verified,
			Predicate:     entry.Statement.Predicate,
		})
	}
	return out
}

// NewestVulnerabilityScan picks the vulnerability-scan attestation that is the
// artifact's current claim about its findings, answering its index in the set
// and whether there was one at all. It is exported because the evaluation and
// the read surface that shows a person the same findings
// (GET /builds/{name}/vex) have to agree about which scan is *the* scan —
// two implementations of "the newest" would eventually disagree, and the
// screen would then explain a decision that was made about something else.
//
// **The order is total and deterministic, deliberately.** The input digest is
// the reproduction contract every stored decision cites, so identical evidence
// has to produce an identical input however the registry happened to list it:
//
//   - newest `scannedAt` wins;
//   - a tie is broken on the envelope's own digest, which is total because two
//     envelopes with the same digest are the same bytes and the store attaches
//     those once (Attach is idempotent by content).
//
// A predicate carrying no readable `scannedAt` is treated as **infinitely
// old**: it loses to anything dated, and among nothing but undated entries the
// digest tie-break still decides. That is a deliberate choice in the one
// direction that cannot go badly wrong — an undated scan that won would shadow
// every dated rescan for as long as it stayed attached, which is a stale
// finding set with no way out. Kitchen's own scans always carry the field
// (see RescanSweeper.attest), so this covers a scan attested by something
// else under Kitchen's predicate type, and such a scan is dropped from the
// evaluation rather than merged into it.
func NewestVulnerabilityScan(attestations []attestation.Evidence) (int, bool) {
	newest := -1
	var newestAt time.Time
	for index, entry := range attestations {
		if entry.PredicateType != attestation.PredicateVulnerabilityScan {
			continue
		}
		at := scannedAt(entry.Statement.Predicate)
		if newest < 0 || laterScan(at, entry.Digest, newestAt, attestations[newest].Digest) {
			newest, newestAt = index, at
		}
	}
	return newest, newest >= 0
}

// laterScan is the one comparison NewestVulnerabilityScan orders by, lifted
// out so the tie-break is visible rather than buried in a condition.
func laterScan(at time.Time, digest string, against time.Time, againstDigest string) bool {
	if !at.Equal(against) {
		return at.After(against)
	}
	return digest > againstDigest
}

// scannedAt reads when a vulnerability-scan predicate says it ran. An absent,
// unparseable or non-RFC3339 stamp answers the zero time, which the ordering
// above reads as infinitely old.
func scannedAt(predicate json.RawMessage) time.Time {
	if len(predicate) == 0 {
		return time.Time{}
	}
	carried := struct {
		ScannedAt string `json:"scannedAt"`
	}{}
	if err := json.Unmarshal(predicate, &carried); err != nil {
		return time.Time{}
	}
	stamp, err := time.Parse(time.RFC3339, strings.TrimSpace(carried.ScannedAt))
	if err != nil {
		return time.Time{}
	}
	return stamp.UTC()
}

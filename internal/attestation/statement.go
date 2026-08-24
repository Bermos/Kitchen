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

// Package attestation is the platform's evidence layer: what Kitchen asserts
// about an artifact it built, how those assertions are signed, and where they
// are kept.
//
// Nothing in it is Kitchen-shaped on the wire. An assertion is an in-toto
// Statement, wrapped in a DSSE envelope, attached to the artifact's content
// digest through OCI referrers — three standards, in that order, so that the
// evidence can be read by anything that speaks them and by nothing that has to
// speak Kitchen. That is the honest engineering answer and it is also the exit
// story: an installation that stops using Kitchen keeps its evidence, because
// the evidence never lived in Kitchen.
//
// # The one rule that makes the scheme work
//
// The artifact is built once and never rebuilt. Every assertion here names a
// content digest, so a rebuild is a different artifact and every statement
// about the old one is a statement about something that no longer exists. The
// build pipeline enforces the rule; this package only assumes it.
package attestation

import (
	"encoding/json"
	"fmt"
	"strings"
)

// StatementType is the in-toto Statement version every payload here declares.
const StatementType = "https://in-toto.io/Statement/v1"

// Kitchen's own predicate types.
//
// They are URIs under the platform's domain because that is what a predicate
// type is for: it says whose vocabulary the predicate is written in, so a
// verifier that does not know the vocabulary knows that it does not. Anything
// with a standard vocabulary uses the standard URI instead — provenance is
// SLSA's, an SBOM is SPDX's or CycloneDX's — and this list is deliberately
// short for that reason. A Kitchen predicate type is an admission that no
// standard covers the claim.
const (
	// PredicateDeployment records that a Release of this artifact went live
	// on a named Environment: which environment, which release, when, and on
	// whose say-so. Nothing standard describes a deployment of an artifact
	// to an environment owned by the platform that built it.
	PredicateDeployment = "https://kitchen.bermos.dev/attestation/deployment/v1"

	// PredicateBuildRecord is what Kitchen knows about how the artifact was
	// produced from its own side of the build: the project, the commit, the
	// strategy and the builder image. It is not SLSA provenance and does not
	// pretend to be — provenance is produced by the builder rather than by
	// the reconciler, carries SLSA's own predicate type, and is harvested and
	// countersigned rather than written here. See harvest.go.
	PredicateBuildRecord = "https://kitchen.bermos.dev/attestation/build-record/v1"

	// PredicateQualityGate records that a gate ran over an artifact and what
	// it found: the gate's name and version, when it ran, and its raw output
	// unmodified.
	//
	// It records **no verdict**, and that is the point rather than an
	// omission. Gates produce facts; policies decide what is disqualifying.
	// Keeping them apart is what lets the same scan be acceptable in staging
	// and blocking in production without running the scanner twice, and it is
	// what keeps developers out of threshold negotiation entirely.
	PredicateQualityGate = "https://kitchen.bermos.dev/attestation/quality-gate/v1"

	// PredicateVulnerabilityScan records that an artifact's bill of materials
	// was matched against a vulnerability database *after the build*, and
	// what came out: the scanner and its version, when it ran, the database
	// snapshot it matched against, and the findings.
	//
	// It is the quality gate's predicate asked again, and the difference is
	// the snapshot. A gate's findings are a statement about what was known on
	// the day of the build; these are a statement about what is known today
	// about an artifact nobody has touched since. Naming the snapshot is what
	// makes the second claim reproducible rather than merely repeatable — and
	// it is why this carries no verdict either. Whether a finding is
	// disqualifying is still the environment's question, decided by the
	// policy engine over this evidence.
	PredicateVulnerabilityScan = "https://kitchen.bermos.dev/attestation/vulnerability-scan/v1"

	// PredicatePromotionDecision records a policy decision about whether an
	// artifact was allowed to move, together with everything needed to
	// replay it. Reserved by the policy engine (issue #132); named here so
	// that the URI space has one owner.
	PredicatePromotionDecision = "https://kitchen.bermos.dev/attestation/promotion-decision/v1"

	// PredicateBreakGlass records that a break-glass exception was used to
	// move this artifact: which exception, which rules it waived, who asked
	// and who approved, and until when. The authoritative record is the
	// Exception object bound to the (release, environment) pair — this
	// travels with the artifact so the fact is visible wherever the image
	// goes, cosign included.
	PredicateBreakGlass = "https://kitchen.bermos.dev/attestation/break-glass/v1"

	// PredicateDataClass records a provider's declaration of what a
	// provisioned resource's data derives from — production, masked or
	// synthetic — at the moment the claim (or one of its preview branches)
	// was bound. Its subject is a claim identity digest rather than an OCI
	// digest: a database claim has no image repository, so the statement's
	// subject is sha256 over the claim's namespace, name and UID — a stable
	// identity the record can be matched back to. The envelope is kept in
	// the store's signed_records table rather than in any registry, for the
	// same reason.
	PredicateDataClass = "https://kitchen.bermos.dev/attestation/data-class/v1"
)

// PredicateOpenVEX is an OpenVEX document: somebody's assertion that a
// vulnerability found in this artifact is or is not exploitable in it.
//
// It is **not** under kitchen.bermos.dev, and that is the rule above being
// followed rather than an exception to it: OpenVEX is a standard vocabulary
// with a specification, a URI of its own and tooling that already reads it, so
// minting a Kitchen predicate type for the same claim would produce evidence
// only Kitchen could interpret — the opposite of what §5.1 of
// docs/COMPLIANCE.md keeps this whole layer standard for. The URI is the
// document's own `@context`, which is how OpenVEX versions itself; older
// documents carry an older one and are recognised by prefix (see
// internal/vex.IsOpenVEX).
//
// The predicate is the submitted document, byte for byte. What the platform
// adds is attribution — who submitted it, recorded on the Build and in the
// audit log — and a signature meaning only that these bytes were submitted by
// that identity at that moment and have not changed since. It is not a claim
// that the assertion is true. Nothing can sign that.
const PredicateOpenVEX = "https://openvex.dev/ns/v0.2.0"

// predicateTypePrefix is the namespace Kitchen's own predicate types live
// under. It is exported through KitchenPredicate so that a reader can ask
// whether a predicate is Kitchen's without matching on the constants.
const predicateTypePrefix = "https://kitchen.bermos.dev/attestation/"

// KitchenPredicate reports whether a predicate type is one of Kitchen's own,
// as opposed to a standard one the platform merely carries.
func KitchenPredicate(predicateType string) bool {
	return strings.HasPrefix(predicateType, predicateTypePrefix)
}

// ResourceDescriptor identifies a thing a statement is about, in in-toto's
// terms. Kitchen only ever describes container images, so Digest always
// carries a `sha256` entry and Name is the repository the image was pushed to.
type ResourceDescriptor struct {
	Name string `json:"name,omitempty"`
	// Digest maps a hash algorithm to the hex digest, without the algorithm
	// prefix — `{"sha256": "abc…"}`, not `{"sha256": "sha256:abc…"}`. It is
	// the one field a verifier matches on, and getting the prefix wrong
	// produces an attestation that verifies and describes nothing.
	Digest    map[string]string `json:"digest,omitempty"`
	MediaType string            `json:"mediaType,omitempty"`
}

// Statement is an in-toto Statement: some subjects, and one claim about them.
//
// Predicate is held as raw JSON rather than as `any` so that a statement read
// back from a registry keeps the exact bytes it was signed over. Re-marshalling
// a decoded map is not guaranteed to reproduce them, and a payload that does
// not reproduce byte for byte does not verify.
type Statement struct {
	Type          string               `json:"_type"`
	Subject       []ResourceDescriptor `json:"subject"`
	PredicateType string               `json:"predicateType"`
	Predicate     json.RawMessage      `json:"predicate"`
}

// NewStatement builds a statement about one image digest.
//
// `digest` is the full reference form (`sha256:abc…`); it is split here so
// that callers pass the string they already have rather than remembering
// in-toto's split.
func NewStatement(repository, digest, predicateType string, predicate any) (Statement, error) {
	algorithm, hex, found := strings.Cut(digest, ":")
	if !found || algorithm == "" || hex == "" {
		return Statement{}, fmt.Errorf(
			"an attestation subject must be a digest of the form <algorithm>:<hex> (got %q)", digest)
	}
	if predicateType == "" {
		return Statement{}, fmt.Errorf("an attestation must declare a predicate type")
	}
	encoded, err := json.Marshal(predicate)
	if err != nil {
		return Statement{}, fmt.Errorf("the attestation predicate could not be encoded: %w", err)
	}
	return Statement{
		Type: StatementType,
		Subject: []ResourceDescriptor{{
			Name:   repository,
			Digest: map[string]string{algorithm: hex},
		}},
		PredicateType: predicateType,
		Predicate:     encoded,
	}, nil
}

// Describes reports whether the statement is about the given digest. A
// verifier that skips this check has verified a signature over somebody else's
// artifact.
func (s Statement) Describes(digest string) bool {
	algorithm, hex, found := strings.Cut(digest, ":")
	if !found {
		return false
	}
	for _, subject := range s.Subject {
		if strings.EqualFold(subject.Digest[algorithm], hex) {
			return true
		}
	}
	return false
}

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

	// PredicatePromotionDecision records a policy decision about whether an
	// artifact was allowed to move, together with everything needed to
	// replay it. Reserved by the policy engine (issue #132); named here so
	// that the URI space has one owner.
	PredicatePromotionDecision = "https://kitchen.bermos.dev/attestation/promotion-decision/v1"
)

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

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

package attestation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// What the builder said, as opposed to what the reconciler that orchestrated
// it said.
//
// The distinction is the whole point of this file. Kitchen's build record
// (PredicateBuildRecord) is the reconciler's account of a build it asked for;
// provenance is the account of the process that actually ran it, and only the
// builder can give it. BuildKit produces both provenance and an SBOM natively,
// so the platform's job is not to invent them but to fetch them, check they
// are about the artifact it thinks they are, and put the platform's signature
// on them.
//
// # How BuildKit leaves them
//
// Asked for attestations, BuildKit no longer pushes a bare image manifest. It
// pushes an OCI *index* holding two things:
//
//   - the image manifest, with a real platform (`linux/amd64`);
//   - beside it an attestation manifest, platform `unknown/unknown`, annotated
//     `vnd.docker.reference.type=attestation-manifest` and
//     `vnd.docker.reference.digest=<the image manifest's digest>`, whose layers
//     are `application/vnd.in-toto+json` — one in-toto Statement each.
//
// Those statements are **unsigned**. The index is the only thing tying them to
// the artifact, and an index is not a signature: anything that can write to the
// repository can write a different one. That is why they are harvested and
// re-signed rather than left where they are and pointed at.
//
// # Which digest is the artifact
//
// The subject of every statement BuildKit writes is the **image manifest**
// digest, not the index digest — verified against BuildKit v0.23.2 rather than
// assumed. So that is the digest Kitchen calls the artifact, and it is also the
// digest Kitchen deployed before any of this existed, which is why turning
// attestations on does not renumber the artifacts an installation already has.
//
// Getting this wrong is not a cosmetic error. Evidence attached to the index
// while the statements inside it describe the image would verify perfectly and
// describe something the platform never claimed, which is exactly the failure
// Statement.Describes exists to prevent.
const (
	// referenceTypeAnnotation and referenceDigestAnnotation are how BuildKit
	// links an attestation manifest back to the image manifest it is about.
	// They are Docker's spelling, not OCI's, and predate the referrers API.
	referenceTypeAnnotation   = "vnd.docker.reference.type"
	referenceDigestAnnotation = "vnd.docker.reference.digest"

	// attestationManifestType is the value of the annotation above that
	// marks a manifest as holding attestations rather than an image.
	attestationManifestType = "attestation-manifest"

	// mediaTypeInToto is the layer media type one bare in-toto Statement is
	// stored under. Bare: no DSSE envelope, no signature.
	mediaTypeInToto = "application/vnd.in-toto+json"
)

// BuilderEvidence is what one push left behind: which digest the artifact
// actually is, and what the builder asserted about it.
type BuilderEvidence struct {
	// ImageDigest is the artifact, as `sha256:…`. For an index it is the
	// image manifest inside it; for a plain manifest it is the reference
	// that was passed in. Either way it is the digest every statement here
	// is about, and the digest the platform should call the artifact.
	ImageDigest string

	// Statements are the builder's, in the order the manifest listed them,
	// already checked to be about ImageDigest. They are unsigned: a caller
	// that stores one without signing it has stored an assertion nobody
	// made.
	Statements []Statement

	// Discarded counts statements that were found but are about some other
	// digest. It is not an error — a caller may reasonably carry on with the
	// rest — but it is never normal, and something should say so out loud.
	Discarded int
}

// Predicate types the platform carries but does not own. They are the
// standards' own URIs, which is the point: an SBOM is worth having because
// Grype and Trivy already know what `https://spdx.dev/Document` means.
//
// The provenance version is BuildKit's choice, not Kitchen's, and both spellings
// are listed because an installation that has not rebuilt since an upgrade has
// artifacts carrying the older one.
const (
	PredicateSLSAProvenanceV1  = "https://slsa.dev/provenance/v1"
	PredicateSLSAProvenanceV02 = "https://slsa.dev/provenance/v0.2"
	PredicateSPDX              = "https://spdx.dev/Document"
	PredicateCycloneDX         = "https://cyclonedx.org/bom"
)

// Provenance reports whether a predicate type is provenance in any version the
// platform knows about.
func Provenance(predicateType string) bool {
	return predicateType == PredicateSLSAProvenanceV1 || predicateType == PredicateSLSAProvenanceV02
}

// SBOM reports whether a predicate type is a bill of materials in a format the
// platform knows how to name. It matches on what the generator declared rather
// than on what was configured, because the generator is the one that knows.
func SBOM(predicateType string) bool {
	return predicateType == PredicateSPDX || predicateType == PredicateCycloneDX
}

// Harvest reads back what the builder attached to a push.
//
// `ref` is what the builder reported — an index digest when attestations were
// asked for, a plain manifest digest when they were not. Both are answered the
// same way, so a caller does not have to know which it has, and an installation
// with build attestations turned off takes the second path and gets an evidence
// set with no statements rather than an error.
func (s *Store) Harvest(ctx context.Context, ref string) (BuilderEvidence, error) {
	pushed, err := s.digestRef(ref)
	if err != nil {
		return BuilderEvidence{}, err
	}
	options := s.options(ctx)

	descriptor, err := remote.Get(pushed, options...)
	if err != nil {
		return BuilderEvidence{}, fmt.Errorf("reading what the build pushed at %s failed: %w", pushed, err)
	}
	if !descriptor.MediaType.IsIndex() {
		// A single manifest carries no attestations: this is what a build
		// with the feature off looks like, and it is not a fault.
		return BuilderEvidence{ImageDigest: pushed.DigestStr()}, nil
	}

	index, err := descriptor.ImageIndex()
	if err != nil {
		return BuilderEvidence{}, fmt.Errorf("reading the index at %s failed: %w", pushed, err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return BuilderEvidence{}, fmt.Errorf("reading the index at %s failed: %w", pushed, err)
	}

	imageDigest, err := imageManifestDigest(manifest)
	if err != nil {
		return BuilderEvidence{}, fmt.Errorf("%s: %w", pushed, err)
	}
	evidence := BuilderEvidence{ImageDigest: imageDigest, Statements: []Statement{}}

	for _, entry := range manifest.Manifests {
		if entry.Annotations[referenceTypeAnnotation] != attestationManifestType {
			continue
		}
		// An attestation manifest says which image it is about. One that
		// names a different image belongs to another platform's build in the
		// same index and is not this artifact's evidence.
		if about := entry.Annotations[referenceDigestAnnotation]; about != "" && about != imageDigest {
			continue
		}
		image, err := remote.Image(pushed.Context().Digest(entry.Digest.String()), options...)
		if err != nil {
			return BuilderEvidence{}, fmt.Errorf(
				"fetching the builder's attestations at %s failed: %w", entry.Digest, err)
		}
		statements, discarded, err := readStatements(image, imageDigest)
		if err != nil {
			return BuilderEvidence{}, err
		}
		evidence.Statements = append(evidence.Statements, statements...)
		evidence.Discarded += discarded
	}
	return evidence, nil
}

// imageManifestDigest picks the artifact out of an index: the one entry that is
// an actual image.
//
// Kitchen builds for one platform, so "the image" is unambiguous — but it is
// found by eliminating the attestation manifests rather than by matching a
// platform string, because `unknown/unknown` is a convention and the day it
// changes, matching on it would silently pick an attestation manifest as the
// artifact.
func imageManifestDigest(manifest *v1.IndexManifest) (string, error) {
	digest := ""
	for _, entry := range manifest.Manifests {
		if entry.Annotations[referenceTypeAnnotation] == attestationManifestType {
			continue
		}
		if digest != "" {
			return "", fmt.Errorf(
				"the build pushed an index holding more than one image, and the platform has no rule for choosing between them")
		}
		digest = entry.Digest.String()
	}
	if digest == "" {
		return "", fmt.Errorf("the build pushed an index holding no image")
	}
	return digest, nil
}

// readStatements pulls every in-toto Statement out of one attestation manifest,
// keeping the ones that are about the artifact.
//
// The subject check is the reason this is not three lines. A statement is a
// claim about a digest, and re-signing one without checking which digest would
// put the platform's signature on somebody else's claim — the precise mistake
// that makes an evidence system worse than no evidence system.
func readStatements(image v1.Image, imageDigest string) ([]Statement, int, error) {
	manifest, err := image.Manifest()
	if err != nil {
		return nil, 0, fmt.Errorf("reading an attestation manifest failed: %w", err)
	}
	statements, discarded := []Statement{}, 0

	for _, layer := range manifest.Layers {
		if string(layer.MediaType) != mediaTypeInToto {
			continue
		}
		blob, err := image.LayerByDigest(layer.Digest)
		if err != nil {
			return nil, 0, fmt.Errorf("reading the attestation %s failed: %w", layer.Digest, err)
		}
		reader, err := blob.Uncompressed()
		if err != nil {
			return nil, 0, fmt.Errorf("reading the attestation %s failed: %w", layer.Digest, err)
		}
		body, err := io.ReadAll(reader)
		closeErr := reader.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("reading the attestation %s failed: %w", layer.Digest, err)
		}
		if closeErr != nil {
			return nil, 0, fmt.Errorf("reading the attestation %s failed: %w", layer.Digest, closeErr)
		}

		statement := Statement{}
		if err := json.Unmarshal(body, &statement); err != nil {
			return nil, 0, fmt.Errorf("the attestation %s is not an in-toto statement: %w", layer.Digest, err)
		}
		if statement.PredicateType == "" {
			return nil, 0, fmt.Errorf("the attestation %s declares no predicate type", layer.Digest)
		}
		if !statement.Describes(imageDigest) {
			discarded++
			continue
		}
		statements = append(statements, statement)
	}
	return statements, discarded, nil
}

// Restate rewrites one of the builder's statements as the platform's own claim
// about the artifact, ready to be signed.
//
// Three things change and one thing must not. The subject is rewritten to the
// repository and digest Kitchen calls the artifact — BuildKit names its subject
// with a package URL carrying the tag it pushed to, which is a moving target and
// not how anything else here identifies an artifact. The statement version
// becomes in-toto v1, because BuildKit still emits v0.1. And the whole thing
// gains a signature it did not have.
//
// What must not change is the predicate: it is copied as the exact bytes that
// came out of the builder, never re-marshalled from a decoded map. Re-encoding
// is not guaranteed to reproduce the original ordering, and a predicate that
// does not reproduce byte for byte is a different claim from the one the builder
// made — which would make the platform's signature an assertion about something
// nobody said.
func Restate(repository, digest string, statement Statement) (Statement, error) {
	if !statement.Describes(digest) {
		return Statement{}, fmt.Errorf(
			"the builder's %s attestation is about a different artifact than %s, and restating it would sign a claim about somebody else's image",
			statement.PredicateType, digest)
	}
	return NewStatement(repository, digest, statement.PredicateType, statement.Predicate)
}

// ArtifactRef assembles the reference evidence is attached to, out of the
// repository and digest the Build status carries separately.
func ArtifactRef(repository, digest string) string {
	return repository + "@" + digest
}

// Resolve is the digest an image reference names right now.
//
// It is what turns a vendored image's tag into something a Release can freeze
// (#307). A tag is a moving target — the whole reason a Release records a
// digest rather than what the project declared — so it is asked once, at the
// moment the artifact is acquired, and never again for that release.
//
// A reference that already names a digest is answered without a request. That
// is not only an optimisation: a project that pinned a digest is entitled to
// deploy without the platform being able to reach the vendor's registry at
// all.
//
// The answer is always `repository@sha256:…`, in the repository as written —
// what came back is a digest for *this* repository, and rewriting the
// repository from a redirect would name an artifact nobody asked for.
func (s *Store) Resolve(ctx context.Context, ref string) (string, error) {
	options := []name.Option{}
	if s.PlainHTTP {
		options = append(options, name.Insecure)
	}
	reference, err := name.ParseReference(ref, options...)
	if err != nil {
		return "", fmt.Errorf("%q is not an image reference: %w", ref, err)
	}
	if digest, ok := reference.(name.Digest); ok {
		return ArtifactRef(digest.Context().Name(), digest.DigestStr()), nil
	}
	descriptor, err := remote.Head(reference, s.options(ctx)...)
	if err != nil {
		return "", fmt.Errorf("the digest of %s could not be read from its registry: %w", ref, err)
	}
	return ArtifactRef(reference.Context().Name(), descriptor.Digest.String()), nil
}

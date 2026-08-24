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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Where evidence lives, and under what names.
//
// An attestation is one blob — a DSSE envelope — in a manifest whose `subject`
// is the artifact it is about. That subject link is the OCI 1.1 referrers
// relationship: a registry that implements the referrers API answers "what
// refers to this digest" directly, and one that does not is served the same
// answer out of the fallback tag the spec defines. Both are handled below the
// call, which is the reason this package uses a registry library rather than
// speaking the API itself.
//
// The manifest is *also* pushed under the `sha256-<hex>.att` tag. That is
// cosign's attachment name, and carrying it costs one tag and makes
// `cosign download attestation` and `cosign verify-attestation --key` work
// against what Kitchen writes without Kitchen being involved. One manifest,
// two ways to find it — not two copies of the evidence.
const (
	// mediaTypeDSSE is the layer media type a DSSE envelope is stored under.
	mediaTypeDSSE = "application/vnd.dsse.envelope.v1+json"

	// artifactTypeAttestation is what the referrers listing reports this
	// manifest as. It reaches the listing through the manifest's config
	// media type, which is what OCI 1.1 says to fall back to when a manifest
	// declares no artifactType of its own.
	artifactTypeAttestation = "application/vnd.dev.cosign.artifact.attestation.v1+json"

	// predicateTypeAnnotation lets a reader tell what a layer asserts
	// without pulling and decoding it. It is a hint and nothing here trusts
	// it: the predicate type inside the signed payload is the authority, and
	// the annotation is outside the signature.
	predicateTypeAnnotation = "predicateType"

	// attachmentSuffix is cosign's for attestations.
	attachmentSuffix = ".att"
)

// Store reads and writes attestations in an OCI registry.
//
// It holds no state about what it has written: everything it knows it reads
// back out of the registry, which is what makes "evidence retrievable by
// digest alone, with no Kitchen database lookup" true rather than aspirational.
type Store struct {
	// Auth is the credential for the registry. Nil means anonymous, which
	// works for a public registry and for nothing Kitchen bundles — its own
	// registry admits nobody anonymously.
	Auth authn.Authenticator

	// Transport overrides the HTTP transport. Tests use it to reach an
	// in-process registry; the operator leaves it nil.
	Transport http.RoundTripper

	// PlainHTTP talks to the registry over HTTP. It exists for tests against
	// an in-process registry and must not be turned on for anything else:
	// evidence pushed over plain HTTP is evidence anything on the path can
	// replace.
	PlainHTTP bool
}

// Evidence is one attestation as it was found, together with what could be
// established about it.
type Evidence struct {
	// PredicateType is read from the signed payload, not from the layer
	// annotation.
	PredicateType string `json:"predicateType"`

	// Statement is the decoded payload. It is present whether or not the
	// signature was accepted — what an unverified attestation claims is
	// worth showing, next to the fact that it is unverified.
	Statement Statement `json:"statement"`

	// Envelope is the evidence as stored, so that a caller can hand the
	// exact bytes to something else to check.
	Envelope Envelope `json:"envelope"`

	// Verified is true when a signature was accepted by one of the keys the
	// caller supplied. False with no verifiers supplied means "not checked",
	// which is why EvidenceSet records whether verification was attempted.
	Verified bool `json:"verified"`

	// KeyIDs are the key ids the envelope's signatures name, in order.
	KeyIDs []string `json:"keyIDs,omitempty"`

	// Digest is the envelope blob's own digest, which is how two copies of
	// the same evidence found by two routes are recognised as one.
	Digest string `json:"digest"`
}

// EvidenceSet is everything attached to one artifact.
type EvidenceSet struct {
	// Subject is the artifact, as repository@digest.
	Subject string `json:"subject"`

	// Verified says whether signatures were checked at all. An evidence set
	// gathered with no keys is a listing, not a verification, and a reader
	// that cannot tell the two apart will eventually treat one as the other.
	Verified bool `json:"verified"`

	// Attestations are what was found, in the order the registry returned
	// them.
	Attestations []Evidence `json:"attestations"`
}

// Attach signs nothing and stores one already-signed envelope against the
// artifact's digest, returning the digest of the manifest that now refers to
// it.
//
// It is idempotent by content: an envelope whose bytes are already attached is
// recognised and not attached twice, so a reconcile that runs again does not
// grow the evidence set. Two envelopes that assert the same thing but were
// signed at different moments are different bytes and both are kept — which is
// correct, because they are two assertions.
func (s *Store) Attach(ctx context.Context, imageRef string, envelope Envelope, predicateType string) (string, error) {
	subject, err := s.digestRef(imageRef)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("the attestation envelope could not be encoded: %w", err)
	}

	options := s.options(ctx)
	attachment := attachmentTag(subject)

	base, err := s.attachmentImage(ctx, attachment)
	if err != nil {
		return "", err
	}

	layer := static.NewLayer(body, mediaTypeDSSE)
	layerDigest, err := layer.Digest()
	if err != nil {
		return "", err
	}
	already, err := hasLayer(base, layerDigest)
	if err != nil {
		return "", err
	}
	if already {
		digest, err := base.Digest()
		if err != nil {
			return "", err
		}
		return digest.String(), nil
	}

	image, err := mutate.Append(base, mutate.Addendum{
		Layer:       layer,
		Annotations: map[string]string{predicateTypeAnnotation: predicateType},
	})
	if err != nil {
		return "", fmt.Errorf("adding the attestation to the artifact's evidence failed: %w", err)
	}
	image = mutate.ConfigMediaType(mutate.MediaType(image, types.OCIManifestSchema1), artifactTypeAttestation)

	// The subject descriptor has to describe the manifest as the registry
	// holds it, so it is read from the registry rather than assembled here.
	// A subject whose size or media type is guessed produces a referrers
	// entry that points at nothing.
	descriptor, err := remote.Head(subject, options...)
	if err != nil {
		return "", fmt.Errorf("the artifact %s could not be found in the registry: %w", subject, err)
	}
	linked, ok := mutate.Subject(image, *descriptor).(v1.Image)
	if !ok {
		return "", errors.New("attaching the subject to the attestation manifest produced something that is not an image")
	}

	if err := remote.Write(attachment, linked, options...); err != nil {
		return "", fmt.Errorf("writing the attestation to %s failed: %w", attachment, err)
	}
	digest, err := linked.Digest()
	if err != nil {
		return "", err
	}
	return digest.String(), nil
}

// Evidence gathers every attestation attached to an artifact and, when keys
// are supplied, checks them.
//
// It looks in both places evidence can be: the referrers listing for the
// digest, and cosign's attachment tag. A registry that implements referrers
// answers the first and the second is the same manifest under another name; a
// registry that does not implement referrers answers the first out of the
// fallback tag. Reading both and merging on the envelope's digest is what
// makes the answer the same either way — and what lets Kitchen see evidence
// something other than Kitchen attached.
func (s *Store) Evidence(ctx context.Context, imageRef string, verifiers ...Verifier) (EvidenceSet, error) {
	subject, err := s.digestRef(imageRef)
	if err != nil {
		return EvidenceSet{}, err
	}
	options := s.options(ctx)

	set := EvidenceSet{
		Subject:      subject.String(),
		Verified:     len(verifiers) > 0,
		Attestations: []Evidence{},
	}
	seen := map[string]bool{}

	manifests, err := s.attestationManifests(ctx, subject, options)
	if err != nil {
		return EvidenceSet{}, err
	}
	for _, image := range manifests {
		found, err := readEnvelopes(image, seen, verifiers)
		if err != nil {
			return EvidenceSet{}, err
		}
		set.Attestations = append(set.Attestations, found...)
	}
	return set, nil
}

// attestationManifests returns every manifest that might hold evidence about
// the subject: whatever refers to it, plus the attachment tag.
func (s *Store) attestationManifests(
	ctx context.Context, subject name.Digest, options []remote.Option,
) ([]v1.Image, error) {
	images := []v1.Image{}

	index, err := remote.Referrers(subject, options...)
	if err != nil {
		return nil, fmt.Errorf("listing what refers to %s failed: %w", subject, err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, err
	}
	for _, descriptor := range manifest.Manifests {
		if descriptor.ArtifactType != "" && descriptor.ArtifactType != artifactTypeAttestation {
			// Something else refers to the artifact — a signature, an SBOM
			// attached by other tooling. Not this reader's business.
			continue
		}
		image, err := remote.Image(subject.Context().Digest(descriptor.Digest.String()), options...)
		if err != nil {
			return nil, fmt.Errorf("fetching the attestation %s failed: %w", descriptor.Digest, err)
		}
		images = append(images, image)
	}

	attachment, err := s.attachmentImage(ctx, attachmentTag(subject))
	if err != nil {
		return nil, err
	}
	// A registry with no attachment tag answers the empty image, whose
	// layers are none — merging it in costs nothing and saves a branch.
	return append(images, attachment), nil
}

// attachmentImage reads the manifest under an attachment tag, answering the
// empty image when there is none yet. A missing attachment is the ordinary
// case for a first attestation, not a fault.
func (s *Store) attachmentImage(ctx context.Context, tag name.Tag) (v1.Image, error) {
	image, err := remote.Image(tag, s.options(ctx)...)
	if err == nil {
		return image, nil
	}
	var transportErr *transport.Error
	if errors.As(err, &transportErr) && transportErr.StatusCode == http.StatusNotFound {
		return empty.Image, nil
	}
	return nil, fmt.Errorf("reading the evidence attached at %s failed: %w", tag, err)
}

// readEnvelopes pulls every DSSE layer out of one manifest.
func readEnvelopes(image v1.Image, seen map[string]bool, verifiers []Verifier) ([]Evidence, error) {
	manifest, err := image.Manifest()
	if err != nil {
		return nil, err
	}
	found := []Evidence{}
	for _, descriptor := range manifest.Layers {
		if descriptor.MediaType != mediaTypeDSSE {
			continue
		}
		digest := descriptor.Digest.String()
		if seen[digest] {
			continue
		}
		seen[digest] = true

		layer, err := image.LayerByDigest(descriptor.Digest)
		if err != nil {
			return nil, err
		}
		reader, err := layer.Compressed()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(reader)
		closeErr := reader.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}

		envelope := Envelope{}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, fmt.Errorf("the attestation blob %s is not a DSSE envelope: %w", digest, err)
		}
		evidence := Evidence{Envelope: envelope, Digest: digest}
		for _, signature := range envelope.Signatures {
			evidence.KeyIDs = append(evidence.KeyIDs, signature.KeyID)
		}

		// An envelope that does not decode is reported rather than dropped:
		// evidence that cannot be read is a finding, and a reader that
		// silently skipped it would show a clean evidence set.
		statement, err := envelope.Statement()
		if err != nil {
			evidence.PredicateType = descriptor.Annotations[predicateTypeAnnotation]
			found = append(found, evidence)
			continue
		}
		evidence.Statement = statement
		evidence.PredicateType = statement.PredicateType

		if len(verifiers) > 0 {
			if _, err := envelope.Verify(verifiers...); err == nil {
				evidence.Verified = true
			}
		}
		found = append(found, evidence)
	}
	return found, nil
}

// EnvelopeDigest is the digest an envelope is attached under, computed from
// the envelope alone — the same value Evidence.Digest carries when the same
// bytes are read back out of the registry.
//
// It exists because a writer needs a stable name for what it just attached and
// the manifest digest is not one: the attachment manifest accumulates every
// envelope attached to the artifact, so its digest moves every time anything
// else is attached. The envelope's own digest is content-addressed and does
// not. It is computed through the same layer constructor Attach pushes with,
// so the two cannot drift.
func EnvelopeDigest(envelope Envelope) (string, error) {
	body, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("the attestation envelope could not be encoded: %w", err)
	}
	digest, err := static.NewLayer(body, mediaTypeDSSE).Digest()
	if err != nil {
		return "", err
	}
	return digest.String(), nil
}

func hasLayer(image v1.Image, digest v1.Hash) (bool, error) {
	manifest, err := image.Manifest()
	if err != nil {
		return false, err
	}
	for _, descriptor := range manifest.Layers {
		if descriptor.Digest == digest {
			return true, nil
		}
	}
	return false, nil
}

// attachmentTag is cosign's name for what is attached to a digest:
// `sha256-<hex>.att`, in the artifact's own repository.
func attachmentTag(subject name.Digest) name.Tag {
	return subject.Context().Tag(strings.Replace(subject.DigestStr(), ":", "-", 1) + attachmentSuffix)
}

func (s *Store) digestRef(imageRef string) (name.Digest, error) {
	options := []name.Option{}
	if s.PlainHTTP {
		options = append(options, name.Insecure)
	}
	reference, err := name.NewDigest(imageRef, options...)
	if err != nil {
		return name.Digest{}, fmt.Errorf(
			"attestations are attached to a digest, and %q is not one — an image referenced by tag is a moving target: %w",
			imageRef, err)
	}
	return reference, nil
}

func (s *Store) options(ctx context.Context) []remote.Option {
	options := []remote.Option{remote.WithContext(ctx)}
	if s.Auth != nil {
		options = append(options, remote.WithAuth(s.Auth))
	}
	if s.Transport != nil {
		options = append(options, remote.WithTransport(s.Transport))
	}
	return options
}

// AuthFromDockerConfig reads the credential for one registry out of a
// docker config, which is the shape every credential in the platform already
// has: the build pods mount one, the Connection stores one.
//
// A config with no entry for the registry is not an error — it is an anonymous
// pull, which is a legitimate thing to attempt against a public registry.
func AuthFromDockerConfig(dockerConfig []byte, registry string) (authn.Authenticator, error) {
	config := struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}{}
	if err := json.Unmarshal(dockerConfig, &config); err != nil {
		return nil, fmt.Errorf("the registry credential is not a docker config: %w", err)
	}
	entry, ok := config.Auths[registry]
	if !ok {
		return authn.Anonymous, nil
	}
	if entry.Username != "" {
		return authn.FromConfig(authn.AuthConfig{Username: entry.Username, Password: entry.Password}), nil
	}
	return authn.FromConfig(authn.AuthConfig{Auth: entry.Auth}), nil
}

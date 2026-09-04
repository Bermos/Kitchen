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
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// What somebody else published about an artifact, and what can be established
// about it here (#309).
//
// The whole of this file rests on one property of the model: **evidence
// attaches to a digest, not to a pipeline**. harvest.go already reads
// statements out of a registry, checks they describe the digest the platform
// intends to call the artifact, and hands them back to be re-signed. None of
// that cares who did the building — so an image the platform only pulled is
// read exactly the same way, and the difference lives entirely in what the
// evidence is *labelled* as afterwards (ArtifactEvidence.Source).
//
// # The two shapes a vendor publishes in
//
// There is no single convention, so both are read and merged:
//
//   - **In the index, unsigned.** `docker buildx build --provenance --sbom`
//     pushes an OCI index holding the image beside an attestation manifest
//     whose layers are bare in-toto statements. That is what Harvest reads,
//     and it is the same shape Kitchen's own builds produce.
//   - **Beside the digest, in a DSSE envelope.** `cosign attest` attaches a
//     signed envelope through the referrers relationship and the `.att` tag.
//     That is what Evidence reads.
//
// Merged and deduplicated, they are "what the vendor said about this digest".
//
// # What is not trusted, and what is checked
//
// Nothing here verifies a vendor's DSSE signature — the platform holds no key
// to check it against, and pretending otherwise is the failure mode this
// package exists to avoid. What *is* checked is the only thing that can be
// checked without a key: that the statement names the digest being deployed.
// A statement about another artifact is discarded rather than restated,
// because re-signing it would put the platform's signature on somebody else's
// claim about somebody else's image.
//
// The upstream signature is a separate question with a separate answer, and it
// is answered honestly: see VerifyUpstream.

// Cosign's names for what it attaches to a digest. They are the de facto
// interchange format for container signatures and they are matched exactly,
// because a signature recognised by resemblance is not a signature.
const (
	// signatureSuffix is cosign's attachment tag suffix for signatures, the
	// sibling of attachmentSuffix.
	signatureSuffix = ".sig"

	// artifactTypeSignature is what the referrers listing reports a cosign
	// signature manifest as.
	artifactTypeSignature = "application/vnd.dev.cosign.artifact.sig.v1+json"

	// mediaTypeSimpleSigning is the layer media type the signed payload is
	// stored under. The payload is the "simple signing" JSON below.
	mediaTypeSimpleSigning = "application/vnd.dev.cosign.simplesigning.v1+json"

	// signatureAnnotation carries the base64 signature over the layer's own
	// bytes. It is outside the payload, which is why the payload is what is
	// verified and this is only where the signature is kept.
	signatureAnnotation = "dev.cosignproject.cosign/signature"

	// certificateAnnotation carries the PEM certificate of a keyless
	// signature, and chainAnnotation the intermediates. Both are read for
	// what they say about *who*, and neither is chained to a trust root —
	// see VerifyUpstream for why that bounds the answer to `unverifiable`.
	certificateAnnotation = "dev.sigstore.cosign/certificate"
)

// fulcioIssuerOID is Fulcio's X.509 extension holding the OIDC issuer that
// certified a keyless signer, in its current (v2, UTF8String) spelling. The
// deprecated raw-value spelling is 1.3.6.1.4.1.57264.1.1 and is read too:
// certificates issued before the change are still attached to images that are
// still deployed.
var (
	fulcioIssuerOID       = []int{1, 3, 6, 1, 4, 1, 57264, 1, 8}
	fulcioIssuerOIDLegacy = []int{1, 3, 6, 1, 4, 1, 57264, 1, 1}
)

// VendorEvidence is what somebody else published about one digest.
type VendorEvidence struct {
	// Statements are the vendor's, deduplicated, already checked to describe
	// the digest they were asked about. They are unsigned as far as this
	// platform is concerned — a caller that stores one without signing it
	// has stored an assertion nobody here made.
	Statements []Statement

	// Discarded counts statements found beside the digest that are about
	// some other artifact. Never normal, never fatal, always worth saying.
	Discarded int
}

// UpstreamSignature is one signature somebody attached to a digest.
type UpstreamSignature struct {
	// Payload is the signed bytes: cosign's simple-signing document. It is
	// kept verbatim, because the signature is over these bytes and a
	// re-encoding of the decoded form is a different message.
	Payload []byte

	// Signature is the raw signature over Payload.
	Signature []byte

	// Identities are who the signature says made it, read out of the
	// certificate where there is one: its subject alternative names and the
	// issuer Fulcio recorded. They are **claims**, not findings — anyone can
	// mint a self-signed certificate naming anyone — and they are used only
	// to refuse a signature that names the wrong signer, never to accept one.
	Identities []string

	// Issuer is the OIDC issuer named in the certificate, empty for a
	// key-signed signature.
	Issuer string

	// Certificate is whether a certificate was attached at all, which is how
	// a keyless signature is told from a key-signed one.
	Certificate bool
}

// simpleSigning is the payload cosign signs: a claim about one manifest
// digest. Only the field that ties it to an artifact is decoded — the rest is
// the vendor's business.
type simpleSigning struct {
	Critical struct {
		Image struct {
			Digest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
}

// Describes reports whether this signature is about the given digest. A
// signature over some other manifest, found in the same repository, is not
// this artifact's.
func (u UpstreamSignature) Describes(digest string) bool {
	payload := simpleSigning{}
	if err := json.Unmarshal(u.Payload, &payload); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(payload.Critical.Image.Digest), digest)
}

// NamesIdentity reports whether the signature's certificate names this
// identity, matched exactly and case-insensitively — never as a pattern,
// because an identity match that surprises whoever wrote it is the one kind
// this must not have.
func (u UpstreamSignature) NamesIdentity(identity string) bool {
	wanted := strings.ToLower(strings.TrimSpace(identity))
	for _, candidate := range u.Identities {
		if strings.ToLower(strings.TrimSpace(candidate)) == wanted {
			return true
		}
	}
	return false
}

// VendorStatements is everything somebody else asserted about a digest, from
// both places a vendor puts it.
//
// `platformKeyID` is this installation's own signing key. Statements in
// envelopes signed under it are the platform's own restatements from a
// previous pass and are skipped — harvesting them again would restate a
// restatement, and an evidence set would grow by one copy of itself every
// reconcile.
func (s *Store) VendorStatements(ctx context.Context, ref, platformKeyID string) (VendorEvidence, error) {
	subject, err := s.digestRef(ref)
	if err != nil {
		return VendorEvidence{}, err
	}
	digest := subject.DigestStr()

	evidence := VendorEvidence{Statements: []Statement{}}
	seen := map[string]bool{}

	// The index shape first, and **tolerantly**. A vendor's reference
	// usually resolves to an index, and an index is an ordinary vendored
	// artifact — a multi-platform one has no single image inside it, which
	// Harvest refuses rather than choosing between them. That refusal is a
	// fact about the artifact and not a failure to read it, so it is
	// skipped rather than returned.
	//
	// What it *does* catch is a vendor that pushed one platform with
	// `docker buildx --provenance`: its statements are about the image
	// manifest inside the index while the platform deploys the index, so
	// they describe something other than the artifact and are counted as
	// discarded. That count is the honest answer to "the vendor published
	// provenance and none of it is about what you are running", and it is
	// worth having rather than silently reading nothing.
	if inIndex, err := s.Harvest(ctx, ref); err == nil {
		evidence.Discarded += inIndex.Discarded
		for _, statement := range inIndex.Statements {
			appendStatement(&evidence, seen, statement, digest)
		}
	}

	// Then the envelope shape. Read with no verifiers: what the platform
	// could check here is nothing, and asking for a verification it cannot
	// perform would only produce a `verified: false` that reads as a finding.
	attached, err := s.Evidence(ctx, ArtifactRef(subject.Context().Name(), digest))
	if err != nil {
		return VendorEvidence{}, err
	}
	for _, found := range attached.Attestations {
		if platformKeyID != "" && containsKeyID(found.KeyIDs, platformKeyID) {
			continue
		}
		if found.PredicateType == "" || len(found.Statement.Predicate) == 0 {
			continue
		}
		if !found.Statement.Describes(digest) {
			evidence.Discarded++
			continue
		}
		appendStatement(&evidence, seen, found.Statement, digest)
	}
	return evidence, nil
}

// appendStatement adds one statement to the set, keeping the first of any two
// that assert the same thing.
//
// Two copies of one claim are one claim: a vendor that publishes provenance
// both in the index and as a cosign attestation has said it once, and
// restating it twice would put two identical attestations on the artifact and
// make an evidence set that grew with the number of places it was read from.
func appendStatement(evidence *VendorEvidence, seen map[string]bool, statement Statement, digest string) {
	if !statement.Describes(digest) {
		evidence.Discarded++
		return
	}
	key := statement.PredicateType + "\x00" + string(statement.Predicate)
	if seen[key] {
		return
	}
	seen[key] = true
	evidence.Statements = append(evidence.Statements, statement)
}

func containsKeyID(keyIDs []string, wanted string) bool {
	for _, keyID := range keyIDs {
		if keyID == wanted {
			return true
		}
	}
	return false
}

// Signatures reads every signature attached to a digest, from the referrers
// listing and from cosign's `.sig` tag.
//
// It reads them and judges nothing: a signature that verifies against no key
// anybody named is still a signature that exists, and "an unsigned image" and
// "an image signed by somebody unexpected" are two different facts that a
// reader must not be shown as one.
func (s *Store) Signatures(ctx context.Context, ref string) ([]UpstreamSignature, error) {
	subject, err := s.digestRef(ref)
	if err != nil {
		return nil, err
	}
	options := s.options(ctx)

	images, err := s.signatureManifests(ctx, subject, options)
	if err != nil {
		return nil, err
	}

	signatures := []UpstreamSignature{}
	seen := map[string]bool{}
	for _, image := range images {
		manifest, err := image.Manifest()
		if err != nil {
			return nil, err
		}
		for _, descriptor := range manifest.Layers {
			if string(descriptor.MediaType) != mediaTypeSimpleSigning {
				continue
			}
			if seen[descriptor.Digest.String()] {
				continue
			}
			seen[descriptor.Digest.String()] = true

			payload, err := layerBytes(image, descriptor.Digest)
			if err != nil {
				return nil, fmt.Errorf("reading the signature %s failed: %w", descriptor.Digest, err)
			}
			raw, err := base64.StdEncoding.DecodeString(
				strings.TrimSpace(descriptor.Annotations[signatureAnnotation]))
			if err != nil {
				// A layer whose signature annotation is not base64 is not a
				// signature. Skipped rather than failed: the rest of the
				// manifest may hold real ones.
				continue
			}
			signature := UpstreamSignature{Payload: payload, Signature: raw}
			readCertificate(&signature, descriptor.Annotations[certificateAnnotation])
			signatures = append(signatures, signature)
		}
	}
	return signatures, nil
}

// readCertificate fills in who a keyless signature claims to be.
func readCertificate(signature *UpstreamSignature, certificatePEM string) {
	block, _ := pem.Decode([]byte(certificatePEM))
	if block == nil {
		return
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return
	}
	signature.Certificate = true
	signature.Identities = append(signature.Identities, certificate.EmailAddresses...)
	for _, uri := range certificate.URIs {
		signature.Identities = append(signature.Identities, uri.String())
	}
	signature.Identities = append(signature.Identities, certificate.DNSNames...)
	if subject := strings.TrimSpace(certificate.Subject.CommonName); subject != "" {
		signature.Identities = append(signature.Identities, subject)
	}
	for _, extension := range certificate.Extensions {
		if extension.Id.Equal(fulcioIssuerOID) || extension.Id.Equal(fulcioIssuerOIDLegacy) {
			signature.Issuer = strings.Trim(strings.TrimSpace(string(extension.Value)), "\x0c\x13\x16")
		}
	}
}

// SignatureRequirement is what an installation asked of a vendor's signature:
// the key to check it with, and the identity it has to name.
type SignatureRequirement struct {
	// PublicKeyPEM is the vendor's verification key. Without it nothing can
	// be verified — see VerifyUpstream.
	PublicKeyPEM []byte

	// Identity the signature must name, empty for "any signer".
	Identity string

	// Issuer narrows Identity to one OIDC issuer, empty for any.
	Issuer string
}

// SignatureFact is the answer, in the three words the status enum uses.
type SignatureFact struct {
	// Result is `verified`, `unverifiable` or `none`.
	Result string

	// Identity is what the signature was required to name, echoed back so
	// that the fact says what was asked as well as what was found.
	Identity string

	// Signatures is how many were attached to the digest.
	Signatures int

	// Message explains an `unverifiable`.
	Message string
}

// The three results, as the API's UpstreamSignatureResult spells them. They
// are strings here rather than the api type because internal/attestation is
// below internal/api and api/v1alpha1 in the import graph and stays there.
const (
	SignatureVerified     = "verified"
	SignatureUnverifiable = "unverifiable"
	SignatureNone         = "none"
)

// VerifyUpstream turns what was found into the one fact the platform will
// stand behind.
//
// Three answers and no fourth:
//
//   - **none** — nothing is attached. Not a failure: most images published in
//     the world are unsigned, and a status that could not say this would
//     report an unsigned image and a bad signature in the same word.
//   - **verified** — a signature over *this digest* checks out under the key
//     the installation configured, and names the identity it required. This
//     is the only path to it, and the reason is in the next paragraph.
//   - **unverifiable** — anything else, with a message saying which: no key
//     configured, the key rejected every signature, or every signature named
//     somebody other than the required identity.
//
// **An identity alone can never produce `verified`.** A keyless signature
// carries a certificate, and a certificate is a claim by whoever issued it;
// believing one requires chaining it to Fulcio's root and checking the
// transparency log, neither of which this platform does. Reading the subject
// out of an unchained certificate and calling the signature verified would be
// evidence that is worse than none — so an installation that names an identity
// and no key is told, in the message, exactly what it would take.
func VerifyUpstream(digest string, requirement SignatureRequirement, signatures []UpstreamSignature) SignatureFact {
	fact := SignatureFact{Signatures: len(signatures), Identity: requirement.Identity}

	about := []UpstreamSignature{}
	for _, signature := range signatures {
		if signature.Describes(digest) {
			about = append(about, signature)
		}
	}
	switch {
	case len(signatures) == 0:
		fact.Result = SignatureNone
		return fact
	case len(about) == 0:
		fact.Result = SignatureUnverifiable
		fact.Message = "the signatures attached here are about another manifest, not this digest"
		return fact
	case len(requirement.PublicKeyPEM) == 0:
		fact.Result = SignatureUnverifiable
		fact.Message = "the vendor signed this digest and no public key is configured to check the " +
			"signature against: name one in the image source's signature.publicKeyRef, or on the " +
			"Connection it is pulled through"
		return fact
	}

	key, err := ParsePublicKey(requirement.PublicKeyPEM)
	if err != nil {
		fact.Result = SignatureUnverifiable
		fact.Message = "the configured verification key could not be read: " + err.Error()
		return fact
	}

	accepted := 0
	for _, signature := range about {
		if err := key.Verify(signature.Payload, signature.Signature); err != nil {
			continue
		}
		accepted++
		if requirement.Identity != "" && !signature.NamesIdentity(requirement.Identity) {
			continue
		}
		if requirement.Issuer != "" && !strings.EqualFold(signature.Issuer, requirement.Issuer) {
			continue
		}
		fact.Result = SignatureVerified
		return fact
	}

	fact.Result = SignatureUnverifiable
	switch {
	case accepted == 0:
		fact.Message = "a signature is attached to this digest and the configured key did not make it"
	case requirement.Issuer != "":
		fact.Message = fmt.Sprintf(
			"the signature on this digest checks out under the configured key but names neither identity %q nor issuer %q",
			requirement.Identity, requirement.Issuer)
	default:
		fact.Message = fmt.Sprintf(
			"the signature on this digest checks out under the configured key but does not name identity %q",
			requirement.Identity)
	}
	return fact
}

// signatureManifests returns every manifest that might hold a signature for
// the subject: whatever refers to it with cosign's signature artifact type,
// plus the `.sig` tag. It is the shape attestationManifests has, for the same
// reason — a registry that implements referrers answers the first, one that
// does not is served out of the tag, and reading both makes the answer the
// same either way.
func (s *Store) signatureManifests(
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
		if descriptor.ArtifactType != artifactTypeSignature {
			continue
		}
		image, err := remote.Image(subject.Context().Digest(descriptor.Digest.String()), options...)
		if err != nil {
			return nil, fmt.Errorf("fetching the signature %s failed: %w", descriptor.Digest, err)
		}
		images = append(images, image)
	}

	attachment, err := s.attachmentImage(ctx, signatureTag(subject))
	if err != nil {
		return nil, err
	}
	return append(images, attachment), nil
}

// signatureTag is cosign's name for a digest's signatures: `sha256-<hex>.sig`,
// in the artifact's own repository.
func signatureTag(subject name.Digest) name.Tag {
	return subject.Context().Tag(strings.Replace(subject.DigestStr(), ":", "-", 1) + signatureSuffix)
}

// layerBytes reads one layer whole, as stored.
func layerBytes(image v1.Image, digest v1.Hash) ([]byte, error) {
	layer, err := image.LayerByDigest(digest)
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
	return body, nil
}

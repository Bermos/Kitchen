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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// What a vendor's registry looks like, assembled and then read back.
//
// Two shapes, because there is no single convention: what `docker buildx
// --provenance` leaves inside the index (already covered by harvest_test.go,
// and reused here), and what `cosign attest` and `cosign sign` attach beside
// the digest. A vendor may do either, both or neither, and the platform has
// to answer the same way in all four cases.

// vendorImage pushes a plain image and answers its repository and digest.
func vendorImage(t *testing.T, host string) (repository, digest string) {
	t.Helper()
	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	image = mutate.MediaType(image, types.OCIManifestSchema1)
	image = mutate.ConfigMediaType(image, types.OCIConfigJSON)

	tag, err := name.NewTag(host+"/vendor/app:2026.9.1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, image); err != nil {
		t.Fatal(err)
	}
	hash, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return host + "/vendor/app", hash.String()
}

// cosignAttest attaches an in-toto statement in a DSSE envelope the way
// `cosign attest` does: under the `.att` tag, signed by somebody the platform
// holds no key for.
func cosignAttest(t *testing.T, repository, digest, predicateType, subjectDigest string) {
	t.Helper()
	statement, err := NewStatement(repository, subjectDigest, predicateType,
		map[string]any{"vendor": "example", "spdxVersion": "SPDX-2.3"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	envelope := Envelope{
		PayloadType: "application/vnd.in-toto+json",
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures:  []Signature{{KeyID: "the-vendor", Sig: "bm90LWNoZWNrZWQ="}},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	subject, err := name.NewDigest(repository+"@"+digest, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	image := mutate.ConfigMediaType(
		mutate.MediaType(empty.Image, types.OCIManifestSchema1), artifactTypeAttestation)
	image, err = mutate.Append(image, mutate.Addendum{
		Layer:       static.NewLayer(body, mediaTypeDSSE),
		Annotations: map[string]string{predicateTypeAnnotation: predicateType},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(attachmentTag(subject), image); err != nil {
		t.Fatal(err)
	}
}

// cosignSign attaches a simple-signing payload the way `cosign sign` does,
// signed by the key it answers, optionally with a certificate naming an
// identity.
func cosignSign(t *testing.T, repository, digest, aboutDigest, identity string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"critical": map[string]any{
			"identity": map[string]any{"docker-reference": repository},
			"image":    map[string]any{"docker-manifest-digest": aboutDigest},
			"type":     "cosign container image signature",
		},
		"optional": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	signature, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}

	annotations := map[string]string{
		signatureAnnotation: base64.StdEncoding.EncodeToString(signature),
	}
	if identity != "" {
		annotations[certificateAnnotation] = selfSignedCertificate(t, key, identity)
	}

	subject, err := name.NewDigest(repository+"@"+digest, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	image := mutate.ConfigMediaType(
		mutate.MediaType(empty.Image, types.OCIManifestSchema1), artifactTypeSignature)
	image, err = mutate.Append(image, mutate.Addendum{
		Layer:       static.NewLayer(payload, mediaTypeSimpleSigning),
		Annotations: annotations,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(signatureTag(subject), image); err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// selfSignedCertificate is the certificate a keyless signature carries — and
// the whole reason an identity alone cannot make a signature verified, since
// anyone can mint this.
func selfSignedCertificate(t *testing.T, key *ecdsa.PrivateKey, identity string) string {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber:   big.NewInt(1),
		Subject:        pkix.Name{CommonName: "vendor"},
		NotBefore:      time.Now().Add(-time.Hour),
		NotAfter:       time.Now().Add(time.Hour),
		EmailAddresses: []string{identity},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestVendorStatementsReadsWhatIsPublishedBesideTheDigest(t *testing.T) {
	host := testRegistry(t, true)
	repository, digest := vendorImage(t, host)

	// What `cosign attest` leaves: a DSSE envelope under the `.att` tag and
	// in the referrers listing, signed by somebody this platform holds no
	// key for.
	cosignAttest(t, repository, digest, PredicateSLSAProvenanceV1, digest)

	store := &Store{PlainHTTP: true}
	evidence, err := store.VendorStatements(context.Background(), repository+"@"+digest, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Statements) != 1 {
		t.Fatalf("harvested %d vendor statements, want the one the vendor published",
			len(evidence.Statements))
	}
	statement := evidence.Statements[0]
	if statement.PredicateType != PredicateSLSAProvenanceV1 {
		t.Errorf("harvested %q, want the vendor's provenance", statement.PredicateType)
	}
	// The only check the platform can make without a key, and the one that
	// matters: this is about the artifact being deployed.
	if !statement.Describes(digest) {
		t.Errorf("the harvested statement is about something other than %s", digest)
	}
}

func TestVendorStatementsToleratesAnIndexItCannotCallOneImage(t *testing.T) {
	host := testRegistry(t, true)

	// A vendor's multi-platform index is an ordinary vendored artifact. The
	// builder-side harvest refuses to choose an image inside one, and that
	// refusal must not stop the platform reading what is attached beside it.
	indexRef, _ := buildkitPush(t, host, PredicateSLSAProvenanceV1)
	repository, indexDigest, _ := strings.Cut(indexRef, "@")
	cosignAttest(t, repository, indexDigest, PredicateSPDX, indexDigest)

	store := &Store{PlainHTTP: true}
	evidence, err := store.VendorStatements(context.Background(), indexRef, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, statement := range evidence.Statements {
		if statement.PredicateType == PredicateSPDX {
			found = true
		}
		if !statement.Describes(indexDigest) {
			t.Errorf("a harvested statement is about something other than %s", indexDigest)
		}
	}
	if !found {
		t.Error("the vendor's bill of materials beside the index was not read")
	}
}

func TestVendorStatementsDiscardsAStatementAboutAnotherArtifact(t *testing.T) {
	host := testRegistry(t, true)
	repository, digest := vendorImage(t, host)

	// A statement about somebody else's image, attached beside this one.
	// Re-signing it would put the platform's signature on a claim about an
	// artifact it never deployed — the precise failure Describes exists for.
	other := "sha256:" + strings.Repeat("a", 64)
	cosignAttest(t, repository, digest, PredicateSPDX, other)

	store := &Store{PlainHTTP: true}
	evidence, err := store.VendorStatements(context.Background(), repository+"@"+digest, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Statements) != 0 {
		t.Errorf("harvested %d statements, want none: it is about %s", len(evidence.Statements), other)
	}
	if evidence.Discarded != 1 {
		t.Errorf("discarded %d, want 1 — it is never normal and something should say so", evidence.Discarded)
	}
}

func TestVendorStatementsSkipsWhatThePlatformAlreadySigned(t *testing.T) {
	host := testRegistry(t, true)
	repository, digest := vendorImage(t, host)

	key, _, _, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	statement, err := NewStatement(repository, digest, PredicateSPDX, map[string]any{"restated": true})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Sign(context.Background(), statement, key)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{PlainHTTP: true}
	if _, err := store.Attach(context.Background(), repository+"@"+digest, envelope, PredicateSPDX); err != nil {
		t.Fatal(err)
	}

	// Harvesting the platform's own restatement back would restate a
	// restatement, and the evidence set would grow by a copy of itself every
	// reconcile.
	evidence, err := store.VendorStatements(context.Background(), repository+"@"+digest, key.KeyID())
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Statements) != 0 {
		t.Errorf("harvested %d statements, want none: they are the platform's own", len(evidence.Statements))
	}
}

func TestUpstreamSignatureIsNoneWhenTheVendorPublishesNoSignature(t *testing.T) {
	host := testRegistry(t, true)
	repository, digest := vendorImage(t, host)

	store := &Store{PlainHTTP: true}
	signatures, err := store.Signatures(context.Background(), repository+"@"+digest)
	if err != nil {
		t.Fatal(err)
	}
	fact := VerifyUpstream(digest, SignatureRequirement{}, signatures)

	// The one thing this must not be is a failure. Most published images are
	// unsigned, and a status that reported an unsigned image and a bad
	// signature in one word would be ignored within the month.
	if fact.Result != SignatureNone {
		t.Errorf("an unsigned image reported %q, want %q", fact.Result, SignatureNone)
	}
	if fact.Message != "" {
		t.Errorf("an unsigned image carries the message %q, and there is nothing to explain", fact.Message)
	}
}

func TestUpstreamSignatureVerifiesAgainstAConfiguredKeyAndIdentity(t *testing.T) {
	host := testRegistry(t, true)
	repository, digest := vendorImage(t, host)
	publicKey := cosignSign(t, repository, digest, digest, "releases@example.com")

	store := &Store{PlainHTTP: true}
	signatures, err := store.Signatures(context.Background(), repository+"@"+digest)
	if err != nil {
		t.Fatal(err)
	}

	fact := VerifyUpstream(digest, SignatureRequirement{
		PublicKeyPEM: publicKey, Identity: "releases@example.com",
	}, signatures)
	if fact.Result != SignatureVerified {
		t.Fatalf("the vendor's signature reported %q (%s), want %q",
			fact.Result, fact.Message, SignatureVerified)
	}
	if fact.Identity != "releases@example.com" {
		t.Errorf("the fact names %q, want the identity that was required", fact.Identity)
	}

	// The same signature, with somebody else named, is not this
	// installation's signature.
	other := VerifyUpstream(digest, SignatureRequirement{
		PublicKeyPEM: publicKey, Identity: "somebody@else.example",
	}, signatures)
	if other.Result != SignatureUnverifiable {
		t.Errorf("a signature naming another identity reported %q, want %q",
			other.Result, SignatureUnverifiable)
	}
}

func TestUpstreamSignatureIsUnverifiableWithNoKeyHoweverTheIdentityReads(t *testing.T) {
	host := testRegistry(t, true)
	repository, digest := vendorImage(t, host)
	cosignSign(t, repository, digest, digest, "releases@example.com")

	store := &Store{PlainHTTP: true}
	signatures, err := store.Signatures(context.Background(), repository+"@"+digest)
	if err != nil {
		t.Fatal(err)
	}

	// The certificate names exactly what was asked for, and the platform
	// still will not call it verified: a certificate is a claim by whoever
	// issued it, and nothing here chains it to a root.
	fact := VerifyUpstream(digest, SignatureRequirement{Identity: "releases@example.com"}, signatures)
	if fact.Result != SignatureUnverifiable {
		t.Fatalf("an unchained certificate reported %q, want %q", fact.Result, SignatureUnverifiable)
	}
	if !strings.Contains(fact.Message, "publicKeyRef") {
		t.Errorf("the message %q does not say what it would take", fact.Message)
	}
	if fact.Signatures != 1 {
		t.Errorf("counted %d signatures, want 1 — an unsigned image and this are different facts",
			fact.Signatures)
	}
}

func TestUpstreamSignatureRefusesASignatureAboutAnotherManifest(t *testing.T) {
	host := testRegistry(t, true)
	repository, digest := vendorImage(t, host)
	other := "sha256:" + strings.Repeat("b", 64)
	publicKey := cosignSign(t, repository, digest, other, "")

	store := &Store{PlainHTTP: true}
	signatures, err := store.Signatures(context.Background(), repository+"@"+digest)
	if err != nil {
		t.Fatal(err)
	}
	fact := VerifyUpstream(digest, SignatureRequirement{PublicKeyPEM: publicKey}, signatures)
	if fact.Result != SignatureUnverifiable {
		t.Fatalf("a signature about %s reported %q over %s", other, fact.Result, digest)
	}
	if !strings.Contains(fact.Message, "another manifest") {
		t.Errorf("the message %q does not say the signature is about something else", fact.Message)
	}
}

// A descriptor the compiler would otherwise let drift: the platform reads
// cosign's own media type, and a signature recognised by resemblance is not a
// signature.
func TestSignatureLayerMediaTypeIsCosigns(t *testing.T) {
	if mediaTypeSimpleSigning != "application/vnd.dev.cosign.simplesigning.v1+json" {
		t.Errorf("the simple-signing media type is %q", mediaTypeSimpleSigning)
	}
	if signatureAnnotation != "dev.cosignproject.cosign/signature" {
		t.Errorf("the signature annotation is %q", signatureAnnotation)
	}
	var _ v1.Image = empty.Image
}

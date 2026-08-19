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
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// The round trip these tests are about is the acceptance criterion the design
// turns on: evidence has to survive a standards-compliant registry and come
// back readable by digest alone. They run against go-containerregistry's own
// in-process registry, which implements the distribution spec — including the
// OCI 1.1 referrers API, which is the half that cannot be checked by reading
// the code.

// testRegistry starts an in-process registry and returns its host.
// referrersAPI decides whether it implements the referrers endpoint, because
// the two answers have to be the same and only one of them is exercised by a
// registry that does.
func testRegistry(t *testing.T, referrersAPI bool) string {
	t.Helper()
	server := httptest.NewServer(registry.New(registry.WithReferrersSupport(referrersAPI)))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}

// pushArtifact puts an image in the registry and answers the digest reference
// evidence will be attached to.
func pushArtifact(t *testing.T, host string) string {
	t.Helper()
	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	image = mutate.MediaType(image, types.OCIManifestSchema1)
	reference, err := name.NewTag(host+"/shop:abc123", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(reference, image); err != nil {
		t.Fatal(err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return host + "/shop@" + digest.String()
}

func signedEnvelope(t *testing.T, digest string) (Envelope, *ECDSAKey) {
	t.Helper()
	key, _, _, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	_, ref, _ := strings.Cut(digest, "@")
	made, err := NewStatement("shop", ref, PredicateBuildRecord, map[string]any{"build": "shop-bld-1"})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Sign(context.Background(), made, key)
	if err != nil {
		t.Fatal(err)
	}
	return envelope, key
}

func TestAttachAndReadBackByDigest(t *testing.T) {
	for _, referrersAPI := range []bool{true, false} {
		name := "with the referrers API"
		if !referrersAPI {
			name = "through the fallback tag"
		}
		t.Run(name, func(t *testing.T) {
			host := testRegistry(t, referrersAPI)
			artifact := pushArtifact(t, host)
			envelope, key := signedEnvelope(t, artifact)

			store := &Store{PlainHTTP: true}
			if _, err := store.Attach(context.Background(), artifact, envelope, PredicateBuildRecord); err != nil {
				t.Fatal(err)
			}

			set, err := store.Evidence(context.Background(), artifact, key)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Verified {
				t.Error("an evidence set gathered with a key reports itself unverified")
			}
			if len(set.Attestations) != 1 {
				t.Fatalf("found %d attestations, want 1", len(set.Attestations))
			}
			found := set.Attestations[0]
			if !found.Verified {
				t.Error("the platform's own attestation did not verify against the key that signed it")
			}
			if found.PredicateType != PredicateBuildRecord {
				t.Errorf("predicate type is %q, want %q", found.PredicateType, PredicateBuildRecord)
			}
			if !found.Statement.Describes(strings.SplitN(artifact, "@", 2)[1]) {
				t.Error("the attestation read back does not describe the artifact it was attached to")
			}
		})
	}
}

// Attaching is idempotent by content: a reconcile that runs twice must not
// grow the evidence set.
func TestAttachDoesNotDuplicateTheSameEnvelope(t *testing.T) {
	host := testRegistry(t, true)
	artifact := pushArtifact(t, host)
	envelope, key := signedEnvelope(t, artifact)
	store := &Store{PlainHTTP: true}

	first, err := store.Attach(context.Background(), artifact, envelope, PredicateBuildRecord)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Attach(context.Background(), artifact, envelope, PredicateBuildRecord)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("re-attaching the same envelope moved the manifest from %s to %s", first, second)
	}

	set, err := store.Evidence(context.Background(), artifact, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Attestations) != 1 {
		t.Errorf("attaching the same envelope twice left %d attestations", len(set.Attestations))
	}
}

// Two different assertions about the same artifact both have to be kept: they
// are two claims, not two copies.
func TestAttachAccumulatesDistinctEvidence(t *testing.T) {
	host := testRegistry(t, true)
	artifact := pushArtifact(t, host)
	first, key := signedEnvelope(t, artifact)

	_, ref, _ := strings.Cut(artifact, "@")
	deployment, err := NewStatement("shop", ref, PredicateDeployment, map[string]any{"environment": "shop-production"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Sign(context.Background(), deployment, key)
	if err != nil {
		t.Fatal(err)
	}

	store := &Store{PlainHTTP: true}
	if _, err := store.Attach(context.Background(), artifact, first, PredicateBuildRecord); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Attach(context.Background(), artifact, second, PredicateDeployment); err != nil {
		t.Fatal(err)
	}

	set, err := store.Evidence(context.Background(), artifact, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Attestations) != 2 {
		t.Fatalf("found %d attestations, want 2", len(set.Attestations))
	}
	predicates := map[string]bool{}
	for _, found := range set.Attestations {
		predicates[found.PredicateType] = true
		if !found.Verified {
			t.Errorf("attestation %s did not verify", found.PredicateType)
		}
	}
	if !predicates[PredicateBuildRecord] || !predicates[PredicateDeployment] {
		t.Errorf("found predicates %v, want both Kitchen's build record and its deployment record", predicates)
	}
}

// The manifest has to be findable the way cosign looks for it — under the
// `sha256-<hex>.att` tag — and it has to name the artifact as its subject, so
// that a referrers listing finds it too. Both are checked here rather than
// inferred, because getting either wrong produces evidence Kitchen can read
// and nothing else can.
func TestAttachedManifestIsBothTaggedAndSubjectLinked(t *testing.T) {
	host := testRegistry(t, true)
	artifact := pushArtifact(t, host)
	envelope, _ := signedEnvelope(t, artifact)

	store := &Store{PlainHTTP: true}
	if _, err := store.Attach(context.Background(), artifact, envelope, PredicateBuildRecord); err != nil {
		t.Fatal(err)
	}

	subject, err := name.NewDigest(artifact, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	tag := attachmentTag(subject)
	if !strings.HasSuffix(tag.TagStr(), ".att") || !strings.HasPrefix(tag.TagStr(), "sha256-") {
		t.Errorf("attachment tag is %q, want cosign's sha256-<hex>.att", tag.TagStr())
	}

	image, err := remote.Image(tag)
	if err != nil {
		t.Fatalf("the attestation is not under the tag cosign would look for: %v", err)
	}
	manifest, err := image.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Subject == nil || manifest.Subject.Digest.String() != subject.DigestStr() {
		t.Errorf("the attestation manifest's subject is %v, want %s", manifest.Subject, subject.DigestStr())
	}
	if len(manifest.Layers) != 1 || string(manifest.Layers[0].MediaType) != mediaTypeDSSE {
		t.Errorf("the attestation layer is %v, want one %s", manifest.Layers, mediaTypeDSSE)
	}

	index, err := remote.Referrers(subject)
	if err != nil {
		t.Fatal(err)
	}
	listing, err := index.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Manifests) != 1 {
		t.Fatalf("the referrers listing holds %d entries, want 1", len(listing.Manifests))
	}
	if listing.Manifests[0].ArtifactType != artifactTypeAttestation {
		t.Errorf("the referrers entry is typed %q, want %q",
			listing.Manifests[0].ArtifactType, artifactTypeAttestation)
	}
}

// Evidence is keyed to content. An attestation attached to one artifact must
// not turn up against another, however alike they are.
func TestEvidenceIsScopedToTheDigest(t *testing.T) {
	host := testRegistry(t, true)
	attested := pushArtifact(t, host)
	other := pushArtifact(t, host)
	envelope, key := signedEnvelope(t, attested)

	store := &Store{PlainHTTP: true}
	if _, err := store.Attach(context.Background(), attested, envelope, PredicateBuildRecord); err != nil {
		t.Fatal(err)
	}

	set, err := store.Evidence(context.Background(), other, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Attestations) != 0 {
		t.Errorf("an unattested artifact came back with %d attestations", len(set.Attestations))
	}
}

// A listing gathered with no keys says what is attached and refuses to claim
// any of it was checked.
func TestEvidenceWithoutKeysIsAListingNotAVerification(t *testing.T) {
	host := testRegistry(t, true)
	artifact := pushArtifact(t, host)
	envelope, _ := signedEnvelope(t, artifact)

	store := &Store{PlainHTTP: true}
	if _, err := store.Attach(context.Background(), artifact, envelope, PredicateBuildRecord); err != nil {
		t.Fatal(err)
	}

	set, err := store.Evidence(context.Background(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if set.Verified {
		t.Error("an evidence set gathered with no keys claims to be verified")
	}
	if len(set.Attestations) != 1 || set.Attestations[0].Verified {
		t.Errorf("attestations %+v, want one that is listed and not verified", set.Attestations)
	}
}

func TestAttachRefusesATagReference(t *testing.T) {
	host := testRegistry(t, true)
	envelope, _ := signedEnvelope(t, "shop@sha256:"+strings.Repeat("a", 64))

	store := &Store{PlainHTTP: true}
	_, err := store.Attach(context.Background(), host+"/shop:latest", envelope, PredicateBuildRecord)
	if err == nil {
		t.Fatal("evidence was attached to a tag, which is a moving target")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}

// An artifact with nothing attached answers an empty set rather than failing:
// "no evidence" is an answer, and a caller that could not tell it from an
// error would report a registry fault every time it asked about a fresh image.
func TestEvidenceForAnUntouchedArtifact(t *testing.T) {
	host := testRegistry(t, true)
	artifact := pushArtifact(t, host)

	set, err := (&Store{PlainHTTP: true}).Evidence(context.Background(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Attestations) != 0 {
		t.Errorf("a fresh artifact came back with %d attestations", len(set.Attestations))
	}
	if set.Subject == "" {
		t.Error("the evidence set does not say what it is about")
	}
}

// Kitchen must not mistake somebody else's referrer — a signature, an SBOM
// attached by other tooling — for an attestation it can read.
func TestEvidenceIgnoresReferrersOfAnotherKind(t *testing.T) {
	host := testRegistry(t, true)
	artifact := pushArtifact(t, host)

	subject, err := name.NewDigest(artifact, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := remote.Head(subject)
	if err != nil {
		t.Fatal(err)
	}
	stranger := mutate.ConfigMediaType(
		mutate.MediaType(empty.Image, types.OCIManifestSchema1),
		"application/vnd.example.signature.v1+json")
	linked, ok := mutate.Subject(stranger, *descriptor).(v1.Image)
	if !ok {
		t.Fatal("could not build a foreign referrer")
	}
	strangerDigest, err := linked.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(subject.Context().Digest(strangerDigest.String()), linked); err != nil {
		t.Fatal(err)
	}

	set, err := (&Store{PlainHTTP: true}).Evidence(context.Background(), artifact)
	if err != nil {
		t.Fatalf("a foreign referrer broke the evidence read: %v", err)
	}
	if len(set.Attestations) != 0 {
		t.Errorf("a foreign referrer was read as %d attestations", len(set.Attestations))
	}
}

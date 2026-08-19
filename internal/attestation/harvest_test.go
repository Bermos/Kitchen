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
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// These tests build what BuildKit builds and then read it back.
//
// The shape they assemble is not invented: it was taken off a real
// `buildctl build --opt attest:provenance=… --opt attest:sbom=` against
// moby/buildkit v0.23.2 pushing to a registry — an OCI index holding the image
// manifest and, beside it, an attestation manifest annotated
// `vnd.docker.reference.type=attestation-manifest` whose layers are bare
// in-toto Statements with the subject set to the *image manifest* digest.
//
// That last detail is the one worth a test rather than a comment. If the
// subject were the index digest, the artifact identity in the reconciler would
// be the index and every line of this would still compile.

// buildkitPush assembles an index the way BuildKit does and pushes it, in
// answer to which it gives back the index reference, the image manifest digest
// inside it, and the predicate types it wrote.
func buildkitPush(t *testing.T, host string, predicateTypes ...string) (indexRef, imageDigest string) {
	t.Helper()

	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	image = mutate.MediaType(image, types.OCIManifestSchema1)
	image = mutate.ConfigMediaType(image, types.OCIConfigJSON)
	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}

	attestations := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	attestations = mutate.ConfigMediaType(attestations, types.OCIConfigJSON)
	for _, predicateType := range predicateTypes {
		statement := map[string]any{
			"_type": "https://in-toto.io/Statement/v0.1",
			"subject": []map[string]any{{
				// BuildKit names the subject with a package URL carrying the
				// tag it pushed to. The digest is what matters and the name is
				// what has to be rewritten.
				"name":   "pkg:docker/" + host + "/shop@latest?platform=linux%2Famd64",
				"digest": map[string]string{"sha256": digest.Hex},
			}},
			"predicateType": predicateType,
			"predicate":     map[string]any{"builder": map[string]any{"id": "https://kitchen.bermos.dev/builder/buildkit"}},
		}
		body, err := json.Marshal(statement)
		if err != nil {
			t.Fatal(err)
		}
		layer := static.NewLayer(body, mediaTypeInToto)
		attestations, err = mutate.Append(attestations, mutate.Addendum{
			Layer:       layer,
			Annotations: map[string]string{"in-toto.io/predicate-type": predicateType},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	index := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	index = mutate.AppendManifests(index,
		mutate.IndexAddendum{
			Add: image,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: "linux", Architecture: "amd64"},
			},
		},
		mutate.IndexAddendum{
			Add: attestations,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: "unknown", Architecture: "unknown"},
				Annotations: map[string]string{
					referenceTypeAnnotation:   attestationManifestType,
					referenceDigestAnnotation: digest.String(),
				},
			},
		},
	)

	tag, err := name.NewTag(host+"/shop:abc123", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(tag, index); err != nil {
		t.Fatal(err)
	}
	indexDigest, err := index.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return host + "/shop@" + indexDigest.String(), digest.String()
}

func TestHarvestReadsTheBuildersStatementsAndNamesTheImageAsTheArtifact(t *testing.T) {
	host := testRegistry(t, true)
	indexRef, imageDigest := buildkitPush(t, host, PredicateSLSAProvenanceV1, PredicateSPDX)

	store := &Store{PlainHTTP: true}
	evidence, err := store.Harvest(context.Background(), indexRef)
	if err != nil {
		t.Fatal(err)
	}

	// The artifact is the image inside the index, not the index. Everything
	// downstream — the Release, the evidence set, a promotion policy — keys on
	// this, and the builder's own statements are about it.
	if evidence.ImageDigest != imageDigest {
		t.Errorf("harvest called %s the artifact, want the image manifest %s", evidence.ImageDigest, imageDigest)
	}
	if strings.Contains(indexRef, evidence.ImageDigest) {
		t.Error("harvest answered the index digest, which is not what the statements are about")
	}
	if len(evidence.Statements) != 2 {
		t.Fatalf("harvested %d statements, want 2", len(evidence.Statements))
	}
	if evidence.Discarded != 0 {
		t.Errorf("discarded %d statements that were about the artifact", evidence.Discarded)
	}
	found := map[string]bool{}
	for _, statement := range evidence.Statements {
		found[statement.PredicateType] = true
		if !statement.Describes(imageDigest) {
			t.Errorf("the %s statement is not about the artifact", statement.PredicateType)
		}
	}
	if !found[PredicateSLSAProvenanceV1] || !found[PredicateSPDX] {
		t.Errorf("harvested %v, want provenance and an SBOM", found)
	}
}

func TestHarvestOfAPlainImageIsNotAFailure(t *testing.T) {
	// What a build with attestations turned off pushes. It has to answer the
	// digest and no statements rather than an error, or turning the feature
	// off would fail every build.
	host := testRegistry(t, true)
	imageRef := pushArtifact(t, host)

	store := &Store{PlainHTTP: true}
	evidence, err := store.Harvest(context.Background(), imageRef)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, _ := strings.Cut(imageRef, "@")
	if evidence.ImageDigest != digest {
		t.Errorf("harvest answered %s, want the pushed digest %s", evidence.ImageDigest, digest)
	}
	if len(evidence.Statements) != 0 {
		t.Errorf("harvested %d statements from an image that carries none", len(evidence.Statements))
	}
}

func TestHarvestDiscardsAStatementAboutAnotherArtifact(t *testing.T) {
	host := testRegistry(t, true)

	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	image = mutate.MediaType(image, types.OCIManifestSchema1)
	image = mutate.ConfigMediaType(image, types.OCIConfigJSON)
	imageDigest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}

	// A statement whose subject is some other digest, in the attestation
	// manifest of this one. Restating it would put the platform's signature on
	// a claim about an image it did not build.
	statement := map[string]any{
		"_type":         "https://in-toto.io/Statement/v0.1",
		"subject":       []map[string]any{{"digest": map[string]string{"sha256": strings.Repeat("b", 64)}}},
		"predicateType": PredicateSLSAProvenanceV1,
		"predicate":     map[string]any{},
	}
	body, err := json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	attestations := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	attestations = mutate.ConfigMediaType(attestations, types.OCIConfigJSON)
	attestations, err = mutate.Append(attestations, mutate.Addendum{
		Layer: static.NewLayer(body, mediaTypeInToto),
	})
	if err != nil {
		t.Fatal(err)
	}

	index := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	index = mutate.AppendManifests(index,
		mutate.IndexAddendum{
			Add:        image,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
		},
		mutate.IndexAddendum{
			Add: attestations,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: "unknown", Architecture: "unknown"},
				Annotations: map[string]string{
					referenceTypeAnnotation:   attestationManifestType,
					referenceDigestAnnotation: imageDigest.String(),
				},
			},
		},
	)
	tag, err := name.NewTag(host+"/shop:abc123", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(tag, index); err != nil {
		t.Fatal(err)
	}
	indexDigest, err := index.Digest()
	if err != nil {
		t.Fatal(err)
	}

	store := &Store{PlainHTTP: true}
	evidence, err := store.Harvest(context.Background(), host+"/shop@"+indexDigest.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Statements) != 0 {
		t.Errorf("kept %d statements about another artifact", len(evidence.Statements))
	}
	if evidence.Discarded != 1 {
		t.Errorf("reported %d discarded, want 1 — a dropped statement has to be countable", evidence.Discarded)
	}
}

func TestRestateKeepsThePredicateBytesAndRewritesTheSubject(t *testing.T) {
	// The predicate has to survive byte for byte: it is what the platform is
	// about to sign, and a re-encoded map is a different claim from the one
	// the builder made.
	predicate := json.RawMessage(`{"z":1,"a":{"nested":[1,2,3]},"m":"x"}`)
	digest := "sha256:" + strings.Repeat("c", 64)
	made := Statement{
		Type:          "https://in-toto.io/Statement/v0.1",
		Subject:       []ResourceDescriptor{{Name: "pkg:docker/host/shop@latest", Digest: map[string]string{"sha256": strings.Repeat("c", 64)}}},
		PredicateType: PredicateSLSAProvenanceV1,
		Predicate:     predicate,
	}

	restated, err := Restate("registry.example.com/shop", digest, made)
	if err != nil {
		t.Fatal(err)
	}
	if string(restated.Predicate) != string(predicate) {
		t.Errorf("the predicate was rewritten:\n got %s\nwant %s", restated.Predicate, predicate)
	}
	if restated.Type != StatementType {
		t.Errorf("the statement declares %s, want in-toto %s", restated.Type, StatementType)
	}
	if restated.PredicateType != PredicateSLSAProvenanceV1 {
		t.Errorf("the predicate type changed to %s", restated.PredicateType)
	}
	if len(restated.Subject) != 1 || restated.Subject[0].Name != "registry.example.com/shop" {
		t.Errorf("the subject was not rewritten to the repository: %+v", restated.Subject)
	}
	if !restated.Describes(digest) {
		t.Error("the restated statement is not about the artifact")
	}
}

func TestRestateRefusesAStatementAboutAnotherArtifact(t *testing.T) {
	made := Statement{
		PredicateType: PredicateSPDX,
		Subject:       []ResourceDescriptor{{Digest: map[string]string{"sha256": strings.Repeat("d", 64)}}},
		Predicate:     json.RawMessage(`{}`),
	}
	if _, err := Restate("registry.example.com/shop", "sha256:"+strings.Repeat("e", 64), made); err == nil {
		t.Fatal("restating a statement about another artifact was allowed")
	}
}

func TestPredicateHelpersNameTheStandards(t *testing.T) {
	for _, predicateType := range []string{PredicateSLSAProvenanceV1, PredicateSLSAProvenanceV02} {
		if !Provenance(predicateType) {
			t.Errorf("%s is not recognised as provenance", predicateType)
		}
		if KitchenPredicate(predicateType) {
			t.Errorf("%s was taken for one of Kitchen's own", predicateType)
		}
	}
	for _, predicateType := range []string{PredicateSPDX, PredicateCycloneDX} {
		if !SBOM(predicateType) {
			t.Errorf("%s is not recognised as a bill of materials", predicateType)
		}
	}
	if Provenance(PredicateBuildRecord) {
		t.Error("Kitchen's build record was taken for SLSA provenance, which is the one confusion the design forbids")
	}
}

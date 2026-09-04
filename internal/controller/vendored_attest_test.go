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

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
)

// The evidence an artifact the platform did not build arrives with (#309).

// vendorFixtures is a project whose web process runs somebody else's image.
func vendorFixtures(t *testing.T) (*BuildReconciler, *stubAttester, *kitchenv1alpha1.Build, *kitchenv1alpha1.Project) {
	t.Helper()
	reconciler, attester, build, project, _ := attestFixtures(t)
	build.Spec.Git = kitchenv1alpha1.GitRevision{}
	build.Annotations = map[string]string{audit.RequestedByAnnotation: admitter}
	project.Spec.Source = kitchenv1alpha1.ProjectSourceSpec{
		Image: &kitchenv1alpha1.ImageSourceSpec{
			Repository: "ghcr.io/vendor/app",
			Tag:        "2026.9.1",
		},
	}
	return reconciler, attester, build, project
}

const vendorImageRef = "ghcr.io/vendor/app@sha256:" +
	"1111111111111111111111111111111111111111111111111111111111111111"

// admitter is whoever asked for the acquisition in these fixtures. A four-eyes
// rule whose first eye is nobody cannot be answered, so every one of them names
// somebody.
const admitter = "ana@example.com"

func TestAcquiredArtifactCarriesTheAdoptionAndTheVendorsOwnClaims(t *testing.T) {
	reconciler, attester, build, project := vendorFixtures(t)

	// What the vendor published beside the digest.
	published, err := attestation.NewStatement(
		"ghcr.io/vendor/app", strings.TrimPrefix(vendorImageRef, "ghcr.io/vendor/app@"),
		attestation.PredicateSPDX, map[string]any{"spdxVersion": "SPDX-2.3"})
	if err != nil {
		t.Fatal(err)
	}
	attester.vendored = []attestation.Statement{published}

	status := reconciler.attestAcquired(context.Background(), build, project, vendoredSubject{
		Source: project.Spec.Source.ImageSource(),
		Image:  vendorImageRef,
	})
	if status == nil {
		t.Fatal("no artifact status was produced for an acquisition")
	}
	if status.Message != "" {
		t.Fatalf("attesting an acquired artifact reported %q", status.Message)
	}

	// It is not a built artifact, and the field says so rather than leaving
	// a reader to infer it from the absence of a build record.
	if status.SourceType != kitchenv1alpha1.ArtifactSourceVendored {
		t.Errorf("the artifact reads sourceType %q, want vendored", status.SourceType)
	}
	if status.Upstream == nil {
		t.Fatal("nothing recorded where the artifact came from")
	}
	if status.Upstream.Reference != "ghcr.io/vendor/app:2026.9.1" {
		t.Errorf("the upstream reads %q, want the reference as declared", status.Upstream.Reference)
	}
	// A four-eyes rule whose first eye is nobody cannot be answered.
	if status.Upstream.AdmittedBy != admitter {
		t.Errorf("admitted by %q, want the caller who asked", status.Upstream.AdmittedBy)
	}
	if status.Upstream.AdmittedAt == nil {
		t.Error("nothing recorded when the digest was admitted")
	}

	// Two attestations: the vendor's, restated, and the platform's own
	// account of taking it. Each labelled with whose claim it is, because
	// the platform's signature is on both and cannot tell them apart.
	sources := map[string]string{}
	for _, entry := range status.Evidence {
		sources[entry.PredicateType] = entry.Source
	}
	if sources[attestation.PredicateSPDX] != sourceVendorAsserted {
		t.Errorf("the vendor's bill of materials is indexed %q, want %q",
			sources[attestation.PredicateSPDX], sourceVendorAsserted)
	}
	if sources[attestation.PredicateArtifactAdoption] != sourcePlatformObserved {
		t.Errorf("the adoption record is indexed %q, want %q",
			sources[attestation.PredicateArtifactAdoption], sourcePlatformObserved)
	}
	if status.Upstream.VendorAttestations != 1 {
		t.Errorf("counted %d vendor attestations, want 1", status.Upstream.VendorAttestations)
	}
	if status.AttestedAt == nil || status.KeyID == "" {
		t.Errorf("the acquired artifact was not recorded as attested: %+v", status)
	}

	// The platform's own restatements must not be harvested back on a later
	// pass, or the evidence set would grow by a copy of itself every time.
	for _, keyID := range attester.vendorKeyIDs {
		if keyID == "" {
			t.Error("the vendor harvest was not told which key is the platform's own")
		}
	}
}

func TestAnAcquiredArtifactSignsNoBuildRecord(t *testing.T) {
	reconciler, attester, build, project := vendorFixtures(t)
	reconciler.attestAcquired(context.Background(), build, project, vendoredSubject{
		Source: project.Spec.Source.ImageSource(),
		Image:  vendorImageRef,
	})
	// Nothing fakes a commit, and nothing fakes a build either: the platform
	// did not build this and has no account of a build to give.
	for _, predicate := range attester.predicates {
		if predicate == attestation.PredicateBuildRecord {
			t.Fatalf("an artifact nobody here built carries a build record: %v", attester.predicates)
		}
	}
}

func TestTheAdoptionRecordSaysWhoAndFromWhereAndWhatBecameOfTheSignature(t *testing.T) {
	reconciler, attester, build, project := vendorFixtures(t)
	reconciler.attestAcquired(context.Background(), build, project, vendoredSubject{
		Source: project.Spec.Source.ImageSource(),
		Image:  vendorImageRef,
	})

	var record map[string]any
	for i, predicate := range attester.predicates {
		if predicate != attestation.PredicateArtifactAdoption {
			continue
		}
		statement, err := attester.attached[i].Statement()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(statement.Predicate, &record); err != nil {
			t.Fatal(err)
		}
	}
	if record == nil {
		t.Fatal("no adoption record was signed")
	}
	if record["admittedBy"] != admitter {
		t.Errorf("the record says admittedBy %v", record["admittedBy"])
	}
	upstream, _ := record["upstream"].(map[string]any)
	if upstream["reference"] != "ghcr.io/vendor/app:2026.9.1" {
		t.Errorf("the record's upstream reads %v", upstream)
	}
	signature, _ := record["signature"].(map[string]any)
	// The vendor published nothing, and that is a fact rather than a
	// failure — the one distinction the whole three-valued result exists to
	// keep.
	if signature["result"] != string(kitchenv1alpha1.UpstreamSignatureNone) {
		t.Errorf("the record's signature reads %v, want the unsigned answer", signature)
	}
	if record["build"] != build.Name || record["project"] != project.Name {
		t.Errorf("the record does not say which build of which project admitted it: %v", record)
	}
}

func TestAnUnsignedVendoredImageIsRecordedAsAFactNotAFailure(t *testing.T) {
	reconciler, _, build, project := vendorFixtures(t)
	status := reconciler.attestAcquired(context.Background(), build, project, vendoredSubject{
		Source: project.Spec.Source.ImageSource(),
		Image:  vendorImageRef,
	})
	if status.Upstream.Signature.Result != kitchenv1alpha1.UpstreamSignatureNone {
		t.Errorf("an unsigned image reads %q", status.Upstream.Signature.Result)
	}
	if status.Upstream.Signature.Message != "" {
		t.Errorf("an unsigned image carries the message %q, and there is nothing to explain",
			status.Upstream.Signature.Message)
	}
	if status.AttestedAt == nil {
		t.Error("an unsigned image was left unattested; the signature is a separate question")
	}
}

func TestAdmittedByFallsBackToWhoeverPointedThePlatformAtTheVendor(t *testing.T) {
	_, _, build, project := vendorFixtures(t)
	// The platform's own first acquisition of a project somebody created:
	// the Build carries no caller, and the project's creator is the person
	// who pointed this installation at that vendor.
	build.Annotations = nil
	project.Annotations = map[string]string{audit.RequestedByAnnotation: admitter}
	if who := admittedBy(build, project); who != admitter {
		t.Errorf("admitted by %q, want the project's creator", who)
	}

	// And failing both, the reconciler — never blank, and never "the
	// operator".
	project.Annotations = nil
	who := admittedBy(build, project)
	if !strings.HasPrefix(who, "system:controller/") {
		t.Errorf("admitted by %q, want the named reconciler", who)
	}
}

func TestAttestationOffLeavesTheUpstreamFactsAndAttachesNothing(t *testing.T) {
	reconciler, attester, build, project := vendorFixtures(t)
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := reconciler.Get(context.Background(),
		client.ObjectKey{Name: KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Spec.Compliance.Attestation.Enabled = false
	if err := reconciler.Update(context.Background(), kitchen); err != nil {
		t.Fatal(err)
	}

	status := reconciler.attestAcquired(context.Background(), build, project, vendoredSubject{
		Source: project.Spec.Source.ImageSource(),
		Image:  vendorImageRef,
	})
	if len(attester.attached) != 0 {
		t.Errorf("attached %d envelopes with attestation off", len(attester.attached))
	}
	// Off by choice is not a message: the platform's compliance status says
	// so once. What is still recorded is where the image came from, because
	// it costs no registry write.
	if status.Message != "" {
		t.Errorf("attestation being off was reported as a fault: %q", status.Message)
	}
	if status.Upstream == nil || status.Upstream.Reference == "" {
		t.Error("the upstream reference was lost when attestation was turned off")
	}
}

// A vendored artifact is judged by the rules as the input carries it, and the
// input is materialized off exactly these fields.
func TestVendoredFactsReachThePolicyInput(t *testing.T) {
	build := &kitchenv1alpha1.Build{
		Status: kitchenv1alpha1.BuildStatus{
			Artifact: &kitchenv1alpha1.ArtifactStatus{
				Repository: "ghcr.io/vendor/app",
				Digest:     "sha256:" + strings.Repeat("1", 64),
				SourceType: kitchenv1alpha1.ArtifactSourceVendored,
				AttestedAt: ptr.To(metav1.NewTime(time.Now())),
				Upstream: &kitchenv1alpha1.UpstreamArtifactStatus{
					Reference:  "ghcr.io/vendor/app:2026.9.1",
					AdmittedBy: admitter,
					Signature: kitchenv1alpha1.UpstreamSignatureStatus{
						Result:   kitchenv1alpha1.UpstreamSignatureVerified,
						Identity: "releases@vendor.example",
					},
				},
			},
		},
	}
	vendored := build.VendoredArtifacts()
	if len(vendored) != 1 || vendored[0].Name() != kitchenv1alpha1.WebProcessName {
		t.Fatalf("the unit reports %d vendored images", len(vendored))
	}

	// And a built artifact is not one of them, whatever else it carries.
	build.Status.Artifact.SourceType = kitchenv1alpha1.ArtifactSourceBuilt
	if len(build.VendoredArtifacts()) != 0 {
		t.Error("a built artifact was reported as vendored")
	}
}

// The promise the rest of this file is only worth anything against: **a built
// artifact's evidence is byte-for-byte what it was** (#309).
//
// It is asserted against literals rather than against a re-derivation,
// because a test that computes the expected payload the same way the code
// does would follow the code wherever it went. Evidence is what is signed:
// a payload that quietly changed shape would leave every signature made
// before it verifying a document that no longer says the same thing, and the
// two source words this issue added to the index would be a schema change to
// every reader of a built artifact's status.
const (
	builtBuildRecord = `{"build":"shop-bld-1","builder":{"platform":"kitchen",` +
		`"version":"dev"},"framework":"nextjs","project":"shop","source":{` +
		`"branch":"main","commit":"abc123def456","repository":"acme/shop"},` +
		`"strategy":"buildpacks"}`

	builtArtifactStatus = `{"repository":"registry.example.com/shop",` +
		`"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
		`"sourceType":"built","evidence":[` +
		`{"predicateType":"https://kitchen.bermos.dev/attestation/build-record/v1",` +
		`"manifest":"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",` +
		`"source":"platform"},` +
		`{"predicateType":"https://slsa.dev/provenance/v1",` +
		`"manifest":"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",` +
		`"source":"builder"}]}`
)

func TestABuiltArtifactsEvidenceIsUnchangedByAnyOfThis(t *testing.T) {
	reconciler, attester, build, project, target := attestFixtures(t)
	imageDigest := "sha256:" + strings.Repeat("a", 64)
	attester.harvestDigest = imageDigest
	attester.harvested = []attestation.Statement{
		builderStatement(t, imageDigest, attestation.PredicateSLSAProvenanceV1),
	}

	status := reconciler.attestBuild(context.Background(), build, project, target,
		artifactSubject{Strategy: target.Strategy, Image: "registry.example.com/shop@" + imageDigest})
	if status == nil || status.Message != "" {
		t.Fatalf("attesting a built artifact reported %+v", status)
	}

	// The signed build record, byte for byte.
	var record []byte
	for i, predicate := range attester.predicates {
		if predicate != attestation.PredicateBuildRecord {
			continue
		}
		statement, err := attester.attached[i].Statement()
		if err != nil {
			t.Fatal(err)
		}
		record = statement.Predicate
	}
	if string(record) != builtBuildRecord {
		t.Errorf("the build record a built artifact carries changed:\n got %s\nwant %s",
			record, builtBuildRecord)
	}

	// And the status it is indexed on. The two fields the platform stamps at
	// signing time are the only ones that vary between two runs of the same
	// build, so they are checked for presence and then cleared; everything
	// else is compared as written.
	if status.AttestedAt == nil || status.KeyID == "" {
		t.Fatalf("the built artifact was not recorded as attested: %+v", status)
	}
	status.AttestedAt, status.KeyID = nil, ""
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != builtArtifactStatus {
		t.Errorf("what a built artifact records changed:\n got %s\nwant %s",
			encoded, builtArtifactStatus)
	}
	// Said once more in the words the criterion is written in, because the
	// literal above is easy to update without noticing what it now permits.
	if status.SourceType != kitchenv1alpha1.ArtifactSourceBuilt {
		t.Errorf("a built artifact reads sourceType %q", status.SourceType)
	}
	if status.Upstream != nil || status.ObservedSBOM != nil {
		t.Error("a built artifact carries an upstream or an observed bill of materials")
	}
	for _, entry := range status.Evidence {
		if entry.Source != sourcePlatform && entry.Source != sourceBuilder {
			t.Errorf("a built artifact's %s is credited to %q", entry.PredicateType, entry.Source)
		}
	}
}

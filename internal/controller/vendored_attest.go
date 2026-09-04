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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/provider"
	"github.com/Bermos/Kitchen/internal/version"
)

// The evidence an artifact the platform did not build arrives with (#309).
//
// A built artifact leaves the build with two claims: the reconciler's account
// of the build it orchestrated, and the builder's account of the build it ran
// (build_attest.go). A vendored artifact has neither, and it is precisely the
// artifact an institution is asked hardest about. What it *can* have is three
// other things, and this file is all three:
//
//  1. **What the vendor published.** Provenance and bills of materials are
//     increasingly attached to a digest as OCI referrers, and evidence keys on
//     a digest rather than on a pipeline — so the harvest path already works
//     for an image nobody here built. Each statement is checked to describe
//     the digest being deployed, restated about it, and signed under the
//     platform's key. It is indexed `vendor-asserted`: the platform's
//     signature means "this is what the vendor said, unaltered", and never
//     "this is true".
//  2. **What the platform observed.** Where the vendor published no bill of
//     materials, the platform generates one over the digest it pulled
//     (vendored_sbom.go) and indexes it `platform-observed`. The two are
//     never merged into one word, because a bill of materials the vendor
//     stands behind and one the platform derived by unpacking layers are
//     different artefacts with different failure modes.
//  3. **The adoption itself.** Who admitted this digest onto this platform,
//     when, from which upstream reference, and what became of the vendor's
//     own signature. Nothing standard describes that — it is a claim about
//     this installation's relationship to the artifact rather than about the
//     artifact — so it is a Kitchen predicate.
//
// # Nothing fakes a commit
//
// There is no build record here and there will not be one. `require-pull-
// request`, `require-independent-review` and `no-self-approval` are claims
// about a change under review; a vendored artifact does not satisfy them and
// must not be made to appear to. The four-eyes question it *can* answer — did
// somebody other than the requester approve bringing this digest in — is a
// different question, it is answered from `AdmittedBy` below, and it has its
// own rule id.
//
// # Where the evidence goes, and when it cannot
//
// Evidence is attached to the artifact's own digest in the artifact's own
// repository, with the credential the image is pulled with — the same rule
// build_attest.go follows and for the same reason: evidence the platform
// could attach where it cannot pull would be sitting next to somebody else's
// artifact. A vendor's public registry does not accept writes from its
// readers, so for an image pulled from one there is nothing to attach to and
// the artifact carries none. That is recorded on `status.artifact.message`
// and it is the documented consequence model, not a silent gap: an
// unattested artifact still deploys, and what it cannot do is satisfy a
// policy that requires evidence.
//
// The facts on `status.artifact.upstream` are recorded either way, because
// they cost no registry write. They are what the outsourcing inventory reads
// and what the two vendored rules are evaluated against, so a public image
// nobody can attach anything to is still an image the four-eyes rule and the
// signature rule have an answer about.

// vendoredSubject is one image of a unit that somebody else built: which
// workload it belongs to, what the project declared, and what that resolved
// to.
type vendoredSubject struct {
	// Workload is the workload whose image this is, empty for the web
	// process's — the project's own `spec.source.image`.
	Workload string

	// Source is the declaration: repository, tag or digest, the Connection
	// it is pulled with, and whose signature is expected.
	Source kitchenv1alpha1.ImageSourceSpec

	// Image is what the reference resolved to, as `repository@sha256:…`.
	Image string

	// Reference is what was actually followed, where that is not what the
	// project declares. A Build created to take a digest a poll already
	// resolved names its own reference (#368), and the adoption record has
	// to say which one this digest arrived under rather than re-reading a
	// declaration that may have moved since. Empty falls back to Source.
	Reference string
}

// reference is what this adoption followed.
func (s vendoredSubject) reference() string {
	if s.Reference != "" {
		return s.Reference
	}
	return s.Source.Reference()
}

// attestAcquired records what the platform knows about an artifact it only
// pulled, and attaches what it can.
//
// Like attestBuild it answers a non-nil status wherever it can identify the
// artifact at all: "which digest is this project running" is worth recording
// whether or not any evidence went anywhere.
func (r *BuildReconciler) attestAcquired(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	subject vendoredSubject,
) *kitchenv1alpha1.ArtifactStatus {
	log := logf.FromContext(ctx)
	repository, digest, byDigest := strings.Cut(subject.Image, "@")

	status := &kitchenv1alpha1.ArtifactStatus{
		Repository: repository,
		SourceType: kitchenv1alpha1.ArtifactSourceVendored,
		Upstream: &kitchenv1alpha1.UpstreamArtifactStatus{
			Reference:      subject.reference(),
			Repository:     repository,
			PullConnection: subject.Source.PullConnection(),
			AdmittedBy:     admittedBy(build, project),
			AdmittedAt:     ptr.To(metav1.Now()),
		},
	}
	if !byDigest {
		status.Message = "the image reference resolved to no digest, so there is nothing to attach evidence to"
		return status
	}
	status.Digest = digest

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		status.Message = "the platform configuration could not be read: " + err.Error()
		return status
	}
	if !kitchen.Spec.Compliance.Attestation.Enabled {
		// Off by choice, exactly as for a built artifact: the platform's own
		// compliance status says so once, and repeating it here would read
		// as a fault.
		return status
	}

	attester, err := r.vendorAttester(ctx, build, subject)
	if err != nil {
		status.Message = err.Error()
		return status
	}
	artifact := attestation.ArtifactRef(repository, digest)

	// The signature fact first, because it is the one thing recorded whether
	// or not anything can be written back to the vendor's registry — and
	// because the adoption record carries it.
	status.Upstream.Signature = r.upstreamSignature(ctx, build, attester, subject, artifact)

	signer, err := SigningKeyFor(ctx, r.Client, kitchen)
	if err != nil {
		status.Message = "the signing key could not be read: " + err.Error()
		return status
	}
	if signer == nil {
		return status
	}
	status.KeyID = signer.KeyID()

	// What the vendor published, restated about the digest the platform
	// deploys and countersigned. A vendor's own DSSE signature is not checked
	// here — the platform holds no key for it, and the signature question is
	// answered above rather than pretended at here.
	vendor, err := attester.VendorStatements(ctx, artifact, signer.KeyID())
	if err != nil {
		status.Message = "the vendor's attestations could not be read: " + err.Error()
		log.Info("vendor attestations unreadable",
			"build", build.Name, "image", artifact, "cause", err.Error())
	}
	if vendor.Discarded > 0 {
		log.Info("vendor attestations describing another artifact were discarded",
			"build", build.Name, "image", artifact, "count", vendor.Discarded)
	}
	for _, published := range vendor.Statements {
		restated, err := attestation.Restate(repository, digest, published)
		if err != nil {
			status.Message = err.Error()
			log.Info("a vendor attestation could not be restated",
				"build", build.Name, "predicateType", published.PredicateType, "cause", err.Error())
			continue
		}
		if err := r.sign(ctx, attester, artifact, restated, signer, status, sourceVendorAsserted); err != nil {
			status.Message = "a vendor attestation could not be attached: " + err.Error()
			log.Info("a vendor attestation was not attached",
				"build", build.Name, "predicateType", published.PredicateType, "cause", err.Error())
			continue
		}
		status.Upstream.VendorAttestations++
	}

	// Then the platform's own account of the adoption, last, so that its
	// `evidence` list names what was actually attached above.
	statement, err := attestation.NewStatement(
		repository, digest, attestation.PredicateArtifactAdoption,
		adoptionRecord(build, project, subject, status))
	if err != nil {
		status.Message = err.Error()
		return status
	}
	if err := r.sign(ctx, attester, artifact, statement, signer, status, sourcePlatformObserved); err != nil {
		status.Message = "the adoption record could not be attached to the artifact: " + err.Error()
		log.Info("vendored artifact left unattested",
			"build", build.Name, "image", artifact, "cause", err.Error())
		return status
	}
	status.AttestedAt = ptr.To(metav1.Now())
	return status
}

// Who made the claim an attestation about a vendored artifact carries. They
// stand beside sourcePlatform and sourceBuilder in build_attest.go and are
// deliberately four words rather than two: the platform's signature is on all
// of them, and "the vendor asserted this" is a different thing from "the
// platform observed this" in exactly the way "the builder said this" is
// different from "the reconciler said this".
const (
	sourceVendorAsserted   = "vendor-asserted"
	sourcePlatformObserved = "platform-observed"
)

// claimantFor is which of those two words the *platform's* own claim about an
// artifact carries: `platform` for an artifact it built, where the reconciler
// is speaking about a build it orchestrated, and `platform-observed` for one
// it only pulled, where it is speaking about an image nobody here compiled.
//
// Everything the platform attaches after the artifact exists goes through it —
// a quality gate's findings, a rescan's — so that a vendored artifact's
// evidence is entirely `vendor-asserted` and `platform-observed`, which is
// what COMPLIANCE.md §18.2's table says it is. It changes nothing about a
// built artifact, whose evidence is `platform` and `builder` exactly as it
// has always been.
func claimantFor(artifact *kitchenv1alpha1.ArtifactStatus) string {
	if artifact != nil && artifact.SourceType == kitchenv1alpha1.ArtifactSourceVendored {
		return sourcePlatformObserved
	}
	return sourcePlatform
}

// upstreamSignature is what became of the vendor's own signature on the
// digest.
//
// It never fails the acquisition. A registry that will not list what is
// attached to a digest is a fact about the registry, and it is recorded as an
// unverifiable signature rather than as a refusal to deploy — the consequence
// of unverifiable evidence belongs at promotion, where the environment's own
// bar is.
func (r *BuildReconciler) upstreamSignature(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	attester ArtifactAttester,
	subject vendoredSubject,
	artifact string,
) kitchenv1alpha1.UpstreamSignatureStatus {
	requirement, err := r.signatureRequirement(ctx, build, subject)
	if err != nil {
		return kitchenv1alpha1.UpstreamSignatureStatus{
			Result:  kitchenv1alpha1.UpstreamSignatureUnverifiable,
			Message: "the configured verification key could not be read: " + err.Error(),
		}
	}

	signatures, err := attester.Signatures(ctx, artifact)
	if err != nil {
		return kitchenv1alpha1.UpstreamSignatureStatus{
			Result:   kitchenv1alpha1.UpstreamSignatureUnverifiable,
			Identity: requirement.Identity,
			Message:  "the registry could not be asked what is signed against this digest: " + err.Error(),
		}
	}

	_, digest, _ := strings.Cut(artifact, "@")
	fact := attestation.VerifyUpstream(digest, requirement, signatures)
	return kitchenv1alpha1.UpstreamSignatureStatus{
		Result:     kitchenv1alpha1.UpstreamSignatureResult(fact.Result),
		Identity:   fact.Identity,
		Signatures: int32(fact.Signatures), //nolint:gosec // a count of manifest layers
		Message:    fact.Message,
	}
}

// signatureRequirement is what this installation asked of the vendor's
// signature: the image's own declaration where it makes one, and the pulling
// Connection's otherwise.
//
// The image wins over the Connection whole rather than field by field. A
// half-merged requirement — this image's identity checked against that
// registry's key — is a rule nobody wrote, and the one thing an identity
// check must not do is surprise whoever configured it.
func (r *BuildReconciler) signatureRequirement(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	subject vendoredSubject,
) (attestation.SignatureRequirement, error) {
	identity, issuer, secretName := "", "", ""
	switch {
	case subject.Source.Signature != nil:
		identity = subject.Source.Signature.Identity
		issuer = subject.Source.Signature.Issuer
		if ref := subject.Source.Signature.PublicKeyRef; ref != nil {
			secretName = ref.Name
		}
	case subject.Source.PullConnection() != "":
		connection := &kitchenv1alpha1.Connection{}
		key := types.NamespacedName{Namespace: build.Namespace, Name: subject.Source.PullConnection()}
		if err := r.Get(ctx, key, connection); err != nil {
			return attestation.SignatureRequirement{}, err
		}
		declared := provider.RegistrySignature(connection)
		identity, issuer, secretName = declared.Identity, declared.Issuer, declared.PublicKeySecret
	}

	requirement := attestation.SignatureRequirement{Identity: identity, Issuer: issuer}
	if secretName == "" {
		return requirement, nil
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: build.Namespace, Name: secretName}
	if err := r.Get(ctx, key, secret); err != nil {
		return attestation.SignatureRequirement{}, fmt.Errorf(
			"the verification key secret %q could not be read: %w", secretName, err)
	}
	requirement.PublicKeyPEM = secret.Data[attestation.SecretKeyPublic]
	if len(requirement.PublicKeyPEM) == 0 {
		return attestation.SignatureRequirement{}, fmt.Errorf(
			"the secret %q holds no %s", secretName, attestation.SecretKeyPublic)
	}
	return requirement, nil
}

// vendorAttester talks to the registry the image was pulled from, with the
// credential it was pulled with — anonymous where there is none, which is
// what a public image wants.
func (r *BuildReconciler) vendorAttester(
	ctx context.Context, build *kitchenv1alpha1.Build, subject vendoredSubject,
) (ArtifactAttester, error) {
	dockerConfig := []byte{}
	if connection := subject.Source.PullConnection(); connection != "" {
		conn := &kitchenv1alpha1.Connection{}
		key := types.NamespacedName{Namespace: build.Namespace, Name: connection}
		if err := r.Get(ctx, key, conn); err != nil {
			return nil, fmt.Errorf("the pull connection %q could not be read: %w", connection, err)
		}
		secret := &corev1.Secret{}
		key = types.NamespacedName{Namespace: build.Namespace, Name: conn.Spec.CredentialsSecretRef.Name}
		if err := r.Get(ctx, key, secret); err != nil {
			return nil, fmt.Errorf("the credentials of pull connection %q could not be read: %w", connection, err)
		}
		dockerConfig = secret.Data[corev1.DockerConfigJsonKey]
	}
	factory := r.Attesters
	if factory == nil {
		factory = defaultAttester
	}
	return factory(dockerConfig, registryServerOf(subject.Source.Repository))
}

// admittedBy is who brought this digest onto the platform.
//
// The REST API stamps the authenticated caller onto everything it creates, so
// an acquisition somebody asked for names them. One the platform decided on
// its own — the first acquisition of a project that was just created — names
// the project's creator, because that is the person who pointed this
// installation at that vendor. Only where neither is recorded does it name
// the reconciler, in Kubernetes' own spelling for a non-human identity.
//
// It is never blank. `digest-approved-by-someone-else` is written against
// this field, and a four-eyes rule whose first eye is nobody is a rule that
// cannot be answered.
func admittedBy(build *kitchenv1alpha1.Build, project *kitchenv1alpha1.Project) string {
	if who := strings.TrimSpace(build.Annotations[audit.RequestedByAnnotation]); who != "" {
		return who
	}
	if project != nil {
		if who := strings.TrimSpace(project.Annotations[audit.RequestedByAnnotation]); who != "" {
			return who
		}
	}
	return audit.ControllerActor(actorBuildController)
}

// adoptionRecord is the predicate: this installation's account of taking one
// artifact from somebody else.
//
// It says nothing it cannot stand behind. The upstream reference and the
// digest are what the platform resolved; the admitting identity is what the
// request carried; the signature result is the one fact in it that took a
// cryptographic check, and it carries its own verdict vocabulary rather than
// a boolean, so "the vendor publishes no signature" reads as the ordinary
// state of the world it is.
//
// It also names which of the artifact's evidence is whose. That is what makes
// a vendor's assertion and the platform's own observation distinguishable by
// a signed predicate rather than by a convention a reader has to know: a bill
// of materials the platform generated carries SPDX's predicate type like any
// other — it must, or nothing downstream would read it — and this says whose
// bill of materials it is.
func adoptionRecord(
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	subject vendoredSubject,
	status *kitchenv1alpha1.ArtifactStatus,
) map[string]any {
	upstream := map[string]any{
		"reference":  subject.reference(),
		"repository": status.Repository,
		"digest":     status.Digest,
	}
	if connection := subject.Source.PullConnection(); connection != "" {
		upstream["pullConnection"] = connection
	}

	signature := map[string]any{"result": string(status.Upstream.Signature.Result)}
	if identity := status.Upstream.Signature.Identity; identity != "" {
		signature["identity"] = identity
	}
	if message := status.Upstream.Signature.Message; message != "" {
		signature["message"] = message
	}
	signature["signatures"] = status.Upstream.Signature.Signatures

	record := map[string]any{
		"project":    project.Name,
		"build":      build.Name,
		"upstream":   upstream,
		"admittedBy": status.Upstream.AdmittedBy,
		"signature":  signature,
		"evidence":   evidenceByClaimant(status),
		"platform": map[string]any{
			"platform": "kitchen",
			"version":  version.Version,
		},
	}
	if subject.Workload != "" {
		record["workload"] = subject.Workload
	}
	if status.Upstream.AdmittedAt != nil {
		record["admittedAt"] = status.Upstream.AdmittedAt.UTC().Format(time.RFC3339)
	}
	return record
}

// evidenceByClaimant splits what is attached to the artifact into whose claim
// each piece is, by predicate type — the vendor's, and the platform's own
// observations.
func evidenceByClaimant(status *kitchenv1alpha1.ArtifactStatus) map[string]any {
	asserted, observed := []string{}, []string{}
	for _, entry := range status.Evidence {
		switch entry.Source {
		case sourceVendorAsserted:
			asserted = append(asserted, entry.PredicateType)
		case sourcePlatformObserved:
			observed = append(observed, entry.PredicateType)
		}
	}
	return map[string]any{"vendorAsserted": asserted, "platformObserved": observed}
}

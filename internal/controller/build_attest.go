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
	"github.com/Bermos/Kitchen/internal/version"
)

// The evidence an artifact leaves the build with: what the reconciler knows
// about the build it asked for, and what the builder knows about the build it
// ran, both signed and attached to the artifact's digest.
//
// They are two different claims and they stay two attestations. Kitchen's
// build record is the reconciler's account of a build it *orchestrated* — the
// commit it was asked to build, the strategy it chose, the times it observed —
// and it carries a Kitchen predicate type because no standard covers it. SLSA
// provenance is the account of the process that did the work, and only the
// builder can make it: which base images it actually resolved and to what
// digests, which source it actually fetched, what it was invoked with.
// Conflating the two would be the kind of evidence that is worse than none.
//
// Failing to attest does not fail the build. The image exists and the
// deployment that follows is honest about what it is running; what an
// unattested artifact cannot do is satisfy a policy that requires evidence,
// which is where the consequence belongs. A build that failed here says so on
// `status.artifact.message`, and the platform's compliance status says whether
// signing works at all.

// ArtifactAttester attaches one signed statement to an artifact's digest. It
// is an interface so that the reconciler can be tested against an in-process
// registry rather than a real one.
type ArtifactAttester interface {
	Attach(ctx context.Context, imageRef string, envelope attestation.Envelope, predicateType string) (string, error)
	// Harvest reads back what the builder left attached to the push, and
	// says which digest inside it the artifact actually is.
	Harvest(ctx context.Context, ref string) (attestation.BuilderEvidence, error)
	// Blob reads back bytes a gate pod stored in the artifact's repository,
	// which is how findings too large for a pod's termination message reach
	// the operator that signs them.
	Blob(ctx context.Context, repository, digest string) ([]byte, error)
}

// AttesterFactory builds the attester for one registry, out of the docker
// config that registry is pushed to with.
type AttesterFactory func(dockerConfig []byte, server string) (ArtifactAttester, error)

// defaultAttester talks to the real registry with the credential the build
// pushed under. Reusing the build's own credential is deliberate: an
// attestation the platform could attach where the build could not push would
// be evidence sitting next to somebody else's artifact.
func defaultAttester(dockerConfig []byte, server string) (ArtifactAttester, error) {
	auth, err := attestation.AuthFromDockerConfig(dockerConfig, server)
	if err != nil {
		return nil, err
	}
	return &attestation.Store{Auth: auth}, nil
}

// artifactSubject is one image of a unit as the attestation path sees it:
// which workload it belongs to, how it was built, and what was pushed.
//
// A commit produces one image per workload that declares a build of its own
// (#271), and every one of them is deployed by the Release. So every one of
// them is attested, each about its own digest — the web process's exactly as
// it always was, which is what keeps a single-workload project's evidence
// byte-for-byte what it was before this existed.
type artifactSubject struct {
	// Workload is the workload whose image this is, empty for the web
	// process's. It reaches the build record only when it is set, so the
	// predicate a single-image project signs is unchanged.
	Workload string

	// Strategy is how *this* image was built. A unit's workloads do not
	// share one: a monorepo whose API is a Dockerfile and whose worker is
	// buildpacks has two answers, and a record that repeated the project's
	// own for both would be a claim about the wrong build.
	Strategy kitchenv1alpha1.BuildStrategy

	// Image is what the builder reported it pushed, by digest.
	Image string
}

// attestBuild signs Kitchen's build record and attaches it to the image
// digest, returning what to publish on the Build's status.
//
// It answers a non-nil status in every case it can identify the artifact at
// all, because "which image did this build produce" is worth recording whether
// or not the evidence went anywhere.
func (r *BuildReconciler) attestBuild(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	target buildTarget,
	subject artifactSubject,
) *kitchenv1alpha1.ArtifactStatus {
	image := subject.Image
	repository, digest, byDigest := strings.Cut(image, "@")
	status := &kitchenv1alpha1.ArtifactStatus{
		Repository: repository,
		// Everything here is an artifact this platform built. The field says
		// so rather than leaving a reader to infer it from the absence of
		// anything else — see ArtifactStatus.SourceType.
		SourceType: kitchenv1alpha1.ArtifactSourceBuilt,
	}
	if !byDigest {
		status.Message = "the builder pushed an image but reported no digest, so there is nothing to attach evidence to"
		return status
	}
	status.Digest = digest

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		status.Message = "the platform configuration could not be read: " + err.Error()
		return status
	}
	if !kitchen.Spec.Compliance.Attestation.Enabled {
		// Off by choice. Not a message: the platform's own compliance status
		// says so once, and repeating it on every build would read as a
		// fault.
		return status
	}

	attester, err := r.attester(ctx, target)
	if err != nil {
		status.Message = err.Error()
		return status
	}

	// What the builder reported may be an index rather than an image: asked
	// for provenance or an SBOM, BuildKit pushes both beside the image and
	// reports the index that holds them. Resolving it is not bookkeeping —
	// the builder's statements are about the *image* manifest, so the image
	// manifest is what the platform must call the artifact, or the evidence
	// would describe something the Release does not deploy.
	builder, err := attester.Harvest(ctx, image)
	if err != nil {
		// Not fatal. The push happened, the artifact exists, and the digest
		// the builder reported still identifies it; what is lost is the
		// builder's own evidence, which is exactly what this field is for.
		status.Message = "the builder's attestations could not be read: " + err.Error()
		logf.FromContext(ctx).Info("builder attestations unreadable",
			"build", build.Name, "image", image, "cause", err.Error())
	} else {
		status.Digest, digest = builder.ImageDigest, builder.ImageDigest
		if builder.Discarded > 0 {
			logf.FromContext(ctx).Info("builder attestations describing another artifact were discarded",
				"build", build.Name, "image", image, "count", builder.Discarded)
		}
	}
	artifact := attestation.ArtifactRef(repository, digest)

	signer, err := SigningKeyFor(ctx, r.Client, kitchen)
	if err != nil {
		status.Message = "the signing key could not be read: " + err.Error()
		return status
	}
	if signer == nil {
		return status
	}
	status.KeyID = signer.KeyID()

	// The platform's own claim first, so that an artifact whose builder said
	// nothing still carries an account of where it came from.
	statement, err := attestation.NewStatement(
		repository, digest, attestation.PredicateBuildRecord, buildRecord(build, project, target, subject))
	if err != nil {
		status.Message = err.Error()
		return status
	}
	if err := r.sign(ctx, attester, artifact, statement, signer, status, sourcePlatform); err != nil {
		status.Message = "the attestation could not be attached to the artifact: " + err.Error()
		logf.FromContext(ctx).Info("artifact left unattested",
			"build", build.Name, "image", artifact, "cause", err.Error())
		return status
	}
	status.AttestedAt = ptr.To(metav1.Now())

	// Then how the change was reviewed, out of what was recorded before the
	// build rather than by asking the provider again: an approval can be
	// dismissed between a build starting and finishing, and the evidence has
	// to say what was true when the change was built.
	r.attestSource(ctx, build, project, attester, signer, repository, digest, status)

	// Then the builder's, countersigned. Each is restated about the digest
	// the platform calls the artifact and signed under the platform's key —
	// the statements arrive unsigned, and an unsigned statement in a registry
	// is a claim anything with push access could have written.
	//
	// A failure here does not undo the build record: an artifact with
	// provenance and no SBOM is a worse artifact than one with both, and a
	// far better one than an artifact with neither.
	for _, made := range builder.Statements {
		restated, err := attestation.Restate(repository, digest, made)
		if err != nil {
			status.Message = err.Error()
			logf.FromContext(ctx).Info("a builder attestation could not be restated",
				"build", build.Name, "predicateType", made.PredicateType, "cause", err.Error())
			continue
		}
		if err := r.sign(ctx, attester, artifact, restated, signer, status, sourceBuilder); err != nil {
			status.Message = "a builder attestation could not be attached: " + err.Error()
			logf.FromContext(ctx).Info("a builder attestation was not attached",
				"build", build.Name, "predicateType", made.PredicateType, "cause", err.Error())
		}
	}
	return status
}

// Who made the claim an attestation carries. The platform's signature is on
// both, so the signature cannot tell them apart — and the difference matters,
// because a claim about what a build did is worth more when it comes from the
// process that did the building.
const (
	sourcePlatform = "platform"
	sourceBuilder  = "builder"
)

// sign signs one statement, attaches it, and records it on the status.
//
// The three steps are one function because they only make sense together: a
// statement signed and not attached is evidence nobody can find, and an
// evidence entry for something that was not attached is a status that lies.
func (r *BuildReconciler) sign(
	ctx context.Context,
	attester ArtifactAttester,
	artifact string,
	statement attestation.Statement,
	signer attestation.Signer,
	status *kitchenv1alpha1.ArtifactStatus,
	source string,
) error {
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		return err
	}
	manifest, err := attester.Attach(ctx, artifact, envelope, statement.PredicateType)
	if err != nil {
		return err
	}
	status.Evidence = append(status.Evidence, kitchenv1alpha1.ArtifactEvidence{
		PredicateType: statement.PredicateType,
		Manifest:      manifest,
		Source:        source,
	})
	return nil
}

// attester resolves how to talk to the registry the build pushed to.
func (r *BuildReconciler) attester(ctx context.Context, target buildTarget) (ArtifactAttester, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{
		Namespace: target.Connection.Namespace,
		Name:      target.Connection.Spec.CredentialsSecretRef.Name,
	}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("the registry credential could not be read: %w", err)
	}
	factory := r.Attesters
	if factory == nil {
		factory = defaultAttester
	}
	return factory(secret.Data[corev1.DockerConfigJsonKey], target.Registry.Server)
}

// buildRecord is the predicate: what Kitchen knows about how the artifact came
// to be.
//
// It says nothing it cannot stand behind. The commit and the project are what
// the reconciler was asked to build; the strategy and framework are what it
// decided; the times are the Job's. There is no claim here about what the
// builder did with any of it — that is provenance, and it has to come from the
// builder.
//
// Two keys are the *subject's* rather than the unit's, and both are written
// only for a workload's own image: `workload`, naming which image of the unit
// this record is about, and `strategy` and `framework`, which a unit does not
// have one of. The web process's record is therefore character for character
// what it was before a commit could produce more than one image — which is
// the point, since re-signing every artifact already shipped is exactly what
// this change is not.
func buildRecord(
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	target buildTarget,
	subject artifactSubject,
) map[string]any {
	strategy := target.Strategy
	if subject.Workload != "" && subject.Strategy != "" {
		strategy = subject.Strategy
	}
	record := map[string]any{
		"project": project.Name,
		"build":   build.Name,
		"source": map[string]any{
			"repository": project.Spec.Source.GitSource().Repo,
			"commit":     build.Spec.Git.SHA,
			"branch":     build.Spec.Git.Branch,
		},
		"strategy": string(strategy),
		"builder": map[string]any{
			"platform": "kitchen",
			"version":  version.Version,
		},
	}
	if subject.Workload != "" {
		record["workload"] = subject.Workload
	}
	if framework := subjectFramework(build, subject); framework != "" {
		record["framework"] = framework
	}
	if pullRequest := build.PullRequestNumber(); pullRequest != nil {
		record["pullRequest"] = *pullRequest
	}
	if build.Status.StartedAt != nil {
		record["startedAt"] = build.Status.StartedAt.UTC().Format(time.RFC3339)
	}
	// The completion time is stamped by the caller before this runs, so a
	// record that has one has the Job's own and not "roughly now".
	if build.Status.CompletedAt != nil {
		record["finishedAt"] = build.Status.CompletedAt.UTC().Format(time.RFC3339)
	}
	return record
}

// subjectFramework is what detection made of the directory *this* image was
// built from: the workload's own answer for a workload, and the Build's for
// the web process.
//
// A unit is several directories, so `status.detectedFramework` is the
// project's own image and nothing else's — a record that repeated it for a
// Python worker beside a Next.js front end would be asserting something the
// platform never detected.
func subjectFramework(build *kitchenv1alpha1.Build, subject artifactSubject) string {
	if subject.Workload == "" {
		return build.Status.DetectedFramework
	}
	for i := range build.Status.Workloads {
		if build.Status.Workloads[i].Name == subject.Workload {
			return build.Status.Workloads[i].DetectedFramework
		}
	}
	return ""
}

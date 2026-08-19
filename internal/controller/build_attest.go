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

// The first piece of evidence any artifact gets: Kitchen's own account of how
// it was built, signed and attached to the image's digest.
//
// It is not SLSA provenance and does not claim to be. Provenance is a
// statement by the thing that did the building, about a build it can vouch for
// from the inside; this is the reconciler's statement about a build it
// orchestrated, which is a weaker and different claim. Conflating the two
// would be the kind of evidence that is worse than none — so this carries a
// Kitchen predicate type, and issue #128's provenance will carry SLSA's.
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
	image string,
) *kitchenv1alpha1.ArtifactStatus {
	repository, digest, byDigest := strings.Cut(image, "@")
	status := &kitchenv1alpha1.ArtifactStatus{Repository: repository}
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

	signer, err := SigningKeyFor(ctx, r.Client, kitchen)
	if err != nil {
		status.Message = "the signing key could not be read: " + err.Error()
		return status
	}
	if signer == nil {
		return status
	}
	status.KeyID = signer.KeyID()

	statement, err := attestation.NewStatement(
		repository, digest, attestation.PredicateBuildRecord, buildRecord(build, project, target))
	if err != nil {
		status.Message = err.Error()
		return status
	}
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		status.Message = err.Error()
		return status
	}

	attester, err := r.attester(ctx, target)
	if err != nil {
		status.Message = err.Error()
		return status
	}
	if _, err := attester.Attach(ctx, image, envelope, attestation.PredicateBuildRecord); err != nil {
		status.Message = "the attestation could not be attached to the artifact: " + err.Error()
		logf.FromContext(ctx).Info("artifact left unattested",
			"build", build.Name, "image", image, "cause", err.Error())
		return status
	}

	status.AttestedAt = ptr.To(metav1.Now())
	return status
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
func buildRecord(
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	target buildTarget,
) map[string]any {
	record := map[string]any{
		"project": project.Name,
		"build":   build.Name,
		"source": map[string]any{
			"repository": project.Spec.Source.Repo,
			"commit":     build.Spec.Git.SHA,
			"branch":     build.Spec.Git.Branch,
		},
		"strategy": string(target.Strategy),
		"builder": map[string]any{
			"platform": "kitchen",
			"version":  version.Version,
		},
	}
	if framework := build.Status.DetectedFramework; framework != "" {
		record["framework"] = framework
	}
	if build.Spec.Git.PullRequest != nil {
		record["pullRequest"] = *build.Spec.Git.PullRequest
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

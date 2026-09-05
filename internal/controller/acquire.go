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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/repoconfig"
)

// Acquiring an artifact the platform did not build (#307).
//
// A Build is still the object, and that is the decision #306 recorded rather
// than an economy: `status.artifact`, the evidence index, the quality gates,
// VEX, the audit chain, the build screens and the CLI all key on a Build, and
// making `Release.BuildRef` optional would orphan every one of them at once.
// What a vendored project's Build does not have is a builder Job — there is
// nothing to build — so this is the whole of the path: resolve what each
// workload of the unit is pointed at, freeze the digests onto a Release, and
// hand it to the same promotion the build path hands one to.
//
// What makes an acquisition happen *again* is the digest poll in
// imagepoll.go (#308): a manifest HEAD per watched reference per interval,
// and a Build pinned to the digest it found where a tag has moved. This is
// still the whole of what such a Build does — the poll decides that one
// should exist, and nothing more. #309 is what attaches evidence to an
// artifact nobody here built, and extends this in its turn.
//
// Two things it does *not* do, both on purpose:
//
//   - **Nothing fakes a commit.** The Build names no SHA and no branch, so
//     the three commit-shaped rules of the default bundle stay inert rather
//     than being satisfied by a substitute (#306, #309).
//   - **Nothing is built, so nothing is claimed about a build.** What an
//     acquired artifact does carry, since #309, is what the vendor published
//     about the digest, what the platform observed of an image it only
//     pulled, and the record of the adoption itself — assembled in
//     vendored_attest.go and indexed on `status.artifact` beside a built
//     artifact's evidence, under source types that keep the three apart.

// ImageResolver answers what digest an image reference names.
//
// It is an interface for the reason ArtifactAttester is: the reconciler is
// tested against an in-process registry, or against no registry at all, and a
// resolver reaching the real internet in a test would be a test about the
// internet.
type ImageResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// ImageResolverFactory builds the resolver for one registry, out of the
// docker config that registry is read with. An empty config is an anonymous
// pull, which is what a public image wants.
type ImageResolverFactory func(dockerConfig []byte, registry string) (ImageResolver, error)

// defaultImageResolver asks the real registry, with the credential the pods
// will pull with. Reusing the pull credential is the point: a digest the
// platform could read where the kubelet cannot would be a Release naming an
// image the environment can never start.
func defaultImageResolver(dockerConfig []byte, registry string) (ImageResolver, error) {
	if len(dockerConfig) == 0 {
		return &attestation.Store{}, nil
	}
	auth, err := attestation.AuthFromDockerConfig(dockerConfig, registry)
	if err != nil {
		return nil, err
	}
	return &attestation.Store{Auth: auth}, nil
}

// reasonImageUnresolved is an acquisition that could not learn which digest a
// vendored reference names. It is the acquisition's equivalent of a build that
// ran and produced no image: nothing was acquired, and the Release that would
// have frozen a digest has nothing to freeze.
const reasonImageUnresolved = "ImageUnresolved"

// reasonSourceMismatch is a Build whose shape does not match its project's
// source: a commit for a project with no repository, or no commit for one that
// has one. Neither can be built, and both are worth saying rather than
// failing further down in the vocabulary of whatever noticed first.
const reasonSourceMismatch = "SourceMismatch"

// AcquisitionNameFor is the Build an acquisition of one image reference runs
// as. It is deterministic in the same way and for the same reason
// BuildNameFor is: two things wanting the same artifact acquired must be one
// Build, not two.
func AcquisitionNameFor(project, reference string) string {
	sum := sha256.Sum256([]byte(reference))
	return fmt.Sprintf("%s-acq-%s", project, hex.EncodeToString(sum[:6]))
}

// acquisitionReleaseName is the Release an acquisition produces, named after
// what it resolved.
//
// A build's Release is named after the commit, because the commit is what
// makes two builds of it one release. An acquisition has no commit and the
// same property to preserve, so it is named after the digest — which means
// acquiring the same artifact twice converges on one Release rather than
// piling up copies of it.
func acquisitionReleaseName(projectName, imageRef string) string {
	digest := imageRef
	if _, after, found := strings.Cut(imageRef, "@sha256:"); found {
		digest = after
	}
	return releaseName(projectName, digest)
}

// acquire is the whole of a Build for a project whose source is an image.
func (r *BuildReconciler) acquire(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}

	// What the project declares, and what this Build was created to take.
	// The two differ exactly when something had already asked the registry
	// before this Build existed: the poll resolves a moved tag and pins the
	// digest that moved it, and an operator may name one outright. Resolving
	// the tag again here would be a different question with a possibly
	// different answer, which is the one thing a pin exists to prevent.
	declared := project.Spec.Source.ImageSource()
	wanted := declared
	if build.Spec.Acquire != nil && build.Spec.Acquire.Digest != "" {
		wanted.Digest = build.Spec.Acquire.Digest
	}
	reference := declared.Reference()
	if named := build.AcquisitionReference(); named != "" {
		reference = named
	}

	// What this project is running now, so that the acquisition can say what
	// it left as well as what it took. It is read before the resolution
	// because it is a fact about the world this Build is about to change.
	previous, err := latestAcquisition(ctx, r.Client, project, build.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	previousImage := ""
	if previous != nil {
		previousImage = previous.Status.Acquisition.Image
	}

	// What this Build is trying to get, recorded before it tries. A failure
	// leaves this behind — `fail` writes the status it was given — so an
	// acquisition that could not reach its registry still says which
	// reference it was following and what asked it to, which is the half of
	// the record that a failed one has.
	build.Status.Acquisition = &kitchenv1alpha1.AcquisitionStatus{
		Reference: reference,
		Previous:  previousImage,
		Trigger:   build.AcquisitionTriggeredBy(),
		Pinned:    declared.Digest != "",
	}

	// The web process first, then every workload that names an image of its
	// own — the same order the build path plans in, so that the web process's
	// answer is `spec.image` on the Release and the rest are rows beside it.
	web, err := r.acquireImage(ctx, build, appNS, wanted)
	if err != nil {
		return r.fail(ctx, build, project, reasonImageUnresolved,
			fmt.Sprintf("the web process's image could not be resolved: %v", err))
	}

	workloads, rows, err := r.vendoredWorkloads(ctx, build, project, appNS)
	if err != nil {
		return r.fail(ctx, build, project, reasonImageUnresolved, err.Error())
	}

	build.Status.CompletedAt = ptr.To(metav1.Now())
	if build.Status.StartedAt == nil {
		build.Status.StartedAt = build.Status.CompletedAt
	}
	build.Status.Workloads = rows
	build.Status.Acquisition.Image = web
	build.Status.Acquisition.ResolvedAt = build.Status.CompletedAt

	// The evidence, before the Release exists: the promotion that follows
	// evaluates the environment's bar over what is attached to the digest,
	// and an artifact attested after it was judged would have been judged as
	// carrying nothing (#309).
	build.Status.Artifact = r.attestAcquired(ctx, build, project, vendoredSubject{
		Source: declared,
		// The reference this Build actually followed, which is the pinned
		// one where a poll resolved the tag before the Build existed.
		Reference: reference,
		Image:     web,
	})

	if err := r.Audit.Record(ctx, audit.Transition{
		Object:      build,
		Kind:        audit.KindBuild,
		Controller:  actorBuildController,
		Correlation: correlationFor(build),
		From:        string(build.Status.Phase),
		To:          string(kitchenv1alpha1.BuildSucceeded),
		Project:     project.Name,
		Reason:      acquisitionReason(web, previousImage),
		Details: map[string]any{
			"image":     web,
			"previous":  previousImage,
			"declared":  reference,
			"trigger":   string(build.AcquisitionTriggeredBy()),
			"workloads": len(workloads),
		},
	}); err != nil {
		return ctrl.Result{}, err
	}

	// The project's settings, frozen exactly as a build freezes them. There
	// is no kitchen.json to merge — a project with no repository has no file
	// to carry one, which is #310 — so the snapshot is the project's own.
	snapshot, err := repoconfig.Snapshot(kitchenv1alpha1.ConfigSnapshot{
		Env:       project.Spec.Env,
		Runtime:   project.Spec.Runtime,
		Processes: project.Spec.Processes,
		Files:     project.Spec.Files,
	}, build.Status.Config)
	if err != nil {
		return r.fail(ctx, build, project, reasonConfigInvalid, err.Error())
	}

	// A posture that cannot be honoured against the images this unit actually
	// runs is refused here, at the one moment the platform holds both halves:
	// the images, resolved to digests and with their own `USER` read off
	// their configs, and the settings the Release is about to freeze (#393).
	// The question is asked per workload, against the posture that workload
	// runs under — the unit's with its own merged over it (#399) — because
	// the four images of one unit no longer share one.
	//
	// It is refused *now* rather than at the pod, because the pod's refusal is
	// invisible — a container status the kubelet writes and nothing else
	// carries — and because a Release nothing can deploy is not a release. It
	// is not refused at the API, either: `runAsNonRoot` without `runAsUser` is
	// exactly right for an image whose USER is a uid, and a 400 for that
	// project would refuse a request that would have worked.
	if unverifiable := unverifiableImages(snapshot, build.Artifacts()); len(unverifiable) > 0 {
		return r.fail(ctx, build, project, reasonImageUserUnverifiable,
			unverifiableImagesMessage(unverifiable))
	}

	release := &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{
			Name:      acquisitionReleaseName(project.Name, web),
			Namespace: build.Namespace,
			Labels:    map[string]string{labelProject: project.Name, labelManagedByKey: labelManagedByValue},
		},
		Spec: kitchenv1alpha1.ReleaseSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			BuildRef:       kitchenv1alpha1.LocalObjectReference{Name: build.Name},
			Image:          web,
			Workloads:      workloads,
			ConfigSnapshot: snapshot,
		},
	}
	if err := r.Audit.Record(ctx, audit.Transition{
		Object:      release,
		Kind:        audit.KindRelease,
		Operation:   clickhouse.AuditCreate,
		Controller:  actorBuildController,
		Correlation: correlationFor(build),
		To:          web,
		Project:     project.Name,
		Reason:      fmt.Sprintf("build %s acquired a release", build.Name),
		Details:     map[string]any{"build": build.Name, "image": web},
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, release); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}

	// Straight to the production target, or onto the first rung of the
	// pipeline where the project has one — the same two answers the build
	// path gives a production-branch commit. There is no third: a vendored
	// project has no preview to route to and no branch that is neither.
	envName := ProductionTargetEnvironmentName(project)
	if stages := project.Spec.Promotion; stages != nil && len(stages.Stages) > 0 {
		envName = stages.Stages[0].Environment
	}
	if err := r.promoteOrFlip(ctx, build.Namespace, project, envName, release.Name, build); err != nil {
		return ctrl.Result{}, err
	}

	build.Status.Phase = kitchenv1alpha1.BuildSucceeded
	build.Status.Image = web
	meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
		Type: condReady, Status: metav1.ConditionTrue, Reason: "ImageAcquired",
		Message:            fmt.Sprintf("image %s resolved; nothing was built", web),
		ObservedGeneration: build.Generation,
	})
	r.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventBuildSucceeded,
		Project: project.Name,
		Build:   build.Name,
		Release: release.Name,
		Message: fmt.Sprintf("build %s %s", build.Name, acquisitionReason(web, previousImage)),
	})
	log.Info("image acquired", "build", build.Name, "release", release.Name, "image", web)
	return ctrl.Result{}, r.Status().Update(ctx, build)
}

// acquireImage resolves one vendored reference to a digest, and puts the
// credential it was resolved with where the pods will need it.
//
// The two are one step because they are one credential: the digest is read
// with the account the kubelet will pull with, so an image the platform can
// resolve is an image the environment can start.
func (r *BuildReconciler) acquireImage(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	appNS string,
	image kitchenv1alpha1.ImageSourceSpec,
) (string, error) {
	conn, dockerConfig, err := pullCredential(ctx, r.Client, build.Namespace, image)
	if err != nil {
		return "", err
	}
	if conn != nil {
		// The pods pull with this, so it has to be in the application
		// namespace under the name pullcredentials.go spells.
		if _, err := r.syncRegistrySecret(ctx, conn, build.Namespace, appNS); err != nil {
			return "", fmt.Errorf("the pull credential could not be put in %s: %w", appNS, err)
		}
	}
	return resolveWith(ctx, r.Resolvers, dockerConfig, image)
}

// pullCredential is the Connection an image is read with and the docker
// config in it, both nil for the public image that needs neither.
//
// It is a free function because two things ask it: the acquisition, which
// also has to put the credential where the pods will find it, and the digest
// poll, which only ever reads. The poll deliberately does not sync anything —
// a pass that wrote a Secret into every application namespace every ten
// minutes would be a poll with side effects, and the acquisition it produces
// syncs the credential a moment later anyway.
func pullCredential(
	ctx context.Context,
	c client.Client,
	namespace string,
	image kitchenv1alpha1.ImageSourceSpec,
) (*kitchenv1alpha1.Connection, []byte, error) {
	if image.Repository == "" {
		return nil, nil, fmt.Errorf("no image is declared")
	}
	connection := image.PullConnection()
	if connection == "" {
		return nil, nil, nil
	}
	conn := &kitchenv1alpha1.Connection{}
	key := types.NamespacedName{Namespace: namespace, Name: connection}
	if err := c.Get(ctx, key, conn); err != nil {
		return nil, nil, fmt.Errorf("the pull connection %q could not be read: %w", connection, err)
	}
	secret := &corev1.Secret{}
	key = types.NamespacedName{Namespace: namespace, Name: conn.Spec.CredentialsSecretRef.Name}
	if err := c.Get(ctx, key, secret); err != nil {
		return nil, nil, fmt.Errorf("the credentials of pull connection %q could not be read: %w", connection, err)
	}
	return conn, secret.Data[corev1.DockerConfigJsonKey], nil
}

// resolveWith asks one registry what digest a reference names, through the
// factory a test replaces. A reference that already names a digest costs no
// request at all — the resolver answers it back — which is what makes a
// pinned project free to poll and free to acquire.
func resolveWith(
	ctx context.Context,
	factory ImageResolverFactory,
	dockerConfig []byte,
	image kitchenv1alpha1.ImageSourceSpec,
) (string, error) {
	if factory == nil {
		factory = defaultImageResolver
	}
	resolver, err := factory(dockerConfig, registryServerOf(image.Repository))
	if err != nil {
		return "", err
	}
	return resolver.Resolve(ctx, image.Reference())
}

// acquisitionReason is the sentence an acquisition is recorded under, in the
// audit log and in the activity feed alike.
//
// It names both digests when there are two, because that is the whole of what
// the entry is for: an environment that changed with no commit behind it is
// only explicable if the record says what it was running and what it is
// running now.
func acquisitionReason(image, previous string) string {
	if previous == "" || previous == image {
		return fmt.Sprintf("acquired %s; nothing was built", image)
	}
	return fmt.Sprintf("acquired %s, replacing %s; nothing was built", image, previous)
}

// latestAcquisition is the newest succeeded acquisition of a project: the
// Build whose resolution is what this project is running now.
//
// It is the comparison the poll makes — has the tag moved since — and the
// "previous" an acquisition records. Both read it off the Build rather than
// off a copy on the Project, so that there is one answer to "what did this
// platform last take from that registry" and the audit trail is where it
// lives.
func latestAcquisition(
	ctx context.Context,
	c client.Client,
	project *kitchenv1alpha1.Project,
	exclude string,
) (*kitchenv1alpha1.Build, error) {
	builds := &kitchenv1alpha1.BuildList{}
	if err := c.List(ctx, builds, client.InNamespace(project.Namespace),
		client.MatchingLabels{kitchenv1alpha1.ProjectLabel: project.Name}); err != nil {
		return nil, err
	}
	var newest *kitchenv1alpha1.Build
	for i := range builds.Items {
		build := &builds.Items[i]
		switch {
		case build.Name == exclude,
			build.Spec.ProjectRef.Name != project.Name,
			build.Status.Phase != kitchenv1alpha1.BuildSucceeded,
			build.Status.Acquisition == nil,
			build.Status.Acquisition.Image == "":
			continue
		}
		if newest == nil || acquiredBefore(newest, build) {
			newest = build
		}
	}
	return newest, nil
}

// acquiredBefore orders two acquisitions. Creation is the ordering that
// matters — a Build is created once and resolves immediately after — and the
// name breaks the tie, because a creation timestamp has one-second
// granularity and two acquisitions of one project can land inside it.
func acquiredBefore(a, b *kitchenv1alpha1.Build) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name < b.Name
	}
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}

// registryServerOf is the server a repository reference is on, which is the
// key a docker config holds its credential under. A reference whose first
// segment carries no dot, no colon and is not `localhost` is on Docker Hub,
// which is the one host in the world that is spelled by omission.
func registryServerOf(repository string) string {
	first, _, found := strings.Cut(repository, "/")
	if !found || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return "index.docker.io"
	}
	return first
}

// vendoredWorkloads is the digest each vendored workload of a unit resolved
// to, and the row each of them writes onto the Build.
//
// A mixed unit is the case ProcessSpec.Image exists for: an upstream image as
// one workload and a sidecar built from the repository as another, in one
// Release. The built half goes through the Jobs and the digests they reported;
// this is the other half, and both end up in the same `spec.workloads` — so a
// rollback restores the exact set either way, and the vendored digests come
// back exactly as the built ones do.
//
// The rows are recorded as succeeded because nothing ran: the image existed
// before this Build did, and a workload of the unit missing from the build
// screens would be a unit reported as three quarters of itself.
func (r *BuildReconciler) vendoredWorkloads(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	appNS string,
) ([]kitchenv1alpha1.WorkloadImage, []kitchenv1alpha1.WorkloadBuildStatus, error) {
	declared := buildWorkloads(project, build)
	images := make([]kitchenv1alpha1.WorkloadImage, 0, len(declared))
	rows := make([]kitchenv1alpha1.WorkloadBuildStatus, 0, len(declared))
	for _, workload := range declared {
		if workload.Image == nil {
			continue
		}
		image, err := r.acquireImage(ctx, build, appNS, *workload.Image)
		if err != nil {
			return nil, nil, fmt.Errorf("workload %s: its image could not be resolved: %w", workload.Name, err)
		}
		images = append(images, kitchenv1alpha1.WorkloadImage{Name: workload.Name, Image: image})
		rows = append(rows, kitchenv1alpha1.WorkloadBuildStatus{
			Name:       workload.Name,
			Repository: workload.Image.Repository,
			Reference:  workload.Image.Reference(),
			Phase:      kitchenv1alpha1.BuildSucceeded,
			Image:      image,
			// Each vendored workload is an artifact in its own right and
			// carries its own evidence against its own digest (#300): a unit
			// whose sidecar is a vendor's image and whose web process is
			// another vendor's has two adoptions, two upstreams and two
			// signature facts, and one record for both would describe
			// neither.
			Artifact: r.attestAcquired(ctx, build, project, vendoredSubject{
				Workload: workload.Name,
				Source:   *workload.Image,
				Image:    image,
			}),
		})
	}
	return images, rows, nil
}

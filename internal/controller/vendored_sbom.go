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
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
)

// A bill of materials for an image the platform did not build (#309).
//
// The rescan pass — the thing that turns a promotion gate into an ongoing
// control — matches an artifact's SBOM against today's vulnerability database
// and signs the findings onto the digest. It needs no rebuild and no redeploy,
// which is exactly why it works for a vendored artifact **given an SBOM**. An
// artifact with none is one nobody ever looks inside again, and `cmd/rescan
// fetch` says so out loud rather than reporting a clean scan of nothing.
//
// Most vendors publish no bill of materials. So where the vendor published
// none, the platform generates one: a scanner pod pointed at the digest, its
// output stored in the registry and countersigned here. With that, `max-
// severity`, the VEX register and the whole rescan pass work over vendored
// software with no further machinery at all — which is where a severity
// ceiling and an expiring exception earn their keep hardest.
//
// # It is an observation, and it is labelled as one
//
// A bill of materials the publisher stands behind and one the platform
// derived by unpacking layers are different artefacts. The generated one is
// indexed `platform-observed`, and the signed adoption record names it as the
// platform's; the vendor's, where there is one, is `vendor-asserted`. The
// generated document itself carries SPDX's or CycloneDX's predicate type like
// any other — it has to, or the rescan and `require-sbom` would not read it —
// so the two are told apart by a signed predicate rather than by a convention.
//
// # Where a vendor published one, nothing runs
//
// Generation is skipped entirely when the artifact already carries a bill of
// materials. Two answers to one question is worse than one: a rescan would
// have to choose between them, and the choice would be arbitrary.
//
// # It runs after the build is terminal
//
// Like the quality gates, and for the same reason: the artifact exists, the
// Release exists, and nothing here can change either. A generation that takes
// twenty minutes over a large image delays evidence and delays nothing else.

const (
	// labelObservedSBOM marks the Jobs this pass creates, so they are told
	// apart from a gate's without parsing their names.
	labelObservedSBOM = "kitchen.bermos.dev/observed-sbom"

	// VendorSBOMGeneratorImage is the default generator: Syft, pinned.
	//
	// It is a different image from SBOMGeneratorImage, which is BuildKit's
	// scanner-protocol wrapper around the same tool and cannot be run
	// standalone. Pinned rather than floating for the reason the other is:
	// evidence about an artifact should not change because somebody else's
	// tag moved overnight.
	VendorSBOMGeneratorImage = "anchore/syft:v1.18.1"

	// observedSBOMFile is where the generator writes and the publisher
	// reads. It reuses the quality gate's volume and its publisher, because
	// it is the same problem: a document that does not fit in a 4 KiB
	// termination message has to reach the operator through the registry.
	observedSBOMFile = gateFindingsDir + "/sbom.json"

	// observedSBOMName is what the run is called in the publisher's report.
	// The publisher echoes it back and the operator does not read it; it is
	// there so a person looking at a pod can see what the pod was for.
	observedSBOMName = "platform-observed-sbom"

	// defaultObservedSBOMTimeoutSeconds bounds one generation when the
	// platform configuration names no timeout. It is the CRD's default too,
	// repeated because a Kitchen object written before the field existed
	// carries a zero.
	defaultObservedSBOMTimeoutSeconds = 1800
)

// reconcileObservedSBOMs generates a bill of materials for every vendored
// artifact of a finished build that carries none, and signs it.
//
// It answers whether anything is still running, so the caller can requeue as
// a backstop the way the gate pass does.
func (r *BuildReconciler) reconcileObservedSBOMs(
	ctx context.Context, build *kitchenv1alpha1.Build,
) (ctrl.Result, error) {
	if build.Status.Phase != kitchenv1alpha1.BuildSucceeded {
		return ctrl.Result{}, nil
	}
	wanted := observableArtifacts(build)
	if len(wanted) == 0 {
		return ctrl.Result{}, nil
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	attest := kitchen.Spec.Compliance.Attestation
	if !attest.Enabled || !attest.Vendored.SBOM {
		return ctrl.Result{}, nil
	}

	project := &kitchenv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: build.Namespace, Name: build.Spec.ProjectRef.Name,
	}, project); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	changed, waiting := false, false
	for _, artifact := range wanted {
		moved, running, err := r.observeSBOM(ctx, build, project, kitchen, artifact)
		if err != nil {
			return ctrl.Result{}, err
		}
		changed = changed || moved
		waiting = waiting || running
	}
	if changed {
		if err := r.Status().Update(ctx, build); err != nil {
			return ctrl.Result{}, err
		}
	}
	if waiting {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	return ctrl.Result{}, nil
}

// observableArtifacts is every vendored image of this unit that wants a bill
// of materials the platform has to produce.
//
// Four things exclude an artifact, and each of them is a different fact:
//
//   - it is not vendored — a built artifact's SBOM comes from the builder,
//     which knows what it put in;
//   - the platform could not attest it at all, so there is nowhere to put a
//     generated document either and running a scanner would spend an
//     application namespace's resources on a file nothing could sign;
//   - the vendor published a bill of materials, which is better evidence;
//   - a run has already finished, whichever way it went. `Failed` is
//     terminal here for the reason it is terminal for a gate: a retry that
//     eventually succeeded would leave a build whose evidence says the
//     generator works and whose history says it did not.
func observableArtifacts(build *kitchenv1alpha1.Build) []kitchenv1alpha1.BuildArtifact {
	wanted := []kitchenv1alpha1.BuildArtifact{}
	for _, artifact := range build.VendoredArtifacts() {
		record := artifact.Artifact
		switch {
		case record.AttestedAt == nil, record.Repository == "", record.Digest == "":
			continue
		case hasBillOfMaterials(record):
			continue
		case record.ObservedSBOM != nil &&
			(record.ObservedSBOM.Phase == kitchenv1alpha1.GateCompleted ||
				record.ObservedSBOM.Phase == kitchenv1alpha1.GateFailed):
			continue
		}
		wanted = append(wanted, artifact)
	}
	return wanted
}

// hasBillOfMaterials is whether the artifact's evidence index already names a
// bill of materials, in either format the platform knows how to name.
func hasBillOfMaterials(artifact *kitchenv1alpha1.ArtifactStatus) bool {
	for _, entry := range artifact.Evidence {
		if attestation.SBOM(entry.PredicateType) {
			return true
		}
	}
	return false
}

// observeSBOM advances one artifact by one step.
func (r *BuildReconciler) observeSBOM(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	kitchen *kitchenv1alpha1.Kitchen,
	artifact kitchenv1alpha1.BuildArtifact,
) (changed, running bool, err error) {
	log := logf.FromContext(ctx)
	appNS := appNamespace(project.Name)
	name := observedSBOMJobName(build.Name, artifact.Workload)
	artifactRef := attestation.ArtifactRef(artifact.Artifact.Repository, artifact.Artifact.Digest)
	generator := vendorSBOMGenerator(kitchen)

	job := &batchv1.Job{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: name}, job); {
	case apierrors.IsNotFound(err):
		credsSecret := pullSecretName(project, project.Spec.Processes, artifact.Workload)
		created := observedSBOMJob(
			name, appNS, build, project, kitchen, credsSecret, artifactRef, generator, r.QualityGateImage)
		if err := r.Create(ctx, created); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, true, nil
			}
			return false, false, err
		}
		log.Info("generating a bill of materials for a vendored artifact",
			"build", build.Name, "artifact", artifact.Name(), "generator", generator)
		artifact.Artifact.ObservedSBOM = &kitchenv1alpha1.ObservedSBOMStatus{
			Phase: kitchenv1alpha1.GateRunning, Generator: generator, Job: name,
		}
		return true, true, nil
	case err != nil:
		return false, false, err
	}

	complete, failed, message := jobOutcome(job)
	switch {
	case complete:
		return r.publishObservedSBOM(ctx, build, project, appNS, name, generator, artifact)
	case failed:
		artifact.Artifact.ObservedSBOM = &kitchenv1alpha1.ObservedSBOMStatus{
			Phase:      kitchenv1alpha1.GateFailed,
			Generator:  generator,
			FinishedAt: ptr.To(metav1.Now()),
			Message: onArtifact(artifact,
				"no bill of materials could be generated: "+message),
		}
		return true, false, nil
	default:
		if artifact.Artifact.ObservedSBOM == nil {
			artifact.Artifact.ObservedSBOM = &kitchenv1alpha1.ObservedSBOMStatus{
				Phase: kitchenv1alpha1.GateRunning, Generator: generator, Job: name,
			}
			return true, true, nil
		}
		return false, true, nil
	}
}

// publishObservedSBOM turns a finished generation into signed evidence.
func (r *BuildReconciler) publishObservedSBOM(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	appNS, jobName, generator string,
	artifact kitchenv1alpha1.BuildArtifact,
) (changed, running bool, err error) {
	status := &kitchenv1alpha1.ObservedSBOMStatus{
		Phase:      kitchenv1alpha1.GateCompleted,
		Generator:  generator,
		FinishedAt: ptr.To(metav1.Now()),
	}
	report, found := r.gateReport(ctx, appNS, jobName)
	switch {
	case !found:
		status.Phase = kitchenv1alpha1.GateFailed
		status.Message = onArtifact(artifact,
			"the generator finished but left no report of where its output went")
	case report.Error != "":
		status.Phase = kitchenv1alpha1.GateFailed
		status.Message = onArtifact(artifact, "the generated bill of materials could not be stored: "+report.Error)
	default:
		if err := r.attestObservedSBOM(ctx, build, project, report, artifact, status); err != nil {
			// The document exists; what is missing is the signature, which
			// is a different failure from a generator that did not run. An
			// unattested bill of materials satisfies no policy, which is
			// where the consequence belongs.
			status.Message = onArtifact(artifact, err.Error())
			logf.FromContext(ctx).Info("a generated bill of materials was not attested",
				"build", build.Name, "artifact", artifact.Name(), "cause", err.Error())
		}
	}
	artifact.Artifact.ObservedSBOM = status
	return true, false, nil
}

// attestObservedSBOM signs the generated document and attaches it to the
// artifact's digest.
//
// The predicate is the document, verbatim. It is never re-marshalled from a
// decoded map and never converted between formats: a bill of materials
// rewritten by something that did not scan the image is a claim by the
// rewriter, and the platform's signature would then be on a document nobody
// produced.
func (r *BuildReconciler) attestObservedSBOM(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	report gateReport,
	artifact kitchenv1alpha1.BuildArtifact,
	status *kitchenv1alpha1.ObservedSBOMStatus,
) error {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return fmt.Errorf("the platform configuration could not be read: %w", err)
	}
	signer, err := SigningKeyFor(ctx, r.Client, kitchen)
	if err != nil {
		return fmt.Errorf("the signing key could not be read: %w", err)
	}
	if signer == nil {
		return fmt.Errorf("the platform holds no signing key, so the bill of materials was left unsigned")
	}

	source, err := vendoredSourceFor(project, artifact.Workload)
	if err != nil {
		return err
	}
	attester, err := r.vendorAttester(ctx, build, vendoredSubject{Workload: artifact.Workload, Source: source})
	if err != nil {
		return err
	}
	repository, digest := artifact.Artifact.Repository, artifact.Artifact.Digest
	document, err := attester.Blob(ctx, repository, report.Blob)
	if err != nil {
		return fmt.Errorf("the generated bill of materials could not be read back: %w", err)
	}

	predicateType, err := billOfMaterialsType(document)
	if err != nil {
		return err
	}
	status.PredicateType = predicateType

	statement, err := attestation.NewStatement(repository, digest, predicateType, json.RawMessage(document))
	if err != nil {
		return err
	}
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		return err
	}
	manifest, err := attester.Attach(
		ctx, attestation.ArtifactRef(repository, digest), envelope, predicateType)
	if err != nil {
		return fmt.Errorf("the bill of materials could not be attached to the artifact: %w", err)
	}
	artifact.Artifact.Evidence = append(artifact.Artifact.Evidence, kitchenv1alpha1.ArtifactEvidence{
		PredicateType: predicateType,
		Manifest:      manifest,
		Source:        sourcePlatformObserved,
	})
	return nil
}

// billOfMaterialsType is what the generator emitted, read off the document
// rather than off what was configured.
//
// The generator is the one that knows. An installation that swapped Syft for
// something emitting CycloneDX gets a CycloneDX attestation whose predicate
// type says so, without having told the platform anything — and a document
// that is neither is refused rather than attested under a type it does not
// match, because a predicate type is a promise about the bytes under it.
func billOfMaterialsType(document []byte) (string, error) {
	shape := struct {
		SPDXVersion string `json:"spdxVersion"`
		BOMFormat   string `json:"bomFormat"`
	}{}
	if err := json.Unmarshal(document, &shape); err != nil {
		return "", fmt.Errorf("the generator's output is not JSON: %w", err)
	}
	switch {
	case shape.SPDXVersion != "":
		return attestation.PredicateSPDX, nil
	case strings.EqualFold(shape.BOMFormat, "CycloneDX"):
		return attestation.PredicateCycloneDX, nil
	}
	return "", fmt.Errorf(
		"the generator's output is neither SPDX nor CycloneDX, and a bill of materials the platform " +
			"cannot name is one nothing downstream can read")
}

// vendoredSourceFor is the image declaration one artifact of a unit was
// acquired from — the project's own for the web process, the workload's for
// anything else. It is where the pull credential and the expected signer are
// written, and both are needed again to write evidence back.
func vendoredSourceFor(
	project *kitchenv1alpha1.Project, workload string,
) (kitchenv1alpha1.ImageSourceSpec, error) {
	if workload == "" {
		return project.Spec.Source.ImageSource(), nil
	}
	for i := range project.Spec.Processes {
		process := &project.Spec.Processes[i]
		if process.Name == workload && process.Image != nil {
			return *process.Image, nil
		}
	}
	return kitchenv1alpha1.ImageSourceSpec{}, fmt.Errorf(
		"workload %s no longer declares an image, so there is nothing to attach its evidence with", workload)
}

// vendorSBOMGenerator is the image to run, the installation's where it named
// one and the pinned default otherwise.
func vendorSBOMGenerator(kitchen *kitchenv1alpha1.Kitchen) string {
	if named := strings.TrimSpace(kitchen.Spec.Compliance.Attestation.Vendored.SBOMGenerator); named != "" {
		return named
	}
	return VendorSBOMGeneratorImage
}

// observedSBOMJob is the pod that describes one vendored artifact: the
// generator first as an init container, then the quality gate's own publisher
// carrying its output out.
//
// The publisher is reused rather than copied because it is the same contract:
// a document too large for a termination message, stored as a blob in the
// artifact's repository, with the digest reported back. What the pod is given
// is the artifact, a credential to pull it with, and nothing else — no service
// account token, no cluster access, an unprivileged user. It is an image
// somebody else wrote, running in an application's namespace, and the only
// thing the platform wants from it is a file.
func observedSBOMJob(
	name, appNS string,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	kitchen *kitchenv1alpha1.Kitchen,
	credsSecret, artifactRef, generator, publisherImage string,
) *batchv1.Job {
	labels := map[string]string{
		labelProject:      project.Name,
		labelBuild:        build.Name,
		labelBuildNS:      build.Namespace,
		labelObservedSBOM: "true",
		labelManagedByKey: labelManagedByValue,
	}
	environment := []corev1.EnvVar{
		{Name: "KITCHEN_ARTIFACT", Value: artifactRef},
		{Name: "KITCHEN_FINDINGS", Value: observedSBOMFile},
		{Name: "KITCHEN_GATE", Value: observedSBOMName},
		{Name: "KITCHEN_TERMINATION_LOG", Value: terminationLogPath},
		{Name: "KITCHEN_PROJECT", Value: project.Name},
		{Name: "KITCHEN_BUILD", Value: build.Name},
		{Name: "DOCKER_CONFIG", Value: dockerConfigDir},
		// Syft and every other scanner want somewhere to cache what they
		// unpack, and a pod that runs as an unprivileged user with no home
		// directory has nowhere. Both are pointed at a writable volume
		// rather than at the container's root filesystem, so the pod works
		// the same way under a read-only root.
		{Name: "HOME", Value: observedSBOMCacheDir},
		{Name: "XDG_CACHE_HOME", Value: observedSBOMCacheDir},
	}
	mounts := []corev1.VolumeMount{
		dockerConfigMount(),
		{Name: "findings", MountPath: gateFindingsDir},
		{Name: "cache", MountPath: observedSBOMCacheDir},
	}
	unprivileged := &corev1.SecurityContext{
		RunAsUser:                ptr.To(int64(1000)),
		RunAsNonRoot:             ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	timeout := kitchen.Spec.Compliance.Attestation.Vendored.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultObservedSBOMTimeoutSeconds
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS, Labels: labels},
		Spec: batchv1.JobSpec{
			// One attempt, for the gate's reason: a retry that eventually
			// succeeded would leave a build whose evidence says the
			// generator works and whose history says it did not.
			BackoffLimit:            ptr.To(int32(0)),
			ActiveDeadlineSeconds:   ptr.To(int64(timeout)),
			TTLSecondsAfterFinished: ptr.To(int32(gateJobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					InitContainers: []corev1.Container{{
						Name:  "sbom",
						Image: generator,
						// Syft's argv, which is also the shape every other
						// generator worth naming accepts: a reference and an
						// output. Nothing from any API request reaches it —
						// the reference is a digest the platform resolved,
						// and the path is a constant.
						Args:            []string{artifactRef, "-o", "spdx-json=" + observedSBOMFile},
						Env:             environment,
						VolumeMounts:    mounts,
						SecurityContext: unprivileged,
					}},
					Containers: []corev1.Container{{
						Name:            "publish",
						Image:           publisherImage,
						Command:         []string{"/qualitygate"},
						Env:             environment,
						VolumeMounts:    mounts,
						SecurityContext: unprivileged,
					}},
					Volumes: []corev1.Volume{
						dockerConfigVolume(credsSecret),
						{
							Name:         "findings",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
						{
							Name:         "cache",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
				},
			},
		},
	}
}

// observedSBOMCacheDir is the scratch a generator unpacks an image into.
const observedSBOMCacheDir = "/kitchen/cache"

// observedSBOMJobName names one generation, within the 63 characters a
// Kubernetes object name allows. It is deterministic on the build and the
// workload for the reason a gate's name is: a reconcile that creates a Job it
// already created finds it there instead of starting a second scanner.
func observedSBOMJobName(buildName, workload string) string {
	name := buildName + "-sbom"
	if workload != "" {
		name = buildName + "-sbom-" + workload
	}
	if len(name) <= 63 {
		return name
	}
	return name[:63]
}

// soonestOf combines the two after-the-build passes' results into one.
//
// Both may ask to be woken again, and a caller can only return one result, so
// the shorter wait wins — a pass that asked for a minute must not be made to
// wait for one that asked for nothing. Zero means "no requeue asked for" and
// therefore loses to any interval, which is the opposite of what a naive
// minimum would do.
func soonestOf(a, b ctrl.Result) ctrl.Result {
	switch {
	case a.RequeueAfter == 0:
		return b
	case b.RequeueAfter == 0:
		return a
	case b.RequeueAfter < a.RequeueAfter:
		return b
	}
	return a
}

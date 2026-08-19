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
	"github.com/Bermos/Kitchen/internal/provider"
)

// Quality gates: things that look at an artifact and say what they found.
//
// # Facts, never verdicts
//
// A gate emits findings. Whether a finding is disqualifying is a property of
// the environment an artifact is being promoted to, not of the artifact, and
// answering it here would fix the answer platform-wide at the moment of
// scanning. That is what would make it impossible for the same scan to be
// acceptable in staging and blocking in production without running the scanner
// twice — and it is what would drag application teams into negotiating
// thresholds, which is the thing this design exists to avoid.
//
// So `kitchen.bermos.dev/attestation/quality-gate/v1` records the gate, its
// version, when it ran and its raw output. It has no pass field. Phase three's
// policy engine reads these and decides.
//
// # Ran and found problems is not failed
//
// A gate that scans an image and reports ninety critical vulnerabilities has
// **completed**: it did exactly its job. `Failed` is reserved for a gate that
// did not run — the image would not pull, the scanner crashed, the timeout
// expired — where nothing is known either way.
//
// Collapsing those two is how a compliance system comes to report green while
// its scanners are quietly broken, so the runner is arranged to keep them
// apart by construction: the gate runs as an **init container**, and its exit
// status is only ever a statement about whether it ran. Nothing here passes a
// scanner the flag that makes it exit non-zero on findings, and a gate
// configured with one is misconfigured in a way that shows up as every artifact
// failing to be scanned rather than as a policy nobody agreed to.
//
// # Where the findings go
//
// The gate writes them to a file on a volume shared with a second container,
// which stores them in the registry and reports the digest through the pod's
// termination message. See cmd/qualitygate for why that indirection exists at
// all — the short version is that a scan report does not fit in a termination
// message, does not reliably fit in a ConfigMap, and cannot be read back out of
// a log without racing the Job finishing.
//
// The blob is unsigned while it waits there. It becomes evidence here, where
// the platform's key is, and nowhere else.

const (
	// labelGate names which gate a Job belongs to, so the Jobs of one build
	// are told apart without parsing their names.
	labelGate = "kitchen.bermos.dev/quality-gate"

	// gateFindingsDir is the volume the gate writes to and the publisher
	// reads from.
	gateFindingsDir = "/kitchen/findings"

	// gateFindingsFile is the file within it. It is passed to both containers
	// through the environment rather than being a convention each end
	// remembers separately.
	gateFindingsFile = gateFindingsDir + "/findings.json"

	// gateJobTTLSeconds is how long a finished gate Job sticks around. Its
	// output is already in the registry by the time it finishes, so this is
	// only a window for the reconciler to read the termination message and
	// for a person to look at what happened.
	gateJobTTLSeconds = 3600

	// defaultGateTimeoutSeconds bounds a run when the gate names no timeout.
	// It is the CRD's default too; it is repeated here because a Kitchen
	// object written before the field existed carries a zero.
	defaultGateTimeoutSeconds = 900
)

// gateReport is what cmd/qualitygate leaves on the pod: where the findings
// are, rather than the findings.
type gateReport struct {
	Gate       string `json:"gate"`
	Blob       string `json:"blob"`
	Bytes      int    `json:"bytes"`
	FinishedAt string `json:"finishedAt"`
	Error      string `json:"error,omitempty"`
}

// reconcileGates runs the platform's gates over a finished build's artifact and
// signs what they find.
//
// It runs after the build is terminal, which is why it is not part of the main
// path: the artifact exists, the Release exists, and nothing here can change
// either. A gate that never finishes delays evidence and delays nothing else.
func (r *BuildReconciler) reconcileGates(
	ctx context.Context, build *kitchenv1alpha1.Build,
) (ctrl.Result, error) {
	if build.Status.Phase != kitchenv1alpha1.BuildSucceeded {
		// Nothing to gate. A failed build produced no artifact, and gating
		// one that does not exist would be recording a fact about nothing.
		return ctrl.Result{}, nil
	}
	artifact := build.Status.Artifact
	if artifact == nil || artifact.Digest == "" || artifact.Repository == "" {
		return ctrl.Result{}, nil
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	gates := enabledGates(kitchen)
	if len(gates) == 0 {
		return ctrl.Result{}, nil
	}
	if !kitchen.Spec.Compliance.Attestation.Enabled {
		// Running gates whose findings nothing will sign spends an
		// application namespace's resources to produce a blob in a registry
		// that no policy can ever trust. Off is off.
		return ctrl.Result{}, nil
	}

	project := &kitchenv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: build.Namespace, Name: build.Spec.ProjectRef.Name,
	}, project); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	appNS := appNamespace(project.Name)
	artifactRef := attestation.ArtifactRef(artifact.Repository, artifact.Digest)

	changed, waiting := false, false
	for _, gate := range gates {
		status := gateStatus(build, gate.Name)
		if status.Phase == kitchenv1alpha1.GateCompleted || status.Phase == kitchenv1alpha1.GateFailed {
			continue
		}
		moved, running, err := r.runGate(ctx, build, project, gate, appNS, artifactRef)
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
		// The Job is watched, so this is a backstop rather than the way the
		// next reconcile normally arrives — a pod that is killed without its
		// Job ever completing would otherwise leave a gate Pending forever.
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	return ctrl.Result{}, nil
}

// runGate advances one gate by one step, answering whether the Build's status
// changed and whether the gate is still running.
func (r *BuildReconciler) runGate(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	gate kitchenv1alpha1.QualityGateSpec,
	appNS, artifactRef string,
) (changed, running bool, err error) {
	log := logf.FromContext(ctx)
	name := gateJobName(build.Name, gate.Name)

	job := &batchv1.Job{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: name}, job); {
	case apierrors.IsNotFound(err):
		credsSecret := registrySecretName(project.Spec.Registry.ConnectionRef.Name)
		if err := r.Create(ctx, gateJob(name, appNS, build, project, gate, credsSecret, artifactRef, r.QualityGateImage)); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, true, nil
			}
			return false, false, err
		}
		log.Info("quality gate started", "build", build.Name, "gate", gate.Name)
		return setGate(build, kitchenv1alpha1.QualityGateStatus{
			Name:   gate.Name,
			Phase:  kitchenv1alpha1.GateRunning,
			Source: gateSourcePlatform,
		}), true, nil
	case err != nil:
		return false, false, err
	}

	complete, failed, message := jobOutcome(job)
	switch {
	case complete:
		return r.publishGate(ctx, build, gate, appNS, name, artifactRef)
	case failed:
		// The gate did not run. That is not the same as a gate that ran and
		// found problems, and the difference is the reason this branch exists
		// rather than recording an empty result.
		return setGate(build, kitchenv1alpha1.QualityGateStatus{
			Name:       gate.Name,
			Phase:      kitchenv1alpha1.GateFailed,
			Source:     gateSourcePlatform,
			FinishedAt: ptr.To(metav1.Now()),
			Message:    "the gate did not run: " + message,
		}), false, nil
	default:
		return setGate(build, kitchenv1alpha1.QualityGateStatus{
			Name:   gate.Name,
			Phase:  kitchenv1alpha1.GateRunning,
			Source: gateSourcePlatform,
		}), true, nil
	}
}

// publishGate turns a finished run into signed evidence.
func (r *BuildReconciler) publishGate(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	gate kitchenv1alpha1.QualityGateSpec,
	appNS, jobName, artifactRef string,
) (changed, running bool, err error) {
	report, found := r.gateReport(ctx, appNS, jobName)
	switch {
	case !found:
		return setGate(build, kitchenv1alpha1.QualityGateStatus{
			Name:       gate.Name,
			Phase:      kitchenv1alpha1.GateFailed,
			Source:     gateSourcePlatform,
			FinishedAt: ptr.To(metav1.Now()),
			Message:    "the gate finished but left no report of where its findings went",
		}), false, nil
	case report.Error != "":
		return setGate(build, kitchenv1alpha1.QualityGateStatus{
			Name:       gate.Name,
			Phase:      kitchenv1alpha1.GateFailed,
			Source:     gateSourcePlatform,
			FinishedAt: ptr.To(metav1.Now()),
			Message:    "the gate's findings could not be stored: " + report.Error,
		}), false, nil
	}

	finished := metav1.Now()
	if stamp, err := time.Parse(time.RFC3339, report.FinishedAt); err == nil {
		finished = metav1.NewTime(stamp)
	}
	status := kitchenv1alpha1.QualityGateStatus{
		Name:          gate.Name,
		Phase:         kitchenv1alpha1.GateCompleted,
		Source:        gateSourcePlatform,
		PredicateType: attestation.PredicateQualityGate,
		FinishedAt:    &finished,
	}

	if err := r.attestGate(ctx, build, gate, artifactRef, report, &status); err != nil {
		// The gate ran, so it completed. What is missing is the signature,
		// which is a different failure and is recorded as one: an unattested
		// result cannot satisfy a policy, and saying "the gate failed" would
		// send somebody to look at the scanner.
		status.Message = err.Error()
		logf.FromContext(ctx).Info("a quality gate's findings were not attested",
			"build", build.Name, "gate", gate.Name, "cause", err.Error())
	}
	return setGate(build, status), false, nil
}

// attestGate signs one gate's findings and attaches them to the artifact.
func (r *BuildReconciler) attestGate(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	gate kitchenv1alpha1.QualityGateSpec,
	artifactRef string,
	report gateReport,
	status *kitchenv1alpha1.QualityGateStatus,
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
		return fmt.Errorf("the platform holds no signing key, so the findings were left unsigned")
	}

	target, err := r.gateTarget(ctx, build)
	if err != nil {
		return err
	}
	attester, err := r.attester(ctx, target)
	if err != nil {
		return err
	}
	repository, digest, _ := strings.Cut(artifactRef, "@")
	findings, err := attester.Blob(ctx, repository, report.Blob)
	if err != nil {
		return fmt.Errorf("the gate's findings could not be read back: %w", err)
	}

	statement, err := attestation.NewStatement(
		repository, digest, attestation.PredicateQualityGate,
		gateRecord(gate, report, findings))
	if err != nil {
		return err
	}
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		return err
	}
	manifest, err := attester.Attach(ctx, artifactRef, envelope, attestation.PredicateQualityGate)
	if err != nil {
		return fmt.Errorf("the gate's findings could not be attached to the artifact: %w", err)
	}

	attested := metav1.Now()
	status.Attested = &attested
	if build.Status.Artifact != nil {
		build.Status.Artifact.Evidence = append(build.Status.Artifact.Evidence, kitchenv1alpha1.ArtifactEvidence{
			PredicateType: attestation.PredicateQualityGate,
			Manifest:      manifest,
			Source:        sourcePlatform,
		})
	}
	return nil
}

// gateRecord is the predicate: which gate, which version, when, and what it
// found, unmodified.
//
// There is no pass field and there will not be one. The raw output is carried
// as the bytes the gate wrote when those bytes are JSON, and as a string when
// they are not — a gate that emits a text report is still a gate, and
// re-encoding its output to fit a shape would be the platform editing evidence.
func gateRecord(gate kitchenv1alpha1.QualityGateSpec, report gateReport, findings []byte) map[string]any {
	record := map[string]any{
		"gate":       gate.Name,
		"image":      gate.Image,
		"finishedAt": report.FinishedAt,
		"bytes":      report.Bytes,
	}
	if gate.Version != "" {
		record["version"] = gate.Version
	}
	if gate.Format != "" {
		record["format"] = gate.Format
	}
	if json.Valid(findings) {
		record["findings"] = json.RawMessage(findings)
	} else {
		record["findings"] = string(findings)
	}
	return record
}

// gateTarget resolves the registry the artifact lives in, which is the same
// one the build pushed to.
func (r *BuildReconciler) gateTarget(
	ctx context.Context, build *kitchenv1alpha1.Build,
) (buildTarget, error) {
	project := &kitchenv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: build.Namespace, Name: build.Spec.ProjectRef.Name,
	}, project); err != nil {
		return buildTarget{}, err
	}
	connection := &kitchenv1alpha1.Connection{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: build.Namespace, Name: project.Spec.Registry.ConnectionRef.Name,
	}, connection); err != nil {
		return buildTarget{}, err
	}
	registry, err := provider.Registry(connection)
	if err != nil {
		return buildTarget{}, err
	}
	return buildTarget{
		Connection: connection,
		Registry:   registry,
		Namespace:  appNamespace(project.Name),
	}, nil
}

// gateReport reads what the publisher left on the pod.
func (r *BuildReconciler) gateReport(ctx context.Context, appNS, jobName string) (gateReport, bool) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(appNS), client.MatchingLabels{"job-name": jobName}); err != nil {
		return gateReport{}, false
	}
	for _, pod := range pods.Items {
		for _, state := range pod.Status.ContainerStatuses {
			if state.State.Terminated == nil || state.State.Terminated.Message == "" {
				continue
			}
			report := gateReport{}
			if err := json.Unmarshal([]byte(state.State.Terminated.Message), &report); err != nil {
				continue
			}
			if report.Blob != "" || report.Error != "" {
				return report, true
			}
		}
	}
	return gateReport{}, false
}

// gateJob is the pod that runs one gate: the gate first as an init container,
// then the publisher that carries its output out.
//
// The gate is given the artifact and a credential to pull it with, and nothing
// else. It has no service account token, no access to the cluster, and it runs
// as an unprivileged user — it is an image somebody else wrote, running in an
// application's namespace, and the only thing the platform wants from it is a
// file.
func gateJob(
	name, appNS string,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	gate kitchenv1alpha1.QualityGateSpec,
	credsSecret, artifactRef, publisherImage string,
) *batchv1.Job {
	labels := map[string]string{
		labelProject:      project.Name,
		labelBuild:        build.Name,
		labelBuildNS:      build.Namespace,
		labelGate:         gate.Name,
		labelManagedByKey: labelManagedByValue,
	}
	environment := []corev1.EnvVar{
		{Name: "KITCHEN_ARTIFACT", Value: artifactRef},
		{Name: "KITCHEN_FINDINGS", Value: gateFindingsFile},
		{Name: "KITCHEN_GATE", Value: gate.Name},
		{Name: "KITCHEN_TERMINATION_LOG", Value: terminationLogPath},
		{Name: "KITCHEN_PROJECT", Value: project.Name},
		{Name: "KITCHEN_BUILD", Value: build.Name},
		{Name: "KITCHEN_COMMIT", Value: build.Spec.Git.SHA},
		{Name: "DOCKER_CONFIG", Value: dockerConfigDir},
	}
	mounts := []corev1.VolumeMount{
		dockerConfigMount(),
		{Name: "findings", MountPath: gateFindingsDir},
	}
	unprivileged := &corev1.SecurityContext{
		RunAsUser:                ptr.To(int64(1000)),
		RunAsNonRoot:             ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	timeout := gate.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultGateTimeoutSeconds
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS, Labels: labels},
		Spec: batchv1.JobSpec{
			// A gate that failed to run is a fact about this artifact, not
			// something to keep trying: a retry that eventually succeeded
			// would leave a build whose evidence says the scanner works and
			// whose history says it did not.
			BackoffLimit:            ptr.To(int32(0)),
			ActiveDeadlineSeconds:   ptr.To(int64(timeout)),
			TTLSecondsAfterFinished: ptr.To(int32(gateJobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// The gate reads an image and writes a file. Nothing it
					// could do with a token is something it should be doing.
					AutomountServiceAccountToken: ptr.To(false),
					InitContainers: []corev1.Container{{
						Name:            "gate",
						Image:           gate.Image,
						Args:            gate.Args,
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
					},
				},
			},
		},
	}
}

// Where a gate result came from. An external result is evidence about who
// reported it as much as about what it says, so the two are never one word.
const (
	gateSourcePlatform = "platform"
	gateSourceExternal = "external"
)

// enabledGates is the gates that should run, in configuration order.
func enabledGates(kitchen *kitchenv1alpha1.Kitchen) []kitchenv1alpha1.QualityGateSpec {
	gates := []kitchenv1alpha1.QualityGateSpec{}
	for _, gate := range kitchen.Spec.Compliance.Gates {
		if !gate.Disabled {
			gates = append(gates, gate)
		}
	}
	return gates
}

// gateStatus is what is already recorded for one gate.
func gateStatus(build *kitchenv1alpha1.Build, name string) kitchenv1alpha1.QualityGateStatus {
	for _, status := range build.Status.Gates {
		if status.Name == name {
			return status
		}
	}
	return kitchenv1alpha1.QualityGateStatus{Name: name}
}

// setGate records one gate's status, answering whether anything changed — so
// that a reconcile which learned nothing does not write to the API server.
func setGate(build *kitchenv1alpha1.Build, status kitchenv1alpha1.QualityGateStatus) bool {
	for index, existing := range build.Status.Gates {
		if existing.Name != status.Name {
			continue
		}
		if existing.Phase == status.Phase && existing.Message == status.Message &&
			(existing.Attested != nil) == (status.Attested != nil) {
			return false
		}
		build.Status.Gates[index] = status
		return true
	}
	build.Status.Gates = append(build.Status.Gates, status)
	return true
}

// gateJobName names a gate's Job for a build, within the 63 characters a
// Kubernetes object name allows.
//
// The build name is kept whole and the gate name is what gives way, because
// the build is what makes the name unique and a truncated build name would
// collide across builds — where two gates whose names agree in their first
// characters are a configuration somebody can see and fix.
func gateJobName(buildName, gateName string) string {
	name := buildName + "-gate-" + gateName
	if len(name) <= 63 {
		return name
	}
	room := 63 - len(buildName) - len("-gate-")
	if room < 1 {
		// A build name this long cannot carry a gate suffix at all. It cannot
		// happen with names the platform generates, and answering something
		// invalid is better than answering something that silently collides.
		return name
	}
	return buildName + "-gate-" + gateName[:room]
}

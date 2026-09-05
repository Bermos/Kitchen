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
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/volumeinit"
)

// How a workload's volume is prepared before the workload starts (#348).
//
// A volume claim hands a process an empty filesystem, and a good deal of the
// vendored estate will not start on one. The declaration is the project's —
// `runtime.init` for the web process, `processes[].init` for the rest — and
// it is snapshotted into the Release like everything else about a workload,
// so a rollback restores the tree and the seeds that release started with.
//
// What is rendered here is one init container in the workload's own pod. It
// runs the operator's own image, whose `/volume-init` does the two typed
// things a plan can say; it mounts exactly the volumes the plan names and the
// platform's copy of the files it seeds from, and nothing else; and it runs
// under the project's own `runtime.security` posture, because it is doing
// work on the application's behalf inside the application's pod and a
// relaxation granted for the platform's convenience would be a hole the
// project never asked for.
//
// **Nothing from a request reaches its command line.** The argv is the one
// fixed word, and the plan travels in the environment as JSON for the program
// to read — the rule the KEDA install job follows, held to here for the same
// reason: this container is inside the application's namespace, but it is the
// platform's code and the platform's image, and a platform that assembled a
// shell line out of a project's declaration would be one `;` from running the
// project's choice of command as the platform.

const (
	// VolumeInitContainerName is the init container, and the name the
	// environment looks for when it explains why a workload never started.
	VolumeInitContainerName = "kitchen-volume-init"

	// volumeInitSeedVolumeName is where the platform's copy of the files a
	// plan seeds from is projected. It is one volume however many sources it
	// draws on — a plain file's content is the environment's ConfigMap and a
	// secret file's is the project's Secret — because the program reads a
	// directory of files and does not care which object each came from.
	volumeInitSeedVolumeName = "kitchen-volume-init-seed"
)

// volumeInitFor is the init declaration one workload of a Release carries:
// the runtime's for the web process, and the process's own for anything else.
func volumeInitFor(release *kitchenv1alpha1.Release, workload string) []kitchenv1alpha1.VolumeInit {
	if workload == kitchenv1alpha1.WebProcessName {
		return release.Spec.ConfigSnapshot.Runtime.Init
	}
	for _, process := range release.Spec.ConfigSnapshot.Processes {
		if process.Name == workload {
			return process.Init
		}
	}
	return nil
}

// volumeInitPlan turns one workload's declaration into the plan its init
// container runs, against the volumes that workload actually mounts.
//
// The error is the whole of the "a step that fails says why" contract's first
// half: everything that can be known before a pod exists is decided here, so
// that a declaration naming a volume this workload does not mount becomes a
// sentence on the Environment rather than a pod that comes up, mounts
// nothing, and writes into its own container's filesystem.
func volumeInitPlan(
	inits []kitchenv1alpha1.VolumeInit,
	mounts []mountedVolume,
	files []kitchenv1alpha1.ConfigFile,
) (volumeinit.Plan, error) {
	plan := volumeinit.Plan{}
	for _, init := range inits {
		mount, found := findMount(mounts, init.Volume)
		switch {
		case !found:
			return plan, fmt.Errorf(
				"it prepares the volume %q and does not mount it: a volume claim names the one process "+
					"that mounts it, so either this workload's init names the wrong claim or the claim "+
					"names the wrong process",
				init.Volume)
		case mount.readOnly:
			return plan, fmt.Errorf(
				"it prepares the volume %q, which this workload mounts read-only: nothing can be created "+
					"on it here — a preview shares production's bound volume read-only, and a claim asking "+
					"for ReadOnlyMany is read-only everywhere",
				init.Volume)
		}
		volume := volumeinit.Volume{Claim: init.Volume, MountPath: mount.mountPath}
		for _, dir := range init.Directories {
			volume.Directories = append(volume.Directories,
				volumeinit.Directory{Path: dir.Path, Mode: dir.Mode})
		}
		for _, seed := range init.Seed {
			if !declaresFile(files, seed.File) {
				return plan, fmt.Errorf(
					"it seeds the volume %q from the configuration file %q, which this release does not "+
						"carry: declare the file in the project's files, or take the seed off",
					init.Volume, seed.File)
			}
			volume.Seeds = append(volume.Seeds,
				volumeinit.Seed{File: seed.File, Path: seed.Path, Mode: seed.Mode})
		}
		plan.Volumes = append(plan.Volumes, volume)
	}
	return plan, nil
}

// findMount is the workload's mount of one claim.
func findMount(mounts []mountedVolume, claim string) (mountedVolume, bool) {
	for _, mount := range mounts {
		if mount.claim == claim {
			return mount, true
		}
	}
	return mountedVolume{}, false
}

// declaresFile reports whether the release carries a configuration file of
// that name — secret or plain, since both are placed for the init container
// to read and only the plain one's content is in the Release.
func declaresFile(files []kitchenv1alpha1.ConfigFile, name string) bool {
	for _, file := range files {
		if file.Name == name {
			return true
		}
	}
	return false
}

// podInit is one workload's prepared init: the container that runs the steps
// and the projected volume it reads seed content from, or an empty value for
// a workload that declares none.
//
// It is built once for the whole reconcile rather than at each pod, because
// every one of the four pod shapes — the web Deployment, a worker's, a
// scheduled run's and a deploy task's — needs the same object and the
// validation behind it has to fail the *environment* rather than one pod.
type podInit struct {
	container  *corev1.Container
	seedVolume *corev1.Volume
}

// declared reports whether this workload prepares anything.
func (p podInit) declared() bool { return p.container != nil }

// volumeInits is every workload of one environment's prepared init, by
// workload name — `web` for the web process, and a process's own name
// otherwise.
type volumeInits map[string]podInit

// buildVolumeInits works out what every workload of this Release does to its
// volumes before it starts, and refuses the whole pass where a declaration
// cannot be honoured.
//
// It is one pass over the unit rather than a decision at each pod, and that
// is the point: a deploy task's init is validated before the task is started,
// so a unit whose migration prepares the wrong volume never runs the
// migration at all.
func buildVolumeInits(
	release *kitchenv1alpha1.Release,
	mounts []mountedVolume,
	envName, operatorImage string,
) (volumeInits, error) {
	inits := volumeInits{}
	files := release.Spec.ConfigSnapshot.Files

	workloads := []string{kitchenv1alpha1.WebProcessName}
	for _, process := range release.Spec.ConfigSnapshot.Processes {
		workloads = append(workloads, process.Name)
	}
	for _, workload := range workloads {
		declared := volumeInitFor(release, workload)
		if len(declared) == 0 {
			continue
		}
		plan, err := volumeInitPlan(declared, mountsFor(mounts, workload), files)
		if err != nil {
			return nil, fmt.Errorf("the %s workload prepares its volumes before it starts, and %w", workload, err)
		}
		if operatorImage == "" {
			return nil, fmt.Errorf(
				"the %s workload prepares its volumes before it starts, and this platform was installed "+
					"without an operator image to run the steps with: set `operator.image` on the chart, "+
					"which is what --quality-gate-image passes in",
				workload)
		}
		// The posture *this* workload runs under, not the unit's (#399): the
		// pod is built under the workload's own resolution, and an init
		// container entering as a different user would leave the tree it
		// creates owned by somebody the process that reads it is not.
		init, err := volumeInitContainer(plan, files, envName, operatorImage,
			workloadSecurity(release.Spec.ConfigSnapshot, workload))
		if err != nil {
			return nil, fmt.Errorf("the %s workload prepares its volumes before it starts, and %w", workload, err)
		}
		inits[workload] = init
	}
	return inits, nil
}

// volumeInitContainer renders one workload's plan as the container that runs
// it, plus the volume it reads seed content from.
func volumeInitContainer(
	plan volumeinit.Plan,
	files []kitchenv1alpha1.ConfigFile,
	envName, operatorImage string,
	security *kitchenv1alpha1.SecuritySpec,
) (podInit, error) {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return podInit{}, err
	}
	init := podInit{seedVolume: volumeInitSeedVolume(plan, files, envName)}

	// The same volumes the application's own container mounts, at the same
	// paths: every path in the plan is relative to those, and two containers
	// disagreeing about where a claim is mounted is the one way a relative
	// path could still land somewhere unintended.
	mounts := make([]corev1.VolumeMount, 0, len(plan.Volumes)+1)
	for _, volume := range plan.Volumes {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      claimVolumeName(volume.Claim),
			MountPath: volume.MountPath,
		})
	}
	if init.seedVolume != nil {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volumeInitSeedVolumeName,
			MountPath: volumeinit.SeedDir,
			ReadOnly:  true,
		})
	}

	init.container = &corev1.Container{
		Name:  VolumeInitContainerName,
		Image: operatorImage,
		// One fixed word. Everything that varies is data in the environment,
		// read by the platform's own program — there is no shell here, and
		// nothing a project declares becomes an argument.
		Command:      []string{"/volume-init"},
		Env:          []corev1.EnvVar{{Name: volumeinit.PlanVariable, Value: string(encoded)}},
		VolumeMounts: mounts,
		// The posture this workload runs under — the unit's with the
		// workload's own merged over it. It is the application's pod and the
		// application's volume; running this more freely than the process
		// that will own the result would be the platform quietly granting
		// itself what the project declined.
		SecurityContext: containerSecurityContext(security),
		// The step writes its own account of a failure to the termination
		// log; the fallback is for the case where it never got that far.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	return init, nil
}

// volumeInitOnPod puts one workload's init container on a pod spec, and takes
// an old one off a workload that no longer declares any.
//
// It edits the spec the caller has finished building, the way
// configFilesOnPod does and for the same reason: the four pod shapes would
// otherwise each grow their own copy of this and drift.
func volumeInitOnPod(spec *corev1.PodSpec, init podInit) {
	// Written whole every reconcile, so a project that has withdrawn its
	// init loses the container rather than keeping yesterday's.
	spec.InitContainers = nil
	if !init.declared() {
		return
	}
	if init.seedVolume != nil {
		spec.Volumes = append(spec.Volumes, *init.seedVolume)
	}
	spec.InitContainers = []corev1.Container{*init.container}
}

// volumeInitSeedVolume is the platform's copy of the files the plan seeds
// from, projected into one directory: the plain ones out of the environment's
// ConfigMap and the secret ones out of the project's Secret.
//
// Items rather than whole objects, for the reason the config file mounts use
// items: a file this workload does not seed has no business being readable in
// its init container, and a secret file least of all.
func volumeInitSeedVolume(
	plan volumeinit.Plan,
	files []kitchenv1alpha1.ConfigFile,
	envName string,
) *corev1.Volume {
	var plain, secret []corev1.KeyToPath
	seen := map[string]bool{}
	for _, volume := range plan.Volumes {
		for _, seed := range volume.Seeds {
			if seen[seed.File] {
				continue
			}
			seen[seed.File] = true
			item := corev1.KeyToPath{Key: seed.File, Path: seed.File}
			if isSecretFile(files, seed.File) {
				secret = append(secret, item)
			} else {
				plain = append(plain, item)
			}
		}
	}
	if len(plain) == 0 && len(secret) == 0 {
		return nil
	}
	projected := &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(0o444))}
	if len(plain) > 0 {
		projected.Sources = append(projected.Sources, corev1.VolumeProjection{
			ConfigMap: &corev1.ConfigMapProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: configFilesName(envName)},
				Items:                plain,
			},
		})
	}
	if len(secret) > 0 {
		projected.Sources = append(projected.Sources, corev1.VolumeProjection{
			Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: ProjectFilesName},
				Items:                secret,
			},
		})
	}
	return &corev1.Volume{
		Name:         volumeInitSeedVolumeName,
		VolumeSource: corev1.VolumeSource{Projected: projected},
	}
}

// isSecretFile reports whether the named configuration file's content is held
// where no response reads it back.
func isSecretFile(files []kitchenv1alpha1.ConfigFile, name string) bool {
	for _, file := range files {
		if file.Name == name {
			return file.Secret
		}
	}
	return false
}

// volumeInitFailure is why a workload's pods are not up when the init
// container is the reason — the second half of the "a step that fails says
// why" contract, and the half a pod is needed for.
//
// It reads the same pods [EnvironmentReconciler.startFailure] reads and is
// called from it, so an init failure lands on the Environment's existing
// condition path rather than on a second one of its own. What it reports is
// the program's own sentence, which names the step and the reason: the
// kubelet copies the termination log into the container status, so the
// account travels out of a pod nobody is watching without anything having to
// read a log.
//
// It is only about a step that *ran and failed*. An init container the
// kubelet would not create at all — an operator image that will not pull — is
// a refused container like any other and is already reported as one by
// pod_refusal.go, which walks a pod's init statuses beside its containers.
func volumeInitFailure(pod *corev1.Pod) string {
	for i := range pod.Status.InitContainerStatuses {
		status := &pod.Status.InitContainerStatuses[i]
		if status.Name != VolumeInitContainerName {
			continue
		}
		// Terminated is the failure as it happens; LastTerminationState is
		// the same failure once the kubelet has put the container into
		// backoff, which is what a reader arriving a minute later sees.
		for _, state := range []corev1.ContainerState{status.State, status.LastTerminationState} {
			if state.Terminated == nil || state.Terminated.ExitCode == 0 {
				continue
			}
			return volumeInitMessage(state.Terminated)
		}
	}
	return ""
}

// volumeInitMessage is what the step left behind, or a stand-in for a step
// that left nothing.
func volumeInitMessage(terminated *corev1.ContainerStateTerminated) string {
	if message := strings.TrimSpace(terminated.Message); message != "" {
		return message
	}
	return fmt.Sprintf("preparing this workload's volumes exited %d and said nothing: "+
		"the init container's log is the only account of it", terminated.ExitCode)
}

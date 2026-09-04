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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The posture an application's own workloads run under (#276).
//
// Nothing used to be applied to them at all: they ran as whatever the image
// happened to be, in a namespace deliberately relaxed to `privileged` for the
// build tooling's sake. That relaxation is the *build's* — rootless BuildKit
// needs an unconfined seccomp and AppArmor profile — and it is precisely why
// the lever for the application is here, per workload, rather than at the
// namespace level. Nothing in this file reaches a build job: the two build
// strategies keep the exemptions they need, and the application's own
// Deployment, its workers and its scheduled runs are what these contexts are
// written onto.
//
// It is [containerProbes]'s shape for the same reasons. Both are computed
// from what the Release snapshotted, both have a defensible default for a
// workload that declared nothing, and both are written *whole* every
// reconcile — a Deployment found with a posture that has since been withdrawn
// has to lose it, which an assignment that only ever tightened would never do.

// podSecurityContext is the pod half of the posture: who the containers run
// as, what owns the volumes they are given, and the seccomp profile they run
// under.
//
// The seccomp profile is the platform's default rather than the project's,
// and it is `RuntimeDefault` for every workload. It is the one hardening a
// working image does not notice — it is the container runtime's own profile,
// which Kubernetes simply does not apply unless asked — so there is nothing
// for a project to decide about it and no field that says so.
func podSecurityContext(security *kitchenv1alpha1.SecuritySpec) *corev1.PodSecurityContext {
	context := &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: kitchenv1alpha1.SeccompProfileRuntimeDefault},
	}
	if security == nil {
		return context
	}
	if security.RunAsNonRoot {
		context.RunAsNonRoot = ptr.To(true)
	}
	// Zero is the image's own user, left alone. It is not a request to run
	// as root: an image that runs as root already does, and writing uid 0
	// here would turn "the project said nothing" into a declaration.
	if security.RunAsUser > 0 {
		context.RunAsUser = ptr.To(security.RunAsUser)
	}
	if security.RunAsGroup > 0 {
		context.RunAsGroup = ptr.To(security.RunAsGroup)
	}
	// The gid the kubelet chowns mounted volumes to before the container
	// starts. Zero is the volume's own ownership left alone, the reading
	// RunAsUser's zero already has — and the reason the field exists at all
	// is that a freshly provisioned volume comes up owned by root, so a
	// workload running as anybody else is handed one it cannot write.
	//
	// The change policy is written only alongside a group, because that is
	// the only time the kubelet reads it, and an unset policy is left unset
	// rather than written as Kubernetes' default: writing `Always` would be
	// the platform declaring the recursive chown a project never asked for.
	if security.FSGroup > 0 {
		context.FSGroup = ptr.To(security.FSGroup)
		if security.FSGroupChangePolicy != "" {
			context.FSGroupChangePolicy = ptr.To(corev1.PodFSGroupChangePolicy(security.FSGroupChangePolicy))
		}
	}
	return context
}

// containerSecurityContext is the container half: what the process inside may
// do, and what it may write to.
//
// `allowPrivilegeEscalation` is written on every container whichever way it
// resolves, because false is the platform's default and true is a project
// taking it back — and a container found with one and since changed to the
// other has to move.
func containerSecurityContext(security *kitchenv1alpha1.SecuritySpec) *corev1.SecurityContext {
	context := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(security.EscalationAllowed()),
		ReadOnlyRootFilesystem:   ptr.To(false),
	}
	if security == nil {
		return context
	}
	context.ReadOnlyRootFilesystem = ptr.To(security.ReadOnlyRootFilesystem)
	if security.RunAsNonRoot {
		context.RunAsNonRoot = ptr.To(true)
	}
	if security.RunAsUser > 0 {
		context.RunAsUser = ptr.To(security.RunAsUser)
	}
	if security.RunAsGroup > 0 {
		context.RunAsGroup = ptr.To(security.RunAsGroup)
	}
	if drops := security.DropCapabilities; len(drops) > 0 {
		dropped := make([]corev1.Capability, 0, len(drops))
		for _, capability := range drops {
			dropped = append(dropped, corev1.Capability(strings.ToUpper(strings.TrimSpace(capability))))
		}
		context.Capabilities = &corev1.Capabilities{Drop: dropped}
	}
	return context
}

// applySecurityContext puts the posture on a pod and its container. It is one
// call rather than four assignments for the reason applyProbes is: a workload
// whose declaration has been withdrawn has to be written back to the
// platform's default, and a caller that only wrote what was asked for would
// leave yesterday's read-only filesystem on it for ever.
func applySecurityContext(
	pod *corev1.PodSpec,
	container *corev1.Container,
	security *kitchenv1alpha1.SecuritySpec,
) {
	pod.SecurityContext = podSecurityContext(security)
	container.SecurityContext = containerSecurityContext(security)
}

// startFailure is why an environment's workloads are not up, when the pods
// themselves know and nothing else does. It answers a condition reason, a
// sentence, and the refusal to record on the status — or nothing at all for an
// environment that is simply still rolling out, which is the ordinary case on
// every deploy.
//
// A workload fails to start in two ways, and neither of them reaches the
// Deployment. A container the kubelet will not create — an image that would
// run as root under `runAsNonRoot`, a uid it cannot resolve, a reference that
// does not parse, an image that will not pull — waits for ever in a reason
// pod_refusal.go calls terminal, with the explanation only on the pod. A
// container that starts and then cannot write to its own filesystem exits,
// restarts, exits again, and reports `CrashLoopBackOff`, which is the symptom
// three layers down from the cause.
//
// This is what turns either into a sentence on the Environment. It reads the
// pods rather than the Deployment because that is where the kubelet's own
// words are, and it names the constraints in force alongside them: for the
// first because they are the likeliest reason the container was refused, and
// for the second because a workload that crash-loops the day a posture is
// declared is nearly always crash-looping *on* it — a reader told what it is
// running under checks that first instead of last.
//
// The first case is reported whether or not this project declared anything,
// because a container the kubelet refused outright is worth a sentence
// either way; the second is reported only under a declared posture, since
// "it keeps exiting" on its own is what the logs are for.
//
// # Every workload, not only the web process
//
// The refusal is looked for across everything the environment keeps running —
// the web pods, the workers, the services — because #393's application was
// four workloads under one posture and the platform said nothing about any of
// them. What is deliberately *not* here is a pod that belongs to a Job: a
// deploy task's run is failed by its own guard and gates the deploy on itself
// (#391), and a scheduled run's is failed on its own row and in the activity
// feed. Neither is the environment's health, and an environment reported
// Degraded because last night's cron pod was refused would be the schedule's
// failure wearing the application's clothes.
//
// The crash loop stays the web process's alone, and only while the Deployment
// is unavailable: it is a guess about a cause, and a guess made about a worker
// nobody is waiting on is noise.
func (r *EnvironmentReconciler) startFailure(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	appNS string,
	security *kitchenv1alpha1.SecuritySpec,
	// webAvailable says the web Deployment has its replicas. The refusal is
	// looked for either way — a refused worker is a refused worker while the
	// URL answers — and the crash-loop guess is not.
	webAvailable bool,
) (reason, message string, refused *kitchenv1alpha1.WorkloadRefusalStatus) {
	log := ctrl.LoggerFrom(ctx)

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(appNS), client.MatchingLabels(map[string]string{
		labelEnvironment: env.Name,
	})); err != nil {
		// The environment is already being reported as it is; failing to
		// explain it is not itself a failure.
		log.V(1).Info("listing the environment's pods to explain a workload that has not started", "error", err)
		return "", "", nil
	}

	declared := security.Declared()
	now := time.Now()
	for i := range pods.Items {
		pod := &pods.Items[i]
		if podRunsAJob(pod) {
			continue
		}
		if refusal, found := refusalOf(pod, now); found {
			refused = &kitchenv1alpha1.WorkloadRefusalStatus{
				Workload:  workloadOfPod(pod),
				Pod:       refusal.Pod,
				Container: refusal.Container,
				Reason:    refusal.Reason,
				Message:   refusal.Sentence(),
			}
			return reasonContainerRefused,
				refusalMessage(refused.Workload, refusal.Sentence(), declared),
				refused
		}
	}
	if webAvailable {
		return "", "", nil
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if podRunsAJob(pod) || pod.Labels[LabelComponent] != ComponentWeb {
			continue
		}
		state, found := appContainerState(pod)
		if !found || state.Waiting == nil {
			continue
		}
		if len(declared) > 0 && state.Waiting.Reason == reasonCrashLoop {
			return reasonRestartingUnderPosture,
				refusalMessage(kitchenv1alpha1.WebProcessName, crashLoopUnderPosture, declared), nil
		}
	}
	return "", "", nil
}

// podRunsAJob reports whether a pod is one run of something rather than a
// workload that keeps running. The job-controller's own label is what says so,
// and it is on the pods of a deploy task and a scheduled run alike.
func podRunsAJob(pod *corev1.Pod) bool {
	_, found := pod.Labels["job-name"]
	return found
}

// workloadOfPod is which of the unit's workloads a pod belongs to, in the name
// the project wrote and the dashboard shows. The web process carries no
// process label — its workload is the environment's own name — so it is the
// answer when there is nothing else to read.
func workloadOfPod(pod *corev1.Pod) string {
	if name := pod.Labels[labelProcess]; name != "" {
		return name
	}
	return kitchenv1alpha1.WebProcessName
}

const (
	// reasonCrashLoop is the kubelet's word for a container that keeps
	// exiting. It says nothing about why, which is the whole problem.
	reasonCrashLoop = "CrashLoopBackOff"

	// The two condition reasons this produces. They name what the platform
	// observed — a container the kubelet would not create, and one that
	// keeps exiting under a posture the project declared — rather than
	// asserting a cause the platform cannot prove.
	reasonContainerRefused       = "ContainerRefused"
	reasonRestartingUnderPosture = "RestartingUnderPosture"

	// crashLoopUnderPosture stands in for the kubelet's message when there
	// is none: a container that exits on its own leaves its reason in its
	// output, not in its status.
	crashLoopUnderPosture = "the container starts and exits repeatedly"
)

// appContainerState is the state of the container the application runs in,
// which is the only one of an application pod's containers this reconciler
// writes.
func appContainerState(pod *corev1.Pod) (corev1.ContainerState, bool) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == AppContainerName {
			return status.State, true
		}
	}
	return corev1.ContainerState{}, false
}

// refusalMessage is the kubelet's account of the failure, whose workload it
// is about, and the posture the workload was started under — which is the half
// the kubelet cannot know.
//
// It opens in the developer's vocabulary rather than the cluster's, because
// the screen this reaches is a developer's: "the container could not be
// started" is what happened, and `CreateContainerConfigError` is the name
// Kubernetes gives it. The name is still there — it is in front of the
// kubelet's own sentence — for whoever is going to search for it.
func refusalMessage(workload, kubelet string, declared []string) string {
	kubelet = strings.TrimSpace(kubelet)
	if kubelet == "" {
		kubelet = "the kubelet gave no reason"
	}
	message := fmt.Sprintf("the container of %s could not be started: %s", workload, kubelet)
	if len(declared) == 0 {
		return message
	}
	return fmt.Sprintf(
		"%s. This project declares a security posture and the workload runs under it: %s. "+
			"Change it in the project's settings (spec.runtime.security), or in kitchen.json, and redeploy",
		message, strings.Join(declared, "; "))
}

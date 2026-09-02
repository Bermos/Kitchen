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
// as, and the seccomp profile they run under.
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

// startFailure is why an environment's pods are not up, when the pods
// themselves know and nothing else does. It answers a condition reason and a
// sentence, or two empty strings for an environment that is simply still
// rolling out — the ordinary case on every deploy.
//
// A tightened posture fails in two ways, and neither of them reaches the
// Deployment. A container the kubelet will not create — an image that would
// run as root under `runAsNonRoot`, a uid it cannot resolve — waits for ever
// in `CreateContainerConfigError`, with the explanation only on the pod. A
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
func (r *EnvironmentReconciler) startFailure(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	appNS string,
	security *kitchenv1alpha1.SecuritySpec,
) (reason, message string) {
	log := ctrl.LoggerFrom(ctx)

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(appNS), client.MatchingLabels(webLabels(map[string]string{
		labelEnvironment: env.Name,
	}))); err != nil {
		// The environment is already being reported as unavailable; failing
		// to explain why is not itself a failure.
		log.V(1).Info("listing the environment's pods to explain a stalled rollout", "error", err)
		return "", ""
	}

	declared := security.Declared()
	for i := range pods.Items {
		state, found := appContainerState(&pods.Items[i])
		if !found || state.Waiting == nil {
			continue
		}
		switch {
		case configErrorReasons[state.Waiting.Reason]:
			return reasonContainerRefused, refusalMessage(state.Waiting.Message, declared)
		case len(declared) > 0 && state.Waiting.Reason == reasonCrashLoop:
			return reasonRestartingUnderPosture, refusalMessage(crashLoopUnderPosture, declared)
		}
	}
	return "", ""
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

// configErrorReasons are the waiting reasons that mean the kubelet refused to
// create the container at all. A security context it cannot satisfy is one of
// the few things that produces them, and the message it leaves is the
// specific one — "container has runAsNonRoot and image will run as root".
var configErrorReasons = map[string]bool{
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
}

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

// refusalMessage is the kubelet's account of the failure plus the posture the
// workload was started under, which is the half the kubelet cannot know.
func refusalMessage(kubelet string, declared []string) string {
	kubelet = strings.TrimSpace(kubelet)
	if kubelet == "" {
		kubelet = "the container did not start"
	}
	if len(declared) == 0 {
		return kubelet
	}
	return fmt.Sprintf(
		"%s. This project declares a security posture and the workload runs under it: %s. "+
			"Change it in the project's settings (spec.runtime.security), or in kitchen.json, and redeploy",
		kubelet, strings.Join(declared, "; "))
}

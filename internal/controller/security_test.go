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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The posture an application's workloads run under (#276), acceptance
// criterion by acceptance criterion:
//
//   - a project can tighten its own posture and the platform applies it;
//   - a project that says nothing gets a defensible default rather than
//     nothing;
//   - and a workload that cannot start under the posture it asked for fails
//     legibly, naming the constraint.

const securityTestNamespace = "kitchen-app-shop"

// A project that declared nothing still runs under something. The two
// defaults are the ones a working image does not notice — the runtime's own
// seccomp profile and no privilege escalation — and deliberately not the
// three that would break one.
func TestAProjectThatDeclaresNothingGetsThePlatformsPosture(t *testing.T) {
	pod := podSecurityContext(nil)
	if pod.SeccompProfile == nil || pod.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("want the runtime's seccomp profile on every pod, got %+v", pod.SeccompProfile)
	}
	if pod.RunAsNonRoot != nil || pod.RunAsUser != nil || pod.RunAsGroup != nil {
		t.Fatalf("nothing about who the image runs as is the platform's to decide: %+v", pod)
	}

	container := containerSecurityContext(nil)
	if container.AllowPrivilegeEscalation == nil || *container.AllowPrivilegeEscalation {
		t.Fatalf("want privilege escalation denied by default, got %+v", container.AllowPrivilegeEscalation)
	}
	if container.ReadOnlyRootFilesystem == nil || *container.ReadOnlyRootFilesystem {
		t.Fatal("a read-only root filesystem is asked for, never assumed: an image that writes to its own is ordinary")
	}
	if container.Capabilities != nil {
		t.Fatalf("the platform drops no capability by default, got %+v", container.Capabilities)
	}
}

// What a project asks for is what its containers get.
func TestADeclaredPostureReachesTheContainer(t *testing.T) {
	security := &kitchenv1alpha1.SecuritySpec{
		RunAsNonRoot:           true,
		RunAsUser:              1001,
		RunAsGroup:             1000,
		ReadOnlyRootFilesystem: true,
		DropCapabilities:       []string{"ALL"},
	}

	pod := podSecurityContext(security)
	if !ptr.Deref(pod.RunAsNonRoot, false) || ptr.Deref(pod.RunAsUser, 0) != 1001 || ptr.Deref(pod.RunAsGroup, 0) != 1000 {
		t.Fatalf("the declared user did not reach the pod: %+v", pod)
	}

	container := containerSecurityContext(security)
	if !ptr.Deref(container.ReadOnlyRootFilesystem, false) {
		t.Fatal("the read-only root filesystem did not reach the container")
	}
	if container.Capabilities == nil || len(container.Capabilities.Drop) != 1 ||
		container.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Fatalf("the dropped capabilities did not reach the container: %+v", container.Capabilities)
	}
	if !security.DropsAll() {
		t.Fatal("dropping ALL is not read as dropping everything")
	}

	// The one field that loosens rather than tightens, because the platform
	// is the one that tightened it.
	allowed := containerSecurityContext(&kitchenv1alpha1.SecuritySpec{AllowPrivilegeEscalation: true})
	if !ptr.Deref(allowed.AllowPrivilegeEscalation, false) {
		t.Fatal("an image that needs a setuid binary cannot say so")
	}
}

// A constraint withdrawn has to leave the workload, which is why the posture
// is written whole every reconcile rather than only where it was asked for.
func TestAWithdrawnPostureIsTakenOffTheWorkload(t *testing.T) {
	pod := corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(1001))}}
	container := corev1.Container{SecurityContext: &corev1.SecurityContext{
		ReadOnlyRootFilesystem: ptr.To(true),
		Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}}

	applySecurityContext(&pod, &container, nil)

	if pod.SecurityContext.RunAsUser != nil {
		t.Fatalf("the withdrawn uid stayed on the pod: %+v", pod.SecurityContext)
	}
	if ptr.Deref(container.SecurityContext.ReadOnlyRootFilesystem, false) {
		t.Fatal("the withdrawn read-only root filesystem stayed on the container")
	}
	if container.SecurityContext.Capabilities != nil {
		t.Fatalf("the withdrawn capability drop stayed on the container: %+v", container.SecurityContext.Capabilities)
	}
}

// A container the kubelet refuses to create explains itself on the pod and
// nowhere else. This is what carries it onto the Environment, with the
// constraints in force beside it.
func TestAContainerRefusedUnderThePostureIsReportedInWords(t *testing.T) {
	const kubelet = "container has runAsNonRoot and image will run as root"
	r, env := securityReconcilerWithPod(t, corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: kubelet},
	})

	reason, message := r.startFailure(context.Background(), env, securityTestNamespace,
		&kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true})
	if reason != reasonContainerRefused {
		t.Fatalf("want the refusal reported as %s, got %q", reasonContainerRefused, reason)
	}
	if !strings.Contains(message, kubelet) || !strings.Contains(message, "it must not run as root") {
		t.Fatalf("the message names neither the kubelet's reason nor the constraint: %q", message)
	}
}

// A container that starts and dies says only CrashLoopBackOff, which is the
// symptom three layers down from a posture it cannot live under. It is
// reported only where a posture was declared: "it keeps exiting" on its own
// is what the logs are for.
func TestACrashLoopUnderADeclaredPostureNamesThePosture(t *testing.T) {
	state := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoop}}
	r, env := securityReconcilerWithPod(t, state)

	reason, message := r.startFailure(context.Background(), env, securityTestNamespace,
		&kitchenv1alpha1.SecuritySpec{ReadOnlyRootFilesystem: true})
	if reason != reasonRestartingUnderPosture {
		t.Fatalf("want the crash loop attributed to the posture, got %q", reason)
	}
	if !strings.Contains(message, "read only") {
		t.Fatalf("the message does not name the constraint in force: %q", message)
	}

	if reason, _ := r.startFailure(context.Background(), env, securityTestNamespace, nil); reason != "" {
		t.Fatalf("a crash loop under no posture is not the posture's, got %q", reason)
	}
}

// The ordinary case: pods that are simply still coming up explain nothing,
// because there is nothing to explain.
func TestAPodStillStartingIsNotAFailure(t *testing.T) {
	r, env := securityReconcilerWithPod(t, corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
	})

	reason, message := r.startFailure(context.Background(), env, securityTestNamespace,
		&kitchenv1alpha1.SecuritySpec{ReadOnlyRootFilesystem: true})
	if reason != "" || message != "" {
		t.Fatalf("a pod on its way up is not a refusal: %q / %q", reason, message)
	}
}

// securityReconcilerWithPod is a reconciler over one web pod of one
// environment, in the state the caller names.
func securityReconcilerWithPod(t *testing.T, state corev1.ContainerState) (
	*EnvironmentReconciler, *kitchenv1alpha1.Environment,
) {
	t.Helper()

	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-production", Namespace: "kitchen-system"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shop-production-abc",
			Namespace: securityTestNamespace,
			Labels:    webLabels(map[string]string{labelEnvironment: env.Name}),
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: AppContainerName, State: state},
		}},
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(env, pod).Build()
	return &EnvironmentReconciler{Client: c, Scheme: scheme}, env
}

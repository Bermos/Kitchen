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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	// Nor who owns the volumes it is given: a project that says nothing gets
	// no fsGroup, exactly as before the field existed.
	if pod.FSGroup != nil || pod.FSGroupChangePolicy != nil {
		t.Fatalf("the ownership of a volume is the project's to declare, not the platform's: %+v", pod)
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
	pod := corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{
		RunAsUser: ptr.To(int64(1001)),
		FSGroup:   ptr.To(int64(1001)),
	}}
	container := corev1.Container{SecurityContext: &corev1.SecurityContext{
		ReadOnlyRootFilesystem: ptr.To(true),
		Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}}

	applySecurityContext(&pod, &container, nil)

	if pod.SecurityContext.RunAsUser != nil {
		t.Fatalf("the withdrawn uid stayed on the pod: %+v", pod.SecurityContext)
	}
	if pod.SecurityContext.FSGroup != nil {
		t.Fatalf("the withdrawn volume group stayed on the pod: %+v", pod.SecurityContext)
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

	reason, message, refused := r.startFailure(context.Background(), env, securityTestNamespace,
		unitPosture(&kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true}), false)
	if reason != reasonContainerRefused {
		t.Fatalf("want the refusal reported as %s, got %q", reasonContainerRefused, reason)
	}
	if !strings.Contains(message, kubelet) || !strings.Contains(message, "it must not run as root") {
		t.Fatalf("the message names neither the kubelet's reason nor the constraint: %q", message)
	}
	if !strings.Contains(message, "could not be started") {
		t.Fatalf("the message is not in the vocabulary of whoever deployed it: %q", message)
	}
	// The operator's half: which pod is carrying it, recorded on the status
	// rather than said in the sentence a developer reads.
	if refused == nil || refused.Pod != "shop-production-abc" || refused.Container != AppContainerName {
		t.Fatalf("the refusal does not say where to look: %+v", refused)
	}
	if refused.Workload != kitchenv1alpha1.WebProcessName || refused.Reason != "CreateContainerConfigError" {
		t.Fatalf("the refusal does not say what was refused: %+v", refused)
	}
}

// A container that starts and dies says only CrashLoopBackOff, which is the
// symptom three layers down from a posture it cannot live under. It is
// reported only where a posture was declared: "it keeps exiting" on its own
// is what the logs are for.
func TestACrashLoopUnderADeclaredPostureNamesThePosture(t *testing.T) {
	state := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoop}}
	r, env := securityReconcilerWithPod(t, state)

	reason, message, _ := r.startFailure(context.Background(), env, securityTestNamespace,
		unitPosture(&kitchenv1alpha1.SecuritySpec{ReadOnlyRootFilesystem: true}), false)
	if reason != reasonRestartingUnderPosture {
		t.Fatalf("want the crash loop attributed to the posture, got %q", reason)
	}
	if !strings.Contains(message, "read only") {
		t.Fatalf("the message does not name the constraint in force: %q", message)
	}

	if reason, _, _ := r.startFailure(
		context.Background(), env, securityTestNamespace, unitPosture(nil), false); reason != "" {
		t.Fatalf("a crash loop under no posture is not the posture's, got %q", reason)
	}

	// Nor is it the environment's while the web process is available: a
	// container that has started is not one the kubelet refused, and the
	// guess is only worth making about a workload somebody is waiting on.
	if reason, _, _ := r.startFailure(context.Background(), env, securityTestNamespace,
		unitPosture(&kitchenv1alpha1.SecuritySpec{ReadOnlyRootFilesystem: true}), true); reason != "" {
		t.Fatalf("a crash loop behind a working URL is not a start failure, got %q", reason)
	}
}

// The ordinary case: pods that are simply still coming up explain nothing,
// because there is nothing to explain.
func TestAPodStillStartingIsNotAFailure(t *testing.T) {
	r, env := securityReconcilerWithPod(t, corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
	})

	reason, message, _ := r.startFailure(context.Background(), env, securityTestNamespace,
		unitPosture(&kitchenv1alpha1.SecuritySpec{ReadOnlyRootFilesystem: true}), false)
	if reason != "" || message != "" {
		t.Fatalf("a pod on its way up is not a refusal: %q / %q", reason, message)
	}
}

// unitPosture is a Release's frozen configuration declaring one posture and
// no workloads: the shape every case that is about the *unit's* posture wants,
// now that what a workload runs under is that with its own merged over it.
func unitPosture(security *kitchenv1alpha1.SecuritySpec) kitchenv1alpha1.ConfigSnapshot {
	return kitchenv1alpha1.ConfigSnapshot{
		Runtime: kitchenv1alpha1.RuntimeSpec{Security: security},
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

// The volume group (#347). A freshly provisioned volume comes up owned by
// root, so a workload that declared a user of its own is handed one it cannot
// write — it starts, reads as healthy, and fails on its first write. These
// pin the three things that stops:
//
//   - the declared group reaches the pod that mounts the volume, for every
//     workload an environment materializes and not only the web process;
//   - the change policy is written only where there is a group to apply, and
//     never on the platform's own initiative;
//   - and both leave the workload when the declaration does, which the
//     withdrawal test above covers alongside the rest of the posture.

// A non-root workload handed a volume can write it: the gid the project
// declared is on the pod the claim is mounted into, for every shape of
// workload the environment materializes.
func TestTheVolumeGroupReachesEveryWorkloadThatMountsAClaim(t *testing.T) {
	security := &kitchenv1alpha1.SecuritySpec{
		RunAsNonRoot:        true,
		RunAsUser:           1001,
		RunAsGroup:          1001,
		FSGroup:             1001,
		FSGroupChangePolicy: kitchenv1alpha1.FSGroupChangeOnRootMismatch,
	}
	release := &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-rel-1", Namespace: "kitchen-system"},
		Spec: kitchenv1alpha1.ReleaseSpec{
			Image: "registry.example.com/shop@sha256:abc",
			ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
				Runtime: kitchenv1alpha1.RuntimeSpec{Security: security},
			},
		},
	}
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "kitchen-system"},
		Spec: kitchenv1alpha1.ProjectSpec{
			Registry: &kitchenv1alpha1.RegistrySpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "harbor"},
			},
		},
	}
	// The claim the workload is given, mounted where it asked for it.
	mounts := []mountedVolume{{
		claim:     "shop-data",
		claimName: "shop-data-production",
		mountPath: "/data",
	}}

	// The four shapes that go through the process pod: a worker, a service,
	// a scheduled run and a task that runs once per deploy.
	for name, process := range map[string]kitchenv1alpha1.ProcessSpec{
		"worker":  {Name: "worker", Type: kitchenv1alpha1.ProcessWorker},
		"service": {Name: "api", Type: kitchenv1alpha1.ProcessService, Port: 8080},
		"cron":    {Name: "nightly", Type: kitchenv1alpha1.ProcessCron, Schedule: "0 3 * * *"},
		"task":    {Name: "migrate", Type: kitchenv1alpha1.ProcessTask},
	} {
		t.Run(name, func(t *testing.T) {
			spec := processPodSpec("env", release, project, nil, process, mounts, podInit{})
			assertVolumeIsWritable(t, spec)
		})
	}

	// And the web process, which is built by the reconciler rather than by
	// processPodSpec — the one place the two could come to disagree.
	t.Run("web", func(t *testing.T) {
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: "shop-production", Namespace: "kitchen-system"},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				Type:       kitchenv1alpha1.EnvironmentProduction,
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			},
		}
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		r := &EnvironmentReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(env).Build(),
			Scheme: scheme,
		}
		labels := map[string]string{labelEnvironment: env.Name}
		if err := r.applyDeployment(context.Background(), env, release, project,
			securityTestNamespace, labels, nil, 1, false, false, mounts, podInit{}); err != nil {
			t.Fatal(err)
		}
		deploy := &appsv1.Deployment{}
		if err := r.Get(context.Background(), client.ObjectKey{
			Name: env.Name, Namespace: securityTestNamespace,
		}, deploy); err != nil {
			t.Fatal(err)
		}
		assertVolumeIsWritable(t, deploy.Spec.Template.Spec)
	})
}

// assertVolumeIsWritable is the two halves that have to meet: the volume is
// mounted, and the pod carries the group the kubelet chowns it to before the
// container starts.
func assertVolumeIsWritable(t *testing.T, spec corev1.PodSpec) {
	t.Helper()
	if len(spec.Volumes) != 1 || spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatalf("the claim is not mounted, so there is nothing to own: %+v", spec.Volumes)
	}
	if spec.SecurityContext == nil {
		t.Fatal("the workload has no pod security context at all")
	}
	if ptr.Deref(spec.SecurityContext.FSGroup, 0) != 1001 {
		t.Fatalf("a workload running as 1001 was handed a volume owned by somebody else: %+v",
			spec.SecurityContext)
	}
	if spec.SecurityContext.FSGroupChangePolicy == nil ||
		*spec.SecurityContext.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Fatalf("the declared change policy did not reach the pod: %+v", spec.SecurityContext)
	}
}

// The change policy is Kubernetes' to default, not the platform's: an unset
// policy is left unset rather than written as `Always`, and a policy without
// a group to apply is nothing the kubelet ever reads.
func TestTheChangePolicyIsWrittenOnlyBesideAGroup(t *testing.T) {
	pod := podSecurityContext(&kitchenv1alpha1.SecuritySpec{FSGroup: 1001})
	if ptr.Deref(pod.FSGroup, 0) != 1001 {
		t.Fatalf("the declared group did not reach the pod: %+v", pod)
	}
	if pod.FSGroupChangePolicy != nil {
		t.Fatalf("an unset policy is Kubernetes' default, not a declaration to write: %+v", pod)
	}

	// Admission and the API both refuse this pair; it can still only ever
	// mean what an unset group means.
	alone := podSecurityContext(&kitchenv1alpha1.SecuritySpec{
		FSGroupChangePolicy: kitchenv1alpha1.FSGroupChangeOnRootMismatch,
	})
	if alone.FSGroup != nil || alone.FSGroupChangePolicy != nil {
		t.Fatalf("a policy with no group to apply reached the pod: %+v", alone)
	}
}

// The posture in words carries it too, which is what the environment's
// condition and the release's config diff both read.
func TestTheVolumeGroupIsNamedInTheDeclaredPosture(t *testing.T) {
	declared := (&kitchenv1alpha1.SecuritySpec{
		FSGroup:             1001,
		FSGroupChangePolicy: kitchenv1alpha1.FSGroupChangeOnRootMismatch,
	}).Declared()
	if len(declared) != 1 || !strings.Contains(declared[0], "gid 1001") {
		t.Fatalf("want the volume ownership named in the posture, got %v", declared)
	}
	if !strings.Contains(declared[0], "root does not already match") {
		t.Fatalf("want the change policy named beside it, got %v", declared)
	}
}

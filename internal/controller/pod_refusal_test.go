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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// A pod the kubelet will not start (#391, #393).
//
// The classification is the one thing everything else in both issues rests
// on: a container that is *refused* is not a container that is slow, and the
// whole failure was that nothing on the platform could tell the two apart.

// The reasons that mean this spec will never produce a running container, and
// the ones that only mean it has not yet.
func TestWhatCountsAsAPodTheKubeletWillNotStart(t *testing.T) {
	const kubelet = "container has runAsNonRoot and image has non-numeric user (node)"
	now := time.Now()

	for name, tc := range map[string]struct {
		reason string
		// waited is how long the pod has been the kubelet's problem, which
		// only the image-pull reasons care about.
		waited time.Duration
		want   bool
	}{
		"a container it cannot configure":       {reason: "CreateContainerConfigError", want: true},
		"a container it could not create":       {reason: "CreateContainerError", want: true},
		"a reference that does not parse":       {reason: "InvalidImageName", want: true},
		"an image the node was told not to get": {reason: "ErrImageNeverPull", want: true},
		// The two that are sometimes temporary. The threshold is the build
		// guard's own, so that the platform has one answer to "long enough".
		"a pull still backing off": {reason: "ImagePullBackOff", waited: time.Minute, want: false},
		"a pull that never worked": {reason: "ImagePullBackOff", waited: imagePullGrace + time.Minute, want: true},
		"a pull that just failed":  {reason: "ErrImagePull", waited: time.Second, want: false},
		"a pull failing for ever":  {reason: "ErrImagePull", waited: imagePullGrace, want: true},
		// The ordinary path, and the failure that is not this one.
		"a container being created":                {reason: "ContainerCreating", waited: time.Hour, want: false},
		"a pod behind its init":                    {reason: "PodInitializing", waited: time.Hour, want: false},
		"a container that started and keeps dying": {reason: reasonCrashLoop, waited: time.Hour, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			pod := waitingPod("shop-production-abc", tc.reason, kubelet)
			pod.Status.StartTime = &metav1.Time{Time: now.Add(-tc.waited)}

			refusal, found := refusalOf(pod, now)
			if found != tc.want {
				t.Fatalf("refusalOf(%s after %s) = %v, want %v", tc.reason, tc.waited, found, tc.want)
			}
			if !found {
				return
			}
			if refusal.Reason != tc.reason || refusal.Pod != pod.Name || refusal.Container != AppContainerName {
				t.Fatalf("the refusal does not say what was refused or where: %+v", refusal)
			}
			// The kubelet's own sentence, kept: it names the field and the
			// image, and nothing above it knows as much.
			if !strings.Contains(refusal.Sentence(), kubelet) ||
				!strings.Contains(refusal.Sentence(), tc.reason) {
				t.Fatalf("the kubelet's account was not carried: %q", refusal.Sentence())
			}
		})
	}
}

// A pod that has run, or is on its way out, is never a refusal — whatever its
// containers still say they are waiting for.
func TestAPodThatIsFinishedOrGoingIsNotARefusal(t *testing.T) {
	now := time.Now()

	for name, prepare := range map[string]func(pod *corev1.Pod){
		"one that succeeded": func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodSucceeded },
		"one that failed":    func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodFailed },
		"one being deleted": func(pod *corev1.Pod) {
			pod.DeletionTimestamp = &metav1.Time{Time: now}
			pod.Finalizers = []string{"kubernetes"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			pod := waitingPod("shop-production-abc", "CreateContainerConfigError", "refused")
			prepare(pod)
			if _, found := refusalOf(pod, now); found {
				t.Fatal("a pod that had its chance, or is already going, is not one the kubelet is refusing")
			}
		})
	}
}

// The init container is read first, because the first thing refused is the
// thing that stopped the pod — everything behind it reports PodInitializing,
// which explains nothing.
func TestTheRefusalIsTheFirstContainerRefused(t *testing.T) {
	pod := waitingPod("shop-production-abc", "PodInitializing", "")
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "clone",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CreateContainerConfigError", Message: "cannot verify user is non-root",
		}},
	}}

	refusal, found := refusalOf(pod, time.Now())
	if !found || refusal.Container != "clone" {
		t.Fatalf("want the init container's refusal, got %+v (found %v)", refusal, found)
	}
}

// #393's own case, in the shape it was found in: four workloads under one
// posture, and the platform saying nothing about any of them. A worker is
// refused while the web process is perfectly available, and the environment
// still has to say so.
func TestAWorkerRefusedIsReportedWhileTheURLAnswers(t *testing.T) {
	const kubelet = "container has runAsNonRoot and image has non-numeric user (node), " +
		"cannot verify user is non-root"

	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-production", Namespace: "kitchen-system"},
	}
	worker := waitingPod("shop-production-worker-xyz", "CreateContainerConfigError", kubelet)
	worker.Namespace = securityTestNamespace
	worker.Labels = processLabels(map[string]string{labelEnvironment: env.Name}, "worker")

	r := refusalReconciler(t, env, worker)
	reason, message, refused := r.startFailure(context.Background(), env, securityTestNamespace,
		unitPosture(&kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true}), true)

	if reason != reasonContainerRefused {
		t.Fatalf("a refused worker behind a working URL was not reported: %q", reason)
	}
	if !strings.Contains(message, "worker") || !strings.Contains(message, kubelet) {
		t.Fatalf("the message names neither the workload nor the kubelet's sentence: %q", message)
	}
	if refused == nil || refused.Workload != "worker" || refused.Pod != worker.Name {
		t.Fatalf("the refusal does not say which workload or where: %+v", refused)
	}
}

// A run's pod is not the environment's health. A deploy task's refusal gates
// the deploy on its own row, and a scheduled run's is a schedule's failure —
// neither is the application being down, and reporting one as the other would
// paint an environment red for last night's cron.
func TestARunsPodIsNotTheEnvironmentsRefusal(t *testing.T) {
	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-production", Namespace: "kitchen-system"},
	}
	run := waitingPod("shop-production-nightly-29", "CreateContainerConfigError", "refused")
	run.Namespace = securityTestNamespace
	run.Labels = processLabels(map[string]string{labelEnvironment: env.Name}, "nightly")
	run.Labels["job-name"] = "shop-production-nightly-29"

	r := refusalReconciler(t, env, run)
	reason, _, refused := r.startFailure(
		context.Background(), env, securityTestNamespace, unitPosture(nil), true)
	if reason != "" || refused != nil {
		t.Fatalf("a scheduled run's refusal was reported as the environment's: %q / %+v", reason, refused)
	}
}

// #391 itself: the run is ended, with the kubelet's sentence, rather than
// waiting on a Job that will never fail.
func TestARefusedRunIsFailedWithTheKubeletsSentence(t *testing.T) {
	const kubelet = "container has runAsNonRoot and image has non-numeric user (node)"

	pod := waitingPod("shop-production-migrate-1-abc", "CreateContainerConfigError", kubelet)
	pod.Namespace = securityTestNamespace
	pod.Labels = map[string]string{"job-name": "shop-production-migrate-1"}

	r := refusalReconciler(t, pod)
	run := kitchenv1alpha1.ProcessRun{
		Name:  "shop-production-migrate-1",
		Phase: kitchenv1alpha1.RunRunning,
	}
	refused, err := r.refuseRun(context.Background(), securityTestNamespace, &run)
	if err != nil {
		t.Fatal(err)
	}
	if !refused {
		t.Fatal("a run whose pod can never start is still reported as running")
	}
	if run.Phase != kitchenv1alpha1.RunFailed || !run.Refused {
		t.Fatalf("the run was not ended as a refusal: %+v", run)
	}
	if !strings.Contains(run.Message, kubelet) {
		t.Fatalf("the run does not carry the kubelet's own account: %q", run.Message)
	}
	if run.FinishedAt == nil {
		t.Fatal("a run that ended has an end")
	}

	// A verdict the Job already reached is the better evidence, and a
	// container that ran is not one that was refused.
	finished := kitchenv1alpha1.ProcessRun{Name: run.Name, Phase: kitchenv1alpha1.RunSucceeded}
	if refused, err := r.refuseRun(context.Background(), securityTestNamespace, &finished); err != nil ||
		refused {
		t.Fatalf("a finished run was overwritten by the refusal guard: %+v (%v)", finished, err)
	}
}

// Ending the run is not what stops it: the job-controller holds the pod until
// the Job goes, which is what left #391 needing `kubectl delete job`.
func TestTheRefusedRunsJobIsDeleted(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-production-migrate-1", Namespace: securityTestNamespace,
	}}
	r := refusalReconciler(t, job)

	r.deleteRefusedJob(context.Background(), securityTestNamespace, job.Name)

	err := r.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the wedged job is still there: %v", err)
	}
	// Deleting one that is already gone is the normal case on a later pass.
	r.deleteRefusedJob(context.Background(), securityTestNamespace, job.Name)
}

// waitingPod is one application pod waiting under the reason given.
func waitingPod(name, reason, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: securityTestNamespace},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  AppContainerName,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
			}},
		},
	}
}

// refusalReconciler is an environment reconciler over the objects given.
func refusalReconciler(t *testing.T, objects ...client.Object) *EnvironmentReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &EnvironmentReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Scheme: scheme,
	}
}

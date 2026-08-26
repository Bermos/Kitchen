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
	"io"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	// buildFailureLogLines is how much of the failing container's output is
	// copied onto the Build. It is a tail, not a log: enough to carry the
	// stack trace or the compiler's last complaint, not so much that a Build
	// object becomes a place logs are stored.
	buildFailureLogLines = 40

	// buildFailureLogBytes caps that tail by size as well as by lines,
	// because one line of a bundler's output can be longer than forty of
	// anything else.
	buildFailureLogBytes = 16 << 10

	// buildFailureMessageMax bounds the kubelet's own message about a
	// terminated container. It is the container's termination log, which a
	// builder writes its metadata into — on a failure it is usually empty and
	// occasionally the first half of a report.
	buildFailureMessageMax = 512

	// reasonContainerError is what the kubelet calls every non-zero exit that
	// is not something more specific. It adds nothing to "exited 1" and is
	// left out of the assembled message for that reason.
	reasonContainerError = "Error"

	// reasonPodInitializing is the waiting reason of every container that is
	// simply behind an init container. It is not a failure, and it is what a
	// naive read of a pod whose *init* container failed reports instead of
	// the failure.
	reasonPodInitializing = "PodInitializing"
)

// PodLogReader reads the tail of one container's output.
//
// It is a function rather than a client because reading logs is the one thing
// the manager's cached client cannot do — it is a subresource, served by the
// kubelet through the API server — and because a test has no kubelet.
type PodLogReader func(ctx context.Context, namespace, pod, container string, lines int64) (string, error)

// clientsetPodLogs reads container logs through a typed client.
func clientsetPodLogs(clientset kubernetes.Interface) PodLogReader {
	return func(ctx context.Context, namespace, pod, container string, lines int64) (string, error) {
		stream, err := clientset.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
			Container:  container,
			TailLines:  ptr.To(lines),
			LimitBytes: ptr.To(int64(buildFailureLogBytes)),
		}).Stream(ctx)
		if err != nil {
			return "", err
		}
		defer func() { _ = stream.Close() }()
		out, err := io.ReadAll(stream)
		return string(out), err
	}
}

// diagnoseJobFailure is why the build job failed, read off the pod it ran in.
//
// The Job says only that it failed, in a sentence that is the same for every
// build that ever failed. The pod knows which container stopped, how it
// exited, and what it printed on the way out — but only for as long as it
// exists, which is the job's TTL. This is the moment that knowledge is
// available and the Build is the thing that outlives it, so this is where the
// copy is taken.
//
// It never returns nil: a pod that cannot be read or has nothing to say still
// produces the Job's own message, so that the Build always carries a failure
// rather than sometimes carrying one.
func (r *BuildReconciler) diagnoseJobFailure(
	ctx context.Context, job *batchv1.Job, jobMessage string,
) *kitchenv1alpha1.BuildFailureStatus {
	log := ctrl.LoggerFrom(ctx)

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		log.Error(err, "listing the build job's pods", "namespace", job.Namespace, "job", job.Name)
		return &kitchenv1alpha1.BuildFailureStatus{Message: jobMessage}
	}

	pod := failedPod(pods.Items)
	if pod == nil {
		return r.failureWithoutPod(ctx, job, jobMessage)
	}
	failure := failureFromPod(pod)
	if failure == nil {
		// The pod exists and has nothing to say, which is not the same
		// question as there being no pod: the Job's events are about
		// creating one, and there was one.
		return &kitchenv1alpha1.BuildFailureStatus{Message: jobMessage}
	}
	if failure.Container != "" {
		failure.Log = r.containerLogTail(ctx, pod, failure.Container)
	}
	return failure
}

// failureWithoutPod is the failure of a build that has no pod to ask.
//
// The Job's own sentence is the same for every build that ever failed, and
// when there is no pod behind it there is nothing to make it specific — except
// on the Job itself, where the controllers put what never becomes pod state.
// A pod refused at admission, refused by a quota, or refused for a service
// account that is not there leaves a warning event on the Job and nothing
// anywhere else, so that is where this looks before giving up.
func (r *BuildReconciler) failureWithoutPod(
	ctx context.Context, job *batchv1.Job, jobMessage string,
) *kitchenv1alpha1.BuildFailureStatus {
	warning := latestWarning(ctx, r.APIReader, &job.ObjectMeta)
	if warning == "" {
		return &kitchenv1alpha1.BuildFailureStatus{Message: jobMessage}
	}
	return &kitchenv1alpha1.BuildFailureStatus{
		Reason:  reasonJobNoPod,
		Message: withReason(jobMessage, warning),
	}
}

// failedPod is the pod that carries the failure. A build job runs one pod at a
// time, but a pod deleted out from under the job leaves the job to create
// another, so the one that failed is preferred over the one that is newest.
func failedPod(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		if pods[i].Status.Phase == corev1.PodFailed {
			return &pods[i]
		}
	}
	if len(pods) > 0 {
		return &pods[len(pods)-1]
	}
	return nil
}

// failureFromPod is the container that ended the build, chosen out of a pod
// that has several.
//
// Init containers are considered first and in their own order, because the
// first thing to fail is the thing that failed: whatever comes after it either
// never ran or ran on what the failure left behind. Reading the containers in
// pod order and taking the first that has *anything* to say is the mistake
// this exists to avoid — for a buildpacks build that is always the clone, and
// the clone always succeeded, so a failed build reports "Completed: exit code
// 0" and the builder's actual failure is never seen.
func failureFromPod(pod *corev1.Pod) *kitchenv1alpha1.BuildFailureStatus {
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for i := range statuses {
			if failure := containerFailure(&statuses[i]); failure != nil {
				return failure
			}
		}
	}
	// Nothing in the pod failed and yet the pod did, so it was ended from
	// outside: evicted for disk, killed for the job's deadline, lost with the
	// node. That verdict lives on the pod, and it is the one explanation no
	// container can give.
	if pod.Status.Reason != "" || pod.Status.Message != "" {
		return &kitchenv1alpha1.BuildFailureStatus{
			Reason:  pod.Status.Reason,
			Message: withReason(pod.Status.Reason, truncate(pod.Status.Message, buildFailureMessageMax)),
		}
	}
	return nil
}

// containerFailure is one container's account of itself, or nil when it has no
// complaint: it exited cleanly, is still running, or is merely waiting for the
// init containers in front of it.
func containerFailure(status *corev1.ContainerStatus) *kitchenv1alpha1.BuildFailureStatus {
	if terminated := status.State.Terminated; terminated != nil && terminated.ExitCode != 0 {
		return &kitchenv1alpha1.BuildFailureStatus{
			Container: status.Name,
			ExitCode:  ptr.To(terminated.ExitCode),
			Reason:    terminated.Reason,
			Message:   terminatedMessage(status.Name, terminated),
		}
	}
	// A container that never started is a failure the exit code cannot
	// describe: the image would not pull, or the pod names a secret that is
	// not there. Both are waiting reasons, and both stop the build.
	if waiting := status.State.Waiting; waiting != nil &&
		waiting.Reason != "" && waiting.Reason != reasonPodInitializing {
		return &kitchenv1alpha1.BuildFailureStatus{
			Container: status.Name,
			Reason:    waiting.Reason,
			Message: fmt.Sprintf("%s never started: %s", status.Name,
				withReason(waiting.Reason, truncate(waiting.Message, buildFailureMessageMax))),
		}
	}
	return nil
}

// terminatedMessage is the one line a terminated container is worth.
func terminatedMessage(name string, terminated *corev1.ContainerStateTerminated) string {
	message := fmt.Sprintf("%s exited %d", name, terminated.ExitCode)
	if terminated.Reason != "" && terminated.Reason != reasonContainerError {
		message += " (" + terminated.Reason + ")"
	}
	if detail := strings.TrimSpace(terminated.Message); detail != "" {
		message += ": " + truncate(detail, buildFailureMessageMax)
	}
	return message
}

// containerLogTail is the last of what the failing container printed, or
// nothing at all.
//
// Nothing at all is a normal answer and never an error the build hears about:
// the pod may already be gone, the installation may not grant the log
// subresource, and neither is a reason to lose the exit code that is already
// in hand.
func (r *BuildReconciler) containerLogTail(ctx context.Context, pod *corev1.Pod, container string) []string {
	if r.PodLogs == nil {
		return nil
	}
	out, err := r.PodLogs(ctx, pod.Namespace, pod.Name, container, buildFailureLogLines)
	if err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("reading the failing container's log",
			"pod", pod.Name, "container", container, "error", err.Error())
		return nil
	}
	return tailLines(out, buildFailureLogLines)
}

// tailLines is the last lines of a blob of output, oldest first, with the
// blank ones at either end dropped.
func tailLines(out string, limit int) []string {
	lines := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		lines = append(lines, strings.TrimRight(line, "\r "))
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	if len(lines) == 0 {
		return nil
	}
	return lines
}

// failureMessage is what the Build's condition and the commit's status check
// say, which is the failure when there is one and the Job's own sentence when
// there is not.
func failureMessage(failure *kitchenv1alpha1.BuildFailureStatus, jobMessage string) string {
	if failure == nil || failure.Message == "" {
		return jobMessage
	}
	return failure.Message
}

// withReason joins a reason and a message the way everything that reports on
// the cluster shows them.
func withReason(reason, message string) string {
	switch {
	case reason == "":
		return message
	case message == "":
		return reason
	default:
		return reason + ": " + message
	}
}

// truncate bounds a message from somewhere else, saying that it did.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

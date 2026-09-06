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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// A pod the kubelet will not start, recognised as such (#391, #393).
//
// The platform already knows how to end a *build* whose Job can never create a
// pod — build_stall.go, and the reason it exists is that nothing about such a
// Job ever changes, so "Running" was the last thing it had to say for itself.
// This is the other half of the same fault, one layer down: the pod was
// created, the kubelet took it, and it refused to create the container. The
// Job's counters then read one active pod for ever, `backoffLimit` is never
// approached, and everything above it reports a workload that is starting.
//
// It is one classification, used by every workload the platform materializes —
// a deploy task's Job, a scheduled run's, the web Deployment's pods and the
// workers' — because "the kubelet will not start this container" is one fact
// however it is packaged, and a second list of reasons would eventually
// disagree with this one.
//
// # What is terminal and what is merely slow
//
// The difference is whether *this spec* can ever produce a running container:
//
//   - `CreateContainerConfigError` and `CreateContainerError` are the
//     kubelet's answer to a container it cannot configure. `runAsNonRoot`
//     against an image whose `USER` is a name is the case that produced both
//     issues, and the message the kubelet leaves is the whole diagnosis.
//   - `InvalidImageName` is a reference that cannot be parsed. Nothing about
//     retrying changes it.
//   - `ErrImageNeverPull` is a pull policy of `Never` against an image the
//     node does not have. The node is not going to acquire it by waiting.
//   - `ImagePullBackOff` and `ErrImagePull` are the two that are *sometimes*
//     temporary — a registry that is down, a credential a moment behind the
//     pod — so they are terminal only once they have persisted, which is what
//     imagePullGrace is.
//
// Everything else a container waits under is normal: `ContainerCreating` and
// `PodInitializing` are the ordinary path, and a `CrashLoopBackOff` container
// did start, which is a different fault reported in different words.
var refusedReasons = map[string]bool{
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"InvalidImageName":           true,
	"ErrImageNeverPull":          true,
}

// imagePullReasons are the two waiting reasons that are a refusal only after
// they have gone on long enough. Both are what the kubelet reports while it
// is still retrying, and a pull that succeeds on the third attempt has passed
// through them.
var imagePullReasons = map[string]bool{
	"ImagePullBackOff": true,
	"ErrImagePull":     true,
}

// imagePullGrace is how long an image pull may keep failing before the
// platform calls it a refusal.
//
// It is buildStallDeadline, deliberately and not by coincidence: that number
// is already the platform's answer to "how long may something the cluster
// cannot start stay unexplained", chosen so that the cluster's own account of
// it — an event — still exists when the verdict is given. A second number
// here would be a second answer to one question, and the two would drift.
const imagePullGrace = buildStallDeadline

// podRefusal is one pod's refusal: which container the kubelet would not
// create, under which reason, and what it said about it.
//
// The message is the kubelet's own and is never rewritten. It names the field
// and the image — "container has runAsNonRoot and image has non-numeric user
// (node), cannot verify user is non-root" — which is more than anything above
// it knows, and it is the sentence somebody has to read to fix this.
type podRefusal struct {
	Pod       string
	Container string
	Reason    string
	Message   string

	// ExitCode is set only for a container that terminated, which is the
	// half of this #442 added: a container the kubelet *created* and could
	// not start ends as a terminated container with a reason, not as a
	// waiting one. It is nil for every refusal read off a waiting container,
	// which has no exit status because it never had a process.
	ExitCode *int32
}

// Sentence is the refusal as a line, the kubelet's words with its reason in
// front where the message alone would not say what kind of failure this is,
// and the exit status behind it where there was one.
func (p podRefusal) Sentence() string {
	line := p.Reason
	if message := strings.TrimSpace(p.Message); message != "" {
		line = withReason(p.Reason, message)
	}
	if p.ExitCode != nil {
		line += fmt.Sprintf(" (exit code %d)", *p.ExitCode)
	}
	return line
}

// Started reports whether the container this refusal is about ever ran.
//
// It is the one distinction every reader downstream acts on. Nothing ran
// means there is nothing half-applied to undo, nothing to read in the logs,
// and a fix that is in the spec or the image rather than in the program — and
// telling somebody to go and read output that cannot exist is precisely what
// #442 was about.
func (p podRefusal) Started() bool {
	return !refusedReasons[p.Reason] && !imagePullReasons[p.Reason] && !startFailureReasons[p.Reason]
}

// startFailureReasons are the terminated reasons that mean the container was
// created and could not be started.
//
// They are the other side of refusedReasons and they are kept apart from it
// on purpose: a refused container was never created, so the kubelet holds the
// pod and retries the same doomed spec for ever; a container that *cannot
// start* ends the pod immediately, the Job counts a failure, and — with
// `backoffLimit` zero — the Job fails under `BackoffLimitExceeded`, which is
// the Job controller's word for "it is over" and says nothing whatever about
// why.
//
//   - `StartError` is the kubelet's answer when the runtime accepted the
//     container and could not run its command. `exec: "node": executable file
//     not found in $PATH` is #440, and that one sentence is the whole
//     diagnosis.
//   - `ContainerCannotRun` is the same fault under the other runtime's name,
//     kept beside it so a cluster's choice of runtime does not decide whether
//     the platform can explain itself.
//
// Everything else a container terminates under did run: `Error` is a program
// that exited non-zero and `OOMKilled` one the kernel stopped, and both left
// output behind them.
var startFailureReasons = map[string]bool{
	"StartError":         true,
	"ContainerCannotRun": true,
}

// refusalOf is the refusal on one pod, or false for a pod with no complaint
// the kubelet has given up on.
//
// It reads every container the pod has, init containers first, because the
// first one refused is the one that stopped the pod — a buildpacks clone
// refused for its security context leaves the application container waiting
// under `PodInitializing`, which explains nothing.
//
// A pod that has finished or is on its way out is never a refusal: a
// terminated container had its chance to run, and one being deleted is
// somebody else's decision already taken.
func refusalOf(pod *corev1.Pod, now time.Time) (podRefusal, bool) {
	if pod.DeletionTimestamp != nil {
		return podRefusal{}, false
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return podRefusal{}, false
	case corev1.PodPending, corev1.PodRunning, corev1.PodUnknown:
	}

	waited := podWaitedFor(pod, now)
	for _, statuses := range podContainerStatuses(pod) {
		for i := range statuses {
			waiting := statuses[i].State.Waiting
			if waiting == nil {
				continue
			}
			switch {
			case refusedReasons[waiting.Reason]:
			case imagePullReasons[waiting.Reason] && waited >= imagePullGrace:
			default:
				continue
			}
			return podRefusal{
				Pod:       pod.Name,
				Container: statuses[i].Name,
				Reason:    waiting.Reason,
				Message:   strings.TrimSpace(waiting.Message),
			}, true
		}
	}
	return podRefusal{}, false
}

// podContainerStatuses is every container status a pod has, init containers
// first — the order they ran in, and so the order a failure has to be read in.
func podContainerStatuses(pod *corev1.Pod) [][]corev1.ContainerStatus {
	return [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses}
}

// podWaitedFor is how long the pod has been the kubelet's problem: from when
// the kubelet took it where that is known, and from its creation otherwise.
// It is the same reading jobStalledSince takes of a Job, for the same reason —
// the clock has to start where the platform stopped being the one holding
// things up.
func podWaitedFor(pod *corev1.Pod, now time.Time) time.Duration {
	from := pod.CreationTimestamp.Time
	if pod.Status.StartTime != nil {
		from = pod.Status.StartTime.Time
	}
	if from.IsZero() {
		return 0
	}
	return now.Sub(from)
}

// refusalIn is the first refusal among a list of pods, which is the whole
// diagnosis for every workload the platform materializes: one refused pod and
// a hundred refused pods have the same cause and the same fix, and they are
// refused for the pod *spec*, so the first is as complete an account as the
// last.
//
// Waiting for every pod of a workload to be refused before saying so would go
// quiet in exactly the case that matters most — a new release whose pods
// cannot start behind pods still serving the old one, where nothing else on
// the platform is going to mention it either.
func refusalIn(pods []corev1.Pod, now time.Time) (podRefusal, bool) {
	for i := range pods {
		if refusal, found := refusalOf(&pods[i], now); found {
			return refusal, true
		}
	}
	return podRefusal{}, false
}

// failureOf is what one pod of a *finished* run has to say for itself: which
// container ended it, under which reason, with what message and what exit
// status.
//
// This is the half of the diagnosis refusalOf cannot make. refusalOf answers
// "will this ever start", which is a question about a pod that is still
// waiting; by the time a Job has failed the pod is over, and the only thing
// left that knows anything is the container status it left behind. Nothing
// above the pod carries it: the Job says `BackoffLimitExceeded` — its word
// for "this run is over" — for every failed run the platform can produce,
// since `backoffLimit` is zero by design (#442).
//
// It reads containers init-first for the same reason refusalOf does: the
// first one that failed is the one that ended the pod, and a clone container
// that could not start leaves the application container reporting nothing at
// all.
//
// A container that terminated is preferred to one that is waiting, and a
// container's last termination is read where its current state is a wait —
// which is how a container that ran, died and is backing off reports the
// death rather than the backoff.
func failureOf(pod *corev1.Pod) (podRefusal, bool) {
	for _, statuses := range podContainerStatuses(pod) {
		for i := range statuses {
			if terminated := terminatedState(&statuses[i]); terminated != nil && terminated.ExitCode != 0 {
				return podRefusal{
					Pod:       pod.Name,
					Container: statuses[i].Name,
					Reason:    terminated.Reason,
					Message:   strings.TrimSpace(terminated.Message),
					ExitCode:  ptr.To(terminated.ExitCode),
				}, true
			}
			waiting := statuses[i].State.Waiting
			if waiting == nil || !(refusedReasons[waiting.Reason] || imagePullReasons[waiting.Reason]) {
				continue
			}
			// A pod killed while it was still waiting — a task that hit its
			// timeout with the image still unpulled. The wait is the whole
			// account of it and there is no exit status to add.
			return podRefusal{
				Pod:       pod.Name,
				Container: statuses[i].Name,
				Reason:    waiting.Reason,
				Message:   strings.TrimSpace(waiting.Message),
			}, true
		}
	}
	return podRefusal{}, false
}

// terminatedState is the container's termination, current or last. A waiting
// container that has already died once is reported on what killed it rather
// than on the wait, which is a fact and not a symptom.
func terminatedState(status *corev1.ContainerStatus) *corev1.ContainerStateTerminated {
	if status.State.Terminated != nil {
		return status.State.Terminated
	}
	return status.LastTerminationState.Terminated
}

// failureIn is the first pod of a run that has something to say. A run makes
// one pod, so the loop is for the case where a collected pod sits beside the
// one that failed.
func failureIn(pods []corev1.Pod) (podRefusal, bool) {
	for i := range pods {
		if failure, found := failureOf(&pods[i]); found {
			return failure, true
		}
	}
	return podRefusal{}, false
}

// DiagnoseRun puts the container's own account of a failed run onto it, from
// the pods of the workload it belongs to.
//
// It is the whole of #442's first half. Without it every failed run on the
// platform reads `BackoffLimitExceeded: Job has reached the specified backoff
// limit` — the Job controller saying the run is over — whatever went wrong,
// and the one case where the reader has least to go on is the case where they
// are told least: a container that never started printed nothing, so the logs
// are correctly empty and there is nowhere else to look.
//
// The pods handed to it are the workload's, not the run's: one list serves
// every run of a process, and each run takes the pods its own Job made. A run
// whose pod has been collected keeps whatever the Job said, which is the best
// answer that still exists.
//
// It is exported because the API answers a run listing out of the same Jobs
// and has to read them the same way the Environment's status was written
// from — a second reading would be a second vocabulary, and the two would
// eventually disagree about the same run.
func DiagnoseRun(run *kitchenv1alpha1.ProcessRun, pods []corev1.Pod) {
	if run == nil || run.Phase != kitchenv1alpha1.RunFailed || run.Refused {
		return
	}
	failure, found := failureIn(podsOfRun(pods, run.Name))
	if !found {
		return
	}
	run.Reason = failure.Reason
	run.ExitCode = failure.ExitCode
	run.Refused = !failure.Started()
	// The container's sentence replaces the Job's, rather than joining it.
	// `BackoffLimitExceeded` is true and useless — it is on every failed run
	// there can be — and a headline that has to be read past is a headline
	// that gets read instead.
	if sentence := failure.Sentence(); sentence != "" {
		run.Message = sentence
	}
}

// DiagnoseRuns is [DiagnoseRun] over a listing.
func DiagnoseRuns(runs []kitchenv1alpha1.ProcessRun, pods []corev1.Pod) {
	for i := range runs {
		DiagnoseRun(&runs[i], pods)
	}
}

// podsOfRun is the pods one Job made, out of a list of a whole workload's.
//
// `job-name` is the job-controller's own label and the same one refuseRun
// selects on, so a pod is attributed to a run by the cluster's answer rather
// than by this package guessing from a name.
func podsOfRun(pods []corev1.Pod, run string) []corev1.Pod {
	mine := make([]corev1.Pod, 0, 1)
	for i := range pods {
		if pods[i].Labels[labelJobName] == run {
			mine = append(mine, pods[i])
		}
	}
	return mine
}

// labelJobName is what the job-controller puts on every pod it creates.
const labelJobName = "job-name"

// diagnoseRunPods reads the pods of one run and puts their account onto it.
// It is the single-run form, for the deploy task path where one run is being
// waited on rather than a listing being answered.
func (r *EnvironmentReconciler) diagnoseRunPods(
	ctx context.Context,
	appNS string,
	run *kitchenv1alpha1.ProcessRun,
) error {
	if run == nil || run.Phase != kitchenv1alpha1.RunFailed || run.Refused {
		return nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(appNS),
		client.MatchingLabels{labelJobName: run.Name}); err != nil {
		return err
	}
	DiagnoseRun(run, pods.Items)
	return nil
}

// refuseRun turns a run whose pod the kubelet will not create into a failed
// run, and reports whether it did.
//
// This is the whole of #391. A `Job` whose pod is refused is not a failing
// Job: nothing is counted as failed, `backoffLimit` is never approached, and
// the kubelet retries the same doomed spec until somebody deletes it. So the
// Job's own conditions — which is all RunOf can read — say "running" for ever,
// and the deploy gated on the run waits on a verdict that is never coming.
//
// The pod is where the verdict actually is, so the pod is what this reads. The
// run is failed with the kubelet's own sentence, which names the field and the
// image and is the whole diagnosis.
//
// A run is only ever refused *forward*: a run that already reached a terminal
// phase is left exactly as it is, because a verdict the Job reached is the
// better evidence and a container that ran is not one that was refused.
func (r *EnvironmentReconciler) refuseRun(
	ctx context.Context,
	appNS string,
	run *kitchenv1alpha1.ProcessRun,
) (bool, error) {
	if run == nil || run.Phase != kitchenv1alpha1.RunRunning {
		return false, nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(appNS),
		client.MatchingLabels{labelJobName: run.Name}); err != nil {
		return false, err
	}
	refusal, found := refusalIn(pods.Items, time.Now())
	if !found {
		return false, nil
	}
	run.Phase = kitchenv1alpha1.RunFailed
	run.Refused = true
	run.Reason = refusal.Reason
	run.Message = refusal.Sentence()
	run.FinishedAt = ptr.To(metav1.Now())
	return true, nil
}

// deleteRefusedJob removes the Job of a run that was refused.
//
// Failing the run is not what stops it. The job-controller is still holding a
// pod the kubelet will not start, and it holds it until something deletes one
// of the two: with `Forbid` that is a schedule which never fires again, and
// for a deploy task it is a Job left behind on every release that follows.
// Nothing else in the platform ends it — which is why the issue could only be
// recovered from with `kubectl`.
//
// **This is safe here and nowhere else**, and for exactly the reason
// deleteStalledJobs is: a refused container never ran, so there is no output
// to delete and no account of a failure to lose. Every other failed run ends
// with a container that ran, and its logs are what the run's row points at.
// Nothing outside this may borrow it.
//
// A deletion that fails is logged and no more. The run is already recorded as
// failed, which is the half that had to be true; what is left behind is a Job
// that was already there.
func (r *EnvironmentReconciler) deleteRefusedJob(ctx context.Context, appNS, name string) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: appNS, Name: name}}
	err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if err != nil && !apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Error(err, "the refused run's job could not be deleted",
			"namespace", appNS, "job", name)
		return
	}
	logf.FromContext(ctx).Info("refused run's job deleted", "namespace", appNS, "job", name)
}

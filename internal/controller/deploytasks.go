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
	"sort"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// Work that runs once per deploy and finishes before the release serves
// anything (#272).
//
// The schema migration is the universal case and it had nowhere to go. A
// readiness probe is not a substitute and the difference is the whole issue:
// a readiness check stops traffic reaching a pod that is not ready, and does
// nothing at all about the *previous* release's pods being retired while a
// migration is half applied. Running it from the application's own entrypoint
// — which is what teams do instead — runs it once per replica, concurrently,
// on every rollout.
//
// So a `task` workload is a batch/v1 Job, and the Environment's reconcile
// stops at it:
//
//   - **Once per deploy, whatever the replica count.** One Job, one run. The
//     environment records which Release the run was for, so a hundred
//     reconciles of one release run one migration, and the run is started
//     again exactly when the release being deployed is not the one recorded —
//     a new build, a rollback, or a retry, which is that record being cleared.
//   - **The deploy waits.** Nothing else is applied while a task is
//     unfinished: not the web Deployment, not the workers, not the services,
//     not the route. Whatever was serving keeps serving, because nothing
//     asked it to stop — this is the one place in the reconcile where doing
//     less is the feature.
//   - **A failure fails the deploy.** The environment goes Degraded with the
//     Job's own message on it, which is a phase the git deployment status
//     reports as a failure and the dashboard paints red. It is not a warning
//     and it does not time out into one: the release stays where it is until
//     somebody fixes the migration and builds again, or retries the run.
//   - **It is the environment's own.** The pod is the environment's other
//     workloads' pod — same image, same variables, same claim bindings, same
//     volumes, same posture, same pull secret — so a preview's run touches
//     the preview's own branch of the database and nothing else. That is the
//     half of the trust model that matters here: the capability is arbitrary
//     code from the repository, which the project file already permits by
//     replacing the entrypoint, and what it must not gain is another
//     environment's credentials.
//
// Reversing a schema change is out of scope, permanently. A rollback runs the
// task the release being rolled back to declared, which is the most the
// platform can honestly do for it.

const (
	// taskRunHistory is how many finished runs of one deploy task are kept in
	// the namespace. A schedule's history is bounded by the CronJob's own two
	// limits; a task has no CronJob, so the reconciler bounds it — deep
	// enough that the failure before last is still there to compare against,
	// shallow enough that a project deploying ten times a day does not
	// accumulate Jobs for ever. The output outlives all of it in the log
	// store, under the run's name.
	taskRunHistory = 5
)

// deployTaskOutcome is what this pass found of an environment's deploy tasks:
// the status row for each, and — the only thing the caller acts on — whether
// the deploy may proceed.
type deployTaskOutcome struct {
	// statuses is one row per declared task, in the order the release
	// declares them.
	statuses []kitchenv1alpha1.ProcessStatus

	// blocked says the release must not be applied yet. A task is running, a
	// task has failed, or the previous deploy's run has not finished.
	blocked bool
	// failed distinguishes the one blocked state that is not going to clear
	// itself, which is what decides the phase and how long until the next
	// look.
	failed bool

	// reason and message are the condition the environment carries while it
	// is blocked.
	reason  string
	message string
}

// DeployTaskRunName is the Job one run of one deploy task is: the workload
// name every other process shares, plus which attempt this is.
//
// The attempt is in the name rather than a generated suffix because the
// reconciler has to be able to find the run it started again on its next pass
// without having written anything down first — a status update that is lost
// between creating the Job and recording it must not produce a second
// migration. The same name is computed, the existing Job is found, and the
// pass carries on.
//
// It is exported because the API says the name of the run a retry is about to
// make, which it can do precisely because the name is derived — and a second
// spelling of a generated name is a rename waiting to break one of them.
func DeployTaskRunName(envName, processName string, attempt int32) string {
	return fitJobName(ProcessWorkloadName(envName, processName) + "-" + strconv.Itoa(int(attempt)))
}

// reconcileDeployTasks brings every deploy task the Release declares up to
// date and answers whether the deploy may proceed.
//
// The list is the *Release's*, like every other workload: the release being
// rolled back to declared the migration it declared, and running today's
// against yesterday's image is how a rollback stops being one.
//
// Tasks run in the order they are declared, one at a time, and a task behind
// one that has not succeeded does not start. Declared order is the only
// ordering a project can express and the only one it needs — "migrate, then
// seed" is a sentence, and running the two at once would make the second
// depend on a race.
func (r *EnvironmentReconciler) reconcileDeployTasks(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	project *kitchenv1alpha1.Project,
	release *kitchenv1alpha1.Release,
	appNS string,
	labels map[string]string,
	podEnv []corev1.EnvVar,
	mounts []mountedVolume,
) (deployTaskOutcome, error) {
	out := deployTaskOutcome{}
	for i := range release.Spec.ConfigSnapshot.Processes {
		process := release.Spec.ConfigSnapshot.Processes[i]
		if !process.RunsOnce() {
			continue
		}
		status := kitchenv1alpha1.ProcessStatus{Name: process.Name, Type: process.Type}
		// What this environment already knows about the task, carried forward
		// before anything is decided: which release it last ran for, how many
		// runs it has started, and how the last one went. The reconciler holds
		// nothing between passes, so this record *is* the memory that keeps a
		// migration to one run per deploy.
		if previous := env.FindProcessStatus(process.Name); previous != nil {
			status.Release = previous.Release
			status.Attempt = previous.Attempt
			status.LastRun = previous.LastRun
			status.LastFailure = previous.LastFailure
		}

		switch {
		case !process.RunsIn(env.Spec.Type):
			// Declared, deliberately not run here — a task somebody took out
			// of previews. Reported rather than omitted, like every other
			// suspended workload, and it holds nothing up.
			status.Suspended = true
			status.Release = ""
			status.LastRun = nil
		case out.blocked:
			// A task behind one that has not finished. Its row is whatever it
			// was; it has not started, and saying so is the point of running
			// them in order.
		default:
			if err := r.advanceDeployTask(ctx, deployTaskContext{
				env: env, project: project, release: release,
				appNS: appNS, labels: labels, podEnv: podEnv, mounts: mounts,
				process: process,
			}, &status, &out); err != nil {
				return out, err
			}
		}
		out.statuses = append(out.statuses, status)
	}
	return out, nil
}

// deployTaskContext is everything one task's run is made of. It is a struct
// rather than nine parameters because every one of them is the environment's
// and none of them is the task's own — which is the property the whole
// feature rests on.
type deployTaskContext struct {
	env     *kitchenv1alpha1.Environment
	project *kitchenv1alpha1.Project
	release *kitchenv1alpha1.Release
	appNS   string
	labels  map[string]string
	podEnv  []corev1.EnvVar
	mounts  []mountedVolume
	process kitchenv1alpha1.ProcessSpec
}

// advanceDeployTask moves one task one step: observe the run this deploy
// already started, or start it.
func (r *EnvironmentReconciler) advanceDeployTask(
	ctx context.Context,
	task deployTaskContext,
	status *kitchenv1alpha1.ProcessStatus,
	out *deployTaskOutcome,
) error {
	// The run this deploy already made. Anything else — a different release
	// recorded, no record at all, or a record cleared by a retry — is a deploy
	// this task has not run for.
	if status.Release == task.release.Name && status.LastRun != nil {
		run, found, err := r.observeRun(ctx, task.appNS, status.LastRun.Name)
		switch {
		case err != nil:
			return err
		case found:
			status.LastRun = &run
			if run.Phase == kitchenv1alpha1.RunFailed {
				status.LastFailure = &run
			}
		case status.LastRun.Phase == kitchenv1alpha1.RunRunning:
			// The Job is gone and the record says it had not finished. There
			// is no verdict to stand on, so the deploy is still gated and the
			// run has to be made again.
			return r.startDeployTask(ctx, task, status, out)
		}
		// A terminal verdict stands whether or not the Job is still there:
		// re-running a migration because a Job was tidied up would be a
		// second run for one deploy, which is the thing this exists to
		// prevent.
		r.recordTaskVerdict(task, status, out)
		return nil
	}

	// A deploy this task has not run for. If the *previous* deploy's run is
	// still in flight it is waited for rather than killed: two deploys racing
	// is a real thing, and stopping a migration halfway through to start
	// another one is the failure mode this feature exists to prevent — not a
	// tidier version of it. The wait is bounded by that run's own timeout.
	if status.LastRun != nil && status.LastRun.Phase == kitchenv1alpha1.RunRunning {
		run, found, err := r.observeRun(ctx, task.appNS, status.LastRun.Name)
		if err != nil {
			return err
		}
		if found {
			status.LastRun = &run
			if run.Phase == kitchenv1alpha1.RunFailed {
				status.LastFailure = &run
			}
			if run.Phase == kitchenv1alpha1.RunRunning {
				out.blocked = true
				out.reason = "PreviousRunActive"
				out.message = fmt.Sprintf(
					"%s is still running for the previous deploy (%s): this release waits for it to finish "+
						"rather than stopping it half way",
					status.LastRun.Name, status.Release)
				return nil
			}
		}
	}
	return r.startDeployTask(ctx, task, status, out)
}

// recordTaskVerdict reads a finished-or-running record and says what it means
// for the deploy.
func (r *EnvironmentReconciler) recordTaskVerdict(
	task deployTaskContext,
	status *kitchenv1alpha1.ProcessStatus,
	out *deployTaskOutcome,
) {
	switch status.LastRun.Phase {
	case kitchenv1alpha1.RunSucceeded:
		// This task is done for this deploy. The next one may start, and if
		// it is the last one the release is applied.
	case kitchenv1alpha1.RunFailed:
		out.blocked, out.failed = true, true
		out.reason = "TaskFailed"
		out.message = fmt.Sprintf(
			"%s failed before this release could take traffic, so nothing was deployed and %s is still "+
				"serving what it was. Run %s: %s",
			task.process.Name, task.env.Name, status.LastRun.Name, taskFailureDetail(status.LastRun))
	default:
		out.blocked = true
		out.reason = "TaskRunning"
		out.message = fmt.Sprintf(
			"%s is running as %s; nothing of this release takes traffic until it succeeds",
			task.process.Name, status.LastRun.Name)
	}
}

// taskFailureDetail is what the Job said, or the sentence to read instead
// when it said nothing worth repeating. The output itself is in the logs
// under the run, which is where a stack trace belongs.
func taskFailureDetail(run *kitchenv1alpha1.ProcessRun) string {
	if run.Message != "" {
		return run.Message
	}
	return "its output is in this environment's logs under this run"
}

// startDeployTask creates the Job for one run of a task.
func (r *EnvironmentReconciler) startDeployTask(
	ctx context.Context,
	task deployTaskContext,
	status *kitchenv1alpha1.ProcessStatus,
	out *deployTaskOutcome,
) error {
	status.Attempt++
	name := DeployTaskRunName(task.env.Name, task.process.Name, status.Attempt)
	podLabels := processLabels(task.labels, task.process.Name)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: task.appNS, Labels: podLabels},
		Spec: batchv1.JobSpec{
			// Zero, like a scheduled run's, and for a stronger reason: the
			// deploy is gated on this, so a retry the platform made on its own
			// would be a second migration nobody asked for while the first
			// one's transaction may still be open. A run that failed is a
			// failed deploy; retrying it is a decision.
			BackoffLimit:          ptr.To(runBackoffLimit),
			ActiveDeadlineSeconds: ptr.To(task.process.TimeoutSeconds()),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
			},
		},
	}
	podSpec := processPodSpec(task.release, task.project, task.podEnv, task.process, task.mounts)
	// Never, not OnFailure: with a backoff limit of zero a restarting
	// container would retry inside a Job that can never fail, which is a
	// migration running twice while the deploy waits for a verdict that never
	// comes.
	podSpec.RestartPolicy = corev1.RestartPolicyNever
	job.Spec.Template.Spec = podSpec

	if err := r.Create(ctx, job); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		// The name is derived rather than generated exactly so that this is
		// recoverable: a pass that created the Job and then failed to record
		// it finds its own run here instead of starting a second one.
		if err := r.Get(ctx, client.ObjectKey{Namespace: task.appNS, Name: name}, job); err != nil {
			return err
		}
	}

	run := RunOf(job)
	status.Release = task.release.Name
	status.LastRun = &run
	out.blocked = true
	out.reason = "TaskRunning"
	out.message = fmt.Sprintf(
		"%s is running as %s; nothing of this release takes traffic until it succeeds",
		task.process.Name, name)

	r.Activity.Record(ctx, clickhouse.Event{
		Type:        clickhouse.EventRunStarted,
		Project:     task.env.Spec.ProjectRef.Name,
		Environment: task.env.Name,
		Process:     task.process.Name,
		Run:         name,
		Message: fmt.Sprintf("deploy task %s started for release %s",
			task.process.Name, task.release.Name),
	})
	return r.pruneTaskRuns(ctx, task.appNS, task.env.Name, task.process.Name, name)
}

// observeRun reads one Job back as a run. found=false means the Job is not
// there — collected, or never created.
func (r *EnvironmentReconciler) observeRun(
	ctx context.Context,
	appNS, name string,
) (kitchenv1alpha1.ProcessRun, bool, error) {
	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: appNS, Name: name}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return kitchenv1alpha1.ProcessRun{}, false, nil
		}
		return kitchenv1alpha1.ProcessRun{}, false, err
	}
	return RunOf(job), true, nil
}

// pruneTaskRuns keeps the last few finished runs of one task and deletes the
// rest. A schedule's history is the CronJob's to bound; a task has no CronJob,
// so this is where the same courtesy is done — and `keep` is the run just
// started, which is never a candidate however the sort comes out.
func (r *EnvironmentReconciler) pruneTaskRuns(ctx context.Context, appNS, envName, processName, keep string) error {
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(appNS), client.MatchingLabels{
		labelEnvironment: envName,
		labelProcess:     processName,
	}); err != nil {
		return err
	}
	finished := make([]*batchv1.Job, 0, len(jobs.Items))
	for i := range jobs.Items {
		if jobs.Items[i].Name == keep || RunOf(&jobs.Items[i]).Phase == kitchenv1alpha1.RunRunning {
			continue
		}
		finished = append(finished, &jobs.Items[i])
	}
	sort.Slice(finished, func(a, b int) bool {
		return runStart(RunOf(finished[a])).After(runStart(RunOf(finished[b])))
	})
	for _, job := range finished[min(len(finished), taskRunHistory):] {
		if err := r.Delete(ctx, job,
			client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil &&
			!apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// recordDeployTasks puts the settled answer on the Environment: every task
// this release declares has run for it, or this environment runs none.
//
// A release that declares no task at all carries no condition. One that
// declares only tasks this environment does not run carries it as True with
// that said in words, because "nothing ran and nothing was meant to" is a
// different sentence from "nothing ran", and a preview is where somebody
// reads it.
func recordDeployTasks(env *kitchenv1alpha1.Environment, tasks deployTaskOutcome) {
	if len(tasks.statuses) == 0 {
		meta.RemoveStatusCondition(&env.Status.Conditions, condDeployTasks)
		return
	}
	ran := make([]string, 0, len(tasks.statuses))
	suspended := make([]string, 0, len(tasks.statuses))
	for i := range tasks.statuses {
		if tasks.statuses[i].Suspended {
			suspended = append(suspended, tasks.statuses[i].Name)
			continue
		}
		ran = append(ran, tasks.statuses[i].Name)
	}
	var parts []string
	if len(ran) > 0 {
		parts = append(parts, fmt.Sprintf("%s finished before this release took traffic",
			strings.Join(ran, ", ")))
	}
	if len(suspended) > 0 {
		parts = append(parts, fmt.Sprintf("%s does not run in this environment",
			strings.Join(suspended, ", ")))
	}
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               condDeployTasks,
		Status:             metav1.ConditionTrue,
		Reason:             "Completed",
		Message:            strings.Join(parts, "; "),
		ObservedGeneration: env.Generation,
	})
}

// mergeProcessStatuses puts the task rows this pass produced over whatever the
// environment already reported, keeping every other workload's row exactly as
// it stood.
//
// It exists because a blocked deploy returns before the workloads are
// reconciled, and the status written on the way out must not read as though
// the environment had stopped running the things it is still running. The
// previous release's workers and services are up; only the tasks have moved.
func mergeProcessStatuses(
	existing, tasks []kitchenv1alpha1.ProcessStatus,
) []kitchenv1alpha1.ProcessStatus {
	merged := make([]kitchenv1alpha1.ProcessStatus, 0, len(existing)+len(tasks))
	replaced := map[string]bool{}
	for i := range existing {
		if task := findProcessStatus(tasks, existing[i].Name); task != nil {
			merged = append(merged, *task)
			replaced[task.Name] = true
			continue
		}
		merged = append(merged, existing[i])
	}
	for i := range tasks {
		if !replaced[tasks[i].Name] {
			merged = append(merged, tasks[i])
		}
	}
	return merged
}

// findProcessStatus is [Environment.FindProcessStatus] over a bare list, for
// the two places that hold one before it is on the object.
func findProcessStatus(statuses []kitchenv1alpha1.ProcessStatus, name string) *kitchenv1alpha1.ProcessStatus {
	for i := range statuses {
		if statuses[i].Name == name {
			return &statuses[i]
		}
	}
	return nil
}

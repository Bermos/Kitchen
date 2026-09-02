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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// A Project's workers and scheduled jobs, materialized.
//
// They are the same Release the web process runs — the same image, the same
// resolved environment — started with another command, which is the whole
// reason #78 is a modelling change rather than a build one. What differs is
// the shape they take in the cluster and, more importantly, what a person can
// find out about them afterwards:
//
//   - **A worker is a Deployment with no Service and no HTTPRoute.** Nothing
//     addresses it, so there is nothing to publish and no certificate to want.
//     Nothing *else's* Service addresses it either, which is a separate claim
//     and was not true for a while: these pods carry the environment label,
//     the environment's own Service selected on that label alone, and a
//     worker has no port to answer the application's on — so every worker was
//     a backend of the URL that refused connections. The web pods carry
//     LabelComponent for the Service to select on instead.
//   - **A scheduled job is a batch/v1 CronJob**, and its runs are Jobs. Their
//     logs are already collected — every container on the node is — but a run
//     is only *findable* if the pipeline knows which run it was, which is why
//     the pods carry the process name and the Job name reaches the log store
//     as `run:`.
//   - **A failure is carried out of the cluster.** A CronJob whose pods fail
//     silently is the classic way this feature disappoints, so every terminal
//     run lands in the activity feed and the most recent failure stays on
//     `status.processes` until a later failure replaces it — not until a
//     success does, which would hide a job that fails four nights in five.

const (
	// LabelProcess names which of a Project's processes a pod belongs to. It
	// is exported because three other places select on it: the collector maps
	// it onto the `kitchen.process` resource attribute, the API finds a
	// process's pods with it, and the reconciler prunes with it.
	//
	// The web process carries no such label. Its workload is the Environment's
	// own name and its logs are the environment's logs, which is what every
	// screen built before this feature already asks for — a `process=web`
	// label would change the meaning of those queries.
	LabelProcess = "kitchen.bermos.dev/process"

	labelProcess = LabelProcess

	// A scheduled process keeps a few finished runs so that a person can look
	// at what happened before the Job's own garbage collection takes them.
	// Failures are kept deeper than successes for the obvious reason.
	successfulRunHistory = int32(3)
	failedRunHistory     = int32(5)

	// runBackoffLimit is zero, and that is a decision: a scheduled run that
	// failed is a failed run, not a retried one, and the schedule is what
	// tries again. A backoff limit above zero turns one nightly failure into
	// a burst of pods and one activity entry that arrives minutes late.
	runBackoffLimit = int32(0)
)

// ProcessWorkloadName is what one process of one environment materializes as.
// The environment leads so that everything an environment owns sorts together
// in a namespace shared by its previews.
//
// It is exported because the API addresses the same objects — a run listing
// finds the CronJob by this name, and triggering a run copies its template —
// and a second spelling of a generated name is a rename waiting to break one
// of them.
func ProcessWorkloadName(envName, processName string) string {
	return envName + "-" + processName
}

// processLabels are the environment's child labels plus the process's own.
func processLabels(base map[string]string, processName string) map[string]string {
	labels := make(map[string]string, len(base)+1)
	for key, value := range base {
		labels[key] = value
	}
	labels[labelProcess] = processName
	return labels
}

// processEnv is the resolved environment of the web process plus the one
// variable a process is entitled to know that the web process is not: which
// process it is. A single image serving three roles has no other way of
// telling, short of parsing its own argv.
func processEnv(podEnv []corev1.EnvVar, processName string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(podEnv)+1)
	out = append(out, corev1.EnvVar{Name: "KITCHEN_PROCESS", Value: processName})
	return append(out, podEnv...)
}

// reconcileProcesses materializes every process the Release declares, tears
// down what it no longer declares, and answers with what it now sees of each.
//
// The list it works from is the *Release's*, never the Project's. That is what
// makes a rollback a rollback: the release being rolled back to declared the
// worker command it declared, and running today's against yesterday's image is
// how a rollback stops being one.
func (r *EnvironmentReconciler) reconcileProcesses(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	project *kitchenv1alpha1.Project,
	release *kitchenv1alpha1.Release,
	appNS string,
	labels map[string]string,
	podEnv []corev1.EnvVar,
	// mounts are the environment's volume claims; each process gets the
	// ones that name it, and nothing else's.
	mounts []mountedVolume,
) ([]kitchenv1alpha1.ProcessStatus, error) {
	declared := release.Spec.ConfigSnapshot.Processes
	statuses := make([]kitchenv1alpha1.ProcessStatus, 0, len(declared))
	live := map[string]bool{}

	for i := range declared {
		process := declared[i]
		status := kitchenv1alpha1.ProcessStatus{Name: process.Name, Type: process.Type}
		if !process.RunsIn(env.Spec.Type) {
			// Declared, deliberately not running here. It is reported rather
			// than omitted so that a preview's process list is the project's
			// process list with the reason beside each entry, instead of a
			// shorter list that reads like a bug.
			status.Suspended = true
			status.Schedule = process.Schedule
			statuses = append(statuses, status)
			continue
		}

		name := ProcessWorkloadName(env.Name, process.Name)
		live[name] = true
		status.Workload = name

		processMounts := mountsFor(mounts, process.Name)
		switch process.Type {
		case kitchenv1alpha1.ProcessCron:
			if err := r.applyCronJob(ctx, env, release, project, appNS, labels, podEnv, process,
				processMounts); err != nil {
				return nil, err
			}
			if err := r.observeCronJob(ctx, env, appNS, process, &status); err != nil {
				return nil, err
			}
		default:
			if err := r.applyWorkerDeployment(ctx, env, release, project, appNS, labels, podEnv, process,
				processMounts); err != nil {
				return nil, err
			}
			if err := r.observeWorker(ctx, appNS, name, &status); err != nil {
				return nil, err
			}
		}
		statuses = append(statuses, status)
	}

	if err := r.pruneProcesses(ctx, env, appNS, live); err != nil {
		return nil, err
	}
	r.recordRuns(ctx, env, statuses)
	return statuses, nil
}

// processPodSpec is the pod both shapes run: the Release's image, the
// environment's resolved variables, the registry credential the image needs to
// be pulled, and the process's own command and resources.
func processPodSpec(
	release *kitchenv1alpha1.Release,
	project *kitchenv1alpha1.Project,
	podEnv []corev1.EnvVar,
	process kitchenv1alpha1.ProcessSpec,
	mounts []mountedVolume,
) corev1.PodSpec {
	volumes, volumeMounts := podVolumes(mounts)
	container := corev1.Container{
		Name:         AppContainerName,
		Image:        release.Spec.Image,
		Command:      process.Command,
		Args:         process.Args,
		Env:          processEnv(podEnv, process.Name),
		Resources:    process.Resources,
		VolumeMounts: volumeMounts,
	}
	// A worker that declared a health check is probed the way the web
	// process is — it publishes no port of its own, so the check names the
	// one it listens on, which is why admission refuses a process health
	// check without a port. A scheduled run is not probed at all: how it
	// went is its exit status, and admission refuses the field there too.
	if process.Type == kitchenv1alpha1.ProcessWorker {
		applyProbes(&container, process.Health, 0)
	}
	pod := corev1.PodSpec{
		// The same credential the web process pulls with. Without it the pods
		// sit in ImagePullBackOff while everything else reads as healthy.
		ImagePullSecrets: []corev1.LocalObjectReference{
			{Name: registrySecretName(project.Spec.Registry.ConnectionRef.Name)},
		},
		Containers: []corev1.Container{container},
		// The volume claims that name this process, and no other's: a
		// volume that attaches once, mounted by two processes, is the
		// Multi-Attach failure the claim names a process to avoid.
		Volumes: volumes,
	}
	// The same posture the web process runs under, and from the same
	// snapshot: a worker and a scheduled run are the project's image started
	// with another command, so a posture that described only the web process
	// would describe a third of the workloads the project ships.
	applySecurityContext(&pod, &pod.Containers[0], release.Spec.ConfigSnapshot.Runtime.Security)
	return pod
}

// applyWorkerDeployment materializes a worker: a Deployment, and nothing else.
// No Service, no HTTPRoute, no certificate — a worker is not addressed, which
// is the whole of what makes it cheap. Its pods deliberately carry no
// component label: that is what keeps them out of the environment's Service.
func (r *EnvironmentReconciler) applyWorkerDeployment(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	project *kitchenv1alpha1.Project,
	appNS string,
	labels map[string]string,
	podEnv []corev1.EnvVar,
	process kitchenv1alpha1.ProcessSpec,
	mounts []mountedVolume,
) error {
	name := ProcessWorkloadName(env.Name, process.Name)
	podLabels := processLabels(labels, process.Name)

	// A worker runs continuously, so a rotated secret reaches it the same way
	// it reaches the web process: by rolling the pods that read it. A
	// scheduled job needs none of this — its next run is a new pod, which
	// reads whatever the Secret holds when it starts.
	var rotation secretRotation
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = podLabels
		// A worker mounting a volume that attaches to one pod at a time
		// runs one replica whatever it asked for, and is recreated below —
		// the claim's declaration, read off its status, rather than the
		// process's own singleton flag.
		deploy.Spec.Replicas = ptr.To(capReplicas(process.ReplicaCount(), mounts))
		// A worker that must not run twice does not get a rolling update
		// either (#250). Left alone it would take the API server's default,
		// which at one replica surges to a second copy and takes none away —
		// so the process the platform recommends *moving* a poller into
		// would be the one that overlapped it, which inverts the whole
		// declaration. Recreate stops the old pod first; nothing addresses a
		// worker, so the gap costs it only a few seconds of not consuming.
		//
		// Withdrawing the declaration puts the rolling update back, and only
		// then: writing the type unconditionally would clear the surge and
		// unavailability parameters the API server defaults onto it, so
		// every reconcile would differ from what it had just written.
		switch {
		case process.Singleton || attachesOnce(mounts):
			deploy.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		case deploy.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType:
			deploy.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType}
		}
		// A Deployment's selector is immutable, so it is written once and
		// then left exactly as it was found — the same rule the component
		// survey follows for the workloads it labels.
		if deploy.Spec.Selector == nil {
			deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{
				labelEnvironment: env.Name,
				labelProcess:     process.Name,
			}}
		}
		deploy.Spec.Template.Labels = podLabels
		deploy.Spec.Template.Spec = processPodSpec(release, project, podEnv, process, mounts)
		var err error
		rotation, err = stampSecretsRevision(ctx, r.Client, appNS, &deploy.Spec.Template)
		return err
	})
	if err != nil {
		return err
	}
	r.announceRotation(ctx, env, process.Name, rotation, process.Singleton || attachesOnce(mounts))
	return nil
}

// applyCronJob materializes a scheduled process.
func (r *EnvironmentReconciler) applyCronJob(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	project *kitchenv1alpha1.Project,
	appNS string,
	labels map[string]string,
	podEnv []corev1.EnvVar,
	process kitchenv1alpha1.ProcessSpec,
	// mounts are the volume claims naming this process. A scheduled run
	// mounts them like a worker does; with a volume that attaches once, a
	// run overlapping the last one on another node waits for it to finish
	// — bounded by the run's timeout, and avoided by the default
	// concurrency policy, Forbid.
	mounts []mountedVolume,
) error {
	name := ProcessWorkloadName(env.Name, process.Name)
	childLabels := processLabels(labels, process.Name)

	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cron, func() error {
		cron.Labels = childLabels
		cron.Spec.Schedule = process.Schedule
		// UTC, always and everywhere. A schedule whose meaning depends on
		// where the cluster happens to be installed is a schedule that moves
		// twice a year in some installations and not in others.
		cron.Spec.TimeZone = ptr.To("Etc/UTC")
		cron.Spec.ConcurrencyPolicy = batchv1.ConcurrencyPolicy(process.EffectiveConcurrency())
		cron.Spec.SuccessfulJobsHistoryLimit = ptr.To(successfulRunHistory)
		cron.Spec.FailedJobsHistoryLimit = ptr.To(failedRunHistory)
		// The Job carries the labels too, so that a run can be found by the
		// process it belongs to without walking back through its owner.
		cron.Spec.JobTemplate.Labels = childLabels
		cron.Spec.JobTemplate.Spec.BackoffLimit = ptr.To(runBackoffLimit)
		cron.Spec.JobTemplate.Spec.ActiveDeadlineSeconds = ptr.To(process.TimeoutSeconds())
		cron.Spec.JobTemplate.Spec.Template.Labels = childLabels
		podSpec := processPodSpec(release, project, podEnv, process, mounts)
		// Never, not OnFailure: with a backoff limit of zero a restarting
		// container would retry inside a Job that can never fail, which is
		// exactly the silent failure this feature exists to end.
		podSpec.RestartPolicy = corev1.RestartPolicyNever
		cron.Spec.JobTemplate.Spec.Template.Spec = podSpec
		return nil
	})
	return err
}

// observeWorker reads a worker's replica counts back.
func (r *EnvironmentReconciler) observeWorker(
	ctx context.Context,
	appNS, name string,
	status *kitchenv1alpha1.ProcessStatus,
) error {
	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: appNS, Name: name}, deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if deploy.Spec.Replicas != nil {
		status.Replicas = *deploy.Spec.Replicas
	}
	status.ReadyReplicas = deploy.Status.ReadyReplicas
	return nil
}

// observeCronJob reads a schedule's state back: what is in flight, when it
// last fired, when it fires next, and how the recent runs ended.
func (r *EnvironmentReconciler) observeCronJob(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	appNS string,
	process kitchenv1alpha1.ProcessSpec,
	status *kitchenv1alpha1.ProcessStatus,
) error {
	status.Schedule = process.Schedule

	cron := &batchv1.CronJob{}
	key := client.ObjectKey{Namespace: appNS, Name: ProcessWorkloadName(env.Name, process.Name)}
	if err := r.Get(ctx, key, cron); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	status.Active = int32(len(cron.Status.Active)) //nolint:gosec // a CronJob's active list is never that long

	runs, err := r.runsOf(ctx, appNS, env.Name, process.Name)
	if err != nil {
		return err
	}
	if len(runs) > 0 {
		status.LastRun = &runs[0]
	}
	for i := range runs {
		if runs[i].Phase == kitchenv1alpha1.RunFailed {
			status.LastFailure = &runs[i]
			break
		}
	}
	return nil
}

// runsOf reads the runs of one scheduled process, newest first.
//
// It reads Jobs rather than the CronJob's `status.active`, because the
// question is what *happened* and an active list holds only what has not.
// What the store keeps is bounded by the two history limits above, which is
// why the API answers a run listing from here and its logs from ClickHouse:
// the Job goes, the output stays.
func (r *EnvironmentReconciler) runsOf(
	ctx context.Context,
	appNS, envName, processName string,
) ([]kitchenv1alpha1.ProcessRun, error) {
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(appNS), client.MatchingLabels{
		labelEnvironment: envName,
		labelProcess:     processName,
	}); err != nil {
		return nil, err
	}
	runs := make([]kitchenv1alpha1.ProcessRun, 0, len(jobs.Items))
	for i := range jobs.Items {
		runs = append(runs, RunOf(&jobs.Items[i]))
	}
	sort.Slice(runs, func(a, b int) bool {
		return runStart(runs[a]).After(runStart(runs[b]))
	})
	return runs, nil
}

// runStart is what a run is ordered by: when it started, or when the Job was
// created for one whose pods never got as far as starting.
func runStart(run kitchenv1alpha1.ProcessRun) time.Time {
	if run.StartedAt != nil {
		return run.StartedAt.Time
	}
	return time.Time{}
}

// RunOf reads one Job as a run.
//
// A Job's conditions are the honest source here: `status.failed` counts pods,
// and a Job that hit its deadline has a Failed condition and can have no
// failed pod at all — the pod was killed, not observed failing.
//
// Exported for the API, which answers a run listing out of the same Jobs and
// must read them the same way the status on the Environment was written from.
func RunOf(job *batchv1.Job) kitchenv1alpha1.ProcessRun {
	run := kitchenv1alpha1.ProcessRun{Name: job.Name, Phase: kitchenv1alpha1.RunRunning}
	run.StartedAt = job.Status.StartTime
	if run.StartedAt == nil {
		run.StartedAt = &job.CreationTimestamp
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			run.Phase = kitchenv1alpha1.RunSucceeded
			run.FinishedAt = job.Status.CompletionTime
		case batchv1.JobFailed:
			run.Phase = kitchenv1alpha1.RunFailed
			run.Message = strings.TrimSpace(condition.Reason + ": " + condition.Message)
			run.Message = strings.TrimSuffix(run.Message, ":")
			if run.FinishedAt == nil {
				run.FinishedAt = &condition.LastTransitionTime
			}
		}
	}
	if run.Phase != kitchenv1alpha1.RunRunning && run.FinishedAt == nil {
		run.FinishedAt = job.Status.CompletionTime
	}
	return run
}

// pruneProcesses tears down what this Environment materialized for a process
// the Release no longer declares — or no longer runs here, which is what
// turning a preview's opt-in back off has to mean.
//
// It selects on the process label, so the web process's own Deployment — which
// carries no such label — can never be caught by it.
func (r *EnvironmentReconciler) pruneProcesses(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	appNS string,
	live map[string]bool,
) error {
	selector := []client.ListOption{
		client.InNamespace(appNS),
		client.MatchingLabels{labelEnvironment: env.Name},
		client.HasLabels{labelProcess},
	}

	deployments := &appsv1.DeploymentList{}
	if err := r.List(ctx, deployments, selector...); err != nil {
		return err
	}
	for i := range deployments.Items {
		if live[deployments.Items[i].Name] {
			continue
		}
		if err := r.Delete(ctx, &deployments.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	crons := &batchv1.CronJobList{}
	if err := r.List(ctx, crons, selector...); err != nil {
		return err
	}
	for i := range crons.Items {
		if live[crons.Items[i].Name] {
			continue
		}
		if err := r.Delete(ctx, &crons.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// deleteProcesses removes everything an Environment materialized for its
// processes. It is the finalizer's, and it selects the same way the pruning
// does: by the process label, in the project's application namespace.
//
// It takes the runs too, which the pruning deliberately does not: a live
// process's finished Jobs are its history and are collected by the CronJob's
// own history limits. Once the environment is gone there is nothing to be the
// history of — and a run somebody started by hand has no CronJob to be
// garbage-collected with, so it would be the one thing left behind.
func (r *EnvironmentReconciler) deleteProcesses(ctx context.Context, env *kitchenv1alpha1.Environment, appNS string) error {
	if err := r.pruneProcesses(ctx, env, appNS, nil); err != nil {
		return err
	}
	return r.DeleteAllOf(ctx, &batchv1.Job{},
		client.InNamespace(appNS),
		client.MatchingLabels{labelEnvironment: env.Name},
		client.HasLabels{labelProcess},
		client.PropagationPolicy(metav1.DeletePropagationBackground),
	)
}

// recordRuns puts every run that has just reached a terminal state into the
// activity feed.
//
// The de-duplication is the previous status: a run is reported when the
// Environment's last-recorded view of that process did not already have it
// finished. That holds across restarts because the status is on the object,
// and it holds across a missed reconcile because a run that finished while
// nobody was looking is still new to the status.
func (r *EnvironmentReconciler) recordRuns(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	statuses []kitchenv1alpha1.ProcessStatus,
) {
	for i := range statuses {
		run := statuses[i].LastRun
		if run == nil || run.Phase == kitchenv1alpha1.RunRunning {
			continue
		}
		if previous := env.FindProcessStatus(statuses[i].Name); previous != nil &&
			previous.LastRun != nil &&
			previous.LastRun.Name == run.Name &&
			previous.LastRun.Phase == run.Phase {
			continue
		}

		event := clickhouse.Event{
			Project:     env.Spec.ProjectRef.Name,
			Environment: env.Name,
			Process:     statuses[i].Name,
			Run:         run.Name,
			Value:       runSeconds(*run),
		}
		if run.Phase == kitchenv1alpha1.RunFailed {
			event.Type = clickhouse.EventRunFailed
			event.Message = fmt.Sprintf("scheduled job %s failed", statuses[i].Name)
			if run.Message != "" {
				event.Message += ": " + run.Message
			}
		} else {
			event.Type = clickhouse.EventRunSucceeded
			event.Message = fmt.Sprintf("scheduled job %s ran", statuses[i].Name)
		}
		r.Activity.Record(ctx, event)
	}
}

// runSeconds is how long a run took, or zero when it cannot be said.
func runSeconds(run kitchenv1alpha1.ProcessRun) float64 {
	if run.StartedAt == nil || run.FinishedAt == nil {
		return 0
	}
	seconds := run.FinishedAt.Sub(run.StartedAt.Time).Seconds()
	if seconds < 0 {
		return 0
	}
	return seconds
}

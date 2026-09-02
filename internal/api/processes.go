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

package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/appconfig"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/detect"
)

// A Project's workloads besides its web process, over the API (#78, #271).
//
// The list itself is a project setting, written through PATCH /projects/{name}
// beside the port and the replica count it belongs with: what a project runs
// and how much of it is one decision, made by the same person, and a second
// route for half of it would only be a second place to look. What is *not* a
// project setting is what those processes are doing right now, which is a fact
// about an environment and answered per environment — a preview runs the
// project's process list minus everything that did not opt in.
//
// The runs are read out of the cluster and their output out of the log store,
// which is the division that makes a run findable after the Job that was it
// has been collected: the Job's name is the log store's `run:`, so a failure
// from three weeks ago still has its output even though nothing in the cluster
// remembers it happened.

// processRequest and the validation behind it live in internal/appconfig,
// which is where the shapes an application's settings arrive in moved when a
// repository's kitchen.json became a second way to send the same ones. The
// aliases keep this package reading as it did; there is one implementation of
// what a process is.
type processRequest = appconfig.Process

func processesFromRequest(requests []processRequest) ([]kitchenv1alpha1.ProcessSpec, error) {
	return appconfig.Processes(requests)
}

// processView is one process of one environment: what the release declared,
// and what the cluster is doing about it.
type processView struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Command  []string `json:"command,omitempty"`
	Args     []string `json:"args,omitempty"`
	Schedule string   `json:"schedule,omitempty"`
	// Port is a service workload's, and Address is where it answers inside
	// the cluster: `http://<host>:<port>`, the same value its siblings are
	// handed as KITCHEN_SERVICE_<NAME>. Both are absent on a worker and a
	// scheduled job, which nothing addresses, and the address is absent for
	// a service this environment does not run.
	//
	// Neither is a URL anyone outside the cluster can reach, and that is the
	// point of a service: publishing is what a route does, and a service
	// gets none.
	Port    int32  `json:"port,omitempty"`
	Address string `json:"address,omitempty"`
	// Image is what this workload runs when that is not the release's own
	// image — a workload with a build of its own. Absent means it runs the
	// project's image with another command, which is the ordinary case.
	Image string `json:"image,omitempty"`
	// Build is this workload's own build, when it has one: which directory
	// of the repository it is and how the image comes out of it.
	Build *processBuildView `json:"build,omitempty"`
	// ConcurrencyPolicy and Timeout are a scheduled process's; Replicas is a
	// worker's declared count and ReadyReplicas what is actually up.
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty"`
	Timeout           string `json:"timeout,omitempty"`
	Replicas          int32  `json:"replicas,omitempty"`
	ReadyReplicas     int32  `json:"readyReplicas,omitempty"`
	// Singleton is a worker two of which must never run at once, so its
	// deploys stop the old pod before starting the new one. It is reported
	// because it is the difference between a rollout that overlaps two
	// copies of a poller and one that does not, and nothing else on the row
	// shows it.
	Singleton bool   `json:"singleton,omitempty"`
	CPU       string `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	// Health is the worker's health check, timings resolved. Absent for a
	// worker that declared none — unlike the web process, a worker is
	// probed only where it asked to be.
	Health *healthView `json:"health,omitempty"`
	// Workload is the Deployment or CronJob behind it, absent for a process
	// this environment does not run.
	Workload string `json:"workload,omitempty"`
	// Suspended is a process the project declares that this environment does
	// not run: a preview whose process did not opt in. It is listed with the
	// reason rather than left out, so a preview's process list is the
	// project's with an explanation beside each entry.
	Suspended bool   `json:"suspended,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Active is how many runs of a schedule are in flight.
	Active int32 `json:"active,omitempty"`
	// LastRun and LastFailure are the two a person actually asks for. The
	// failure is kept until a later failure replaces it, never until a
	// success does: a job that fails four nights in five must not read as
	// healthy on the fifth.
	LastRun     *processRunView `json:"lastRun,omitempty"`
	LastFailure *processRunView `json:"lastFailure,omitempty"`
	// Healthy is false for a worker with no ready replica and for a schedule
	// whose most recent run failed. It is the one derived field here, and it
	// is derived at the API rather than in each client so that the dashboard
	// and the CLI cannot disagree about what a red dot means.
	Healthy bool `json:"healthy"`
}

// processBuildView is one workload's own build, as the project declares it.
type processBuildView struct {
	Strategy       string `json:"strategy"`
	DockerfilePath string `json:"dockerfilePath,omitempty"`
	// DockerfileTarget is the stage of that Dockerfile this workload names,
	// absent when it names none — in which case the unit's own stage stands
	// in, and `GET /builds/{name}` says which stage each image was actually
	// built to. It reads as declared rather than resolved because the
	// declaration is the release's and the resolution is a build's: a
	// release that was rolled back to would otherwise read as the stage
	// today's project settings name.
	DockerfileTarget string `json:"dockerfileTarget,omitempty"`
	RootDirectory    string `json:"rootDirectory,omitempty"`
}

func newProcessBuildView(build *kitchenv1alpha1.ProcessBuildSpec) *processBuildView {
	if build == nil {
		return nil
	}
	view := &processBuildView{
		Strategy: string(build.EffectiveStrategy()),
		// Spelled the way the build spells it, by the one place that says
		// what a build root is — so the answer names the directory the build
		// would read rather than a near miss of it.
		RootDirectory: detect.NormalizeRoot(build.RootDirectory),
	}
	// The Dockerfile is a dockerfile build's alone. Reporting it beside a
	// buildpacks strategy would be a setting that reads back and does
	// nothing, which is the shape of confusion this repository refuses
	// everywhere else.
	if build.EffectiveStrategy() == kitchenv1alpha1.BuildStrategyDockerfile {
		view.DockerfilePath = detect.NormalizeDockerfile(build.DockerfilePath)
		view.DockerfileTarget = detect.NormalizeTarget(build.DockerfileTarget)
	}
	return view
}

// processRunView is one run of a scheduled process.
type processRunView struct {
	Name       string     `json:"name"`
	Phase      string     `json:"phase"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	// DurationSeconds is how long it took, absent while it is still going.
	DurationSeconds *float64 `json:"durationSeconds,omitempty"`
	Message         string   `json:"message,omitempty"`
}

func newProcessRunView(run *kitchenv1alpha1.ProcessRun) *processRunView {
	if run == nil {
		return nil
	}
	view := &processRunView{Name: run.Name, Phase: string(run.Phase), Message: run.Message}
	if run.StartedAt != nil {
		view.StartedAt = &run.StartedAt.Time
	}
	if run.FinishedAt != nil {
		view.FinishedAt = &run.FinishedAt.Time
		if run.StartedAt != nil {
			seconds := run.FinishedAt.Sub(run.StartedAt.Time).Seconds()
			view.DurationSeconds = &seconds
		}
	}
	return view
}

// newProcessView joins what the Release declared to what the reconciler last
// saw. The declaration is the Release's, not the Project's, for the same
// reason the reconciler works from it: an environment on an older release runs
// that release's processes, and reporting today's would describe something
// that is not there.
func newProcessView(process kitchenv1alpha1.ProcessSpec, status *kitchenv1alpha1.ProcessStatus) processView {
	view := processView{
		Name:     process.Name,
		Type:     string(process.Type),
		Command:  process.Command,
		Args:     process.Args,
		Schedule: process.Schedule,
		Port:     process.Port,
		Build:    newProcessBuildView(process.Build),
		Healthy:  true,
	}
	if process.Type == kitchenv1alpha1.ProcessCron {
		view.ConcurrencyPolicy = string(process.EffectiveConcurrency())
		view.Timeout = (time.Duration(process.TimeoutSeconds()) * time.Second).String()
	} else {
		view.Replicas = process.ReplicaCount()
		view.Singleton = process.Singleton
	}
	if quantity, ok := process.Resources.Limits[corev1.ResourceCPU]; ok {
		view.CPU = quantity.String()
	}
	if quantity, ok := process.Resources.Limits[corev1.ResourceMemory]; ok {
		view.Memory = quantity.String()
	}
	if process.Health != nil {
		view.Health = newHealthView(process.Health)
	}
	if status == nil {
		// The reconciler has not been round since this release landed. Not an
		// error, and deliberately not reported as unhealthy: nothing is known
		// yet, which is a different thing from something being wrong.
		return view
	}

	view.Workload = status.Workload
	view.Address = status.Address
	view.Image = status.Image
	view.Suspended = status.Suspended
	view.Active = status.Active
	view.LastRun = newProcessRunView(status.LastRun)
	view.LastFailure = newProcessRunView(status.LastFailure)
	switch {
	case status.Suspended:
		view.Reason = suspendedReason(process)
	case process.Type == kitchenv1alpha1.ProcessCron:
		view.Healthy = status.LastRun == nil || status.LastRun.Phase != kitchenv1alpha1.RunFailed
	default:
		view.ReadyReplicas = status.ReadyReplicas
		view.Replicas = status.Replicas
		view.Healthy = status.Replicas == 0 || status.ReadyReplicas > 0
	}
	return view
}

// suspendedReason is why a workload the release declares is not running here.
//
// A service reads the other way round from a worker: it is in a preview
// unless it was taken out of one, so the sentence that tells a worker how to
// opt in would tell a service's reader to do what it already did.
func suspendedReason(process kitchenv1alpha1.ProcessSpec) string {
	if process.Addressed() {
		return "this service was taken out of preview environments — " +
			"nothing in this preview can reach it, so unset previews on it to put it back"
	}
	return "this process does not run in preview environments — set previews on it to opt in"
}

// environmentProcesses answers what this environment runs besides its web
// process, and what each of them is doing.
func (s *Server) environmentProcesses(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}
	declared, err := s.declaredProcesses(ctx, env)
	if err != nil {
		s.writeError(w, err)
		return
	}
	views := make([]processView, 0, len(declared))
	for _, process := range declared {
		views = append(views, newProcessView(process, env.FindProcessStatus(process.Name)))
	}
	writeList(w, views)
}

// declaredProcesses is the process list of the Release this environment is on.
// A missing Release is an empty list rather than an error: the environment's
// own conditions already say the release is gone, and this endpoint answering
// 500 for it would be a second, less informative way of hearing about it.
func (s *Server) declaredProcesses(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
) ([]kitchenv1alpha1.ProcessSpec, error) {
	release := &kitchenv1alpha1.Release{}
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Spec.ReleaseRef.Name}
	if err := s.Client.Get(ctx, key, release); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return release.Spec.ConfigSnapshot.Processes, nil
}

// processRuns lists a scheduled process's recent runs, newest first.
//
// What is listed is what the cluster still holds — the CronJob keeps a few
// finished Jobs and collects the rest — which is why a run's *output* is
// asked for separately, from the log store, where it outlives the Job by the
// whole container-log retention.
func (s *Server) processRuns(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}
	process, ok := s.scheduledProcess(w, ctx, env, req.PathValue("process"))
	if !ok {
		return
	}

	jobs := &batchv1.JobList{}
	if err := s.reader().List(ctx, jobs,
		client.InNamespace(controller.AppNamespace(env.Spec.ProjectRef.Name)),
		client.MatchingLabels{
			controller.LabelEnvironment: env.Name,
			controller.LabelProcess:     process.Name,
		},
	); err != nil {
		s.writeError(w, err)
		return
	}

	runs := make([]kitchenv1alpha1.ProcessRun, 0, len(jobs.Items))
	for i := range jobs.Items {
		runs = append(runs, controller.RunOf(&jobs.Items[i]))
	}
	sort.Slice(runs, func(a, b int) bool {
		if runs[a].StartedAt == nil || runs[b].StartedAt == nil {
			return runs[b].StartedAt == nil
		}
		return runs[a].StartedAt.After(runs[b].StartedAt.Time)
	})

	views := make([]processRunView, 0, len(runs))
	for i := range runs {
		views = append(views, *newProcessRunView(&runs[i]))
	}
	writeList(w, views)
}

// triggerProcessRun starts one run of a scheduled process now, off its
// schedule.
//
// It is a copy of the CronJob's own job template rather than anything this
// handler composes, so a manual run is the same run the schedule would have
// made — same image, same command, same timeout. Nothing from the request
// reaches it: the body is empty and the only caller-supplied values are the
// two names in the path, both of which resolve to objects before anything is
// created.
func (s *Server) triggerProcessRun(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}
	process, ok := s.scheduledProcess(w, ctx, env, req.PathValue("process"))
	if !ok {
		return
	}

	appNS := controller.AppNamespace(env.Spec.ProjectRef.Name)
	workload := controller.ProcessWorkloadName(env.Name, process.Name)
	cron := &batchv1.CronJob{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: appNS, Name: workload}, cron); err != nil {
		if apierrors.IsNotFound(err) {
			badRequest(w, "nothing is scheduled for process %q on environment %q yet: "+
				"the platform has not materialized it — the environment's conditions say why",
				process.Name, env.Name)
			return
		}
		s.writeError(w, err)
		return
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			// A generated suffix rather than a timestamp: two people pressing
			// the button in the same second get two runs, which is what they
			// each asked for, instead of one 409.
			GenerateName: workload + "-manual-",
			Namespace:    appNS,
			Labels:       cron.Spec.JobTemplate.Labels,
			Annotations:  cron.Spec.JobTemplate.Annotations,
		},
		Spec: *cron.Spec.JobTemplate.Spec.DeepCopy(),
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    env,
		Kind:      audit.KindEnvironment,
		Operation: clickhouse.AuditCreate,
		Project:   env.Spec.ProjectRef.Name,
		Reason:    fmt.Sprintf("a run of scheduled job %s was started by hand on %s", process.Name, env.Name),
		Details:   map[string]any{"process": process.Name, "environment": env.Name},
	}) {
		return
	}
	if err := s.Client.Create(ctx, job); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	run := controller.RunOf(job)
	s.log().Info("scheduled job run through the api",
		"environment", env.Name, "process", process.Name, "run", job.Name, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:        clickhouse.EventRunStarted,
		Project:     env.Spec.ProjectRef.Name,
		Environment: env.Name,
		Process:     process.Name,
		Run:         job.Name,
		Actor:       callerName(caller),
		Message:     fmt.Sprintf("scheduled job %s was run by hand", process.Name),
	})
	writeJSON(w, http.StatusAccepted, newProcessRunView(&run))
}

// scheduledProcess resolves the named process of an environment's release and
// insists it is a scheduled one, writing the refusal itself.
//
// A worker or a service is refused rather than quietly accepted: "run it now"
// has no meaning for a workload that is already running, and a 404 would
// suggest the workload does not exist when it plainly does.
func (s *Server) scheduledProcess(
	w http.ResponseWriter,
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	name string,
) (kitchenv1alpha1.ProcessSpec, bool) {
	declared, err := s.declaredProcesses(ctx, env)
	if err != nil {
		s.writeError(w, err)
		return kitchenv1alpha1.ProcessSpec{}, false
	}
	process := kitchenv1alpha1.FindProcess(declared, name)
	if process == nil {
		s.writeError(w, apierrors.NewNotFound(
			kitchenv1alpha1.GroupVersion.WithResource("processes").GroupResource(), name))
		return kitchenv1alpha1.ProcessSpec{}, false
	}
	if process.Type != kitchenv1alpha1.ProcessCron {
		badRequest(w, "process %q is a %s, not a scheduled job: it has no runs, "+
			"and it is already running — its replicas are on GET /environments/%s/processes",
			name, process.Type, env.Name)
		return kitchenv1alpha1.ProcessSpec{}, false
	}
	return *process, true
}

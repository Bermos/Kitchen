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
	"k8s.io/utils/ptr"
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
	// ImageSource is what this workload declares when the image is one the
	// platform did not build: a repository somebody else publishes and a tag
	// or a digest of it. It is the declaration, where `Image` above is what
	// the release resolved it to — the two differ exactly when a tag has
	// moved since, which is the difference a rollback is about.
	ImageSource *imageSourceView `json:"imageSource,omitempty"`
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
	// Deploy is what a deploy task is doing to the deploy it belongs to, and
	// is absent on every other type of workload: `pending` (this release's
	// run has not started), `running` (nothing of this release takes traffic
	// until it finishes), `complete`, or `failed` (the release did not land
	// and whatever was serving is still serving).
	//
	// It is derived here rather than in each client for the reason `healthy`
	// is: it is the answer to "did my deploy land", and the dashboard and the
	// CLI must not be able to disagree about it.
	Deploy string `json:"deploy,omitempty"`
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
	// everywhere else. `auto` keeps it: the file is what `auto` looks for,
	// and a workload that has one is a dockerfile build.
	if build.EffectiveStrategy() != kitchenv1alpha1.BuildStrategyBuildpacks {
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
//
// `release` is the release the environment is being deployed to, and is empty
// where the question is not about an environment at all — a project's own
// declaration. It is what turns a deploy task's recorded run into an answer
// about *this* deploy: a run recorded against another release is a run that
// has not happened for this one.
func newProcessView(
	process kitchenv1alpha1.ProcessSpec,
	status *kitchenv1alpha1.ProcessStatus,
	release string,
) processView {
	view := processView{
		Name:        process.Name,
		Type:        string(process.Type),
		Command:     process.Command,
		Args:        process.Args,
		Schedule:    process.Schedule,
		Port:        process.Port,
		Build:       newProcessBuildView(process.Build),
		ImageSource: newImageSourceView(process.Image),
		Healthy:     true,
	}
	switch {
	case process.Type == kitchenv1alpha1.ProcessCron:
		view.ConcurrencyPolicy = string(process.EffectiveConcurrency())
		view.Timeout = (time.Duration(process.TimeoutSeconds()) * time.Second).String()
	case process.RunsOnce():
		// A task has no replicas to report and no concurrency to choose. What
		// it has is the bound on how long the deploy waits for it, which is
		// the one number somebody watching a stuck deploy wants.
		view.Timeout = (time.Duration(process.TimeoutSeconds()) * time.Second).String()
	default:
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
	case process.RunsOnce():
		view.Deploy = deployStateOf(status, release)
		view.Healthy = view.Deploy != deployFailed
	case process.Type == kitchenv1alpha1.ProcessCron:
		view.Healthy = status.LastRun == nil || status.LastRun.Phase != kitchenv1alpha1.RunFailed
	default:
		view.ReadyReplicas = status.ReadyReplicas
		view.Replicas = status.Replicas
		view.Healthy = status.Replicas == 0 || status.ReadyReplicas > 0
	}
	return view
}

// The four things a deploy task can be doing to the deploy it belongs to.
// They are the API's own vocabulary, not the run phases: a run that succeeded
// for another release is a task that is `pending` for this one.
const (
	deployPending  = "pending"
	deployRunning  = "running"
	deployComplete = "complete"
	deployFailed   = "failed"
)

// deployStateOf reads a task's record against the release being deployed.
//
// The release comparison is the whole of it. A task carries the release its
// last run was made for, so a run recorded against an older one — a rollback,
// or a fresh build — has not happened for this deploy however well it went,
// and the environment is about to make it happen.
func deployStateOf(status *kitchenv1alpha1.ProcessStatus, release string) string {
	if status.LastRun == nil || (release != "" && status.Release != release) {
		return deployPending
	}
	switch status.LastRun.Phase {
	case kitchenv1alpha1.RunSucceeded:
		return deployComplete
	case kitchenv1alpha1.RunFailed:
		return deployFailed
	default:
		return deployRunning
	}
}

// suspendedReason is why a workload the release declares is not running here.
//
// A service and a task read the other way round from a worker: both are in a
// preview unless they were taken out of one, so the sentence that tells a
// worker how to opt in would tell their reader to do what they already did.
func suspendedReason(process kitchenv1alpha1.ProcessSpec) string {
	if process.Addressed() {
		return "this service was taken out of preview environments — " +
			"nothing in this preview can reach it, so unset previews on it to put it back"
	}
	if process.RunsOnce() {
		return "this task was taken out of preview environments — nothing prepares this preview's own " +
			"resources before it deploys, so unset previews on it to put it back"
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
		views = append(views, newProcessView(process, env.FindProcessStatus(process.Name), env.Spec.ReleaseRef.Name))
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

// processRuns lists a process's recent runs, newest first — a schedule's
// firings, or a deploy task's one run per deploy.
//
// What is listed is what the cluster still holds — the CronJob keeps a few
// finished Jobs and collects the rest, and a task's runs are bounded the same
// way by the reconciler — which is why a run's *output* is asked for
// separately, from the log store, where it outlives the Job by the whole
// container-log retention. One endpoint for both because a run is a run: the
// question "what did it print and how did it end" does not change with what
// started it.
func (s *Server) processRuns(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}
	process, ok := s.runnableProcess(w, ctx, env, req.PathValue("process"))
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

// manualRunTTLSeconds is how long a finished manual run's Job (and its pod)
// sticks around before the job-controller collects it.
//
// It is the one Job the platform creates that nothing else would ever collect:
// a scheduled run is owned by its CronJob and falls off the two history
// limits, and a run started by hand has no CronJob to be collected with. So it
// carries its own TTL instead — and a generous one, because the other TTLs in
// the platform are collection windows for a reconciler and this one is a
// window for a *person*. Whoever pressed the button has to be able to find the
// run afterwards, on the listing and on `status.processes[].lastRun`, which is
// a question asked in days rather than in the hour a build gets. Seven days is
// past any of that and still an end.
//
// The run's output outlives the Job either way: the logs are in the store
// under the Job's name, which is what `kitchen logs --run` reads.
const manualRunTTLSeconds = 7 * 24 * 3600

// triggerProcessRun starts one run now: a scheduled process off its schedule,
// or a deploy task whose failure is holding a release back.
//
// The two are one route because they are one request — "run that again" — and
// they are carried out differently because a run means something different in
// each. A schedule's run is a copy of the CronJob's own job template, so a
// manual run is the run the schedule would have made. A task's is the
// *deploy's*, so this hands it back to the reconciler that owns the deploy
// rather than composing a second one beside it; see retryDeployTask.
//
// Nothing from the request reaches either: the body is empty and the only
// caller-supplied values are the two names in the path, both of which resolve
// to objects before anything is created.
func (s *Server) triggerProcessRun(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}
	process, ok := s.runnableProcess(w, ctx, env, req.PathValue("process"))
	if !ok {
		return
	}
	if process.RunsOnce() {
		s.retryDeployTask(w, req, env, process)
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
	// The template's own TTL is not the one that applies here: it is the
	// CronJob's, written for a Job the CronJob owns and its history limits
	// collect. This copy is owned by nobody, so it is given the TTL that
	// collects it.
	job.Spec.TTLSecondsAfterFinished = ptr.To(int32(manualRunTTLSeconds))
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

// retryDeployTask asks for a deploy task to be run again for the release this
// environment is on.
//
// It creates nothing. The run is the *deploy's* — the environment's own
// variables, its claim bindings, its volumes, and the ordering that keeps the
// release off the network until the task succeeds — and all of that is the
// reconciler's. Composing a second Job here would be a run that looked like
// the deploy's and was not gating it, which is the worst of both.
//
// So what it writes is the one fact the reconciler decides from: which
// release the recorded run was for. Clearing it makes the next pass see a
// deploy this task has not run for, and start one — a fresh Job, under the
// next attempt's name, with the failed one left where it is to be read.
//
// It answers with the run that is about to exist rather than one that does,
// which is what 202 is for: the reconciler is what creates it, and the name is
// derived rather than generated exactly so that it can be said in advance.
func (s *Server) retryDeployTask(
	w http.ResponseWriter,
	req *http.Request,
	env *kitchenv1alpha1.Environment,
	process kitchenv1alpha1.ProcessSpec,
) {
	ctx := req.Context()
	status := env.FindProcessStatus(process.Name)
	if status == nil || status.Suspended {
		badRequest(w, "task %q does not run on environment %q: "+
			"the platform has not materialized it — the environment's conditions say why",
			process.Name, env.Name)
		return
	}
	// A run that is still going is refused rather than queued behind. Asking
	// for a second migration while the first one may still hold a transaction
	// open is the thing this feature exists to prevent, and the run that is
	// already going is the one the deploy is waiting for — bounded by the
	// task's own timeout, so this refusal cannot last for ever.
	if status.LastRun != nil && status.LastRun.Phase == kitchenv1alpha1.RunRunning {
		badRequest(w, "task %q is already running as %s on environment %q: "+
			"the deploy is waiting for that run, and a second one beside it is what "+
			"running this once per deploy exists to prevent",
			process.Name, status.LastRun.Name, env.Name)
		return
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    env,
		Kind:      audit.KindEnvironment,
		Operation: clickhouse.AuditCreate,
		Project:   env.Spec.ProjectRef.Name,
		Reason: fmt.Sprintf("deploy task %s was asked to run again on %s",
			process.Name, env.Name),
		Details: map[string]any{"process": process.Name, "environment": env.Name,
			"release": env.Spec.ReleaseRef.Name},
	}) {
		return
	}

	next := controller.DeployTaskRunName(env.Name, process.Name, status.Attempt+1)
	status.Release = ""
	if err := s.Client.Status().Update(ctx, env); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("deploy task retried through the api",
		"environment", env.Name, "process", process.Name, "run", next, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:        clickhouse.EventRunStarted,
		Project:     env.Spec.ProjectRef.Name,
		Environment: env.Name,
		Process:     process.Name,
		Run:         next,
		Actor:       callerName(caller),
		Message:     fmt.Sprintf("deploy task %s was asked to run again", process.Name),
	})
	writeJSON(w, http.StatusAccepted, &processRunView{Name: next, Phase: string(kitchenv1alpha1.RunRunning)})
}

// runnableProcess resolves the named process of an environment's release and
// insists it is one with runs, writing the refusal itself.
//
// A scheduled job and a deploy task both have them; a worker or a service is
// refused rather than quietly accepted, because "run it now" has no meaning
// for a workload that is already running and a 404 would suggest the workload
// does not exist when it plainly does.
func (s *Server) runnableProcess(
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
	if process.Type != kitchenv1alpha1.ProcessCron && !process.RunsOnce() {
		badRequest(w, "process %q is a %s, not a scheduled job or a deploy task: it has no runs, "+
			"and it is already running — its replicas are on GET /environments/%s/processes",
			name, process.Type, env.Name)
		return kitchenv1alpha1.ProcessSpec{}, false
	}
	return *process, true
}

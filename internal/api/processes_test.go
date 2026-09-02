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
	"net/http"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The process endpoints (#78): declaring workers and scheduled jobs with the
// rest of a project's settings, reading what an environment is running, and
// running a scheduled job off its schedule.

const processesPath = "/api/v1/environments/" + testEnvironment + "/processes"

// declaredProcesses is the pair every case here starts from, on the Release
// the fixture environment is running: what the environment runs is the
// release's list, never the project's.
func declaredProcesses() []kitchenv1alpha1.ProcessSpec {
	return []kitchenv1alpha1.ProcessSpec{
		{
			Name:     "worker",
			Type:     kitchenv1alpha1.ProcessWorker,
			Command:  []string{"node", "worker.js"},
			Replicas: ptr.To(int32(2)),
		},
		{
			Name:     "nightly",
			Type:     kitchenv1alpha1.ProcessCron,
			Schedule: "0 3 * * *",
			Timeout:  &metav1.Duration{Duration: 30 * time.Minute},
		},
	}
}

// withProcesses is fixtures() with the release carrying a process list, plus
// whatever else the case needs in the application namespace.
func withProcesses(extra ...runtime.Object) []runtime.Object {
	objs := fixtures()
	for _, obj := range objs {
		release, ok := obj.(*kitchenv1alpha1.Release)
		if ok && release.Name == testRelease {
			release.Spec.ConfigSnapshot.Processes = declaredProcesses()
		}
	}
	return append(objs, extra...)
}

// cronJobFor is the CronJob the environment reconciler would have written for
// the nightly job, which is what a manual run is copied from.
func cronJobFor(process string) *batchv1.CronJob {
	labels := map[string]string{
		controller.LabelEnvironment: testEnvironment,
		controller.LabelProcess:     process,
	}
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.ProcessWorkloadName(testEnvironment, process),
			Namespace: controller.AppNamespace("shop"),
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					BackoffLimit:          ptr.To(int32(0)),
					ActiveDeadlineSeconds: ptr.To(int64(1800)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers: []corev1.Container{{
								Name: "app", Image: testReleaseImage,
							}},
						},
					},
				},
			},
		},
	}
}

func TestDeclaringProcessesWithTheProjectsSettings(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{
		"processes": [
			{"name": "worker", "type": "worker", "command": ["node", "worker.js"], "replicas": 2, "memory": "512Mi"},
			{"name": "nightly", "type": "cron", "schedule": "0 3 * * *", "timeout": "30m",
			 "concurrencyPolicy": "Replace", "previews": true}
		]
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Processes) != 2 {
		t.Fatalf("want two processes, got %+v", stored.Spec.Processes)
	}
	worker := kitchenv1alpha1.FindProcess(stored.Spec.Processes, "worker")
	if worker == nil || worker.ReplicaCount() != 2 || worker.PreviewsEnabled() {
		t.Fatalf("the worker did not stick, or opted itself into previews: %+v", worker)
	}
	if quantity, ok := worker.Resources.Limits[corev1.ResourceMemory]; !ok || quantity.String() != "512Mi" {
		t.Fatalf("the worker's memory did not stick: %+v", worker.Resources)
	}
	nightly := kitchenv1alpha1.FindProcess(stored.Spec.Processes, "nightly")
	if nightly == nil || nightly.TimeoutSeconds() != 1800 ||
		nightly.EffectiveConcurrency() != kitchenv1alpha1.ConcurrencyReplace || !nightly.PreviewsEnabled() {
		t.Fatalf("the scheduled job did not stick: %+v", nightly)
	}

	By := func(body string) *kitchenv1alpha1.Project {
		t.Helper()
		if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", body); recorder.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
		}
		project := &kitchenv1alpha1.Project{}
		if err := h.server.get(context.Background(), "shop", project); err != nil {
			t.Fatal(err)
		}
		return project
	}

	// A worker that must never run twice says so on its own entry, which is
	// what a poller moved out of the web process needs (#250).
	declared := By(`{"processes": [{"name": "poller", "type": "worker", "singleton": true}]}`)
	if poller := kitchenv1alpha1.FindProcess(declared.Spec.Processes, "poller"); poller == nil || !poller.Singleton {
		t.Fatalf("the worker's singleton declaration did not stick: %+v", declared.Spec.Processes)
	}

	// The write replaces rather than merges, like the promotion stages: the
	// list is short and unordered, and a merge would leave no way to delete.
	if project := By(`{"processes": [{"name": "worker", "type": "worker"}]}`); len(project.Spec.Processes) != 1 {
		t.Fatalf("the write did not replace the list: %+v", project.Spec.Processes)
	}
	if project := By(`{"processes": []}`); len(project.Spec.Processes) != 0 {
		t.Fatalf("an empty list did not remove them: %+v", project.Spec.Processes)
	}
}

func TestRefusingAnUnworkableProcessList(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, body := range map[string]string{
		"a cron with no schedule":  `[{"name": "nightly", "type": "cron"}]`,
		"a worker with a schedule": `[{"name": "w", "type": "worker", "schedule": "0 3 * * *"}]`,
		"a service with a schedule": `[{"name": "api", "type": "service", "port": 8080, ` +
			`"schedule": "0 3 * * *"}]`,
		"a process called web": `[{"name": "web", "type": "worker"}]`,
		"an unknown type":      `[{"name": "w", "type": "sidecar"}]`,
		"a name that is not a label": `[{"name": "Nightly Report", "type": "cron", ` +
			`"schedule": "0 3 * * *"}]`,
		"two of the same name": `[{"name": "w", "type": "worker"}, {"name": "w", "type": "worker"}]`,
		"a bad timeout": `[{"name": "n", "type": "cron", "schedule": "0 3 * * *", ` +
			`"timeout": "half an hour"}]`,
		"a bad concurrency policy": `[{"name": "n", "type": "cron", "schedule": "0 3 * * *", ` +
			`"concurrencyPolicy": "Whenever"}]`,
		"a quantity that is not one": `[{"name": "w", "type": "worker", "cpu": "loads"}]`,
		// A process publishes no port of its own, so a health check that
		// named none would be a setting that read back and did nothing.
		"a worker health check with no port": `[{"name": "w", "type": "worker", ` +
			`"health": {"path": "/healthz"}}]`,
		"a health check on a scheduled process": `[{"name": "n", "type": "cron", ` +
			`"schedule": "0 3 * * *", "health": {"port": 9000}}]`,
		// Refused rather than clamped, the same way the project's own runtime
		// refuses it: a count quietly lowered reads back as a setting that
		// did not take.
		"a singleton worker asking for three of itself": `[{"name": "w", "type": "worker", ` +
			`"singleton": true, "replicas": 3}]`,
		// Whether two runs of a schedule may overlap is concurrencyPolicy, so
		// a second spelling of it would read back and do nothing.
		"a singleton schedule": `[{"name": "n", "type": "cron", "schedule": "0 3 * * *", ` +
			`"singleton": true}]`,
		// Only a service is addressed, so only a service has a port. A port
		// on a worker would read back as a setting that took, and nothing
		// would ever connect to it.
		"a service with no port":         `[{"name": "api", "type": "service"}]`,
		"a worker with a port":           `[{"name": "w", "type": "worker", "port": 8080}]`,
		"a service port that is not one": `[{"name": "api", "type": "service", "port": 70000}]`,
		// A scheduled process runs an image; it is not one.
		"a build on a scheduled process": `[{"name": "n", "type": "cron", "schedule": "0 3 * * *", ` +
			`"build": {"rootDirectory": "services/report"}}]`,
		// There is no auto for a workload: detection answers a port and a
		// buildpack, and a workload has neither question open.
		"a workload build asking for auto": `[{"name": "api", "type": "service", "port": 8080, ` +
			`"build": {"strategy": "auto"}}]`,
		// A workload's root directory is its build root, and its Dockerfile
		// path is relative to that root — the same two relations a project's
		// own build has, so they are refused the same way. Nothing above a
		// build root is part of the build, so there is nothing above it for
		// such a path to be resolved against.
		"a workload build climbing out of the repository": `[{"name": "api", "type": "service", ` +
			`"port": 8080, "build": {"rootDirectory": "../elsewhere"}}]`,
		"a workload Dockerfile above its own build root": `[{"name": "api", "type": "service", ` +
			`"port": 8080, "build": {"rootDirectory": "services/api", ` +
			`"dockerfilePath": "../shared/Dockerfile"}}]`,
		"an absolute workload Dockerfile": `[{"name": "api", "type": "service", "port": 8080, ` +
			`"build": {"dockerfilePath": "/Dockerfile"}}]`,
		// A stage is refused on the shape of its name, by the one rule the
		// project's own target is refused by: a name the dockerfile frontend
		// cannot hold could never match a stage that exists.
		"a workload stage no Dockerfile stage could be called": `[{"name": "api", "type": "service", ` +
			`"port": 8080, "build": {"dockerfileTarget": "1st stage"}}]`,
		// A task is one run per deploy: it has no schedule, nothing to keep
		// alive, and no second copy of itself to overlap.
		"a task with a schedule": `[{"name": "migrate", "type": "task", ` +
			`"schedule": "0 3 * * *"}]`,
		"a health check on a task": `[{"name": "migrate", "type": "task", "health": {"port": 9000}}]`,
		"a singleton task":         `[{"name": "migrate", "type": "task", "singleton": true}]`,
		"a task with a port":       `[{"name": "migrate", "type": "task", "port": 8080}]`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"processes": `+body+`}`)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// A monorepo's other workloads are declared with the rest of the project's
// settings, on the route that already carries the worker and the schedule
// (#271). There is no second tier and no second route: the project stays the
// unit, and this is one more kind of entry in the list it already had.
func TestDeclaringAServiceWorkloadWithItsOwnBuild(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{
		"processes": [
			{"name": "api", "type": "service", "port": 8080,
			 "build": {"rootDirectory": "services/api"}},
			{"name": "billing", "type": "service", "port": 9000, "previews": false,
			 "build": {"strategy": "buildpacks", "rootDirectory": "services/billing"}}
		]
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}

	api := kitchenv1alpha1.FindProcess(stored.Spec.Processes, "api")
	if api == nil || api.Port != 8080 || api.Build == nil {
		t.Fatalf("the service did not stick: %+v", api)
	}
	if api.Build.RootDirectory != "services/api" ||
		api.Build.EffectiveStrategy() != kitchenv1alpha1.BuildStrategyDockerfile ||
		api.Build.DockerfilePath != "" {
		t.Fatalf("a workload build defaults to a Dockerfile in its own directory: %+v", api.Build)
	}
	// A service is in a preview unless it says otherwise: a preview missing
	// one of its own services is a broken preview, not a protected one.
	if !api.PreviewsEnabled() {
		t.Fatalf("a service opted itself out of previews: %+v", api)
	}

	billing := kitchenv1alpha1.FindProcess(stored.Spec.Processes, "billing")
	if billing == nil || billing.Build == nil ||
		billing.Build.EffectiveStrategy() != kitchenv1alpha1.BuildStrategyBuildpacks {
		t.Fatalf("the buildpacks workload did not stick: %+v", billing)
	}
	// The pointer is what lets "false" survive a write: a plain bool with
	// omitempty is dropped, and the default would put the service back.
	if billing.PreviewsEnabled() {
		t.Fatalf("a service taken out of previews was put back: %+v", billing)
	}
}

// A workload's build paths are spelled once, by the one place that says what
// a build root is — so `./services/api/` and `services/api` are one directory
// to the builder, to detection and to whoever reads the project back.
func TestAWorkloadsBuildPathsAreSpelledTheWayABuildSpellsThem(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{
		"processes": [
			{"name": "api", "type": "service", "port": 8080,
			 "build": {"rootDirectory": " ./services/api/ ", "dockerfilePath": "./docker/prod.Dockerfile",
			           "dockerfileTarget": " api-runtime "}},
			{"name": "worker", "type": "worker", "build": {"rootDirectory": "."}}
		]
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}

	api := kitchenv1alpha1.FindProcess(stored.Spec.Processes, "api")
	if api == nil || api.Build == nil {
		t.Fatalf("the workload build did not stick: %+v", api)
	}
	if api.Build.RootDirectory != "services/api" {
		t.Errorf("the build root was not spelled the way a build spells it: %q", api.Build.RootDirectory)
	}
	if api.Build.DockerfilePath != "docker/prod.Dockerfile" {
		t.Errorf("the Dockerfile was not cleaned: %q", api.Build.DockerfilePath)
	}
	// A stage is spelled by the same one place, so it reads back as the name
	// a build would look for rather than as what was typed around it.
	if api.Build.DockerfileTarget != "api-runtime" {
		t.Errorf("the stage was not spelled the way a build spells it: %q", api.Build.DockerfileTarget)
	}

	// "." is the repository itself, not a path component — the reading the
	// project's own root directory already takes.
	worker := kitchenv1alpha1.FindProcess(stored.Spec.Processes, "worker")
	if worker == nil || worker.Build == nil || worker.Build.RootDirectory != "" {
		t.Errorf("a workload built from the repository itself kept a path component: %+v", worker)
	}
}

func TestReadingWhatAnEnvironmentRuns(t *testing.T) {
	h := newHarness(t, nil, withProcesses()...)

	recorder := h.do(t, http.MethodGet, processesPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[struct {
		Items []processView `json:"items"`
	}](t, recorder)
	if len(body.Items) != 2 {
		t.Fatalf("want the release's two processes, got %+v", body.Items)
	}

	worker := body.Items[0]
	if worker.Name != "worker" || worker.Type != "worker" || worker.Replicas != 2 {
		t.Fatalf("the worker did not come back whole: %+v", worker)
	}
	nightly := body.Items[1]
	if nightly.Schedule != "0 3 * * *" || nightly.Timeout != "30m0s" {
		t.Fatalf("the scheduled job did not come back whole: %+v", nightly)
	}
	// The reconciler has not been round, so nothing is known — which is not
	// the same as something being wrong, and must not read as unhealthy.
	if !nightly.Healthy || !worker.Healthy {
		t.Fatalf("an environment with no observed state reads as broken: %+v", body.Items)
	}
	if nightly.ConcurrencyPolicy != string(kitchenv1alpha1.ConcurrencyForbid) {
		t.Fatalf("the default concurrency policy is not reported: %q", nightly.ConcurrencyPolicy)
	}
}

// A worker that must never run twice is deployed differently from every other
// one, and its replica count does not say so — 1/1 ready reads the same either
// way — so the declaration is on the answer (#250).
func TestASingletonWorkerSaysSoWhenAnEnvironmentIsRead(t *testing.T) {
	objs := withProcesses()
	for _, obj := range objs {
		release, ok := obj.(*kitchenv1alpha1.Release)
		if !ok || release.Name != testRelease {
			continue
		}
		release.Spec.ConfigSnapshot.Processes[0].Singleton = true
		release.Spec.ConfigSnapshot.Processes[0].Replicas = ptr.To(int32(1))
	}
	h := newHarness(t, nil, objs...)

	recorder := h.do(t, http.MethodGet, processesPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[struct {
		Items []processView `json:"items"`
	}](t, recorder)

	if !body.Items[0].Singleton {
		t.Fatalf("the worker's singleton declaration is not reported: %+v", body.Items[0])
	}
	if body.Items[1].Singleton {
		t.Fatalf("a schedule cannot be a singleton, so it must never claim to be: %+v", body.Items[1])
	}
}

func TestAFailedRunIsVisibleWithoutKubectl(t *testing.T) {
	objs := withProcesses()
	for _, obj := range objs {
		env, ok := obj.(*kitchenv1alpha1.Environment)
		if !ok || env.Name != testEnvironment {
			continue
		}
		env.Status.Processes = []kitchenv1alpha1.ProcessStatus{{
			Name:     "nightly",
			Type:     kitchenv1alpha1.ProcessCron,
			Workload: controller.ProcessWorkloadName(testEnvironment, "nightly"),
			Schedule: "0 3 * * *",
			LastRun: &kitchenv1alpha1.ProcessRun{
				Name:    "shop-production-nightly-1",
				Phase:   kitchenv1alpha1.RunFailed,
				Message: "BackoffLimitExceeded: Job has reached the specified backoff limit",
			},
			LastFailure: &kitchenv1alpha1.ProcessRun{
				Name:  "shop-production-nightly-1",
				Phase: kitchenv1alpha1.RunFailed,
			},
		}}
	}
	h := newHarness(t, nil, objs...)

	recorder := h.do(t, http.MethodGet, processesPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[struct {
		Items []processView `json:"items"`
	}](t, recorder)

	var nightly *processView
	for i := range body.Items {
		if body.Items[i].Name == "nightly" {
			nightly = &body.Items[i]
		}
	}
	if nightly == nil {
		t.Fatalf("the scheduled job is missing: %+v", body.Items)
	}
	if nightly.Healthy {
		t.Fatal("a schedule whose last run failed reads as healthy")
	}
	if nightly.LastFailure == nil || nightly.LastRun == nil ||
		!strings.Contains(nightly.LastRun.Message, "backoff limit") {
		t.Fatalf("the failure did not come out of the cluster: %+v", nightly)
	}
}

func TestListingAScheduledJobsRuns(t *testing.T) {
	finished := metav1.NewTime(time.Now().Add(-time.Hour))
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shop-production-nightly-1",
			Namespace: controller.AppNamespace("shop"),
			Labels: map[string]string{
				controller.LabelEnvironment: testEnvironment,
				controller.LabelProcess:     "nightly",
			},
		},
		Status: batchv1.JobStatus{
			StartTime: &finished,
			Conditions: []batchv1.JobCondition{{
				Type:               batchv1.JobFailed,
				Status:             corev1.ConditionTrue,
				Reason:             "DeadlineExceeded",
				Message:            "Job was active longer than specified deadline",
				LastTransitionTime: metav1.NewTime(finished.Add(30 * time.Minute)),
			}},
		},
	}
	h := newHarness(t, nil, withProcesses(job)...)

	recorder := h.do(t, http.MethodGet, processesPath+"/nightly/runs", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[struct {
		Items []processRunView `json:"items"`
	}](t, recorder)
	if len(body.Items) != 1 || body.Items[0].Phase != string(kitchenv1alpha1.RunFailed) {
		t.Fatalf("the run did not come back: %+v", body.Items)
	}
	// A run that hit its deadline was killed rather than observed failing, so
	// it has no failed pod to count. The condition is the honest source.
	if !strings.Contains(body.Items[0].Message, "deadline") {
		t.Fatalf("the reason is missing: %+v", body.Items[0])
	}
	if body.Items[0].DurationSeconds == nil || *body.Items[0].DurationSeconds != 1800 {
		t.Fatalf("the duration is wrong: %+v", body.Items[0])
	}

	t.Run("a worker has none", func(t *testing.T) {
		recorder := h.do(t, http.MethodGet, processesPath+"/worker/runs", "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("want 400 naming what a worker is, got %d: %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("a process nobody declared is not found", func(t *testing.T) {
		recorder := h.do(t, http.MethodGet, processesPath+"/imaginary/runs", "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestRunningAScheduledJobNow(t *testing.T) {
	h := newHarness(t, nil, withProcesses(cronJobFor("nightly"))...)

	recorder := h.do(t, http.MethodPost, processesPath+"/nightly/runs", "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	started := decode[processRunView](t, recorder)
	if !strings.HasPrefix(started.Name, "shop-production-nightly-manual-") {
		t.Fatalf("a manual run is not named as one: %q", started.Name)
	}

	jobs := &batchv1.JobList{}
	if err := h.server.Client.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("want one Job, got %d", len(jobs.Items))
	}
	job := jobs.Items[0]
	// The run is a copy of what the schedule would have made — same timeout,
	// same no-retry, same labels, so its logs are found the same way.
	if *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != 1800 {
		t.Fatalf("a manual run is not what the schedule would have run: %+v", job.Spec)
	}
	if job.Labels[controller.LabelProcess] != "nightly" {
		t.Fatalf("the run cannot be found by its process: %+v", job.Labels)
	}
	if job.Spec.Template.Spec.Containers[0].Image != testReleaseImage {
		t.Fatalf("the run is not on the release's image: %+v", job.Spec.Template.Spec.Containers[0])
	}
}

func TestRunningAScheduledJobNothingHasMaterialized(t *testing.T) {
	h := newHarness(t, nil, withProcesses()...)

	recorder := h.do(t, http.MethodPost, processesPath+"/nightly/runs", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400 explaining that nothing is scheduled yet, got %d: %s",
			recorder.Code, recorder.Body.String())
	}
}

// Deploy-time work over the API (#272). The reads are the run machinery that
// was already there — a task's runs are runs — and the one write is "run it
// again", which is what picks a deploy back up after a migration failed.

const migrateTask = "migrate"

// withDeployTask is the fixture release plus a task, and the environment's
// record of what that task last did.
func withDeployTask(status *kitchenv1alpha1.ProcessStatus, extra ...runtime.Object) []runtime.Object {
	objs := fixtures()
	for _, obj := range objs {
		if release, ok := obj.(*kitchenv1alpha1.Release); ok && release.Name == testRelease {
			release.Spec.ConfigSnapshot.Processes = []kitchenv1alpha1.ProcessSpec{{
				Name:    migrateTask,
				Type:    kitchenv1alpha1.ProcessTask,
				Command: []string{"npm", "run", "migrate"},
			}}
		}
		if env, ok := obj.(*kitchenv1alpha1.Environment); ok && env.Name == testEnvironment && status != nil {
			env.Status.Processes = []kitchenv1alpha1.ProcessStatus{*status}
		}
	}
	return append(objs, extra...)
}

func taskView(t *testing.T, h *harness) processView {
	t.Helper()
	recorder := h.do(t, http.MethodGet, processesPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[struct {
		Items []processView `json:"items"`
	}](t, recorder)
	for i := range body.Items {
		if body.Items[i].Name == migrateTask {
			return body.Items[i]
		}
	}
	t.Fatalf("the task is missing: %+v", body.Items)
	return processView{}
}

func TestADeployTaskSaysWhatItDidToTheDeploy(t *testing.T) {
	failed := &kitchenv1alpha1.ProcessRun{
		Name:    controller.DeployTaskRunName(testEnvironment, migrateTask, 1),
		Phase:   kitchenv1alpha1.RunFailed,
		Message: "BackoffLimitExceeded: relation \"orders\" already exists",
	}

	t.Run("a failed run is a release that did not land", func(t *testing.T) {
		h := newHarness(t, nil, withDeployTask(&kitchenv1alpha1.ProcessStatus{
			Name: migrateTask, Type: kitchenv1alpha1.ProcessTask,
			Release: testRelease, Attempt: 1, LastRun: failed, LastFailure: failed,
		})...)
		view := taskView(t, h)
		if view.Deploy != deployFailed || view.Healthy {
			t.Fatalf("a failed deploy task reads as well: %+v", view)
		}
		if view.LastFailure == nil || !strings.Contains(view.LastRun.Message, "already exists") {
			t.Fatalf("the run's own words did not come out of the cluster: %+v", view)
		}
	})

	t.Run("a run recorded against another release has not happened for this deploy", func(t *testing.T) {
		h := newHarness(t, nil, withDeployTask(&kitchenv1alpha1.ProcessStatus{
			Name: migrateTask, Type: kitchenv1alpha1.ProcessTask,
			Release: "shop-rel-older", Attempt: 1,
			LastRun: &kitchenv1alpha1.ProcessRun{Name: "old", Phase: kitchenv1alpha1.RunSucceeded},
		})...)
		if view := taskView(t, h); view.Deploy != deployPending || !view.Healthy {
			t.Fatalf("a rollback's task read as already done: %+v", view)
		}
	})

	t.Run("nothing is known until the reconciler has been round", func(t *testing.T) {
		h := newHarness(t, nil, withDeployTask(nil)...)
		if view := taskView(t, h); view.Deploy != "" || !view.Healthy {
			t.Fatalf("an unreconciled task claimed to know something: %+v", view)
		}
	})
}

func TestRunningADeployTaskAgainResumesTheDeploy(t *testing.T) {
	failed := &kitchenv1alpha1.ProcessRun{
		Name:  controller.DeployTaskRunName(testEnvironment, migrateTask, 2),
		Phase: kitchenv1alpha1.RunFailed,
	}
	h := newHarness(t, nil, withDeployTask(&kitchenv1alpha1.ProcessStatus{
		Name: migrateTask, Type: kitchenv1alpha1.ProcessTask,
		Release: testRelease, Attempt: 2, LastRun: failed, LastFailure: failed,
	})...)

	recorder := h.do(t, http.MethodPost, processesPath+"/"+migrateTask+"/runs", "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// The name is derived rather than generated, so the answer can name the
	// run before the reconciler has made it.
	started := decode[processRunView](t, recorder)
	if want := controller.DeployTaskRunName(testEnvironment, migrateTask, 3); started.Name != want {
		t.Fatalf("want the next attempt %q, got %q", want, started.Name)
	}

	// Nothing is created here: the run is the deploy's, and the deploy is the
	// reconciler's. What the API writes is the one fact it decides from.
	jobs := &batchv1.JobList{}
	if err := h.server.Client.List(context.Background(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("the API composed a run beside the deploy instead of asking for one: %+v", jobs.Items)
	}
	env := &kitchenv1alpha1.Environment{}
	if err := h.server.get(context.Background(), testEnvironment, env); err != nil {
		t.Fatal(err)
	}
	status := env.FindProcessStatus(migrateTask)
	if status == nil || status.Release != "" {
		t.Fatalf("the release the failed run was for was not cleared: %+v", status)
	}
	if status.LastFailure == nil {
		t.Fatal("the failure was forgotten, so there is nothing left to read about it")
	}
}

func TestListingADeployTasksRuns(t *testing.T) {
	started := metav1.NewTime(time.Now().Add(-time.Hour))
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.DeployTaskRunName(testEnvironment, migrateTask, 1),
			Namespace: controller.AppNamespace("shop"),
			Labels: map[string]string{
				controller.LabelEnvironment: testEnvironment,
				controller.LabelProcess:     migrateTask,
			},
		},
		Status: batchv1.JobStatus{
			StartTime: &started,
			Conditions: []batchv1.JobCondition{{
				Type:               batchv1.JobComplete,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(started.Add(time.Minute)),
			}},
		},
	}
	h := newHarness(t, nil, withDeployTask(nil, job)...)

	recorder := h.do(t, http.MethodGet, processesPath+"/"+migrateTask+"/runs", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[struct {
		Items []processRunView `json:"items"`
	}](t, recorder)
	if len(body.Items) != 1 || body.Items[0].Phase != string(kitchenv1alpha1.RunSucceeded) {
		t.Fatalf("a task's runs are read the way every other run is: %+v", body.Items)
	}
}

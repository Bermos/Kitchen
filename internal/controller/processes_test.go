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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

var _ = Describe("Workers and scheduled jobs", func() {
	const (
		projectName = "procshop"
		namespace   = "default"
		image       = "registry.example.com/kitchen/procshop@sha256:0123456789abcdef"
		releaseName = "procshop-rel-000001"
		prodName    = "procshop-production"
		previewName = "procshop-pr-3"
	)
	appNS := "kitchen-" + projectName

	ctx := context.Background()
	releaseKey := types.NamespacedName{Name: releaseName, Namespace: namespace}

	var reconciler *EnvironmentReconciler

	// The two processes every case here starts from: a worker with a command
	// of its own, and a nightly job.
	baseProcesses := func() []kitchenv1alpha1.ProcessSpec {
		return []kitchenv1alpha1.ProcessSpec{
			{
				Name:     "worker",
				Type:     kitchenv1alpha1.ProcessWorker,
				Command:  []string{"node", "worker.js"},
				Replicas: ptr.To(int32(2)),
			},
			{
				Name:              "nightly",
				Type:              kitchenv1alpha1.ProcessCron,
				Schedule:          "0 3 * * *",
				Command:           []string{"node", "report.js"},
				Timeout:           &metav1.Duration{Duration: 30 * time.Minute},
				ConcurrencyPolicy: kitchenv1alpha1.ConcurrencyReplace,
			},
		}
	}

	// declare rewrites the Release's process list. A Release spec is immutable
	// once written, so each case creates its own rather than editing one.
	declare := func(processes []kitchenv1alpha1.ProcessSpec) {
		release := &kitchenv1alpha1.Release{}
		ExpectWithOffset(1, k8sClient.Get(ctx, releaseKey, release)).To(Succeed())
		ExpectWithOffset(1, k8sClient.Delete(ctx, release)).To(Succeed())
		ExpectWithOffset(1, k8sClient.Create(ctx, &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "procshop-bld-1"},
				Image:      image,
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime:   kitchenv1alpha1.RuntimeSpec{Port: 8080},
					Processes: processes,
				},
			},
		})).To(Succeed())
	}

	// environment creates one of the two shapes and reconciles it twice: the
	// first pass adds the finalizer, the second does the work.
	environment := func(name string, envType kitchenv1alpha1.EnvironmentType) *kitchenv1alpha1.Environment {
		spec := kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
			Type:       envType,
			ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
		}
		if envType == kitchenv1alpha1.EnvironmentPreview {
			spec.Preview = &kitchenv1alpha1.PreviewInfo{PullRequest: 3, Branch: "feat/queue"}
		}
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       spec,
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
		key := types.NamespacedName{Name: name, Namespace: namespace}
		for range 2 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, env)).To(Succeed())
		return env
	}

	reconcileAgain := func(name string) *kitchenv1alpha1.Environment {
		key := types.NamespacedName{Name: name, Namespace: namespace}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		env := &kitchenv1alpha1.Environment{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, env)).To(Succeed())
		return env
	}

	deployment := func(name string) (*appsv1.Deployment, bool) {
		deploy := &appsv1.Deployment{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appNS}, deploy)
		if errors.IsNotFound(err) {
			return nil, false
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return deploy, true
	}

	cronJob := func(name string) (*batchv1.CronJob, bool) {
		cron := &batchv1.CronJob{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appNS}, cron)
		if errors.IsNotFound(err) {
			return nil, false
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return cron, true
	}

	BeforeEach(func() {
		reconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
		}))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/procshop",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
				Previews: kitchenv1alpha1.PreviewsSpec{Protected: ptr.To(false)},
			},
		}))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "procshop-bld-1"},
				Image:      image,
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime:   kitchenv1alpha1.RuntimeSpec{Port: 8080},
					Processes: baseProcesses(),
				},
			},
		}))).To(Succeed())
	})

	AfterEach(func() {
		for _, name := range []string{prodName, previewName} {
			env := &kitchenv1alpha1.Environment{}
			key := types.NamespacedName{Name: name, Namespace: namespace}
			if err := k8sClient.Get(ctx, key, env); err != nil {
				continue
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("runs a worker as a Deployment with no Service and no route", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction)

		deploy, found := deployment(prodName + "-worker")
		Expect(found).To(BeTrue())
		Expect(deploy.Labels).To(HaveKeyWithValue(labelProcess, "worker"))
		Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))
		Expect(deploy.Spec.Selector.MatchLabels).To(Equal(map[string]string{
			labelEnvironment: prodName, labelProcess: "worker",
		}), "a worker's pods are its own, not the web process's")

		container := deploy.Spec.Template.Spec.Containers[0]
		Expect(container.Image).To(Equal(image))
		Expect(container.Command).To(Equal([]string{"node", "worker.js"}))
		Expect(container.Env).To(ContainElement(corev1.EnvVar{Name: "KITCHEN_PROCESS", Value: "worker"}))
		Expect(container.Env).To(ContainElement(corev1.EnvVar{Name: "PORT", Value: "8080"}),
			"a worker gets the platform's own variables like everything else")
		Expect(deploy.Spec.Template.Spec.ImagePullSecrets).NotTo(BeEmpty(),
			"a registry that wanted a credential to push wants one to pull")

		By("publishing nothing for it")
		service := &corev1.Service{}
		Expect(errors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: prodName + "-worker", Namespace: appNS}, service))).To(BeTrue())
	})

	It("keeps a worker and a scheduled run out of the environment's Service", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction)

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: prodName, Namespace: appNS}, svc)).To(Succeed())
		selector := labels.SelectorFromSet(svc.Spec.Selector)

		web, found := deployment(prodName)
		Expect(found).To(BeTrue())
		Expect(selector.Matches(labels.Set(web.Spec.Template.Labels))).To(BeTrue(),
			"the web pods are what the URL is meant to reach")

		// A worker has no port to answer the application's on, so a worker
		// pod behind this Service is a backend that refuses connections —
		// which is what one bad endpoint in three looked like in production.
		worker, found := deployment(prodName + "-worker")
		Expect(found).To(BeTrue())
		Expect(selector.Matches(labels.Set(worker.Spec.Template.Labels))).To(BeFalse(),
			"a worker is not addressed, and nothing else's Service addresses it either")

		// A finished run is not ready and therefore harmless; a running one
		// has no probes either, so it would take its share of the traffic for
		// as long as the run took.
		cron, found := cronJob(prodName + "-nightly")
		Expect(found).To(BeTrue())
		Expect(selector.Matches(labels.Set(cron.Spec.JobTemplate.Spec.Template.Labels))).To(BeFalse(),
			"a scheduled run is not a replica of the web process")
	})

	// The declaration the web process got in #239 and a worker did not (#250).
	// Left alone a worker takes the API server's default rolling update, which
	// at one replica surges to a second copy: two of a poller, every rollout.
	It("recreates rather than rolls a singleton worker, and rolls again once it is not one", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction)

		deploy, found := deployment(prodName + "-worker")
		Expect(found).To(BeTrue())
		Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType),
			"a queue consumer is fine overlapping, so this is not the default")

		By("declaring it a singleton")
		single := baseProcesses()
		single[0].Singleton = true
		single[0].Replicas = ptr.To(int32(1))
		declare(single)
		reconcileAgain(prodName)

		deploy, found = deployment(prodName + "-worker")
		Expect(found).To(BeTrue())
		Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))

		By("putting the rolling update back when the declaration is withdrawn")
		declare(baseProcesses())
		reconcileAgain(prodName)

		deploy, found = deployment(prodName + "-worker")
		Expect(found).To(BeTrue())
		Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType),
			"a Deployment left on Recreate keeps an outage after the reason for it has gone")
	})

	// Refused at admission, not clamped: a count quietly lowered reads back as
	// a setting that did not take.
	It("refuses a singleton worker that asks for three of itself, and a singleton schedule", func() {
		project := &kitchenv1alpha1.Project{}
		key := types.NamespacedName{Name: projectName, Namespace: namespace}
		Expect(k8sClient.Get(ctx, key, project)).To(Succeed())

		project.Spec.Processes = []kitchenv1alpha1.ProcessSpec{{
			Name: "worker", Type: kitchenv1alpha1.ProcessWorker,
			Singleton: true, Replicas: ptr.To(int32(3)),
		}}
		err := k8sClient.Update(ctx, project)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("singleton"))

		By("refusing it on a schedule, whose answer to the same question is concurrencyPolicy")
		Expect(k8sClient.Get(ctx, key, project)).To(Succeed())
		project.Spec.Processes = []kitchenv1alpha1.ProcessSpec{{
			Name: "nightly", Type: kitchenv1alpha1.ProcessCron,
			Schedule: "0 3 * * *", Singleton: true,
		}}
		err = k8sClient.Update(ctx, project)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("concurrencyPolicy"))

		By("accepting the worker at one replica")
		Expect(k8sClient.Get(ctx, key, project)).To(Succeed())
		project.Spec.Processes = []kitchenv1alpha1.ProcessSpec{{
			Name: "worker", Type: kitchenv1alpha1.ProcessWorker,
			Singleton: true, Replicas: ptr.To(int32(1)),
		}}
		Expect(k8sClient.Update(ctx, project)).To(Succeed())
	})

	It("runs a scheduled job as a CronJob that does not retry", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction)

		cron, found := cronJob(prodName + "-nightly")
		Expect(found).To(BeTrue())
		Expect(cron.Spec.Schedule).To(Equal("0 3 * * *"))
		Expect(*cron.Spec.TimeZone).To(Equal("Etc/UTC"),
			"a schedule whose meaning depends on where the cluster is installed moves twice a year")
		Expect(cron.Spec.ConcurrencyPolicy).To(Equal(batchv1.ReplaceConcurrent))
		Expect(*cron.Spec.JobTemplate.Spec.ActiveDeadlineSeconds).To(Equal(int64(1800)))
		Expect(*cron.Spec.JobTemplate.Spec.BackoffLimit).To(BeZero(),
			"a scheduled run that failed is a failed run; the schedule is what tries again")
		Expect(cron.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		Expect(cron.Spec.JobTemplate.Labels).To(HaveKeyWithValue(labelProcess, "nightly"),
			"a run is found by the process it belongs to without walking back through its owner")
	})

	It("defaults a scheduled job's timeout and concurrency rather than leaving them open", func() {
		declare([]kitchenv1alpha1.ProcessSpec{{
			Name: "nightly", Type: kitchenv1alpha1.ProcessCron, Schedule: "0 3 * * *",
		}})
		environment(prodName, kitchenv1alpha1.EnvironmentProduction)

		cron, found := cronJob(prodName + "-nightly")
		Expect(found).To(BeTrue())
		Expect(cron.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent))
		Expect(*cron.Spec.JobTemplate.Spec.ActiveDeadlineSeconds).To(Equal(int64(3600)))
	})

	It("runs none of them in a preview unless the process opted in", func() {
		processes := baseProcesses()
		processes[0].Previews = true
		declare(processes)

		env := environment(previewName, kitchenv1alpha1.EnvironmentPreview)

		_, workerFound := deployment(previewName + "-worker")
		Expect(workerFound).To(BeTrue(), "the worker opted in")
		_, cronFound := cronJob(previewName + "-nightly")
		Expect(cronFound).To(BeFalse(),
			"a preview that emails customers nightly is a bad afternoon")

		By("still listing what it will not run, with the reason")
		nightly := env.FindProcessStatus("nightly")
		Expect(nightly).NotTo(BeNil(), "a shorter list would read like a bug")
		Expect(nightly.Suspended).To(BeTrue())
		Expect(nightly.Workload).To(BeEmpty())
	})

	It("tears down a process the release no longer declares, and leaves the web process alone", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction)
		_, found := deployment(prodName + "-worker")
		Expect(found).To(BeTrue())

		By("declaring only the scheduled job")
		declare([]kitchenv1alpha1.ProcessSpec{{
			Name: "nightly", Type: kitchenv1alpha1.ProcessCron, Schedule: "0 3 * * *",
		}})
		env := reconcileAgain(prodName)

		_, workerFound := deployment(prodName + "-worker")
		Expect(workerFound).To(BeFalse())
		_, cronFound := cronJob(prodName + "-nightly")
		Expect(cronFound).To(BeTrue())
		Expect(env.FindProcessStatus("worker")).To(BeNil())

		By("never catching the web process, which carries no process label")
		_, webFound := deployment(prodName)
		Expect(webFound).To(BeTrue())
	})

	It("carries a failed run out of the cluster and keeps it after a later success", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction)

		failed := runJob(ctx, appNS, prodName, "nightly", prodName+"-nightly-1", batchv1.JobFailed,
			"BackoffLimitExceeded", "Job has reached the specified backoff limit", time.Now().Add(-2*time.Hour))
		env := reconcileAgain(prodName)

		nightly := env.FindProcessStatus("nightly")
		Expect(nightly).NotTo(BeNil())
		Expect(nightly.LastRun).NotTo(BeNil())
		Expect(nightly.LastRun.Name).To(Equal(failed))
		Expect(nightly.LastRun.Phase).To(Equal(kitchenv1alpha1.RunFailed))
		Expect(nightly.LastRun.Message).To(ContainSubstring("backoff limit"))
		Expect(nightly.LastFailure).NotTo(BeNil())

		By("a later success moving lastRun on and leaving lastFailure where it is")
		runJob(ctx, appNS, prodName, "nightly", prodName+"-nightly-2", batchv1.JobComplete,
			"", "", time.Now().Add(-time.Hour))
		env = reconcileAgain(prodName)

		nightly = env.FindProcessStatus("nightly")
		Expect(nightly.LastRun.Phase).To(Equal(kitchenv1alpha1.RunSucceeded))
		Expect(nightly.LastFailure).NotTo(BeNil(),
			"a job that fails four nights in five must not read as healthy on the fifth")
		Expect(nightly.LastFailure.Name).To(Equal(failed))
	})

	It("reports a worker's readiness", func() {
		env := environment(prodName, kitchenv1alpha1.EnvironmentProduction)

		worker := env.FindProcessStatus("worker")
		Expect(worker).NotTo(BeNil())
		Expect(worker.Type).To(Equal(kitchenv1alpha1.ProcessWorker))
		Expect(worker.Workload).To(Equal(prodName + "-worker"))
		Expect(worker.Replicas).To(Equal(int32(2)))
		Expect(worker.ReadyReplicas).To(BeZero(), "envtest runs no kubelet, so nothing becomes ready")
	})

	It("takes everything down with the environment", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction)
		// A run started by hand has no CronJob to be garbage-collected with,
		// so it is the one that would be left behind.
		runJob(ctx, appNS, prodName, "nightly", prodName+"-nightly-manual-1", batchv1.JobComplete,
			"", "", time.Now().Add(-time.Hour))

		env := &kitchenv1alpha1.Environment{}
		key := types.NamespacedName{Name: prodName, Namespace: namespace}
		Expect(k8sClient.Get(ctx, key, env)).To(Succeed())
		Expect(k8sClient.Delete(ctx, env)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		_, workerFound := deployment(prodName + "-worker")
		Expect(workerFound).To(BeFalse())
		_, cronFound := cronJob(prodName + "-nightly")
		Expect(cronFound).To(BeFalse())

		By("taking the runs with it, including one nothing owns")
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(appNS))).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())
	})
})

// runJob writes a finished Job the way a CronJob would have left one, so the
// reconciler has something to read a run off. The conditions are what it reads
// — not `status.failed`, which counts pods and is zero for a run that hit its
// deadline.
func runJob(
	ctx context.Context,
	appNS, envName, processName, name string,
	condition batchv1.JobConditionType,
	reason, message string,
	startedAt time.Time,
) string {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: appNS,
			Labels: map[string]string{
				labelEnvironment: envName,
				labelProcess:     processName,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "app", Image: "example.invalid/app:latest"}},
				},
			},
		},
	}
	ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, job))).To(Succeed())

	start := metav1.NewTime(startedAt)
	finish := metav1.NewTime(startedAt.Add(30 * time.Second))
	job.Status.StartTime = &start
	job.Status.Conditions = []batchv1.JobCondition{{
		Type:               condition,
		Status:             corev1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: finish,
	}}
	// The API server refuses either terminal condition on its own: the Job
	// controller sets the interim one first, and the status subresource
	// enforces that order. The reconciler reads only the terminal one, which
	// is what these two lines make it possible to write down.
	interim := batchv1.JobFailureTarget
	if condition == batchv1.JobComplete {
		interim = batchv1.JobSuccessCriteriaMet
		job.Status.CompletionTime = &finish
	}
	job.Status.Conditions = append([]batchv1.JobCondition{{
		Type:               interim,
		Status:             corev1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: finish,
	}}, job.Status.Conditions...)
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, job)).To(Succeed())
	return name
}

// A process's pod, and which of them the platform probes (#236). A worker is
// probed only where it asked to be — it publishes no port to fall back on —
// and a scheduled run is never probed, because how it went is its exit status.
func TestProcessPodSpecProbes(t *testing.T) {
	release := &kitchenv1alpha1.Release{Spec: kitchenv1alpha1.ReleaseSpec{Image: "registry.example.com/app@sha256:abc"}}
	project := &kitchenv1alpha1.Project{Spec: kitchenv1alpha1.ProjectSpec{
		Registry: kitchenv1alpha1.RegistrySpec{ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "harbor"}},
	}}

	for name, tc := range map[string]struct {
		process kitchenv1alpha1.ProcessSpec

		wantProbes   bool
		wantPort     int32
		wantLiveness bool
	}{
		"a worker that declared no health check is not probed": {
			process: kitchenv1alpha1.ProcessSpec{Name: "worker", Type: kitchenv1alpha1.ProcessWorker},
		},
		"a worker with a health port gets a TCP check on it": {
			process: kitchenv1alpha1.ProcessSpec{
				Name: "worker", Type: kitchenv1alpha1.ProcessWorker,
				Health: &kitchenv1alpha1.HealthSpec{Port: 9000},
			},
			wantProbes: true,
			wantPort:   9000,
		},
		"a worker with a path gets liveness too": {
			process: kitchenv1alpha1.ProcessSpec{
				Name: "worker", Type: kitchenv1alpha1.ProcessWorker,
				Health: &kitchenv1alpha1.HealthSpec{Path: "/healthz", Port: 9000},
			},
			wantProbes:   true,
			wantPort:     9000,
			wantLiveness: true,
		},
		"a scheduled process is never probed": {
			process: kitchenv1alpha1.ProcessSpec{
				Name: "nightly", Type: kitchenv1alpha1.ProcessCron, Schedule: "0 3 * * *",
				// Admission refuses this, so it can only arrive on an object
				// written before the rule existed. It is still not probed.
				Health: &kitchenv1alpha1.HealthSpec{Port: 9000},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := processPodSpec(release, project, nil, tc.process)
			container := spec.Containers[0]
			if !tc.wantProbes {
				if container.StartupProbe != nil || container.ReadinessProbe != nil || container.LivenessProbe != nil {
					t.Fatalf("want no probes, got %+v", container)
				}
				return
			}
			if container.StartupProbe == nil || container.ReadinessProbe == nil {
				t.Fatalf("want a startup and a readiness probe, got %+v", container)
			}
			if (container.LivenessProbe != nil) != tc.wantLiveness {
				t.Fatalf("want liveness=%v, got %+v", tc.wantLiveness, container.LivenessProbe)
			}
			port := container.ReadinessProbe.TCPSocket
			if container.ReadinessProbe.HTTPGet != nil {
				if got := container.ReadinessProbe.HTTPGet.Port.IntVal; got != tc.wantPort {
					t.Fatalf("want port %d, got %d", tc.wantPort, got)
				}
				return
			}
			if port == nil || port.Port.IntVal != tc.wantPort {
				t.Fatalf("want a TCP check on %d, got %+v", tc.wantPort, container.ReadinessProbe.ProbeHandler)
			}
		})
	}
}

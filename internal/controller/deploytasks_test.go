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
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Work that runs once per deploy and finishes before the release takes
// traffic (#272).
//
// What these cases hold up is the issue's five criteria, and the one property
// underneath all of them: while a task is unfinished the reconcile applies
// *nothing*, so whatever was serving is still serving.

var _ = Describe("A deploy-time task", func() {
	const (
		projectName = "taskshop"
		namespace   = "default"
		firstImage  = "registry.example.com/kitchen/taskshop@sha256:1111111111111111"
		secondImage = "registry.example.com/kitchen/taskshop@sha256:2222222222222222"
		prodName    = "taskshop-production"
		previewName = "taskshop-pr-3"
		migrate     = "migrate"
	)
	appNS := "kitchen-" + projectName

	ctx := context.Background()

	var reconciler *EnvironmentReconciler
	var releases int

	task := func() kitchenv1alpha1.ProcessSpec {
		return kitchenv1alpha1.ProcessSpec{
			Name:    migrate,
			Type:    kitchenv1alpha1.ProcessTask,
			Command: []string{"npm", "run", "migrate"},
		}
	}

	release := func(image string, processes []kitchenv1alpha1.ProcessSpec) string {
		releases++
		name := "taskshop-rel-" + strconv.Itoa(releases)
		ExpectWithOffset(1, k8sClient.Create(ctx, &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "taskshop-bld-1"},
				Image:      image,
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime:   kitchenv1alpha1.RuntimeSpec{Port: 3000},
					Processes: processes,
				},
			},
		})).To(Succeed())
		return name
	}

	reconcileOnce := func(name string) {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	// environment creates one and reconciles it twice, which is what every
	// other case in this package does — the difference here is that two passes
	// leave a task running rather than a deploy finished.
	environment := func(name string, envType kitchenv1alpha1.EnvironmentType, releaseName string) {
		spec := kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
			Type:       envType,
			ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
		}
		if envType == kitchenv1alpha1.EnvironmentPreview {
			spec.Preview = &kitchenv1alpha1.PreviewInfo{PullRequest: 3, Branch: "feat/schema"}
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       spec,
		}))).To(Succeed())
		for range 2 {
			reconcileOnce(name)
		}
	}

	moveTo := func(name, releaseName string) {
		env := &kitchenv1alpha1.Environment{}
		key := types.NamespacedName{Name: name, Namespace: namespace}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, env)).To(Succeed())
		env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: releaseName}
		ExpectWithOffset(1, k8sClient.Update(ctx, env)).To(Succeed())
		reconcileOnce(name)
	}

	get := func(name string, into client.Object) bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appNS}, into)
		if errors.IsNotFound(err) {
			return false
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return true
	}

	// The runs of one task, however many there have been. envtest runs no Job
	// controller, so a run only ever ends because a case says it did.
	runs := func(processName string) []batchv1.Job {
		jobs := &batchv1.JobList{}
		ExpectWithOffset(1, k8sClient.List(ctx, jobs, client.InNamespace(appNS), client.MatchingLabels{
			labelEnvironment: prodName, labelProcess: processName,
		})).To(Succeed())
		return jobs.Items
	}

	finish := func(name string, condition batchv1.JobConditionType) {
		job := &batchv1.Job{}
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: appNS}, job)).To(Succeed())
		now := metav1.Now()
		job.Status.StartTime = &now
		// The API server refuses a terminal condition without the interim one
		// the Job controller sets first, so a run is ended here the way the
		// controller would end it.
		interim := batchv1.JobFailureTarget
		if condition == batchv1.JobComplete {
			interim = batchv1.JobSuccessCriteriaMet
		}
		job.Status.Conditions = []batchv1.JobCondition{
			{
				Type:               interim,
				Status:             corev1.ConditionTrue,
				Reason:             "BackoffLimitExceeded",
				LastTransitionTime: now,
			},
			{
				Type:               condition,
				Status:             corev1.ConditionTrue,
				Reason:             "BackoffLimitExceeded",
				Message:            "relation \"orders\" already exists",
				LastTransitionTime: now,
			},
		}
		if condition == batchv1.JobComplete {
			job.Status.Conditions[0].Reason = "Completed"
			job.Status.Conditions[1].Reason = "Completed"
			job.Status.Conditions[1].Message = ""
			job.Status.CompletionTime = &now
		}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	condition := func(env *kitchenv1alpha1.Environment, name string) *metav1.Condition {
		return meta.FindStatusCondition(env.Status.Conditions, name)
	}

	BeforeEach(func() {
		reconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		releases = 0

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
		})
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/taskshop",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
				Previews: kitchenv1alpha1.PreviewsSpec{Protected: ptr.To(false)},
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
			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			for range 2 {
				reconcileOnce(name)
			}
		}
		list := &kitchenv1alpha1.ReleaseList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
		for i := range list.Items {
			if strings.HasPrefix(list.Items[i].Name, "taskshop-rel-") {
				Expect(k8sClient.Delete(ctx, &list.Items[i])).To(Succeed())
			}
		}
	})

	It("runs once for a deploy, and nothing of the release starts until it succeeds", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction,
			release(firstImage, []kitchenv1alpha1.ProcessSpec{
				task(),
				{Name: "worker", Type: kitchenv1alpha1.ProcessWorker, Previews: ptr.To(true)},
			}))

		By("making exactly one run, however many times the environment is reconciled")
		for range 3 {
			reconcileOnce(prodName)
		}
		started := runs(migrate)
		Expect(started).To(HaveLen(1), "a run per reconcile would be a migration per reconcile")
		Expect(started[0].Name).To(Equal(prodName + "-" + migrate + "-1"))

		By("holding everything else back while it runs")
		Expect(get(prodName, &appsv1.Deployment{})).To(BeFalse(),
			"a deploy that writes the Deployment first is a deploy that takes traffic mid-migration")
		Expect(get(prodName, &corev1.Service{})).To(BeFalse())
		Expect(get(prodName+"-worker", &appsv1.Deployment{})).To(BeFalse(),
			"a unit deploys as one, so a worker is as much 'taking traffic' as the web process")

		By("saying so on the environment rather than only in a log")
		env := environmentStatus(ctx, prodName)
		Expect(env.Status.Phase).To(Equal(kitchenv1alpha1.EnvironmentDeploying))
		Expect(condition(env, condDeployTasks).Reason).To(Equal("TaskRunning"))
		Expect(condition(env, condDeployTasks).Message).To(ContainSubstring(started[0].Name))
		row := env.FindProcessStatus(migrate)
		Expect(row).NotTo(BeNil())
		Expect(row.LastRun.Name).To(Equal(started[0].Name))
		Expect(row.LastRun.Phase).To(Equal(kitchenv1alpha1.RunRunning))

		By("deploying once it succeeds")
		finish(started[0].Name, batchv1.JobComplete)
		reconcileOnce(prodName)
		Expect(get(prodName, &appsv1.Deployment{})).To(BeTrue())
		Expect(get(prodName+"-worker", &appsv1.Deployment{})).To(BeTrue())
		env = environmentStatus(ctx, prodName)
		Expect(condition(env, condDeployTasks).Status).To(Equal(metav1.ConditionTrue))
		Expect(env.FindProcessStatus(migrate).LastRun.Phase).To(Equal(kitchenv1alpha1.RunSucceeded))

		By("not running it again for the release it has already run for")
		for range 3 {
			reconcileOnce(prodName)
		}
		Expect(runs(migrate)).To(HaveLen(1))
	})

	It("runs as the environment's own workload, and is not one of its backends", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction,
			release(firstImage, []kitchenv1alpha1.ProcessSpec{task()}))

		job := runs(migrate)[0]
		Expect(job.Labels[labelEnvironment]).To(Equal(prodName))
		Expect(job.Labels[labelProcess]).To(Equal(migrate))
		Expect(job.Spec.Template.Labels).To(HaveKeyWithValue(labelEnvironment, prodName))
		Expect(job.Spec.Template.Labels).NotTo(HaveKey(LabelComponent),
			"a Service selector is equality-only, so a component label here would put a migration "+
				"pod behind the environment's URL")

		pod := job.Spec.Template.Spec
		Expect(pod.Containers[0].Image).To(Equal(firstImage), "the release's own image, run with another command")
		Expect(pod.Containers[0].Command).To(Equal([]string{"npm", "run", "migrate"}))
		Expect(pod.ImagePullSecrets).To(HaveLen(1), "a private registry needs the credential to pull, too")
		Expect(pod.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		Expect(*job.Spec.BackoffLimit).To(BeNumerically("==", 0),
			"a retried run is a second migration nobody asked for while the deploy waits")
		Expect(*job.Spec.ActiveDeadlineSeconds).To(BeNumerically("==", int64(kitchenv1alpha1.DefaultRunTimeout.Seconds())))

		var kitchenProcess, port string
		for _, variable := range pod.Containers[0].Env {
			switch variable.Name {
			case "KITCHEN_PROCESS":
				kitchenProcess = variable.Value
			case "PORT":
				port = variable.Value
			}
		}
		Expect(kitchenProcess).To(Equal(migrate), "one image serving four roles has no other way to know which it is")
		Expect(port).To(Equal("3000"), "the platform's own variables reach a task like every other workload")
	})

	It("fails the deploy rather than degrading into a warning, and leaves the release that was serving", func() {
		first := release(firstImage, []kitchenv1alpha1.ProcessSpec{task()})
		environment(prodName, kitchenv1alpha1.EnvironmentProduction, first)
		finish(runs(migrate)[0].Name, batchv1.JobComplete)
		reconcileOnce(prodName)

		deploy := &appsv1.Deployment{}
		Expect(get(prodName, deploy)).To(BeTrue())
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(firstImage))

		By("moving to a release whose task fails")
		moveTo(prodName, release(secondImage, []kitchenv1alpha1.ProcessSpec{task()}))
		second := runs(migrate)
		Expect(second).To(HaveLen(2))
		finish(prodName+"-"+migrate+"-2", batchv1.JobFailed)
		reconcileOnce(prodName)

		By("leaving the previous release serving")
		Expect(get(prodName, deploy)).To(BeTrue())
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(firstImage),
			"a failed migration that still rolled the pods would be the outage this feature exists to prevent")

		By("saying what failed, where, and with the run's own words")
		env := environmentStatus(ctx, prodName)
		Expect(env.Status.Phase).To(Equal(kitchenv1alpha1.EnvironmentDegraded))
		blocked := condition(env, condDeployTasks)
		Expect(blocked.Status).To(Equal(metav1.ConditionFalse))
		Expect(blocked.Reason).To(Equal("TaskFailed"))
		Expect(blocked.Message).To(ContainSubstring("relation \"orders\" already exists"))
		Expect(condition(env, condReady).Reason).To(Equal("TaskFailed"))
		Expect(env.FindProcessStatus(migrate).LastFailure.Name).To(Equal(prodName + "-" + migrate + "-2"))

		By("staying failed rather than retrying itself")
		for range 3 {
			reconcileOnce(prodName)
		}
		Expect(runs(migrate)).To(HaveLen(2))

		By("running again when the failed run's record is cleared, which is what a retry is")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prodName, Namespace: namespace}, env)).To(Succeed())
		env.FindProcessStatus(migrate).Release = ""
		Expect(k8sClient.Status().Update(ctx, env)).To(Succeed())
		reconcileOnce(prodName)
		Expect(runs(migrate)).To(HaveLen(3))
		finish(prodName+"-"+migrate+"-3", batchv1.JobComplete)
		reconcileOnce(prodName)
		Expect(get(prodName, deploy)).To(BeTrue())
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(secondImage),
			"a retry that succeeds is the deploy carrying on, not a run beside it")
	})

	It("runs the work again for the release a rollback goes back to", func() {
		first := release(firstImage, []kitchenv1alpha1.ProcessSpec{task()})
		environment(prodName, kitchenv1alpha1.EnvironmentProduction, first)
		finish(prodName+"-"+migrate+"-1", batchv1.JobComplete)
		reconcileOnce(prodName)

		moveTo(prodName, release(secondImage, []kitchenv1alpha1.ProcessSpec{task()}))
		finish(prodName+"-"+migrate+"-2", batchv1.JobComplete)
		reconcileOnce(prodName)

		By("rolling back")
		moveTo(prodName, first)
		Expect(runs(migrate)).To(HaveLen(3),
			"the release being rolled back to declared work that has to run for the environment to serve it")
		env := environmentStatus(ctx, prodName)
		Expect(env.FindProcessStatus(migrate).Release).To(Equal(first))
		Expect(env.FindProcessStatus(migrate).LastRun.Name).To(Equal(prodName + "-" + migrate + "-3"))

		By("holding the rollback until it succeeds, then finishing it")
		deploy := &appsv1.Deployment{}
		Expect(get(prodName, deploy)).To(BeTrue())
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(secondImage))
		finish(prodName+"-"+migrate+"-3", batchv1.JobComplete)
		reconcileOnce(prodName)
		Expect(get(prodName, deploy)).To(BeTrue())
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(firstImage))
	})

	It("waits for the previous deploy's run rather than stopping it half way", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction,
			release(firstImage, []kitchenv1alpha1.ProcessSpec{task()}))
		Expect(runs(migrate)).To(HaveLen(1))

		By("moving to another release while the first run is still going")
		moveTo(prodName, release(secondImage, []kitchenv1alpha1.ProcessSpec{task()}))
		Expect(runs(migrate)).To(HaveLen(1),
			"two migrations at once is worse than a deploy that waited")
		env := environmentStatus(ctx, prodName)
		Expect(condition(env, condDeployTasks).Reason).To(Equal("PreviousRunActive"))

		By("starting the new one once the old one is out of the way")
		finish(prodName+"-"+migrate+"-1", batchv1.JobComplete)
		reconcileOnce(prodName)
		Expect(runs(migrate)).To(HaveLen(2))
		env = environmentStatus(ctx, prodName)
		Expect(env.FindProcessStatus(migrate).LastRun.Name).To(Equal(prodName + "-" + migrate + "-2"))
	})

	It("runs the tasks in the order they are declared, one at a time", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction,
			release(firstImage, []kitchenv1alpha1.ProcessSpec{
				task(),
				{Name: "seed", Type: kitchenv1alpha1.ProcessTask, Command: []string{"npm", "run", "seed"}},
			}))

		Expect(runs(migrate)).To(HaveLen(1))
		Expect(runs("seed")).To(BeEmpty(), "\"migrate, then seed\" is a sentence, not a race")

		finish(prodName+"-"+migrate+"-1", batchv1.JobComplete)
		reconcileOnce(prodName)
		Expect(runs("seed")).To(HaveLen(1))
		Expect(get(prodName, &appsv1.Deployment{})).To(BeFalse(), "one of two tasks is not the work done")

		finish(prodName+"-seed-1", batchv1.JobComplete)
		reconcileOnce(prodName)
		Expect(get(prodName, &appsv1.Deployment{})).To(BeTrue())
	})

	It("runs in a preview by default, against the preview's own resources", func() {
		environment(previewName, kitchenv1alpha1.EnvironmentPreview,
			release(firstImage, []kitchenv1alpha1.ProcessSpec{task()}))

		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(appNS), client.MatchingLabels{
			labelEnvironment: previewName, labelProcess: migrate,
		})).To(Succeed())
		Expect(jobs.Items).To(HaveLen(1),
			"a preview gets its own branch of the database, and a branch nothing migrated is a broken preview")
		Expect(jobs.Items[0].Name).To(Equal(previewName + "-" + migrate + "-1"))
	})

	It("holds nothing up where it was taken out of previews", func() {
		environment(previewName, kitchenv1alpha1.EnvironmentPreview,
			release(firstImage, []kitchenv1alpha1.ProcessSpec{{
				Name: migrate, Type: kitchenv1alpha1.ProcessTask, Previews: ptr.To(false),
			}}))

		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(appNS), client.MatchingLabels{
			labelEnvironment: previewName, labelProcess: migrate,
		})).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())
		Expect(get(previewName, &appsv1.Deployment{})).To(BeTrue(),
			"a task that does not run here cannot be a task this environment waits for")

		env := environmentStatus(ctx, previewName)
		Expect(env.FindProcessStatus(migrate).Suspended).To(BeTrue(),
			"a preview's workload list is the project's with a reason beside each entry")
		Expect(condition(env, condDeployTasks).Status).To(Equal(metav1.ConditionTrue))
	})

	It("says nothing at all on a release that declares no task", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction,
			release(firstImage, []kitchenv1alpha1.ProcessSpec{
				{Name: "worker", Type: kitchenv1alpha1.ProcessWorker},
			}))
		env := environmentStatus(ctx, prodName)
		Expect(condition(env, condDeployTasks)).To(BeNil(),
			"a condition every environment carries and that always says the same thing is noise")
		Expect(get(prodName, &appsv1.Deployment{})).To(BeTrue())
	})
})

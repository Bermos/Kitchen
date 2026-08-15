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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

var _ = Describe("PlatformUpdate Controller", func() {
	const (
		runningVersion = "0.2.0"
		updateSA       = "kitchen-self-update"
		updateChart    = "oci://ghcr.io/bermos/charts/kitchen"
	)

	ctx := context.Background()

	enabledConfig := SelfUpdateConfig{
		Chart:          updateChart,
		Release:        "kitchen",
		ServiceAccount: updateSA,
		HelmImage:      DefaultHelmImage,
		Timeout:        DefaultSelfUpdateTimeout,
	}

	// reconcilerFor builds a reconciler around one configuration, so each spec
	// can say which installation it is describing.
	reconcilerFor := func(cfg SelfUpdateConfig) *PlatformUpdateReconciler {
		return &PlatformUpdateReconciler{
			Client:         k8sClient,
			Scheme:         k8sClient.Scheme(),
			SelfUpdate:     cfg,
			CurrentVersion: runningVersion,
		}
	}

	createUpdate := func(name, version string) types.NamespacedName {
		update := &kitchenv1alpha1.PlatformUpdate{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       kitchenv1alpha1.PlatformUpdateSpec{Version: version},
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, update))).To(Succeed())
		return types.NamespacedName{Name: name}
	}

	reconcileOnce := func(r *PlatformUpdateReconciler, key types.NamespacedName) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	fetch := func(key types.NamespacedName) *kitchenv1alpha1.PlatformUpdate {
		update := &kitchenv1alpha1.PlatformUpdate{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, update)).To(Succeed())
		return update
	}

	// completeJob and failJob drive a Job's status the way the job controller
	// would. Both conditions and startTime are required by the API server's
	// validation of a finished Job, so a half-filled status is rejected
	// rather than simply looking odd.
	completeJob := func(key types.NamespacedName) {
		job := &batchv1.Job{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, job)).To(Succeed())
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.CompletionTime = &now
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	failJob := func(key types.NamespacedName, message string) {
		job := &batchv1.Job{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, job)).To(Succeed())
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Message: message},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: message},
		}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())
		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: updateSA, Namespace: PlatformNamespace},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, sa))).To(Succeed())
	})

	// PlatformUpdates are cluster-scoped and outlive the spec that made them,
	// and one left Running holds the concurrency gate shut against every spec
	// after it. Clearing them is what keeps each spec describing one
	// installation rather than the residue of the last.
	// Background propagation, not the default: deleting a Job orphans its pods
	// unless told otherwise, and orphaning adds a finalizer that only the
	// garbage collector clears — which envtest does not run. The default would
	// leave every job of every spec behind, still readable by the next one.
	AfterEach(func() {
		Expect(k8sClient.DeleteAllOf(ctx, &kitchenv1alpha1.PlatformUpdate{})).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &batchv1.Job{},
			client.InNamespace(PlatformNamespace),
			client.MatchingLabels{labelComponentKind: selfUpdateComponent},
			client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
	})

	Context("preflight", func() {
		It("refuses to run when the chart did not enable self-update", func() {
			key := createUpdate("pu-disabled", "0.3.0")
			reconcileOnce(reconcilerFor(SelfUpdateConfig{}), key)

			update := fetch(key)
			Expect(update.Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdateFailed))
			Expect(update.Status.Message).To(ContainSubstring("selfUpdate.enabled=true"))

			By("creating no job at all")
			job := &batchv1.Job{}
			err := k8sClient.Get(ctx,
				types.NamespacedName{Namespace: PlatformNamespace, Name: selfUpdateJobName("pu-disabled")}, job)
			Expect(err).To(HaveOccurred())
		})

		It("refuses a downgrade and points at helm rollback", func() {
			key := createUpdate("pu-downgrade", "0.1.4")
			reconcileOnce(reconcilerFor(enabledConfig), key)

			update := fetch(key)
			Expect(update.Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdateFailed))
			Expect(update.Status.Message).To(ContainSubstring("helm rollback"))
		})

		It("refuses the version it is already on", func() {
			key := createUpdate("pu-current", runningVersion)
			reconcileOnce(reconcilerFor(enabledConfig), key)

			Expect(fetch(key).Status.Message).To(ContainSubstring("already running"))
		})

		It("refuses to cross a minor version unless allowed", func() {
			key := createUpdate("pu-minor", "0.3.0")
			reconcileOnce(reconcilerFor(enabledConfig), key)

			update := fetch(key)
			Expect(update.Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdateFailed))
			Expect(update.Status.Message).To(ContainSubstring("selfUpdate.allowMinor=true"))
		})

		It("crosses a minor version when the chart allows it", func() {
			cfg := enabledConfig
			cfg.AllowMinor = true
			key := createUpdate("pu-minor-allowed", "0.3.0")
			reconcileOnce(reconcilerFor(cfg), key)

			Expect(fetch(key).Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdateRunning))
		})

		It("refuses when the operator is not a published build", func() {
			r := reconcilerFor(enabledConfig)
			r.CurrentVersion = "dev"
			key := createUpdate("pu-dev", "0.2.1")
			reconcileOnce(r, key)

			Expect(fetch(key).Status.Message).To(ContainSubstring("Upgrade with helm"))
		})

		It("refuses when the promised ServiceAccount is missing, before touching anything", func() {
			cfg := enabledConfig
			cfg.ServiceAccount = "kitchen-self-update-absent"
			key := createUpdate("pu-no-sa", "0.2.1")
			reconcileOnce(reconcilerFor(cfg), key)

			update := fetch(key)
			Expect(update.Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdateFailed))
			Expect(update.Status.Message).To(ContainSubstring("kitchen-self-update-absent"))
		})
	})

	Context("running the upgrade", func() {
		const updateName = "pu-patch"

		var (
			r      *PlatformUpdateReconciler
			key    types.NamespacedName
			jobKey types.NamespacedName
		)

		BeforeEach(func() {
			r = reconcilerFor(enabledConfig)
			key = createUpdate(updateName, "0.2.1")
			jobKey = types.NamespacedName{Namespace: PlatformNamespace, Name: selfUpdateJobName(updateName)}
		})

		It("runs helm in a job that outlives the operator it replaces", func() {
			reconcileOnce(r, key)

			update := fetch(key)
			Expect(update.Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdateRunning))
			Expect(update.Status.FromVersion).To(Equal(runningVersion))
			Expect(update.Status.JobName).To(Equal(jobKey.Name))

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal(updateSA))
			Expect(*job.Spec.BackoffLimit).To(BeEquivalentTo(0))

			By("giving the job a deadline helm's own timeout expires well inside of")
			Expect(*job.Spec.ActiveDeadlineSeconds).To(
				BeNumerically(">", int64(DefaultSelfUpdateTimeout.Seconds())))

			container := job.Spec.Template.Spec.Containers[0]
			args := strings.Join(container.Args, " ")
			Expect(args).To(ContainSubstring("upgrade kitchen " + updateChart))
			Expect(args).To(ContainSubstring("--version 0.2.1"))
			Expect(args).To(ContainSubstring("--atomic"))

			By("keeping values the new chart introduced rather than only the ones already set")
			Expect(args).To(ContainSubstring("--reset-then-reuse-values"))
			Expect(args).NotTo(ContainSubstring("--reuse-values "))

			By("passing nothing through from the request but the version")
			Expect(args).NotTo(ContainSubstring("--set"))
			Expect(args).NotTo(ContainSubstring("--values"))
		})

		It("reports success from the job, not from what it remembers starting", func() {
			reconcileOnce(r, key)

			completeJob(jobKey)

			// A fresh reconciler: the operator that finishes an upgrade is
			// never the process that started it.
			reconcileOnce(reconcilerFor(enabledConfig), key)

			update := fetch(key)
			Expect(update.Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdateSucceeded))
			Expect(update.Status.CompletedAt).NotTo(BeNil())
		})

		It("reports a failed upgrade as rolled back", func() {
			reconcileOnce(r, key)

			failJob(jobKey, "BackoffLimitExceeded")
			reconcileOnce(r, key)

			update := fetch(key)
			Expect(update.Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdateFailed))
			Expect(update.Status.Message).To(ContainSubstring("rolled back"))
		})

		It("does not start a second upgrade once the job has been reaped", func() {
			reconcileOnce(r, key)

			completeJob(jobKey)
			reconcileOnce(r, key)
			Expect(fetch(key).Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdateSucceeded))

			By("deleting the finished job as its TTL would")
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			Expect(k8sClient.Delete(ctx, job,
				client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
			reconcileOnce(r, key)

			err := k8sClient.Get(ctx, jobKey, &batchv1.Job{})
			Expect(err).To(HaveOccurred(), "a finished update must not create a second job")
			Expect(fetch(key).Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdateSucceeded))
		})
	})

	Context("when an upgrade is already in flight", func() {
		It("queues the second one rather than running two helms at the same release", func() {
			first := createUpdate("pu-first", "0.2.1")
			reconcileOnce(reconcilerFor(enabledConfig), first)
			Expect(fetch(first).Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdateRunning))

			second := createUpdate("pu-second", "0.2.2")
			reconcileOnce(reconcilerFor(enabledConfig), second)

			update := fetch(second)
			Expect(update.Status.Phase).To(Equal(kitchenv1alpha1.PlatformUpdatePending))
			Expect(update.Status.Message).To(ContainSubstring("pu-first"))

			job := &batchv1.Job{}
			err := k8sClient.Get(ctx,
				types.NamespacedName{Namespace: PlatformNamespace, Name: selfUpdateJobName("pu-second")}, job)
			Expect(err).To(HaveOccurred())
		})
	})
})

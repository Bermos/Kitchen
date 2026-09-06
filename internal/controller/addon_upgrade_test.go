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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// An addon's installed versions are singular and current. These are the specs
// that say the platform remembers moving them.
var _ = Describe("Recording an addon upgrade", func() {
	ctx := context.Background()
	entry := entryOf(AddonKeda)

	var reconciler *AddonReconciler
	var created []client.Object

	track := func(obj client.Object) {
		GinkgoHelper()
		if addon, ok := obj.(*kitchenv1alpha1.Addon); ok {
			ensureAddon(ctx, addon)
		} else {
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		}
		created = append(created, obj)
	}

	// Each spec installs its own version pair, so the job it creates — and so
	// the record named after that job — can only be its own.
	pinned := func(version string) AddonInstalls {
		return AddonInstalls{AddonKeda: {
			ServiceAccount: "kitchen-keda-install",
			Versions:       map[string]string{kedaChartName: version},
		}}
	}

	reconcile := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: PlatformNamespace, Name: AddonKeda},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	addonOf := func() *kitchenv1alpha1.Addon {
		GinkgoHelper()
		addon := &kitchenv1alpha1.Addon{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: PlatformNamespace, Name: AddonKeda}, addon)).To(Succeed())
		return addon
	}

	// installedAlready is the state the reconciler treats as "the platform
	// put this here, at these versions" — which is what makes the next
	// reconcile an upgrade rather than an install.
	installedAlready := func(charts ...kitchenv1alpha1.AddonChartStatus) {
		GinkgoHelper()
		addon := addonOf()
		addon.Status.Managed = true
		addon.Status.Namespace = entry.DefaultNamespace
		addon.Status.Charts = charts
		Expect(k8sClient.Status().Update(ctx, addon)).To(Succeed())
	}

	// jobFor is the install job this config would create, once it exists.
	jobFor := func(cfg AddonInstallConfig) *batchv1.Job {
		GinkgoHelper()
		job := &batchv1.Job{}
		key := types.NamespacedName{Namespace: PlatformNamespace, Name: addonInstallJobName(entry, cfg)}
		Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
		created = append(created, job)
		return job
	}

	// recordsFor is every upgrade record left by one job. Records are kept
	// forever and this suite shares a namespace, so a spec asserts about its
	// own job and never about the list.
	recordsFor := func(job *batchv1.Job) []kitchenv1alpha1.AddonUpgrade {
		GinkgoHelper()
		upgrades := &kitchenv1alpha1.AddonUpgradeList{}
		Expect(k8sClient.List(ctx, upgrades, client.InNamespace(PlatformNamespace))).To(Succeed())
		mine := []kitchenv1alpha1.AddonUpgrade{}
		for _, upgrade := range upgrades.Items {
			if upgrade.Spec.JobName == job.Name {
				mine = append(mine, upgrade)
			}
		}
		return mine
	}

	finish := func(job *batchv1.Job, conditions ...batchv1.JobCondition) {
		GinkgoHelper()
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Conditions = conditions
		for _, condition := range conditions {
			if condition.Type == batchv1.JobComplete {
				job.Status.CompletionTime = &now
				job.Status.Succeeded = 1
			}
			if condition.Type == batchv1.JobFailed {
				job.Status.Failed = 1
			}
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	succeeded := []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	failed := []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Message: "Job has reached the specified backoff limit"},
	}

	BeforeEach(func() {
		reconciler = &AddonReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
		}
		created = nil
		Expect((&KitchenReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}).
			ensurePlatformNamespace(ctx)).To(Succeed())
		track(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: "kitchen-keda-install", Namespace: PlatformNamespace,
		}})
	})

	AfterEach(func() {
		for _, obj := range created {
			if addon, isAddon := obj.(*kitchenv1alpha1.Addon); isAddon {
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(addon), addon); err == nil {
					addon.Finalizers = nil
					Expect(client.IgnoreNotFound(k8sClient.Update(ctx, addon))).To(Succeed())
				}
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		// The records outlive their jobs on purpose, so the suite takes its
		// own away rather than the operator doing it.
		upgrades := &kitchenv1alpha1.AddonUpgradeList{}
		Expect(k8sClient.List(ctx, upgrades, client.InNamespace(PlatformNamespace))).To(Succeed())
		for i := range upgrades.Items {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &upgrades.Items[i]))).To(Succeed())
		}
	})

	// The whole point, and the seeded pair with it: KEDA's two charts are
	// pinned as a pair and move together, so the bump is one record naming
	// both sides of both charts.
	It("records the pair bump, from and to, and keeps it after the job finishes", func() {
		reconciler.Installs = pinned("2.0.0-recorded")
		cfg := reconciler.Installs.forEntry(entry)
		track(asked(AddonKeda, true))
		reconcile()

		installedAlready(
			kitchenv1alpha1.AddonChartStatus{Name: kedaChartName, Version: "1.0.0-was"},
			kitchenv1alpha1.AddonChartStatus{Name: kedaHTTPChartName, Version: "0.1.0-was"},
		)
		reconcile()
		job := jobFor(cfg)

		// The record is opened from the job, which is why it is there before
		// anybody knows how the upgrade went.
		reconcile()
		records := recordsFor(job)
		Expect(records).To(HaveLen(1))
		record := records[0]
		Expect(record.Spec.Addon).To(Equal(AddonKeda))
		Expect(record.Spec.From).To(Equal([]kitchenv1alpha1.AddonChartStatus{
			{Name: kedaChartName, Version: "1.0.0-was"},
			{Name: kedaHTTPChartName, Version: "0.1.0-was"},
		}))
		Expect(record.Spec.To).To(Equal([]kitchenv1alpha1.AddonChartStatus{
			{Name: kedaChartName, Version: "2.0.0-recorded"},
			{Name: kedaHTTPChartName, Version: DefaultKedaHTTPChartVersion},
		}))
		Expect(record.Spec.Namespace).To(Equal(entry.DefaultNamespace))
		Expect(record.Status.Phase).To(Equal(kitchenv1alpha1.AddonUpgradeRunning))
		Expect(record.Status.StartedAt).NotTo(BeNil())
		Expect(record.Status.CompletedAt).To(BeNil())

		finish(job, succeeded...)
		reconcile()

		records = recordsFor(job)
		Expect(records).To(HaveLen(1), "one attempt is one job is one record")
		Expect(records[0].Status.Phase).To(Equal(kitchenv1alpha1.AddonUpgradeSucceeded))
		Expect(records[0].Status.CompletedAt).NotTo(BeNil())

		// The addon has moved on to the new versions — which is exactly the
		// overwrite this record exists to survive.
		Expect(addonOf().Status.Charts[0].Version).To(Equal("2.0.0-recorded"))

		// And a settled reconcile afterwards neither adds a second record nor
		// reopens the first.
		reconcile()
		records = recordsFor(job)
		Expect(records).To(HaveLen(1))
		Expect(records[0].Status.Phase).To(Equal(kitchenv1alpha1.AddonUpgradeSucceeded))
	})

	// An upgrade that broke something is at least as interesting as one that
	// worked, so the record is opened before the outcome is known.
	It("keeps an attempt that failed, in the words the job used", func() {
		reconciler.Installs = pinned("2.0.0-recorded-failure")
		cfg := reconciler.Installs.forEntry(entry)
		track(asked(AddonKeda, true))
		reconcile()

		installedAlready(kitchenv1alpha1.AddonChartStatus{Name: kedaChartName, Version: "1.0.0-was"})
		reconcile()
		job := jobFor(cfg)
		finish(job, failed...)
		reconcile()

		records := recordsFor(job)
		Expect(records).To(HaveLen(1))
		Expect(records[0].Status.Phase).To(Equal(kitchenv1alpha1.AddonUpgradeFailed))
		Expect(records[0].Status.Message).To(ContainSubstring("backoff limit"))
		Expect(records[0].Status.CompletedAt).NotTo(BeNil())
	})

	// The other half of the rule: a reconcile that changes nothing records
	// nothing. A history full of "2.20.2 → 2.20.2" would be worse than none.
	It("records nothing for a reconcile that moves no version", func() {
		reconciler.Installs = pinned("2.0.0-noop")
		cfg := reconciler.Installs.forEntry(entry)
		track(asked(AddonKeda, true))
		reconcile()

		// The install job of a platform already at these pins, completed.
		job := addonInstallJob(addonInstallJobName(entry, cfg), entry.DefaultNamespace, entry, cfg)
		track(job)
		finish(job, succeeded...)

		installedAlready(
			kitchenv1alpha1.AddonChartStatus{Name: kedaChartName, Version: "2.0.0-noop"},
			kitchenv1alpha1.AddonChartStatus{Name: kedaHTTPChartName, Version: DefaultKedaHTTPChartVersion},
		)
		reconcile()
		reconcile()

		Expect(recordsFor(job)).To(BeEmpty(), "nothing moved, so nothing happened to record")
	})

	// A first install has nothing to move from. The Addon's own condition
	// states it, and a record whose `from` is the empty set would be a
	// history entry for an event that is not an upgrade.
	It("records nothing for a first install", func() {
		reconciler.Installs = pinned("2.0.0-first")
		cfg := reconciler.Installs.forEntry(entry)
		track(asked(AddonKeda, true))
		reconcile()

		// Managed, but nothing recorded as installed: the platform is putting
		// this entry in for the first time.
		installedAlready()
		reconcile()
		job := jobFor(cfg)
		finish(job, succeeded...)
		reconcile()

		Expect(recordsFor(job)).To(BeEmpty())
	})

	// "No upgrades recorded" and "never upgraded" are different sentences,
	// and this is what lets the API tell them apart.
	It("says when it started keeping this entry's history", func() {
		reconciler.Installs = pinned("2.0.0-since")
		track(asked(AddonKeda, true))
		reconcile()

		since := addonOf().Status.UpgradeHistorySince
		Expect(since).NotTo(BeNil(), "an addon this operator has reconciled has a history that starts")

		// It is written once and never moved: a history that reset itself on
		// every reconcile would say the installation had no past at all.
		reconcile()
		Expect(addonOf().Status.UpgradeHistorySince).To(Equal(since))
	})
})

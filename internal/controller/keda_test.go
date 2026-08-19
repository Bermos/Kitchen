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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// idling builds a Kitchen that idles environments, which is the precondition
// for the operator caring about KEDA at all.
func idling(install bool) *kitchenv1alpha1.Kitchen {
	return &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			ScaleToZero: kitchenv1alpha1.ScaleToZeroSpec{Enabled: true, Install: install},
		},
	}
}

var _ = Describe("Planning the KEDA install", func() {
	// permitted is a chart that granted the operator an account, with
	// everything else at the operator's own pins.
	permitted := KedaInstallConfig{ServiceAccount: "kitchen-keda-install"}.withDefaults()

	It("carries nothing at all while the platform idles nothing", func() {
		kitchen := idling(true)
		kitchen.Spec.ScaleToZero.Enabled = false

		plan := planKeda(kitchen, permitted, "keda", false, false)
		Expect(plan.clear).To(BeTrue())
		Expect(plan.ready).To(BeTrue())
		Expect(plan.install).To(BeFalse())
	})

	It("adopts an add-on that is already serving, and records that it owns nothing", func() {
		kitchen := idling(true)

		plan := planKeda(kitchen, permitted, "keda", true, true)
		Expect(plan.install).To(BeFalse())
		Expect(plan.status).To(Equal(metav1.ConditionTrue))
		Expect(plan.reason).To(Equal("AddOnPresent"))
		Expect(plan.record).NotTo(BeNil())
		Expect(plan.record.Managed).To(BeFalse())
		Expect(plan.record.Version).To(BeEmpty())
	})

	It("leaves an adopted add-on alone forever, however the pins move", func() {
		kitchen := idling(true)
		kitchen.Status.ScaleToZero = &kitchenv1alpha1.ScaleToZeroStatus{Namespace: "keda"}

		plan := planKeda(kitchen, permitted, "keda", true, true)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("AddOnPresent"))
	})

	It("says nothing idles when the add-on is absent and it was not asked to install", func() {
		plan := planKeda(idling(false), permitted, "keda", false, false)
		Expect(plan.install).To(BeFalse())
		Expect(plan.status).To(Equal(metav1.ConditionFalse))
		Expect(plan.reason).To(Equal("NotInstalled"))
		// Terminal: a spec edit reconciles the object again on its own.
		Expect(plan.ready).To(BeTrue())
	})

	It("names the chart value when asked to install without a grant", func() {
		plan := planKeda(idling(true), KedaInstallConfig{}.withDefaults(), "keda", false, false)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("InstallNotPermitted"))
		Expect(plan.message).To(ContainSubstring("scaleToZero.install.enabled=true"))
	})

	It("refuses to install over a KEDA it does not own", func() {
		plan := planKeda(idling(true), permitted, "keda", false, true)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("KedaNotOurs"))
		Expect(plan.ready).To(BeTrue())
	})

	It("installs when neither half is in the cluster", func() {
		plan := planKeda(idling(true), permitted, "keda", false, false)
		Expect(plan.install).To(BeTrue())
	})

	It("refuses a namespace that is not a namespace name", func() {
		plan := planKeda(idling(true), permitted, "Keda System", false, false)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("NamespaceInvalid"))
	})

	It("reports its own install by the versions it recorded", func() {
		kitchen := idling(true)
		kitchen.Status.ScaleToZero = &kitchenv1alpha1.ScaleToZeroStatus{
			Managed: true, Namespace: "keda",
			Version: permitted.ChartVersion, AddOnVersion: permitted.AddOnChartVersion,
		}

		plan := planKeda(kitchen, permitted, "keda", true, false)
		Expect(plan.install).To(BeFalse())
		Expect(plan.status).To(Equal(metav1.ConditionTrue))
		Expect(plan.reason).To(Equal("AddOnInstalled"))
		Expect(plan.message).To(ContainSubstring(permitted.ChartVersion))
	})

	It("upgrades its own install when the operator's pins have moved", func() {
		kitchen := idling(true)
		kitchen.Status.ScaleToZero = &kitchenv1alpha1.ScaleToZeroStatus{
			Managed: true, Namespace: "keda", Version: "2.0.0", AddOnVersion: "0.1.0",
		}

		plan := planKeda(kitchen, permitted, "keda", true, false)
		Expect(plan.install).To(BeTrue())
	})

	It("leaves its own install where it is once the grant is withdrawn", func() {
		kitchen := idling(true)
		kitchen.Status.ScaleToZero = &kitchenv1alpha1.ScaleToZeroStatus{
			Managed: true, Namespace: "keda", Version: "2.0.0", AddOnVersion: "0.1.0",
		}

		plan := planKeda(kitchen, KedaInstallConfig{}.withDefaults(), "keda", true, false)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("AddOnInstalled"))
	})
})

var _ = Describe("The KEDA install job", func() {
	cfg := KedaInstallConfig{ServiceAccount: "kitchen-keda-install"}.withDefaults()

	It("is named after the pair it installs, so a bump is a different job", func() {
		name := kedaInstallJobName(cfg)
		Expect(name).To(HavePrefix("kitchen-keda-install-"))
		Expect(name).To(ContainSubstring(strings.ReplaceAll(cfg.ChartVersion, ".", "-")))
		Expect(len(name)).To(BeNumerically("<=", 63))

		moved := cfg
		moved.AddOnChartVersion = "0.99.0"
		Expect(kedaInstallJobName(moved)).NotTo(Equal(name))
	})

	It("installs KEDA before the add-on, without a shell between them", func() {
		job := kedaInstallJob("kitchen-keda-install-x", "keda", cfg)

		Expect(job.Spec.Template.Spec.InitContainers).To(HaveLen(1))
		Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
		keda := job.Spec.Template.Spec.InitContainers[0]
		addOn := job.Spec.Template.Spec.Containers[0]

		// The ordering Helm cannot express within one release: KEDA's CRDs are
		// established by an init container that has already exited before the
		// add-on's own ScaledObject is applied against them.
		Expect(keda.Args).To(ContainElements("upgrade", kedaReleaseName, "keda", "--install", "--wait"))
		Expect(keda.Args).To(ContainElements("--version", cfg.ChartVersion))
		Expect(addOn.Args).To(ContainElements("upgrade", kedaHTTPReleaseName, "keda-add-ons-http"))
		Expect(addOn.Args).To(ContainElements("--version", cfg.AddOnChartVersion))

		for _, container := range []corev1.Container{keda, addOn} {
			Expect(container.Command).To(Equal([]string{"helm"}))
			Expect(container.Args).To(ContainElements("--namespace", "keda"))
			Expect(container.Args).To(ContainElements("--repo", cfg.Repository))
			// No shell anywhere: every value reaches helm as its own argv
			// element, which is what keeps a cluster-admin job from being a
			// way to run something else.
			Expect(container.Command[0]).NotTo(Equal("sh"))
		}

		Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal(cfg.ServiceAccount))
		Expect(*job.Spec.BackoffLimit).To(Equal(int32(0)))
		// The deadline has to lose the race to helm's own timeout, twice over.
		Expect(*job.Spec.ActiveDeadlineSeconds).To(BeNumerically(">", int64(2*cfg.Timeout.Seconds())))
	})
})

var _ = Describe("Running the KEDA install", func() {
	ctx := context.Background()

	var reconciler *KitchenReconciler
	var kitchen *kitchenv1alpha1.Kitchen
	var created []client.Object
	cfg := KedaInstallConfig{ServiceAccount: "kitchen-keda-install"}.withDefaults()

	setCond := func(condType string, status metav1.ConditionStatus, reason, message string) {
		kitchen.Status.Conditions = append(kitchen.Status.Conditions, metav1.Condition{
			Type: condType, Status: status, Reason: reason, Message: message,
			LastTransitionTime: metav1.Now(),
		})
	}

	condition := func() *metav1.Condition {
		for i := range kitchen.Status.Conditions {
			if kitchen.Status.Conditions[i].Type == condScaleToZeroReady {
				return &kitchen.Status.Conditions[i]
			}
		}
		return nil
	}

	track := func(obj client.Object) {
		ExpectWithOffset(1, k8sClient.Create(ctx, obj)).To(Succeed())
		created = append(created, obj)
	}

	BeforeEach(func() {
		reconciler = &KitchenReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			KedaInstall: cfg,
		}
		kitchen = idling(true)
		created = nil
		Expect(reconciler.ensurePlatformNamespace(ctx)).To(Succeed())
	})

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	// Each spec installs its own version pair, so that a job one spec created
	// can never be the job another spec finds — the name is derived from the
	// pair on purpose, and the tests lean on that rather than on ordering.
	pinned := func(version string) KedaInstallConfig {
		own := cfg
		own.ChartVersion = version
		return own
	}

	It("refuses to create the job when the promised account is not there", func() {
		own := pinned("2.0.0-noaccount")
		Expect(reconciler.runKedaInstall(ctx, kitchen, own, "keda", setCond)).To(BeFalse())
		Expect(condition().Reason).To(Equal("ServiceAccountMissing"))

		job := &batchv1.Job{}
		err := k8sClient.Get(ctx,
			types.NamespacedName{Namespace: PlatformNamespace, Name: kedaInstallJobName(own)}, job)
		Expect(client.IgnoreNotFound(err)).To(Succeed())
		Expect(err).To(HaveOccurred(), "no job should exist without the account it would run as")
	})

	Context("with the account the chart promised", func() {
		BeforeEach(func() {
			track(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name: cfg.ServiceAccount, Namespace: PlatformNamespace,
			}})
		})

		It("creates the job, reports progress, and reads its outcome", func() {
			cfg := pinned("2.0.0-succeeds")
			Expect(reconciler.runKedaInstall(ctx, kitchen, cfg, "keda", setCond)).To(BeFalse())
			Expect(condition().Reason).To(Equal("Installing"))

			job := &batchv1.Job{}
			key := types.NamespacedName{Namespace: PlatformNamespace, Name: kedaInstallJobName(cfg)}
			Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
			created = append(created, job)
			Expect(job.Labels[labelComponentKind]).To(Equal(kedaInstallComponent))

			// A second reconcile while it runs neither duplicates the job nor
			// changes its mind about what is happening.
			kitchen.Status.Conditions = nil
			Expect(reconciler.runKedaInstall(ctx, kitchen, cfg, "keda", setCond)).To(BeFalse())
			Expect(condition().Reason).To(Equal("Installing"))

			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.CompletionTime = &now
			job.Status.Succeeded = 1
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			kitchen.Status.Conditions = nil
			Expect(reconciler.runKedaInstall(ctx, kitchen, cfg, "keda", setCond)).To(BeTrue())
			Expect(condition().Status).To(Equal(metav1.ConditionTrue))
			Expect(condition().Reason).To(Equal("AddOnInstalled"))

			// The record is what a later reconcile reads as permission to
			// upgrade these two releases, so it has to survive the job.
			Expect(kitchen.Status.ScaleToZero).NotTo(BeNil())
			Expect(kitchen.Status.ScaleToZero.Managed).To(BeTrue())
			Expect(kitchen.Status.ScaleToZero.Namespace).To(Equal("keda"))
			Expect(kitchen.Status.ScaleToZero.Version).To(Equal(cfg.ChartVersion))
			Expect(kitchen.Status.ScaleToZero.AddOnVersion).To(Equal(cfg.AddOnChartVersion))
		})

		It("reports a failed install, and claims nothing it did not do", func() {
			cfg := pinned("2.0.0-fails")
			Expect(reconciler.runKedaInstall(ctx, kitchen, cfg, "keda", setCond)).To(BeFalse())

			job := &batchv1.Job{}
			key := types.NamespacedName{Namespace: PlatformNamespace, Name: kedaInstallJobName(cfg)}
			Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
			created = append(created, job)
			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.Failed = 1
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
					Message: "Job has reached the specified backoff limit"},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			kitchen.Status.Conditions = nil
			Expect(reconciler.runKedaInstall(ctx, kitchen, cfg, "keda", setCond)).To(BeFalse())
			Expect(condition().Status).To(Equal(metav1.ConditionFalse))
			Expect(condition().Reason).To(Equal("InstallFailed"))
			Expect(condition().Message).To(ContainSubstring("backoff limit"))
			Expect(kitchen.Status.ScaleToZero).To(BeNil())
		})
	})

	It("adopts the add-on this cluster already serves", func() {
		// The suite installs the add-on's CRD, so this envtest cluster is one
		// that already runs it — which is the case the operator must leave
		// entirely alone.
		reconciler.KedaInstall = pinned("2.0.0-adopted")
		Expect(reconciler.reconcileKeda(ctx, kitchen, setCond)).To(BeTrue())
		Expect(condition().Reason).To(Equal("AddOnPresent"))
		Expect(kitchen.Status.ScaleToZero.Managed).To(BeFalse())

		job := &batchv1.Job{}
		err := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: PlatformNamespace, Name: kedaInstallJobName(reconciler.KedaInstall),
		}, job)
		Expect(err).To(HaveOccurred(), "adoption must not install anything")
	})

	It("carries no condition at all while the platform idles nothing", func() {
		kitchen.Spec.ScaleToZero.Enabled = false
		kitchen.Status.ScaleToZero = &kitchenv1alpha1.ScaleToZeroStatus{Namespace: "keda"}
		setCond(condScaleToZeroReady, metav1.ConditionFalse, "Stale", "from an earlier reconcile")

		Expect(reconciler.reconcileKeda(ctx, kitchen, setCond)).To(BeTrue())
		Expect(condition()).To(BeNil())
		Expect(kitchen.Status.ScaleToZero).To(BeNil())
	})
})

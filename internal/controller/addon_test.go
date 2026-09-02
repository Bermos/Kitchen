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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The install engine, exercised through the two entries that were two
// near-identical files before it.
//
// Every scenario here was a scenario in keda_test.go or cnpg_test.go, and
// they are together now for the reason the engine is: they were the same
// scenarios. Where the two entries genuinely differ — KEDA installs a pair
// and refuses to install over half of one, CloudNativePG is used whoever
// installed it — the spec says which entry it is about and why.

// ensureAddon creates the Addon, or brings one an earlier spec left behind to
// this spec's shape — spec *and* status.
//
// Every suite that reconciles the platform singleton now seeds these, so an
// addon suite cannot assume a clean namespace any more than it can assume a
// clean cluster-scoped Kitchen. Adopting rather than failing is what keeps a
// spec's result its own.
func ensureAddon(ctx context.Context, addon *kitchenv1alpha1.Addon) {
	GinkgoHelper()
	err := k8sClient.Create(ctx, addon)
	if !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
		return
	}
	existing := &kitchenv1alpha1.Addon{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(addon), existing)).To(Succeed())
	existing.Spec = addon.Spec
	Expect(k8sClient.Update(ctx, existing)).To(Succeed())
	existing.Status = kitchenv1alpha1.AddonStatus{}
	Expect(k8sClient.Status().Update(ctx, existing)).To(Succeed())
	*addon = *existing
}

// releaseAddon drops an Addon's finalizer and deletes it, re-reading first:
// a reconcile has usually moved the object on since the spec created it, and
// writing a stale copy back is a conflict rather than a cleanup.
func releaseAddon(ctx context.Context, name string) {
	GinkgoHelper()
	addon := &kitchenv1alpha1.Addon{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: name}
	if err := k8sClient.Get(ctx, key, addon); err != nil {
		Expect(client.IgnoreNotFound(err)).To(Succeed())
		return
	}
	addon.Finalizers = nil
	Expect(client.IgnoreNotFound(k8sClient.Update(ctx, addon))).To(Succeed())
	Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, addon))).To(Succeed())
}

// asked builds an Addon that has (or has not) asked for its entry.
func asked(id string, install bool) *kitchenv1alpha1.Addon {
	return &kitchenv1alpha1.Addon{
		ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: PlatformNamespace},
		Spec:       kitchenv1alpha1.AddonSpec{Install: install},
	}
}

// entryOf is the catalogue entry, or a failed spec rather than a zero value.
func entryOf(id string) addonEntry {
	entry, ok := lookupAddon(id)
	ExpectWithOffset(1, ok).To(BeTrue(), "the catalogue must carry "+id)
	return entry
}

// installedAt is the record an install job of the platform's own making
// would justify, at the pins given.
func installedAt(entry addonEntry, cfg AddonInstallConfig, namespace string) *addonRecord {
	charts := make([]kitchenv1alpha1.AddonChartStatus, 0, len(entry.Charts))
	for _, chart := range entry.Charts {
		charts = append(charts, kitchenv1alpha1.AddonChartStatus{
			Name: chart.Chart, Version: cfg.version(chart),
		})
	}
	return &addonRecord{namespace: namespace, charts: charts}
}

var _ = Describe("Planning an addon install", func() {
	// permitted is a chart that granted the operator an account, with
	// everything else at the operator's own pins.
	permitted := func(id string) (addonEntry, AddonInstallConfig) {
		entry := entryOf(id)
		installs := AddonInstalls{id: {ServiceAccount: "kitchen-" + id + "-install"}}
		return entry, installs.forEntry(entry)
	}
	ungranted := func(id string) (addonEntry, AddonInstallConfig) {
		entry := entryOf(id)
		return entry, AddonInstalls{}.forEntry(entry)
	}

	for _, id := range []string{AddonKeda, AddonCNPG} {
		Context("for "+id, func() {
			It("carries no install while nobody has asked for it", func() {
				entry, cfg := permitted(id)
				plan := planAddon(asked(id, false), entry, cfg,
					addonObservation{permitted: true, namespace: entry.DefaultNamespace})
				Expect(plan.install).To(BeFalse())
				Expect(plan.reason).To(Equal("NotInstalled"))
				Expect(plan.ready).To(BeTrue())
			})

			// The adoption rule, and the one that matters most: both of these
			// are popular things for a cluster to already run, and a platform
			// that "helpfully" upgraded somebody's would be a worse neighbour
			// than one that never offered.
			It("adopts one that is already serving, and records that it owns nothing", func() {
				entry, cfg := permitted(id)
				plan := planAddon(asked(id, true), entry, cfg,
					addonObservation{served: true, permitted: true, namespace: entry.DefaultNamespace})
				Expect(plan.install).To(BeFalse())
				Expect(plan.status).To(Equal(metav1.ConditionTrue))
				Expect(plan.reason).To(Equal("AlreadyServing"))
				Expect(plan.managed).To(BeFalse())
				Expect(plan.serving).To(BeTrue())
				Expect(plan.message).To(ContainSubstring("installed nothing"))
			})

			It("leaves an adopted installation alone forever, however the pins move", func() {
				entry, cfg := permitted(id)
				addon := asked(id, true)
				addon.Status.Namespace = entry.DefaultNamespace

				plan := planAddon(addon, entry, cfg,
					addonObservation{served: true, permitted: true, namespace: entry.DefaultNamespace})
				Expect(plan.install).To(BeFalse())
				Expect(plan.reason).To(Equal("AlreadyServing"))
			})

			It("installs where the cluster runs none and the installation asked it to", func() {
				entry, cfg := permitted(id)
				plan := planAddon(asked(id, true), entry, cfg,
					addonObservation{permitted: true, namespace: entry.DefaultNamespace})
				Expect(plan.install).To(BeTrue())
			})

			It("refuses to install without the account the chart only creates when asked", func() {
				entry, cfg := ungranted(id)
				plan := planAddon(asked(id, true), entry, cfg,
					addonObservation{namespace: entry.DefaultNamespace})
				Expect(plan.install).To(BeFalse())
				Expect(plan.reason).To(Equal(kitchenv1alpha1.AddonRefused))
				Expect(plan.message).To(ContainSubstring(entry.ChartValue))
				Expect(plan.ready).To(BeTrue())
			})

			It("refuses a namespace that is not a namespace before creating a cluster-admin job", func() {
				entry, cfg := permitted(id)
				plan := planAddon(asked(id, true), entry, cfg,
					addonObservation{permitted: true, namespace: "Not A Namespace"})
				Expect(plan.install).To(BeFalse())
				Expect(plan.reason).To(Equal("NamespaceInvalid"))
			})

			It("settles once its own install has finished", func() {
				entry, cfg := permitted(id)
				plan := planAddon(asked(id, true), entry, cfg, addonObservation{
					served: true, permitted: true, namespace: entry.DefaultNamespace,
					installed: installedAt(entry, cfg, entry.DefaultNamespace),
				})
				Expect(plan.install).To(BeFalse())
				Expect(plan.status).To(Equal(metav1.ConditionTrue))
				Expect(plan.reason).To(Equal("Installed"))
				Expect(plan.managed).To(BeTrue())
				Expect(plan.ready).To(BeTrue())
			})

			// An operator upgrade carries its dependency forward, rather than
			// leaving the platform on whatever the first install pulled.
			It("reinstalls what it owns when the operator's pins have moved", func() {
				entry, cfg := permitted(id)
				stale := installedAt(entry, cfg, entry.DefaultNamespace)
				stale.charts[0].Version = "0.0.1"

				plan := planAddon(asked(id, true), entry, cfg, addonObservation{
					served: true, permitted: true, namespace: entry.DefaultNamespace, installed: stale,
				})
				Expect(plan.install).To(BeTrue())
			})

			// Issue #244: the platform installed the dependency, the status
			// write recording it did not land, and every reconcile afterwards
			// read the cluster as somebody else's and said so — permanently,
			// because the branch it took rewrote the record it had misread.
			// The install job is the fact the record was only a copy of.
			It("keeps what it installed even where the record of it was lost", func() {
				entry, cfg := permitted(id)
				plan := planAddon(asked(id, true), entry, cfg, addonObservation{
					served: true, permitted: true, namespace: entry.DefaultNamespace,
					installed: installedAt(entry, cfg, entry.DefaultNamespace),
				})
				Expect(plan.reason).To(Equal("Installed"))
				Expect(plan.managed).To(BeTrue())
				Expect(plan.install).To(BeFalse())
			})

			It("corrects a record that disclaims a release its own job installed", func() {
				entry, cfg := permitted(id)
				addon := asked(id, true)
				addon.Status.Namespace = entry.DefaultNamespace

				plan := planAddon(addon, entry, cfg, addonObservation{
					served: true, permitted: true, namespace: entry.DefaultNamespace,
					installed: installedAt(entry, cfg, entry.DefaultNamespace),
				})
				Expect(plan.reason).To(Equal("Installed"))
				Expect(plan.managed).To(BeTrue())
				Expect(plan.message).NotTo(ContainSubstring("installed nothing"))
			})

			// An install job from before the operator labelled them says who
			// installed the dependency but not which version. Unknown reads
			// as drift, and drift reinstalls at the pin — which records it.
			It("reinstalls at the pin where its own job does not say what it installed", func() {
				entry, cfg := permitted(id)
				unknown := installedAt(entry, cfg, entry.DefaultNamespace)
				for i := range unknown.charts {
					unknown.charts[i].Version = ""
				}

				plan := planAddon(asked(id, true), entry, cfg, addonObservation{
					served: true, permitted: true, namespace: entry.DefaultNamespace, installed: unknown,
				})
				Expect(plan.install).To(BeTrue())
			})

			It("never acts on drift in an installation that has withdrawn the grant", func() {
				entry, cfg := permitted(id)
				stale := installedAt(entry, cfg, entry.DefaultNamespace)
				stale.charts[0].Version = "0.0.1"

				plan := planAddon(asked(id, true), entry, cfg, addonObservation{
					served: true, namespace: entry.DefaultNamespace, installed: stale,
				})
				Expect(plan.install).To(BeFalse())
				Expect(plan.reason).To(Equal("Installed"))
			})
		})
	}

	// KEDA alone gives the platform nothing, and installing over it is what
	// helm would find out half-way through.
	It("refuses to install over a KEDA it does not own", func() {
		entry, cfg := permitted(AddonKeda)
		plan := planAddon(asked(AddonKeda, true), entry, cfg, addonObservation{
			partiallyServed: true, permitted: true, namespace: entry.DefaultNamespace,
		})
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("KedaNotOurs"))
		Expect(plan.ready).To(BeTrue())
	})

	// The ordering a Helm release cannot express, enforced rather than hoped
	// for: an entry whose dependency is not serving waits, and says which.
	It("waits for a dependency that is not serving, naming it", func() {
		entry, cfg := permitted(AddonCNPG)
		plan := planAddon(asked(AddonCNPG, true), entry, cfg, addonObservation{
			permitted: true, namespace: entry.DefaultNamespace, blockedBy: "something-else",
		})
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("DependencyNotReady"))
		Expect(plan.message).To(ContainSubstring("something-else"))
		Expect(plan.ready).To(BeFalse(), "it will change on its own, so it is looked at again")
	})
})

var _ = Describe("An addon install job", func() {
	cfgFor := func(id string) (addonEntry, AddonInstallConfig) {
		entry := entryOf(id)
		return entry, AddonInstalls{id: {ServiceAccount: "kitchen-" + id + "-install"}}.forEntry(entry)
	}

	It("is named after what it installs, so a bump is a new job", func() {
		entry, cfg := cfgFor(AddonCNPG)
		bumped := cfg
		bumped.Versions = map[string]string{cnpgChartName: "0.30.0"}

		Expect(addonInstallJobName(entry, cfg)).NotTo(Equal(addonInstallJobName(entry, bumped)))
		Expect(len(addonInstallJobName(entry, cfg))).To(BeNumerically("<=", 63))
	})

	// The job is bound to cluster-admin. Anything from a request in its argv
	// would make the grant meaningless, and a shell would make the check
	// pointless.
	It("runs helm directly, with an argv nothing from a request reaches", func() {
		entry, cfg := cfgFor(AddonCNPG)
		job := addonInstallJob(addonInstallJobName(entry, cfg), "cnpg-system", entry, cfg)

		Expect(job.Spec.Template.Spec.InitContainers).To(BeEmpty())
		Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Command).To(Equal([]string{"helm"}))
		Expect(strings.Join(container.Args, " ")).To(ContainSubstring("upgrade cnpg cloudnative-pg --install"))
		Expect(container.Args).To(ContainElement(cfg.Repository))
		Expect(container.Args).To(ContainElement(DefaultCNPGChartVersion))
		Expect(container.Args).To(ContainElement("cnpg-system"))
		Expect(container.Args).To(ContainElement("--wait"))
		Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal("kitchen-cloudnative-pg-install"))
		Expect(*job.Spec.BackoffLimit).To(BeEquivalentTo(0))
	})

	// The ordering Helm cannot express within one release: the add-on ships a
	// ScaledObject of KEDA's own CRD, so KEDA runs to completion first.
	It("installs an entry's charts in order, without a shell between them", func() {
		entry, cfg := cfgFor(AddonKeda)
		job := addonInstallJob(addonInstallJobName(entry, cfg), "keda", entry, cfg)

		Expect(job.Spec.Template.Spec.InitContainers).To(HaveLen(1))
		Expect(job.Spec.Template.Spec.InitContainers[0].Name).To(Equal(kedaChartName))
		Expect(job.Spec.Template.Spec.InitContainers[0].Command).To(Equal([]string{"helm"}))
		Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(job.Spec.Template.Spec.Containers[0].Name).To(Equal(kedaHTTPChartName))
		Expect(job.Spec.Template.Spec.Containers[0].Command).To(Equal([]string{"helm"}))
	})

	// The job's name is sanitised into a DNS label and so cannot be read back
	// as a version. The labels are what a later reconcile reads ownership
	// from, so they say what the name cannot.
	It("says what it installed and where, in labels a later reconcile can read", func() {
		entry, cfg := cfgFor(AddonCNPG)
		job := addonInstallJob(addonInstallJobName(entry, cfg), "cnpg-system", entry, cfg)
		Expect(job.Labels[labelInstallVersion]).To(Equal(DefaultCNPGChartVersion))
		Expect(job.Labels[labelInstallNamespace]).To(Equal("cnpg-system"))

		owned := addonInstalled(job, entry, cfg, "somewhere-else")
		Expect(owned).NotTo(BeNil())
		Expect(owned.charts[0].Version).To(Equal(DefaultCNPGChartVersion))
		Expect(owned.namespace).To(Equal("cnpg-system"), "what it installed, not what is configured now")
	})

	It("reads a job from before the labels by its name, and no further", func() {
		entry, cfg := cfgFor(AddonCNPG)
		unlabelled := addonInstallJob(addonInstallJobName(entry, cfg), "cnpg-system", entry, cfg)
		unlabelled.Labels = map[string]string{}

		owned := addonInstalled(unlabelled, entry, cfg, "cnpg-system")
		Expect(owned.charts[0].Version).To(Equal(DefaultCNPGChartVersion), "its name can only be this pin's")

		older := addonInstalled(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: "kitchen-cloudnative-pg-install-0-0-1",
		}}, entry, cfg, "cnpg-system")
		Expect(older.charts[0].Version).To(BeEmpty(), "unknown, which the next install settles")
	})

	It("is no evidence at all when there is no job", func() {
		entry, cfg := cfgFor(AddonCNPG)
		Expect(addonInstalled(nil, entry, cfg, "cnpg-system")).To(BeNil())
	})
})

var _ = Describe("Running an addon install", func() {
	ctx := context.Background()

	var reconciler *AddonReconciler
	var created []client.Object
	entry := entryOf(AddonKeda)

	track := func(obj client.Object) {
		GinkgoHelper()
		if addon, ok := obj.(*kitchenv1alpha1.Addon); ok {
			ensureAddon(ctx, addon)
		} else {
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		}
		created = append(created, obj)
	}

	// Each spec installs its own version pair, so a job one spec created can
	// never be the job another spec finds — the name is derived from the pins
	// on purpose, and these lean on that rather than on ordering.
	pinned := func(version string) AddonInstalls {
		return AddonInstalls{AddonKeda: {
			ServiceAccount: "kitchen-keda-install",
			Versions:       map[string]string{kedaChartName: version},
		}}
	}

	addonOf := func(id string) *kitchenv1alpha1.Addon {
		addon := &kitchenv1alpha1.Addon{}
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Namespace: PlatformNamespace, Name: id}, addon)).To(Succeed())
		return addon
	}

	ready := func(addon *kitchenv1alpha1.Addon) metav1.Condition {
		for _, condition := range addon.Status.Conditions {
			if condition.Type == kitchenv1alpha1.AddonReady {
				return condition
			}
		}
		return metav1.Condition{}
	}

	reconcile := func(id string) ctrl.Result {
		result, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: PlatformNamespace, Name: id},
		})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return result
	}

	BeforeEach(func() {
		reconciler = &AddonReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
		}
		created = nil
		Expect((&KitchenReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}).
			ensurePlatformNamespace(ctx)).To(Succeed())
	})

	AfterEach(func() {
		for _, obj := range created {
			addon, isAddon := obj.(*kitchenv1alpha1.Addon)
			if isAddon {
				// The finalizer would hold it, and every one of these
				// installed nothing this cluster depends on.
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(addon), addon); err == nil {
					addon.Finalizers = nil
					Expect(client.IgnoreNotFound(k8sClient.Update(ctx, addon))).To(Succeed())
				}
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("refuses an addon naming no catalogue entry, and installs nothing", func() {
		track(asked("not-a-real-addon", true))
		reconcile("not-a-real-addon")

		condition := ready(addonOf("not-a-real-addon"))
		Expect(condition.Reason).To(Equal(kitchenv1alpha1.AddonRefused))
		Expect(condition.Message).To(ContainSubstring("compiled in"))
	})

	It("refuses to create the job when the promised account is not there", func() {
		reconciler.Installs = pinned("2.0.0-noaccount")
		track(asked(AddonKeda, true))
		reconcile(AddonKeda)
		// This envtest cluster serves the add-on's CRD, so the only way to
		// reach the install branch is a pin that has moved past what the
		// (absent) install job recorded.
		addon := addonOf(AddonKeda)
		addon.Status.Managed = true
		addon.Status.Charts = []kitchenv1alpha1.AddonChartStatus{{Name: kedaChartName, Version: "0.0.1"}}
		Expect(k8sClient.Status().Update(ctx, addon)).To(Succeed())

		reconcile(AddonKeda)
		Expect(ready(addonOf(AddonKeda)).Reason).To(Equal("ServiceAccountMissing"))

		job := &batchv1.Job{}
		err := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: PlatformNamespace,
			Name:      addonInstallJobName(entry, reconciler.Installs.forEntry(entry)),
		}, job)
		Expect(err).To(HaveOccurred(), "no job should exist without the account it would run as")
	})

	Context("with the account the chart promised", func() {
		BeforeEach(func() {
			track(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name: "kitchen-keda-install", Namespace: PlatformNamespace,
			}})
		})

		// The whole cycle: the job is created once, its progress is reported
		// without duplicating it, and what it achieved outlives it.
		It("creates the job, reports progress, and reads its outcome", func() {
			reconciler.Installs = pinned("2.0.0-succeeds")
			cfg := reconciler.Installs.forEntry(entry)
			track(asked(AddonKeda, true))
			reconcile(AddonKeda)

			addon := addonOf(AddonKeda)
			addon.Status.Managed = true
			addon.Status.Charts = []kitchenv1alpha1.AddonChartStatus{{Name: kedaChartName, Version: "0.0.1"}}
			Expect(k8sClient.Status().Update(ctx, addon)).To(Succeed())

			reconcile(AddonKeda)
			Expect(ready(addonOf(AddonKeda)).Reason).To(Equal("Installing"))

			job := &batchv1.Job{}
			key := types.NamespacedName{Namespace: PlatformNamespace, Name: addonInstallJobName(entry, cfg)}
			Expect(k8sClient.Get(ctx, key, job)).To(Succeed())
			created = append(created, job)
			Expect(job.Labels[labelComponentKind]).To(Equal(kedaInstallComponent))

			// A second reconcile while it runs neither duplicates the job nor
			// changes its mind about what is happening.
			reconcile(AddonKeda)
			Expect(ready(addonOf(AddonKeda)).Reason).To(Equal("Installing"))

			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.CompletionTime = &now
			job.Status.Succeeded = 1
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			reconcile(AddonKeda)
			settled := addonOf(AddonKeda)
			Expect(ready(settled).Status).To(Equal(metav1.ConditionTrue))
			Expect(ready(settled).Reason).To(Equal("Installed"))

			// The record is what a later reconcile reads as permission to
			// upgrade these releases, so it has to survive the job.
			Expect(settled.Status.Managed).To(BeTrue())
			Expect(settled.Status.Namespace).To(Equal(entry.DefaultNamespace))
			Expect(settled.Status.Charts).To(HaveLen(2))
			Expect(settled.Status.Charts[0].Version).To(Equal("2.0.0-succeeds"))
			Expect(settled.Status.Charts[1].Version).To(Equal(DefaultKedaHTTPChartVersion))
		})

		It("reports a failed install, and claims nothing it did not do", func() {
			reconciler.Installs = pinned("2.0.0-fails")
			cfg := reconciler.Installs.forEntry(entry)
			track(asked(AddonKeda, true))
			reconcile(AddonKeda)

			addon := addonOf(AddonKeda)
			addon.Status.Managed = true
			addon.Status.Charts = []kitchenv1alpha1.AddonChartStatus{{Name: kedaChartName, Version: "0.0.1"}}
			Expect(k8sClient.Status().Update(ctx, addon)).To(Succeed())
			reconcile(AddonKeda)

			job := &batchv1.Job{}
			key := types.NamespacedName{Namespace: PlatformNamespace, Name: addonInstallJobName(entry, cfg)}
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

			reconcile(AddonKeda)
			settled := addonOf(AddonKeda)
			Expect(ready(settled).Status).To(Equal(metav1.ConditionFalse))
			Expect(ready(settled).Reason).To(Equal("InstallFailed"))
			Expect(ready(settled).Message).To(ContainSubstring("backoff limit"))
		})
	})

	It("adopts the add-on this cluster already serves", func() {
		// The suite installs the add-on's CRD, so this envtest cluster is one
		// that already runs it — the case the operator must leave alone.
		reconciler.Installs = pinned("2.0.0-adopted")
		track(asked(AddonKeda, true))
		reconcile(AddonKeda)

		settled := addonOf(AddonKeda)
		Expect(ready(settled).Reason).To(Equal("AlreadyServing"))
		Expect(settled.Status.Managed).To(BeFalse())
		Expect(settled.Status.Serving).To(BeTrue())

		job := &batchv1.Job{}
		err := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: PlatformNamespace,
			Name:      addonInstallJobName(entry, reconciler.Installs.forEntry(entry)),
		}, job)
		Expect(err).To(HaveOccurred(), "adoption must not install anything")
	})

	// The end of issue #244, reconciled rather than planned: this envtest
	// cluster serves the add-on's API, so a platform whose record of its own
	// install was lost is exactly the cluster the bug was found on.
	It("reads its own completed install job rather than the record of it", func() {
		reconciler.Installs = pinned("2.0.0-evidence")
		cfg := reconciler.Installs.forEntry(entry)
		track(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: "kitchen-keda-install", Namespace: PlatformNamespace,
		}})

		job := addonInstallJob(addonInstallJobName(entry, cfg), entry.DefaultNamespace, entry, cfg)
		track(job)
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.CompletionTime = &now
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		// The state the lost write leaves behind: the add-on is serving, and
		// the object remembers nothing about having installed it.
		track(asked(AddonKeda, true))
		reconcile(AddonKeda)

		settled := addonOf(AddonKeda)
		Expect(ready(settled).Reason).To(Equal("Installed"))
		Expect(settled.Status.Managed).To(BeTrue())
		Expect(settled.Status.Charts[0].Version).To(Equal("2.0.0-evidence"))
		Expect(settled.Status.Charts[1].Version).To(Equal(DefaultKedaHTTPChartVersion))
	})

	It("takes a job that only failed as no evidence of anything", func() {
		cfg := pinned("2.0.0-failed-evidence").forEntry(entry)
		job := addonInstallJob(addonInstallJobName(entry, cfg), entry.DefaultNamespace, entry, cfg)
		track(job)
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "helm: release failed"},
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		found, err := latestCompletedInstall(ctx, k8sClient, kedaInstallComponent)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeNil(), "a helm run that died half-way may have applied nothing")
	})

	It("reads the newest install that finished, because a bump is a new job", func() {
		older := pinned("2.0.0-older").forEntry(entry)
		newer := pinned("2.0.0-newer").forEntry(entry)

		for i, cfg := range []AddonInstallConfig{older, newer} {
			job := addonInstallJob(addonInstallJobName(entry, cfg), entry.DefaultNamespace, entry, cfg)
			track(job)
			finished := metav1.NewTime(metav1.Now().Add(time.Duration(i) * time.Hour))
			job.Status.StartTime = &finished
			job.Status.CompletionTime = &finished
			job.Status.Succeeded = 1
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		}

		found, err := latestCompletedInstall(ctx, k8sClient, kedaInstallComponent)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).NotTo(BeNil())
		Expect(found.Labels[labelInstallVersion]).To(Equal("2.0.0-newer"))
	})
})

var _ = Describe("Removing an addon", func() {
	ctx := context.Background()

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

	BeforeEach(func() {
		reconciler = &AddonReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
		}
		created = nil
		Expect((&KitchenReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}).
			ensurePlatformNamespace(ctx)).To(Succeed())
	})

	AfterEach(func() {
		for _, obj := range created {
			if addon, ok := obj.(*kitchenv1alpha1.Addon); ok {
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(addon), addon); err == nil {
					addon.Finalizers = nil
					Expect(client.IgnoreNotFound(k8sClient.Update(ctx, addon))).To(Succeed())
				}
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	// The blast radius that is refused rather than stated: a database the
	// platform is provisioning through it.
	It("refuses while a connection still provisions through it, and names it", func() {
		addon := asked(AddonCNPG, true)
		track(addon)
		track(&kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "platform-postgres", Namespace: PlatformNamespace},
			Spec:       kitchenv1alpha1.ConnectionSpec{Provider: "cnpg"},
		})
		// One pass so the finalizer is there: without it the object would
		// simply disappear and there would be nothing to refuse on.
		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: PlatformNamespace, Name: AddonCNPG},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: PlatformNamespace, Name: AddonCNPG}, addon)).To(Succeed())
		Expect(k8sClient.Delete(ctx, addon)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: PlatformNamespace, Name: AddonCNPG},
		})
		Expect(err).NotTo(HaveOccurred())

		held := &kitchenv1alpha1.Addon{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: PlatformNamespace, Name: AddonCNPG}, held)).To(Succeed())
		condition := meta.FindStatusCondition(held.Status.Conditions, kitchenv1alpha1.AddonReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal("UninstallRefused"))
		Expect(condition.Message).To(ContainSubstring("platform-postgres"))
	})

	// A release the platform did not create is not the platform's to remove:
	// the record goes and the release stays.
	It("lets go of an entry it never installed, without uninstalling anything", func() {
		addon := asked(AddonKeda, true)
		track(addon)
		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: PlatformNamespace, Name: AddonKeda},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Delete(ctx, addon)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: PlatformNamespace, Name: AddonKeda},
		})
		Expect(err).NotTo(HaveOccurred())

		gone := &kitchenv1alpha1.Addon{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: AddonKeda}, gone)
		Expect(err).To(HaveOccurred(), "nothing holds an addon whose release is somebody else's")

		job := &batchv1.Job{}
		err = k8sClient.Get(ctx, types.NamespacedName{
			Namespace: PlatformNamespace, Name: addonUninstallJobName(entryOf(AddonKeda)),
		}, job)
		Expect(err).To(HaveOccurred(), "it installed nothing, so it removes nothing")
	})
})

var _ = Describe("The addon catalogue", func() {
	// The catalogue is what a cluster-admin job is allowed to install, so
	// every entry has to say what its account may do and what value grants
	// it. An entry that says neither would be a grant nobody can review.
	It("has every entry declare its grant and the value that makes it", func() {
		for _, entry := range addonEntries() {
			Expect(entry.ChartValue).NotTo(BeEmpty(), entry.ID+" names no chart value")
			Expect(entry.Grant.Because).NotTo(BeEmpty(), entry.ID+" does not say why its account is what it is")
			Expect(entry.BlastRadius).NotTo(BeEmpty(), entry.ID+" does not say what removing it costs")
			Expect(entry.Charts).NotTo(BeEmpty(), entry.ID+" installs nothing")
			Expect(entry.Component).NotTo(BeEmpty(), entry.ID+" has no name in the logs")
		}
	})

	It("only depends on entries it has", func() {
		for _, entry := range addonEntries() {
			for _, id := range entry.DependsOn {
				_, known := lookupAddon(id)
				Expect(known).To(BeTrue(), entry.ID+" depends on "+id+", which is not in the catalogue")
			}
		}
	})

	// Nothing from a request reaches an install job's argv, so a flag naming
	// an entry the operator does not have is a misconfiguration rather than
	// something to route around.
	It("refuses a grant for an entry it does not have", func() {
		flags := AddonFlags{ServiceAccounts: EntryValues{"nothing-like-this": "an-account"}}
		_, err := flags.Installs()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("nothing-like-this"))
	})

	It("takes a pin per chart, and refuses one that names no chart", func() {
		flags := AddonFlags{Versions: EntryValues{AddonKeda + "/" + kedaChartName: "9.9.9"}}
		installs, err := flags.Installs()
		Expect(err).NotTo(HaveOccurred())
		Expect(installs.forEntry(entryOf(AddonKeda)).Versions[kedaChartName]).To(Equal("9.9.9"))

		flags = AddonFlags{Versions: EntryValues{AddonKeda: "9.9.9"}}
		_, err = flags.Installs()
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Seeding the addons", func() {
	ctx := context.Background()

	var reconciler *KitchenReconciler
	var kitchen *kitchenv1alpha1.Kitchen

	cleanUp := func(id string) {
		addon := &kitchenv1alpha1.Addon{}
		key := types.NamespacedName{Namespace: PlatformNamespace, Name: id}
		if err := k8sClient.Get(ctx, key, addon); err == nil {
			addon.Finalizers = nil
			Expect(client.IgnoreNotFound(k8sClient.Update(ctx, addon))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, addon))).To(Succeed())
		}
	}

	BeforeEach(func() {
		reconciler = &KitchenReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Addons: AddonInstalls{AddonCNPG: {ServiceAccount: "kitchen-cnpg-install"}},
		}
		kitchen = &kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}}
		Expect(reconciler.ensurePlatformNamespace(ctx)).To(Succeed())
		cleanUp(AddonCNPG)
		cleanUp(AddonKeda)
	})

	AfterEach(func() {
		cleanUp(AddonCNPG)
		cleanUp(AddonKeda)
	})

	// The grant decides what the object asks for, not whether it exists.
	// Granting the account is an explicit act — a chart value somebody set,
	// creating a ServiceAccount bound to cluster-admin — and nobody performs
	// it without wanting the dependency, so a permitted entry is seeded
	// asking for the install rather than waiting to be asked twice.
	It("seeds every entry, and only a permitted one asks to be installed", func() {
		changed, err := reconciler.seedAddons(ctx, kitchen)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeTrue())
		Expect(kitchen.Status.Addons.Seeded).To(ConsistOf(AddonCNPG, AddonKeda))

		granted := &kitchenv1alpha1.Addon{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: PlatformNamespace, Name: AddonCNPG}, granted)).To(Succeed())
		Expect(granted.Spec.Install).To(BeTrue())

		// The entry with no account still gets an object, because "is this
		// serving in this cluster" is a fact about the cluster and the
		// platform has to answer it for a KEDA somebody installed by hand.
		// It just asks for nothing.
		ungranted := &kitchenv1alpha1.Addon{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: PlatformNamespace, Name: AddonKeda}, ungranted)).To(Succeed())
		Expect(ungranted.Spec.Install).To(BeFalse())
	})

	// The seed is a good default, not a fixture the platform keeps
	// reinstating: an installation that would rather run its own has to be
	// able to end up with no object at all.
	It("leaves a seeded addon somebody deleted deleted", func() {
		_, err := reconciler.seedAddons(ctx, kitchen)
		Expect(err).NotTo(HaveOccurred())
		cleanUp(AddonCNPG)
		cleanUp(AddonKeda)

		changed, err := reconciler.seedAddons(ctx, kitchen)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed).To(BeFalse(), "the record is what stops it coming back")

		gone := &kitchenv1alpha1.Addon{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: AddonCNPG}, gone)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Rolling an addon up onto the platform", func() {
	ctx := context.Background()

	var reconciler *KitchenReconciler
	var kitchen *kitchenv1alpha1.Kitchen

	setCond := func(condType string, status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&kitchen.Status.Conditions, metav1.Condition{
			Type: condType, Status: status, Reason: reason, Message: message,
		})
	}
	condition := func(condType string) *metav1.Condition {
		return meta.FindStatusCondition(kitchen.Status.Conditions, condType)
	}

	BeforeEach(func() {
		reconciler = &KitchenReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient}
		kitchen = &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				ScaleToZero: kitchenv1alpha1.ScaleToZeroSpec{Enabled: true},
			},
		}
		Expect(reconciler.ensurePlatformNamespace(ctx)).To(Succeed())
	})

	It("carries no condition at all while the platform idles nothing", func() {
		kitchen.Spec.ScaleToZero.Enabled = false
		setCond(condScaleToZeroReady, metav1.ConditionFalse, "Stale", "from an earlier reconcile")

		Expect(reconciler.reconcileKeda(ctx, kitchen, setCond)).To(BeTrue())
		Expect(condition(condScaleToZeroReady)).To(BeNil())
	})

	// The guidance is not lost by staying quiet: a cnpg Connection in a
	// cluster without the operator says exactly this, on the connection,
	// where somebody is looking.
	It("says nothing about databases where nothing has asked for one", func() {
		setCond(condDatabasesReady, metav1.ConditionFalse, "Stale", "from an earlier reconcile")

		Expect(reconciler.reconcileDatabases(ctx, kitchen, setCond)).To(BeTrue())
		Expect(condition(condDatabasesReady)).To(BeNil())
	})

	// The failure this suite exists for. Adoption must not depend on the
	// grant: "is the add-on serving" is a fact about the cluster, and the
	// documented path is that somebody installs KEDA themselves. A platform
	// that answered "no addon, so nothing idles" in a cluster plainly running
	// one would be wrong about its own capabilities.
	It("adopts an entry nobody granted an account for", func() {
		addon := asked(AddonKeda, false)
		Expect(k8sClient.Create(ctx, addon)).To(Succeed())
		defer releaseAddon(ctx, addon.Name)

		// This envtest cluster serves the add-on's CRD, so it is exactly the
		// cluster somebody installed KEDA into by hand.
		installer := &AddonReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient}
		_, err := installer.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: PlatformNamespace, Name: AddonKeda},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(reconciler.reconcileKeda(ctx, kitchen, setCond)).To(BeTrue())
		Expect(condition(condScaleToZeroReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(condScaleToZeroReady).Reason).To(Equal("AlreadyServing"))
	})

	It("says which addon is missing where scale-to-zero is on and none was seeded", func() {
		Expect(reconciler.reconcileKeda(ctx, kitchen, setCond)).To(BeTrue())
		Expect(condition(condScaleToZeroReady).Reason).To(Equal("AddonMissing"))
		Expect(condition(condScaleToZeroReady).Message).To(ContainSubstring(AddonKeda))
	})

	// The roll-up is the Addon's own words: they were written to be read by
	// whoever can fix them, and rewording them here would leave two texts to
	// keep in step.
	It("copies the addon's own verdict rather than restating it", func() {
		addon := asked(AddonKeda, true)
		Expect(k8sClient.Create(ctx, addon)).To(Succeed())
		defer releaseAddon(ctx, addon.Name)
		meta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
			Type: kitchenv1alpha1.AddonReady, Status: metav1.ConditionTrue,
			Reason: "AlreadyServing", Message: "somebody else installed it",
		})
		Expect(k8sClient.Status().Update(ctx, addon)).To(Succeed())

		Expect(reconciler.reconcileKeda(ctx, kitchen, setCond)).To(BeTrue())
		Expect(condition(condScaleToZeroReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(condScaleToZeroReady).Reason).To(Equal("AlreadyServing"))
		Expect(condition(condScaleToZeroReady).Message).To(Equal("somebody else installed it"))
	})
})

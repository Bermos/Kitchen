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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// wantsDatabases builds a Kitchen that has (or has not) asked the operator to
// install CloudNativePG for it.
func wantsDatabases(install bool) *kitchenv1alpha1.Kitchen {
	return &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			Databases: kitchenv1alpha1.DatabasesSpec{
				Install:           install,
				Namespace:         "kitchen-databases",
				OperatorNamespace: "cnpg-system",
			},
		},
	}
}

var _ = Describe("Planning the CloudNativePG install", func() {
	// permitted is a chart that granted the operator an account, with
	// everything else at the operator's own pins.
	permitted := CNPGInstallConfig{ServiceAccount: "kitchen-cnpg-install"}.withDefaults()

	It("says nothing at all where no database has been asked for", func() {
		plan := planDatabases(wantsDatabases(false), permitted, "cnpg-system", false, nil)
		Expect(plan.clear).To(BeTrue())
		Expect(plan.ready).To(BeTrue())
		Expect(plan.install).To(BeFalse())
	})

	// The seed rule, and the one that matters most: cnpg is a popular thing
	// for a cluster to already run, and the platform provisions into it
	// happily while never writing to a release it did not create.
	It("adopts a CloudNativePG that is already serving, and records that it owns nothing", func() {
		plan := planDatabases(wantsDatabases(true), permitted, "cnpg-system", true, nil)
		Expect(plan.install).To(BeFalse())
		Expect(plan.status).To(Equal(metav1.ConditionTrue))
		Expect(plan.reason).To(Equal("OperatorPresent"))
		Expect(plan.record).NotTo(BeNil())
		Expect(plan.record.Managed).To(BeFalse())
		Expect(plan.record.Version).To(BeEmpty())
		Expect(plan.message).To(ContainSubstring("installed nothing"))
	})

	It("leaves an adopted installation alone forever, however the pin moves", func() {
		kitchen := wantsDatabases(true)
		kitchen.Status.Databases = &kitchenv1alpha1.DatabasesStatus{Namespace: "cnpg-system"}

		plan := planDatabases(kitchen, permitted, "cnpg-system", true, nil)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("OperatorPresent"))
	})

	It("installs where the cluster runs none and the installation asked it to", func() {
		plan := planDatabases(wantsDatabases(true), permitted, "cnpg-system", false, nil)
		Expect(plan.install).To(BeTrue())
	})

	It("refuses to install without the account the chart only creates when asked", func() {
		plan := planDatabases(wantsDatabases(true), CNPGInstallConfig{}.withDefaults(), "cnpg-system", false, nil)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("InstallNotPermitted"))
		Expect(plan.message).To(ContainSubstring("databases.install.enabled"))
		Expect(plan.ready).To(BeTrue())
	})

	It("refuses a namespace that is not a namespace before creating a cluster-admin job", func() {
		plan := planDatabases(wantsDatabases(true), permitted, "Not A Namespace", false, nil)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("NamespaceInvalid"))
	})

	It("settles once its own install has finished", func() {
		kitchen := wantsDatabases(true)
		kitchen.Status.Databases = &kitchenv1alpha1.DatabasesStatus{
			Managed: true, Namespace: "cnpg-system", Version: permitted.ChartVersion,
		}

		plan := planDatabases(kitchen, permitted, "cnpg-system", true, nil)
		Expect(plan.install).To(BeFalse())
		Expect(plan.status).To(Equal(metav1.ConditionTrue))
		Expect(plan.reason).To(Equal("OperatorInstalled"))
		Expect(plan.ready).To(BeTrue())
	})

	// An operator upgrade carries its dependency forward, rather than leaving
	// the platform on whatever the first install happened to pull.
	It("reinstalls what it owns when the operator's pin has moved", func() {
		kitchen := wantsDatabases(true)
		kitchen.Status.Databases = &kitchenv1alpha1.DatabasesStatus{
			Managed: true, Namespace: "cnpg-system", Version: "0.0.1",
		}

		plan := planDatabases(kitchen, permitted, "cnpg-system", true, nil)
		Expect(plan.install).To(BeTrue())
	})

	// Issue #244: the platform installed CloudNativePG, the status write that
	// recorded it did not land, and every reconcile afterwards read the
	// cluster as somebody else's and said so — permanently, because the
	// branch it took rewrote the record it had just misread. The install job
	// is the fact the record was only a copy of.
	It("keeps what it installed even where the record of it was lost", func() {
		kitchen := wantsDatabases(true)

		plan := planDatabases(kitchen, permitted, "cnpg-system", true, &kitchenv1alpha1.DatabasesStatus{
			Managed: true, Namespace: "cnpg-system", Version: permitted.ChartVersion,
		})
		Expect(plan.reason).To(Equal("OperatorInstalled"))
		Expect(plan.record).NotTo(BeNil())
		Expect(plan.record.Managed).To(BeTrue())
		Expect(plan.record.Version).To(Equal(permitted.ChartVersion))
		Expect(plan.install).To(BeFalse())
	})

	It("corrects a record that disclaims a release its own job installed", func() {
		kitchen := wantsDatabases(true)
		kitchen.Status.Databases = &kitchenv1alpha1.DatabasesStatus{Namespace: "cnpg-system"}

		plan := planDatabases(kitchen, permitted, "cnpg-system", true, &kitchenv1alpha1.DatabasesStatus{
			Managed: true, Namespace: "cnpg-system", Version: permitted.ChartVersion,
		})
		Expect(plan.reason).To(Equal("OperatorInstalled"))
		Expect(plan.record.Managed).To(BeTrue())
		Expect(plan.message).NotTo(ContainSubstring("installed nothing"))
	})

	It("writes no record at all where the one there already agrees", func() {
		installed := &kitchenv1alpha1.DatabasesStatus{
			Managed: true, Namespace: "cnpg-system", Version: permitted.ChartVersion,
		}
		kitchen := wantsDatabases(true)
		kitchen.Status.Databases = installed.DeepCopy()

		plan := planDatabases(kitchen, permitted, "cnpg-system", true, installed)
		Expect(plan.reason).To(Equal("OperatorInstalled"))
		Expect(plan.record).To(BeNil(), "an identical record would be a status write every 30 seconds")
	})

	// An install job from before the operator labelled them says who
	// installed CloudNativePG but not which version. Unknown reads as drift,
	// and drift reinstalls at the pin — which is what records the version.
	It("reinstalls at the pin where its own job does not say what it installed", func() {
		plan := planDatabases(wantsDatabases(true), permitted, "cnpg-system", true,
			&kitchenv1alpha1.DatabasesStatus{Managed: true, Namespace: "cnpg-system"})
		Expect(plan.install).To(BeTrue())
	})

	It("never acts on drift in an installation that has withdrawn the grant", func() {
		kitchen := wantsDatabases(true)
		kitchen.Status.Databases = &kitchenv1alpha1.DatabasesStatus{
			Managed: true, Namespace: "cnpg-system", Version: "0.0.1",
		}

		plan := planDatabases(kitchen, CNPGInstallConfig{}.withDefaults(), "cnpg-system", true, nil)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("OperatorInstalled"))
	})
})

// The credential exception, enforced where it has to be: at admission, so a
// Connection that reached the cluster another way cannot be shaped wrongly
// either.
var _ = Describe("A connection to the platform's own Postgres", func() {
	ctx := context.Background()

	It("is admitted with no credentials secret at all", func() {
		conn := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "cl-cnpg-admit", Namespace: "default"},
			Spec:       kitchenv1alpha1.ConnectionSpec{Provider: "cnpg"},
		}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		Expect(k8sClient.Delete(ctx, conn)).To(Succeed())
	})

	It("is refused when it names one, because nothing would read it", func() {
		conn := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "cl-cnpg-refused", Namespace: "default"},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "cnpg",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "somewhere"},
			},
		}
		err := k8sClient.Create(ctx, conn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("takes no credentialsSecretRef"))
	})

	It("leaves every other provider requiring one", func() {
		conn := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "cl-neon-refused", Namespace: "default"},
			Spec:       kitchenv1alpha1.ConnectionSpec{Provider: "neon"},
		}
		err := k8sClient.Create(ctx, conn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("credentialsSecretRef is required"))
	})
})

var _ = Describe("The CloudNativePG install job", func() {
	cfg := CNPGInstallConfig{ServiceAccount: "kitchen-cnpg-install"}.withDefaults()

	It("is named after what it installs, so a bump is a new job", func() {
		bumped := cfg
		bumped.ChartVersion = "0.30.0"
		Expect(cnpgInstallJobName(cfg)).NotTo(Equal(cnpgInstallJobName(bumped)))
		Expect(len(cnpgInstallJobName(cfg))).To(BeNumerically("<=", 63))
	})

	// The job is bound to cluster-admin. Anything from a request in its argv
	// would make the grant meaningless, and a shell would make the check
	// pointless.
	It("runs helm directly, with an argv nothing from a request reaches", func() {
		job := cnpgInstallJob(cnpgInstallJobName(cfg), "cnpg-system", cfg)

		Expect(job.Spec.Template.Spec.InitContainers).To(BeEmpty())
		Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Command).To(Equal([]string{"helm"}))
		Expect(strings.Join(container.Args, " ")).To(ContainSubstring(
			"upgrade cnpg cloudnative-pg --install"))
		Expect(container.Args).To(ContainElement(cfg.Repository))
		Expect(container.Args).To(ContainElement(cfg.ChartVersion))
		Expect(container.Args).To(ContainElement("cnpg-system"))
		Expect(container.Args).To(ContainElement("--wait"))
		Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal("kitchen-cnpg-install"))
		Expect(*job.Spec.BackoffLimit).To(BeEquivalentTo(0))
	})

	// The job's name is sanitised into a DNS label and so cannot be read back
	// as a version. The labels are what a later reconcile reads ownership
	// from, so they say what the name cannot.
	It("says what it installed and where, in labels a later reconcile can read", func() {
		job := cnpgInstallJob(cnpgInstallJobName(cfg), "cnpg-system", cfg)
		Expect(job.Labels[labelInstallVersion]).To(Equal(cfg.ChartVersion))
		Expect(job.Labels[labelInstallNamespace]).To(Equal("cnpg-system"))

		owned := cnpgInstalled(job, cfg, "somewhere-else")
		Expect(owned.Managed).To(BeTrue())
		Expect(owned.Version).To(Equal(cfg.ChartVersion))
		Expect(owned.Namespace).To(Equal("cnpg-system"), "what it installed, not what is configured now")
	})

	It("reads a job from before the labels by its name, and no further", func() {
		unlabelled := cnpgInstallJob(cnpgInstallJobName(cfg), "cnpg-system", cfg)
		unlabelled.Labels = map[string]string{}

		owned := cnpgInstalled(unlabelled, cfg, "cnpg-system")
		Expect(owned.Managed).To(BeTrue())
		Expect(owned.Version).To(Equal(cfg.ChartVersion), "its name can only be this pin's")

		older := cnpgInstalled(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: "kitchen-cnpg-install-0-0-1",
		}}, cfg, "cnpg-system")
		Expect(older.Managed).To(BeTrue())
		Expect(older.Version).To(BeEmpty(), "unknown, which the next install settles")
	})

	It("is no evidence at all when there is no job", func() {
		Expect(cnpgInstalled(nil, cfg, "cnpg-system")).To(BeNil())
	})
})

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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
		plan := planDatabases(wantsDatabases(false), permitted, "cnpg-system", false)
		Expect(plan.clear).To(BeTrue())
		Expect(plan.ready).To(BeTrue())
		Expect(plan.install).To(BeFalse())
	})

	// The seed rule, and the one that matters most: cnpg is a popular thing
	// for a cluster to already run, and the platform provisions into it
	// happily while never writing to a release it did not create.
	It("adopts a CloudNativePG that is already serving, and records that it owns nothing", func() {
		plan := planDatabases(wantsDatabases(true), permitted, "cnpg-system", true)
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

		plan := planDatabases(kitchen, permitted, "cnpg-system", true)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("OperatorPresent"))
	})

	It("installs where the cluster runs none and the installation asked it to", func() {
		plan := planDatabases(wantsDatabases(true), permitted, "cnpg-system", false)
		Expect(plan.install).To(BeTrue())
	})

	It("refuses to install without the account the chart only creates when asked", func() {
		plan := planDatabases(wantsDatabases(true), CNPGInstallConfig{}.withDefaults(), "cnpg-system", false)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("InstallNotPermitted"))
		Expect(plan.message).To(ContainSubstring("databases.install.enabled"))
		Expect(plan.ready).To(BeTrue())
	})

	It("refuses a namespace that is not a namespace before creating a cluster-admin job", func() {
		plan := planDatabases(wantsDatabases(true), permitted, "Not A Namespace", false)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("NamespaceInvalid"))
	})

	It("settles once its own install has finished", func() {
		kitchen := wantsDatabases(true)
		kitchen.Status.Databases = &kitchenv1alpha1.DatabasesStatus{
			Managed: true, Namespace: "cnpg-system", Version: permitted.ChartVersion,
		}

		plan := planDatabases(kitchen, permitted, "cnpg-system", true)
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

		plan := planDatabases(kitchen, permitted, "cnpg-system", true)
		Expect(plan.install).To(BeTrue())
	})

	It("never acts on drift in an installation that has withdrawn the grant", func() {
		kitchen := wantsDatabases(true)
		kitchen.Status.Databases = &kitchenv1alpha1.DatabasesStatus{
			Managed: true, Namespace: "cnpg-system", Version: "0.0.1",
		}

		plan := planDatabases(kitchen, CNPGInstallConfig{}.withDefaults(), "cnpg-system", true)
		Expect(plan.install).To(BeFalse())
		Expect(plan.reason).To(Equal("OperatorInstalled"))
	})
})

var _ = Describe("The CloudNativePG install job", func() {
	cfg := CNPGInstallConfig{ServiceAccount: "kitchen-cnpg-install"}.withDefaults()

	It("is named after what it installs, so a bump is a new job", func() {
		bumped := cfg
		bumped.ChartVersion = "0.30.0"
		Expect(cnpgInstallJobName(cfg)).NotTo(Equal(cnpgInstallJobName(bumped)))
		Expect(cnpgInstallJobName(cfg)).To(HaveLen(len(cnpgInstallJobName(cfg))))
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
})

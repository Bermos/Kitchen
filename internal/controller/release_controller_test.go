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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

var _ = Describe("Release Controller", func() {
	const (
		projectName = "relshop"
		envName     = "relshop-production"
		previewName = "relshop-pr-7"
	)

	ctx := context.Background()

	var reconciler *ReleaseReconciler

	// Releases minted inside one test share a creation timestamp — etcd
	// stores seconds — so the reconciler's tie-break on name is what orders
	// them. The zero-padded counter makes that order the intended one:
	// higher is newer.
	name := func(n int) string { return fmt.Sprintf("%s-rel-%06d", projectName, n) }

	key := func(n int) types.NamespacedName {
		return types.NamespacedName{Name: name(n), Namespace: PlatformNamespace}
	}

	makeRelease := func(n int) *kitchenv1alpha1.Release {
		release := &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: name(n), Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: fmt.Sprintf("%s-bld-%06d", projectName, n)},
				Image:      fmt.Sprintf("registry.example.com/kitchen/%s@sha256:%064d", projectName, n),
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, release)).To(Succeed())
		return release
	}

	makeEnvironment := func(environment, releaseName string, envType kitchenv1alpha1.EnvironmentType) {
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: environment, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       envType,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
			},
		}
		if envType == kitchenv1alpha1.EnvironmentPreview {
			env.Spec.Preview = &kitchenv1alpha1.PreviewInfo{PullRequest: 7, Branch: "feature"}
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, env)).To(Succeed())
	}

	reconcileRelease := func(n int) {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key(n)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	getRelease := func(n int) *kitchenv1alpha1.Release {
		release := &kitchenv1alpha1.Release{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(n), release)).To(Succeed())
		return release
	}

	exists := func(n int) bool {
		err := k8sClient.Get(ctx, key(n), &kitchenv1alpha1.Release{})
		if apierrors.IsNotFound(err) {
			return false
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return true
	}

	// setRetention writes the count on the platform singleton, which is where
	// the reconciler reads it from.
	setRetention := func(keep int32) {
		kitchen := &kitchenv1alpha1.Kitchen{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen)).To(Succeed())
		kitchen.Spec.Builds.ReleaseRetention = keep
		ExpectWithOffset(1, k8sClient.Update(ctx, kitchen)).To(Succeed())
	}

	BeforeEach(func() {
		reconciler = &ReleaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
			},
		}))).To(Succeed())
		// Every spec sets the count it needs, so a leftover from the previous
		// one is never inherited.
		setRetention(0)
	})

	AfterEach(func() {
		releases := &kitchenv1alpha1.ReleaseList{}
		Expect(k8sClient.List(ctx, releases, client.InNamespace(PlatformNamespace))).To(Succeed())
		for i := range releases.Items {
			if releases.Items[i].Spec.ProjectRef.Name == projectName {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &releases.Items[i]))).To(Succeed())
			}
		}
		environments := &kitchenv1alpha1.EnvironmentList{}
		Expect(k8sClient.List(ctx, environments, client.InNamespace(PlatformNamespace))).To(Succeed())
		for i := range environments.Items {
			if environments.Items[i].Spec.ProjectRef.Name == projectName {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &environments.Items[i]))).To(Succeed())
			}
		}
		// The singleton goes with the specs that made it: other suites create
		// theirs with IgnoreAlreadyExists, so one left behind here would be
		// the one they run against.
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		}))).To(Succeed())
	})

	Context("where the release is live", func() {
		It("names every environment pointing at it, sorted", func() {
			makeRelease(1)
			makeEnvironment(previewName, name(1), kitchenv1alpha1.EnvironmentPreview)
			makeEnvironment(envName, name(1), kitchenv1alpha1.EnvironmentProduction)

			reconcileRelease(1)

			Expect(getRelease(1).Status.Environments).To(Equal([]string{previewName, envName}))
		})

		It("drops the environment once it has moved to another release", func() {
			makeRelease(1)
			makeRelease(2)
			makeEnvironment(envName, name(1), kitchenv1alpha1.EnvironmentProduction)
			reconcileRelease(1)
			Expect(getRelease(1).Status.Environments).To(Equal([]string{envName}))

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: PlatformNamespace}, env)).To(Succeed())
			env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: name(2)}
			Expect(k8sClient.Update(ctx, env)).To(Succeed())

			// Which is why an environment change requeues every release of
			// the project, not just the one it now names: the release it came
			// off is the one holding the stale answer.
			reconcileRelease(1)
			reconcileRelease(2)

			Expect(getRelease(1).Status.Environments).To(BeEmpty())
			Expect(getRelease(2).Status.Environments).To(Equal([]string{envName}))
		})

		It("says nothing when no environment runs it", func() {
			makeRelease(1)
			reconcileRelease(1)
			Expect(getRelease(1).Status.Environments).To(BeEmpty())
		})
	})

	Context("immutability", func() {
		// The CRD's CEL transition rule, not a webhook and not the
		// reconciler: an edit never reaches etcd at all.
		It("refuses an edit to the spec at admission", func() {
			release := makeRelease(1)
			release.Spec.Image = "registry.example.com/kitchen/relshop@sha256:" + fmt.Sprintf("%064d", 99)
			err := k8sClient.Update(ctx, release)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Release spec is immutable"))
		})
	})

	Context("retention", func() {
		It("keeps the newest and deletes the rest", func() {
			for n := 1; n <= 5; n++ {
				makeRelease(n)
			}
			setRetention(3)

			reconcileRelease(5)

			Expect(exists(5)).To(BeTrue())
			Expect(exists(4)).To(BeTrue())
			Expect(exists(3)).To(BeTrue())
			Expect(exists(2)).To(BeFalse())
			Expect(exists(1)).To(BeFalse())
		})

		It("keeps a release an environment still runs, however old", func() {
			for n := 1; n <= 5; n++ {
				makeRelease(n)
			}
			// Production rolled back and parked on the oldest release; the
			// preview is on the newest.
			makeEnvironment(envName, name(1), kitchenv1alpha1.EnvironmentProduction)
			makeEnvironment(previewName, name(5), kitchenv1alpha1.EnvironmentPreview)
			setRetention(2)

			reconcileRelease(5)

			Expect(exists(5)).To(BeTrue())
			Expect(exists(4)).To(BeTrue())
			Expect(exists(3)).To(BeFalse())
			Expect(exists(2)).To(BeFalse())
			Expect(exists(1)).To(BeTrue())
		})

		It("keeps everything when the count is zero", func() {
			for n := 1; n <= 4; n++ {
				makeRelease(n)
			}
			setRetention(0)

			reconcileRelease(4)

			for n := 1; n <= 4; n++ {
				Expect(exists(n)).To(BeTrue())
			}
		})

		It("leaves another project's releases alone", func() {
			for n := 1; n <= 4; n++ {
				makeRelease(n)
			}
			other := &kitchenv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: "relother-rel-000001", Namespace: PlatformNamespace},
				Spec: kitchenv1alpha1.ReleaseSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "relother"},
					BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "relother-bld-000001"},
					Image:      "registry.example.com/kitchen/relother@sha256:" + fmt.Sprintf("%064d", 1),
				},
			}
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, other))).To(Succeed())
			}()
			setRetention(1)

			reconcileRelease(4)

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(other), &kitchenv1alpha1.Release{})).To(Succeed())
		})
	})
})

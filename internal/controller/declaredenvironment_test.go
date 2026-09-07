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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// An environment declared before anything deployed into it (#491).
//
// Two things have to hold for the feature to be worth having, and neither was
// true before the API could create one. An environment with no release has to
// read as what it is rather than as a fault; and the first build to arrive has
// to *adopt* it — keeping the bar its owners set, and going through the
// promotion path that bar exists for — rather than treating it as an
// environment of its own making.
var _ = Describe("A declared environment", func() {
	const (
		projectName = "declared"
		envName     = "declared-staging"
		releaseName = "declared-rel-000001"
		namespace   = "default"
		baseDomain  = "apps.example.com"
		image       = "registry.example.com/kitchen/declared@sha256:0123456789abcdef"
	)
	// The bar a declared environment sets before its first release, which is
	// the whole point of declaring one.
	bundleDigest := "sha256:" + strings.Repeat("cd", 32)

	ctx := context.Background()
	appNS := "kitchen-" + projectName

	var (
		environments *EnvironmentReconciler
		builds       *BuildReconciler
	)

	// declare writes the Environment the API's create route writes: a project,
	// a type, a bar — and no release.
	declare := func(requirements *kitchenv1alpha1.EnvironmentRequirements) *kitchenv1alpha1.Environment {
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef:   kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:         kitchenv1alpha1.EnvironmentStage,
				Owners:       []string{"risk-officer@example.com"},
				DataClass:    kitchenv1alpha1.DataClass("internal"),
				Residency:    "CH",
				Requirements: requirements,
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, env)).To(Succeed())
		return env
	}

	build := func() *kitchenv1alpha1.Build {
		return &kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{Name: projectName + "-bld-1", Namespace: namespace},
			Spec: kitchenv1alpha1.BuildSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Git:        kitchenv1alpha1.GitRevision{SHA: "0123456789ab", Branch: "main"},
			},
		}
	}

	read := func() *kitchenv1alpha1.Environment {
		env := &kitchenv1alpha1.Environment{}
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Name: envName, Namespace: namespace}, env)).To(Succeed())
		return env
	}

	BeforeEach(func() {
		environments = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		builds = &BuildReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: baseDomain, TLS: acmeTLS()},
		})

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/declared",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		release := &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:       kitchenv1alpha1.LocalObjectReference{Name: projectName + "-bld-1"},
				Image:          image,
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8080}},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())
	})

	AfterEach(func() {
		env := &kitchenv1alpha1.Environment{}
		key := types.NamespacedName{Name: envName, Namespace: namespace}
		if err := k8sClient.Get(ctx, key, env); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
			_, err := environments.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Promotion{ObjectMeta: metav1.ObjectMeta{
				Name: automaticPromotionName(projectName, releaseName, envName), Namespace: namespace}},
			&kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	// The empty state, on the object the dashboard reads: Pending, a Ready
	// condition naming what it waits for, and nothing materialized. Before
	// this the same object reported ReleaseMissing — a fault, requeued every
	// fifteen seconds, about a release it had never had.
	It("waits for its first deployment instead of reporting a missing release", func() {
		declare(nil)
		key := types.NamespacedName{Name: envName, Namespace: namespace}
		// The finalizer pass is the first one; the second is the answer.
		_, err := environments.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		result, err := environments.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero(),
			"nothing about this state changes on a clock: the build that deploys here wakes it")

		env := read()
		Expect(env.Status.Phase).To(Equal(kitchenv1alpha1.EnvironmentPending))
		ready := meta.FindStatusCondition(env.Status.Conditions, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonAwaitingDeployment))
		Expect(env.Status.URL).To(BeEmpty())

		err = k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "an environment with no release materializes nothing")
	})

	// Adoption. ensureEnvironment is untouched for the environments nothing
	// declared, and for a declared one it does what it has always done to an
	// environment that already exists: it points spec.releaseRef at the
	// release and writes nothing else. The declarations are the object's own.
	It("is adopted by the first build, which keeps its declarations", func() {
		declare(nil)

		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: projectName, Namespace: namespace}, project)).To(Succeed())
		Expect(builds.promoteOrFlip(ctx, namespace, project, envName, releaseName, build())).To(Succeed())

		env := read()
		Expect(env.Spec.ReleaseRef.Name).To(Equal(releaseName), "the build deploys into the declared environment")
		Expect(env.Spec.Owners).To(ConsistOf("risk-officer@example.com"))
		Expect(env.Spec.DataClass).To(Equal(kitchenv1alpha1.DataClass("internal")))
		Expect(env.Spec.Residency).To(Equal("CH"))
		Expect(env.Spec.Type).To(Equal(kitchenv1alpha1.EnvironmentStage))

		promotion := &kitchenv1alpha1.Promotion{}
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name: automaticPromotionName(projectName, releaseName, envName), Namespace: namespace}, promotion)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"an environment that declares no bar takes the fast path, exactly as it always has")
	})

	// And the case the whole issue is about: a bar set *before* the first
	// release is a bar the first release is judged against. The build
	// controller must not flip it — the promotion reconciler is the only
	// thing that may move a gated environment.
	It("takes its first release through a promotion when it declares requirements", func() {
		declare(&kitchenv1alpha1.EnvironmentRequirements{BundleDigest: bundleDigest})

		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: projectName, Namespace: namespace}, project)).To(Succeed())
		Expect(builds.promoteOrFlip(ctx, namespace, project, envName, releaseName, build())).To(Succeed())

		Expect(read().Spec.ReleaseRef.Name).To(BeEmpty(),
			"the first release into a gated environment is a Promotion, not a flip")

		promotion := &kitchenv1alpha1.Promotion{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: automaticPromotionName(projectName, releaseName, envName), Namespace: namespace},
			promotion)).To(Succeed())
		Expect(promotion.Spec.Trigger).To(Equal(kitchenv1alpha1.PromotionAutomatic))
		Expect(promotion.Spec.ReleaseRef.Name).To(Equal(releaseName))
		Expect(promotion.Spec.EnvironmentRef.Name).To(Equal(envName))
	})
})

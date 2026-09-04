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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// A corrected setting reaching an environment with no next commit (#392).
//
// The API route is what a person presses; what it makes is the Release these
// cases are about, and the reason they run against a real API server is that
// the two things that could go wrong here are both the API server's opinions:
// a Release spec is immutable, so the correction has to be a *new* object, and
// the new object's name has to be one Kubernetes will accept.

var _ = Describe("Redeploying the commit an environment is already on", func() {
	const (
		projectName = "cfgshop"
		namespace   = "default"
		envName     = "cfgshop-production"
		buildName   = "cfgshop-bld-1"
		image       = "registry.example.com/kitchen/cfgshop@sha256:3333333333333333"
	)
	appNS := "kitchen-" + projectName
	releaseName := projectName + "-rel-abc123def456"

	ctx := context.Background()
	envKey := types.NamespacedName{Name: envName, Namespace: namespace}

	var reconciler *EnvironmentReconciler

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	// project is the fixture in the state the issue found it in: the setting
	// has already been corrected, and the release that is running froze the
	// wrong one.
	project := func() *kitchenv1alpha1.Project {
		found := &kitchenv1alpha1.Project{}
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Name: projectName, Namespace: namespace}, found)).To(Succeed())
		return found
	}

	BeforeEach(func() {
		reconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
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
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/cfgshop",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
				// The correction: a posture the running release never froze.
				Runtime: kitchenv1alpha1.RuntimeSpec{
					Port:     8080,
					Security: &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true, RunAsUser: 1000},
				},
			},
		}))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{Name: buildName, Namespace: namespace},
			Spec: kitchenv1alpha1.BuildSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Git: kitchenv1alpha1.GitRevision{
					SHA: "abc123def4567890", Branch: "main",
				},
			},
		}))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: buildName},
				Image:      image,
				// What was frozen before the setting was corrected.
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8080, Replicas: ptr.To(int32(1))},
				},
			},
		}))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentProduction,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
			},
		}))).To(Succeed())
	})

	AfterEach(func() {
		env := &kitchenv1alpha1.Environment{}
		if err := k8sClient.Get(ctx, envKey, env); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
			reconcileOnce()
		}
		releases := &kitchenv1alpha1.ReleaseList{}
		Expect(k8sClient.List(ctx, releases, client.InNamespace(namespace))).To(Succeed())
		for i := range releases.Items {
			if releases.Items[i].Spec.ProjectRef.Name == projectName {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &releases.Items[i]))).To(Succeed())
			}
		}
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Build{ObjectMeta: metav1.ObjectMeta{Name: buildName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("cuts a new release the environment converges on, and leaves the old one alone", func() {
		By("deploying what is running, with the setting that was wrong")
		reconcileOnce()
		reconcileOnce()
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, deploy)).To(Succeed())
		Expect(deploy.Spec.Template.Spec.SecurityContext.RunAsUser).To(BeNil(),
			"the running release froze no posture, and that is exactly the problem")

		By("cutting the release a redeploy makes")
		build := &kitchenv1alpha1.Build{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: buildName, Namespace: namespace}, build)).To(Succeed())
		current := &kitchenv1alpha1.Release{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: releaseName, Namespace: namespace}, current)).To(Succeed())

		fresh, err := RedeployRelease(project(), build, current)
		Expect(err).NotTo(HaveOccurred())
		Expect(fresh.Name).NotTo(Equal(releaseName))
		Expect(fresh.Name).To(HavePrefix(releaseName), "it is still a release of that commit")
		Expect(len(fresh.Name)).To(BeNumerically("<=", 63),
			"the name is a label value on the rescan job")
		Expect(fresh.Spec.Image).To(Equal(image), "the artifact does not move")
		Expect(fresh.Spec.ConfigSnapshot.Runtime.Security).NotTo(BeNil())
		// The API server is what proves the new object is legal, which is the
		// whole reason this case is here rather than beside the handler.
		Expect(k8sClient.Create(ctx, fresh)).To(Succeed())

		By("pointing the environment at it, the way any other move does")
		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: fresh.Name}
		Expect(k8sClient.Update(ctx, env)).To(Succeed())
		reconcileOnce()
		reconcileOnce()

		By("finding the corrected setting on what is running")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, deploy)).To(Succeed())
		Expect(deploy.Spec.Template.Spec.SecurityContext.RunAsUser).To(Equal(ptr.To(int64(1000))))
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(image),
			"the same commit's image, still")
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		Expect(env.Status.ObservedRelease).To(Equal(fresh.Name))

		By("leaving the release that was running exactly as it was")
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: releaseName, Namespace: namespace}, current)).To(Succeed())
		Expect(current.Spec.ConfigSnapshot.Runtime.Security).To(BeNil())
	})

	It("gives one name to one snapshot, and a different one to a different snapshot", func() {
		build := &kitchenv1alpha1.Build{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: buildName, Namespace: namespace}, build)).To(Succeed())
		current := &kitchenv1alpha1.Release{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: releaseName, Namespace: namespace}, current)).To(Succeed())

		first, err := RedeployRelease(project(), build, current)
		Expect(err).NotTo(HaveOccurred())
		again, err := RedeployRelease(project(), build, current)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Name).To(Equal(first.Name), "the same snapshot is the same release")

		By("correcting the setting a second time")
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		corrected := project()
		corrected.Spec.Runtime.Replicas = ptr.To(int32(4))
		Expect(k8sClient.Update(ctx, corrected)).To(Succeed())

		// Redeploying a redeploy: the fingerprint is replaced rather than
		// stacked, so the name stays one shape however many corrections there
		// have been.
		second, err := RedeployRelease(project(), build, first)
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Name).NotTo(Equal(first.Name))
		Expect(second.Name).To(HavePrefix(releaseName))
		Expect(second.Name).To(HaveLen(len(first.Name)))
		Expect(k8sClient.Create(ctx, second)).To(Succeed())
	})
})

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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/previewgate"
)

var _ = Describe("Environment Controller", func() {
	Context("When reconciling a production environment", func() {
		const (
			projectName = "envshop"
			envName     = "envshop-production"
			releaseName = "envshop-rel-000001"
			namespace   = "default"
			image       = "registry.example.com/kitchen/envshop@sha256:0123456789abcdef"
		)

		ctx := context.Background()

		envKey := types.NamespacedName{Name: envName, Namespace: namespace}
		appNS := "kitchen-" + projectName

		var reconciler *EnvironmentReconciler

		reconcileOnce := func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		BeforeEach(func() {
			reconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// The platform namespace is where the shared Gateway and the
			// forward-auth gate live. Kitchen is installed into it, so it
			// exists before any Environment does.
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
			}))).To(Succeed())

			kitchen := &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
				Spec: kitchenv1alpha1.KitchenSpec{
					BaseDomain: "apps.example.com",
					TLS:        acmeTLS(),
					// As the chart writes it: an identity provider, and a gate
					// for previews to be protected by.
					Auth: kitchenv1alpha1.AuthSpec{
						Enabled:     true,
						PreviewGate: kitchenv1alpha1.PreviewGateSpec{Enabled: true},
					},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())

			project := &kitchenv1alpha1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
				Spec: kitchenv1alpha1.ProjectSpec{
					Source: kitchenv1alpha1.GitSourceSpec{
						ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
						Repo:          "acme/shop",
					},
					Registry: kitchenv1alpha1.RegistrySpec{
						ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
					},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

			release := &kitchenv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace},
				Spec: kitchenv1alpha1.ReleaseSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "shop-bld-1"},
					Image:      image,
					ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
						Env: []kitchenv1alpha1.EnvVar{
							{Name: "PUBLIC_API", Value: "https://api.example.com", PreviewValue: "https://api-staging.example.com"},
							{Name: "SESSION_SECRET", SecretRef: &kitchenv1alpha1.SecretKeySelector{Name: "shop-secrets", Key: "session"}},
						},
						Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8080, Replicas: ptr.To(int32(2))},
					},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())

			env := &kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: namespace},
				Spec: kitchenv1alpha1.EnvironmentSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					Type:       kitchenv1alpha1.EnvironmentProduction,
					ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
		})

		AfterEach(func() {
			env := &kitchenv1alpha1.Environment{}
			if err := k8sClient.Get(ctx, envKey, env); err == nil {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
				// Run the finalizer so the object actually goes away.
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
				Expect(err).NotTo(HaveOccurred())
			}
			for _, obj := range []client.Object{
				&kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace}},
				&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
				&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
			} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		It("materializes the release as Deployment, Service and HTTPRoute", func() {
			By("reconciling until the finalizer and children are in place")
			reconcileOnce()
			reconcileOnce()

			By("checking the Deployment")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := deploy.Spec.Template.Spec.Containers[0]
			Expect(container.Image).To(Equal(image))
			Expect(container.Ports[0].ContainerPort).To(Equal(int32(8080)))
			Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))
			Expect(container.Env).To(ContainElement(corev1.EnvVar{Name: "PUBLIC_API", Value: "https://api.example.com"}))
			// The platform's own variables come first, so that a project
			// setting one of them wins.
			Expect(container.Env[0]).To(Equal(corev1.EnvVar{Name: "PORT", Value: "8080"}))
			// Where this environment is published, which the application has
			// no other way of knowing — a preview's hostname carries a pull
			// request number nothing in the repository has heard of.
			Expect(container.Env).To(ContainElement(corev1.EnvVar{
				Name: "KITCHEN_URL", Value: "https://" + projectName + ".apps.example.com"}))
			Expect(container.Env[len(container.Env)-1].ValueFrom.SecretKeyRef.Name).To(Equal("shop-secrets"))
			// The image came from a registry that wanted a credential to push
			// and wants one to pull: the build left that docker config in this
			// namespace, and without naming it the pods would sit in
			// ImagePullBackOff with everything else reading as healthy.
			Expect(deploy.Spec.Template.Spec.ImagePullSecrets).To(ConsistOf(
				corev1.LocalObjectReference{Name: registrySecretName("registry")}))

			By("checking the Service")
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, svc)).To(Succeed())
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(80)))
			Expect(svc.Spec.Ports[0].TargetPort.IntValue()).To(Equal(8080))

			By("checking the HTTPRoute")
			route := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, route)).To(Succeed())
			Expect(route.Spec.Hostnames).To(ConsistOf(gatewayv1.Hostname("envshop.apps.example.com")))
			Expect(string(route.Spec.ParentRefs[0].Name)).To(Equal(SharedGatewayName))
			Expect(string(*route.Spec.ParentRefs[0].Namespace)).To(Equal(PlatformNamespace))

			By("checking the Environment status")
			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.Status.URL).To(Equal("https://envshop.apps.example.com"))
			Expect(env.Status.ObservedRelease).To(Equal(releaseName))
			Expect(env.Status.Phase).To(Equal(kitchenv1alpha1.EnvironmentDeploying))
		})

		It("records a release move nobody else recorded", func() {
			By("reconciling until the first release is observed")
			reconcileOnce()
			reconcileOnce()

			By("moving the spec to another release directly, as kubectl would")
			second := &kitchenv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: releaseName + "-next", Namespace: namespace},
				Spec: kitchenv1alpha1.ReleaseSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "shop-bld-2"},
					Image:      image,
				},
			}
			Expect(k8sClient.Create(ctx, second)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, second))).To(Succeed())
			})
			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: second.Name}
			Expect(k8sClient.Update(ctx, env)).To(Succeed())

			reconcileOnce()

			By("checking the backstop entry: superseded, mover unknown")
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.Status.ObservedRelease).To(Equal(second.Name))
			Expect(env.Status.History).To(HaveLen(1))
			entry := env.Status.History[0]
			Expect(entry.Release).To(Equal(releaseName))
			Expect(entry.Reason).To(Equal(kitchenv1alpha1.ReleaseMoveSuperseded))
			Expect(entry.By).To(BeEmpty())
			Expect(entry.From.Equal(&env.CreationTimestamp)).To(BeTrue(),
				"the first stint starts where the environment does")

			By("reconciling again without another entry appearing")
			reconcileOnce()
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.Status.History).To(HaveLen(1))
		})

		It("uses preview overlays and a preview hostname for preview environments", func() {
			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			env.Spec.Type = kitchenv1alpha1.EnvironmentPreview
			env.Spec.Preview = &kitchenv1alpha1.PreviewInfo{PullRequest: 42, Branch: "feat/checkout"}
			Expect(k8sClient.Update(ctx, env)).To(Succeed())

			reconcileOnce()
			reconcileOnce()

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, deploy)).To(Succeed())
			Expect(*deploy.Spec.Replicas).To(Equal(int32(1)), "previews always run a single replica")
			Expect(deploy.Spec.Template.Spec.Containers[0].Env).To(
				ContainElement(corev1.EnvVar{Name: "PUBLIC_API", Value: "https://api-staging.example.com"}))

			route := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, route)).To(Succeed())
			Expect(route.Spec.Hostnames).To(ConsistOf(gatewayv1.Hostname("envshop-pr-42.apps.example.com")))
		})

		It("routes a protected preview through the forward-auth gate", func() {
			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			env.Spec.Type = kitchenv1alpha1.EnvironmentPreview
			env.Spec.Preview = &kitchenv1alpha1.PreviewInfo{PullRequest: 42, Branch: "feat/checkout"}
			Expect(k8sClient.Update(ctx, env)).To(Succeed())

			reconcileOnce()
			reconcileOnce()

			By("sending the preview's traffic to the gate instead of the application")
			route := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, route)).To(Succeed())
			Expect(route.Spec.Hostnames).To(ConsistOf(gatewayv1.Hostname("envshop-pr-42.apps.example.com")))
			backend := route.Spec.Rules[0].BackendRefs[0]
			Expect(string(backend.Name)).To(Equal(PreviewGateName))
			Expect(string(*backend.Namespace)).To(Equal(PlatformNamespace))

			By("telling the gate which application the request belongs to")
			filters := route.Spec.Rules[0].Filters
			Expect(filters).To(HaveLen(1))
			Expect(filters[0].Type).To(Equal(gatewayv1.HTTPRouteFilterRequestHeaderModifier))
			Expect(filters[0].RequestHeaderModifier.Set).To(ConsistOf(
				gatewayv1.HTTPHeader{
					Name:  previewgate.UpstreamHeader,
					Value: envName + "." + appNS + ".svc.cluster.local:80",
				},
				gatewayv1.HTTPHeader{
					Name:  previewgate.ProjectHeader,
					Value: projectName,
				},
			), "and with Set, so a client cannot choose either of them")

			By("granting the project's namespace permission to route there")
			grant := &gatewayv1beta1.ReferenceGrant{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: referenceGrantName(appNS), Namespace: PlatformNamespace,
			}, grant)).To(Succeed())
			Expect(string(grant.Spec.From[0].Namespace)).To(Equal(appNS))
			Expect(string(grant.Spec.From[0].Kind)).To(Equal("HTTPRoute"))
			Expect(string(*grant.Spec.To[0].Name)).To(Equal(PreviewGateName))

			By("saying so on the Environment")
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(env.Status.Conditions, condPreviewProtected)).To(BeTrue())
			Expect(env.Status.URL).To(Equal("https://envshop-pr-42.apps.example.com"))
		})

		It("serves a preview openly when the project turns protection off", func() {
			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, project)).To(Succeed())
			project.Spec.Previews.Protected = ptr.To(false)
			Expect(k8sClient.Update(ctx, project)).To(Succeed())

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			env.Spec.Type = kitchenv1alpha1.EnvironmentPreview
			env.Spec.Preview = &kitchenv1alpha1.PreviewInfo{PullRequest: 42, Branch: "feat/checkout"}
			Expect(k8sClient.Update(ctx, env)).To(Succeed())

			reconcileOnce()
			reconcileOnce()

			route := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, route)).To(Succeed())
			Expect(string(route.Spec.Rules[0].BackendRefs[0].Name)).To(Equal(envName),
				"an unprotected preview goes straight at the application")
			Expect(route.Spec.Rules[0].Filters).To(BeEmpty())

			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			cond := meta.FindStatusCondition(env.Status.Conditions, condPreviewProtected)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("Public"))
		})

		It("publishes no route for a protected preview when the platform has no gate", func() {
			By("starting from a preview that is routed through the gate")
			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			env.Spec.Type = kitchenv1alpha1.EnvironmentPreview
			env.Spec.Preview = &kitchenv1alpha1.PreviewInfo{PullRequest: 42, Branch: "feat/checkout"}
			Expect(k8sClient.Update(ctx, env)).To(Succeed())
			reconcileOnce()
			reconcileOnce()
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS},
				&gatewayv1.HTTPRoute{})).To(Succeed())

			By("turning the identity provider off underneath it")
			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen)).To(Succeed())
			kitchen.Spec.Auth.Enabled = false
			Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

			reconcileOnce()

			By("withdrawing the URL rather than publishing an unprotected one")
			err := k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, &gatewayv1.HTTPRoute{})
			Expect(errors.IsNotFound(err)).To(BeTrue())

			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.Status.URL).To(BeEmpty())
			cond := meta.FindStatusCondition(env.Status.Conditions, condPreviewProtected)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal("PreviewGateUnavailable"))
			Expect(cond.Message).To(ContainSubstring("spec.previews.protected=false"),
				"the way out has to be in the message")
			Expect(meta.IsStatusConditionFalse(env.Status.Conditions, condReady)).To(BeTrue())

			By("leaving the workload alone: it is the URL that is withheld")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS},
				&appsv1.Deployment{})).To(Succeed())
		})

		It("never gates a production environment", func() {
			reconcileOnce()
			reconcileOnce()

			route := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, route)).To(Succeed())
			Expect(string(route.Spec.Rules[0].BackendRefs[0].Name)).To(Equal(envName))

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(meta.FindStatusCondition(env.Status.Conditions, condPreviewProtected)).To(BeNil())
		})

		It("cleans up children when the environment is deleted", func() {
			reconcileOnce()
			reconcileOnce()

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			reconcileOnce()

			err := k8sClient.Get(ctx, envKey, &kitchenv1alpha1.Environment{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "environment should be gone after finalization")

			err = k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, &appsv1.Deployment{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "deployment should be deleted")
			err = k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, &corev1.Service{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "service should be deleted")
			err = k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, &gatewayv1.HTTPRoute{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "httproute should be deleted")
		})
	})
})

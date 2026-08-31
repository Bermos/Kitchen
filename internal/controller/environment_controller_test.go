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
	"testing"

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
						Runtime: kitchenv1alpha1.RuntimeSpec{
							Port:     8080,
							Replicas: ptr.To(int32(2)),
							// Same artifact, different flags: what a preview
							// runs instead of production's arguments.
							Command:     []string{"./server"},
							Args:        []string{"--config=prod.toml"},
							PreviewArgs: []string{"--config=fake.toml"},
						},
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
			Expect(container.Args).To(Equal([]string{"--config=prod.toml"}), "production runs the release's arguments")
			// Nothing is Ready until the platform has asked. Absent a health
			// path the question is a TCP connect, never GET /.
			Expect(container.ReadinessProbe).NotTo(BeNil())
			Expect(container.ReadinessProbe.TCPSocket.Port.IntValue()).To(Equal(8080))
			Expect(container.StartupProbe.FailureThreshold).To(
				Equal(kitchenv1alpha1.DefaultStartupFailureThreshold), "startup gets the generous threshold")
			Expect(container.LivenessProbe).To(BeNil(), "a TCP connect cannot tell a wedge from a working pod")
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
			// The component label as well as the environment's: every worker
			// and every scheduled run carries the environment label too, and
			// a Service selector cannot say "and no process label".
			Expect(svc.Spec.Selector).To(Equal(map[string]string{
				labelEnvironment: envName, LabelComponent: ComponentWeb,
			}))
			Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue(LabelComponent, ComponentWeb),
				"the web pods carry what the Service selects on")

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
			Expect(deploy.Spec.Template.Spec.Containers[0].Args).To(
				Equal([]string{"--config=fake.toml"}), "a preview runs the preview arguments")
			Expect(deploy.Spec.Template.Spec.Containers[0].Command).To(
				Equal([]string{"./server"}), "the command is not overridden per environment")

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

		// A workload that must not run twice is deployed by stopping the old
		// copy first (#239). The rolling update comes back when the
		// declaration is withdrawn — a Deployment left on Recreate would
		// keep the outage the declaration paid for after the reason for it
		// had gone.
		It("recreates rather than rolls a singleton, and rolls again once it is not one", func() {
			release := &kitchenv1alpha1.Release{}
			releaseKey := types.NamespacedName{Name: releaseName, Namespace: namespace}
			Expect(k8sClient.Get(ctx, releaseKey, release)).To(Succeed())

			deployKey := types.NamespacedName{Name: envName, Namespace: appNS}
			deploy := &appsv1.Deployment{}

			reconcileOnce()
			Expect(k8sClient.Get(ctx, deployKey, deploy)).To(Succeed())
			Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))

			// The snapshot is immutable at admission, so the declaration
			// arrives the way a rollback's would: on another Release.
			singleton := release.DeepCopy()
			singleton.ObjectMeta = metav1.ObjectMeta{Name: releaseName + "-single", Namespace: namespace}
			singleton.Spec.ConfigSnapshot.Runtime.Singleton = true
			singleton.Spec.ConfigSnapshot.Runtime.Replicas = ptr.To(int32(1))
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, singleton))).To(Succeed())

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: singleton.Name}
			Expect(k8sClient.Update(ctx, env)).To(Succeed())

			reconcileOnce()
			Expect(k8sClient.Get(ctx, deployKey, deploy)).To(Succeed())
			Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))

			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: releaseName}
			Expect(k8sClient.Update(ctx, env)).To(Succeed())

			reconcileOnce()
			Expect(k8sClient.Get(ctx, deployKey, deploy)).To(Succeed())
			Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
		})

		// Refused at admission, not clamped: a replica count quietly lowered
		// reads back as a setting that did not take.
		It("refuses a project that declares a singleton and asks for three of it", func() {
			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, project)).
				To(Succeed())
			project.Spec.Runtime.Singleton = true
			project.Spec.Runtime.Replicas = ptr.To(int32(3))
			err := k8sClient.Update(ctx, project)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("singleton"))

			project.Spec.Runtime.Replicas = ptr.To(int32(1))
			Expect(k8sClient.Update(ctx, project)).To(Succeed())
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

// The probes an application container gets (#236). Before them the kubelet
// marked a pod Ready the moment its process started, so every rollout served
// from a container that had not finished coming up.
func TestContainerProbes(t *testing.T) {
	for name, tc := range map[string]struct {
		health *kitchenv1alpha1.HealthSpec
		port   int32

		wantNone     bool
		wantHTTP     string
		wantPort     int32
		wantLiveness bool
		wantPeriod   int32
		wantStartup  int32
	}{
		"no health check falls back to a TCP connect on the container port": {
			port:        3000,
			wantPort:    3000,
			wantPeriod:  kitchenv1alpha1.DefaultProbePeriodSeconds,
			wantStartup: kitchenv1alpha1.DefaultStartupFailureThreshold,
		},
		"a declared path is asked over HTTP, and buys a liveness probe": {
			health:       &kitchenv1alpha1.HealthSpec{Path: "/healthz"},
			port:         3000,
			wantHTTP:     "/healthz",
			wantPort:     3000,
			wantLiveness: true,
			wantPeriod:   kitchenv1alpha1.DefaultProbePeriodSeconds,
			wantStartup:  kitchenv1alpha1.DefaultStartupFailureThreshold,
		},
		"a health port overrides the container's": {
			health:      &kitchenv1alpha1.HealthSpec{Port: 9000},
			port:        3000,
			wantPort:    9000,
			wantPeriod:  kitchenv1alpha1.DefaultProbePeriodSeconds,
			wantStartup: kitchenv1alpha1.DefaultStartupFailureThreshold,
		},
		"declared timings are used as declared": {
			health: &kitchenv1alpha1.HealthSpec{
				Path: "/live", PeriodSeconds: 5, TimeoutSeconds: 1,
				FailureThreshold: 2, StartupFailureThreshold: 60,
			},
			port:         8080,
			wantHTTP:     "/live",
			wantPort:     8080,
			wantLiveness: true,
			wantPeriod:   5,
			wantStartup:  60,
		},
		"a workload with no port and no check is not probed at all": {
			wantNone: true,
		},
		"a worker that named its port is probed on it": {
			health:      &kitchenv1alpha1.HealthSpec{Port: 9000},
			wantPort:    9000,
			wantPeriod:  kitchenv1alpha1.DefaultProbePeriodSeconds,
			wantStartup: kitchenv1alpha1.DefaultStartupFailureThreshold,
		},
	} {
		t.Run(name, func(t *testing.T) {
			startup, readiness, liveness := containerProbes(tc.health, tc.port)
			if tc.wantNone {
				if startup != nil || readiness != nil || liveness != nil {
					t.Fatalf("want no probes at all, got %+v %+v %+v", startup, readiness, liveness)
				}
				return
			}
			if startup == nil || readiness == nil {
				t.Fatalf("want a startup and a readiness probe, got %+v %+v", startup, readiness)
			}
			if (liveness != nil) != tc.wantLiveness {
				t.Fatalf("want liveness=%v, got %+v", tc.wantLiveness, liveness)
			}
			// A startup probe that shared the liveness threshold would defeat
			// the whole point of having two.
			if startup.FailureThreshold != tc.wantStartup {
				t.Errorf("want startup threshold %d, got %d", tc.wantStartup, startup.FailureThreshold)
			}
			if readiness.PeriodSeconds != tc.wantPeriod {
				t.Errorf("want period %d, got %d", tc.wantPeriod, readiness.PeriodSeconds)
			}
			for _, probe := range []*corev1.Probe{startup, readiness} {
				switch {
				case tc.wantHTTP != "":
					if probe.HTTPGet == nil || probe.HTTPGet.Path != tc.wantHTTP {
						t.Fatalf("want an HTTP GET of %q, got %+v", tc.wantHTTP, probe.ProbeHandler)
					}
					if probe.HTTPGet.Port.IntVal != tc.wantPort {
						t.Errorf("want port %d, got %s", tc.wantPort, probe.HTTPGet.Port.String())
					}
				default:
					if probe.TCPSocket == nil {
						t.Fatalf("want a TCP connect, got %+v", probe.ProbeHandler)
					}
					if probe.TCPSocket.Port.IntVal != tc.wantPort {
						t.Errorf("want port %d, got %s", tc.wantPort, probe.TCPSocket.Port.String())
					}
					if probe.HTTPGet != nil {
						t.Error("a check with no path must never become GET /")
					}
				}
			}
		})
	}
}

// Which arguments an environment starts the application with (#237). A
// preview override is the sibling of an environment variable's preview value:
// same commit, same artifact, different flags.
func TestRuntimeArgsForAnEnvironmentType(t *testing.T) {
	for name, tc := range map[string]struct {
		runtime kitchenv1alpha1.RuntimeSpec
		envType kitchenv1alpha1.EnvironmentType
		want    []string
	}{
		"production runs the project's arguments": {
			runtime: kitchenv1alpha1.RuntimeSpec{Args: []string{"--config=prod.toml"}},
			envType: kitchenv1alpha1.EnvironmentProduction,
			want:    []string{"--config=prod.toml"},
		},
		"production ignores the preview override": {
			runtime: kitchenv1alpha1.RuntimeSpec{
				Args:        []string{"--config=prod.toml"},
				PreviewArgs: []string{"--config=fake.toml"},
			},
			envType: kitchenv1alpha1.EnvironmentProduction,
			want:    []string{"--config=prod.toml"},
		},
		"a preview takes the override where there is one": {
			runtime: kitchenv1alpha1.RuntimeSpec{
				Args:        []string{"--config=prod.toml"},
				PreviewArgs: []string{"--config=fake.toml"},
			},
			envType: kitchenv1alpha1.EnvironmentPreview,
			want:    []string{"--config=fake.toml"},
		},
		"a preview inherits production's where there is none": {
			runtime: kitchenv1alpha1.RuntimeSpec{Args: []string{"--config=prod.toml"}},
			envType: kitchenv1alpha1.EnvironmentPreview,
			want:    []string{"--config=prod.toml"},
		},
		// An empty override is no override, the same reading an empty
		// previewValue gets — which is what lets one be taken away through
		// an API that cannot tell an absent field from a cleared one.
		"an empty override is no override": {
			runtime: kitchenv1alpha1.RuntimeSpec{
				Args:        []string{"--config=prod.toml"},
				PreviewArgs: []string{},
			},
			envType: kitchenv1alpha1.EnvironmentPreview,
			want:    []string{"--config=prod.toml"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := tc.runtime.ArgsFor(tc.envType)
			if len(got) != len(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("want %v, got %v", tc.want, got)
				}
			}
		})
	}
}

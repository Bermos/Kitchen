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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/previewgate"
)

var _ = Describe("Scale to zero", func() {
	const (
		projectName = "idleshop"
		envName     = "idleshop-pr-7"
		releaseName = "idleshop-rel-000001"
		namespace   = "default"
		image       = "registry.example.com/kitchen/idleshop@sha256:0123456789abcdef"

		interceptorService = "keda-add-ons-http-interceptor-proxy"
	)

	ctx := context.Background()

	envKey := types.NamespacedName{Name: envName, Namespace: namespace}
	projectKey := types.NamespacedName{Name: projectName, Namespace: namespace}
	kitchenKey := types.NamespacedName{Name: KitchenSingletonName}
	childKey := types.NamespacedName{Name: envName, Namespace: "kitchen-" + projectName}
	appNS := "kitchen-" + projectName

	var reconciler *EnvironmentReconciler

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	// scaledObject reads the HTTPScaledObject the operator writes for the
	// environment, or nil when there is none.
	scaledObject := func() *unstructured.Unstructured {
		scaled := &unstructured.Unstructured{}
		scaled.SetGroupVersionKind(HTTPScaledObjectGVK())
		err := k8sClient.Get(ctx, childKey, scaled)
		if errors.IsNotFound(err) {
			return nil
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return scaled
	}

	route := func() *gatewayv1.HTTPRoute {
		route := &gatewayv1.HTTPRoute{}
		ExpectWithOffset(1, k8sClient.Get(ctx, childKey, route)).To(Succeed())
		return route
	}

	// setPlatform turns the platform switch on or off and lets the
	// environment reconcile against it.
	setPlatform := func(enabled bool) {
		kitchen := &kitchenv1alpha1.Kitchen{}
		ExpectWithOffset(1, k8sClient.Get(ctx, kitchenKey, kitchen)).To(Succeed())
		kitchen.Spec.ScaleToZero.Enabled = enabled
		ExpectWithOffset(1, k8sClient.Update(ctx, kitchen)).To(Succeed())
	}

	setPolicy := func(policy kitchenv1alpha1.ScaleToZeroPolicy) {
		project := &kitchenv1alpha1.Project{}
		ExpectWithOffset(1, k8sClient.Get(ctx, projectKey, project)).To(Succeed())
		project.Spec.ScaleToZero = policy
		ExpectWithOffset(1, k8sClient.Update(ctx, project)).To(Succeed())
	}

	BeforeEach(func() {
		reconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())

		// A platform that idles environments, as the chart writes it with
		// scaleToZero.enabled. No identity provider, so previews are served
		// openly and the route names the application directly — the gate has
		// its own test below.
		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				ScaleToZero: kitchenv1alpha1.ScaleToZeroSpec{
					Enabled: true,
					Interceptor: kitchenv1alpha1.InterceptorSpec{
						Service:   interceptorService,
						Namespace: PlatformNamespace,
						Port:      8080,
					},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/idleshop",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
				Previews: kitchenv1alpha1.PreviewsSpec{Protected: ptr.To(false)},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		release := &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "idleshop-bld-1"},
				Image:      image,
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8080, Replicas: ptr.To(int32(3))},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())

		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentPreview,
				Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 7, Branch: "feat/idle"},
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
	})

	AfterEach(func() {
		env := &kitchenv1alpha1.Environment{}
		if err := k8sClient.Get(ctx, envKey, env); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())
		}
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
			&gatewayv1beta1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{
				Name: interceptorGrantName(appNS), Namespace: PlatformNamespace,
			}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("parks an idle preview at zero and routes it through the interceptor", func() {
		reconcileOnce()
		reconcileOnce()

		By("writing the HTTPScaledObject the add-on scales it by")
		scaled := scaledObject()
		Expect(scaled).NotTo(BeNil())
		spec, _, err := unstructured.NestedMap(scaled.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(spec["hosts"]).To(ConsistOf("idleshop-pr-7.apps.example.com"),
			"the interceptor picks the application out of the Host header")
		Expect(spec["scaleTargetRef"]).To(Equal(map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"name":       envName,
			"service":    envName,
			"port":       int64(80),
		}))
		Expect(spec["replicas"]).To(Equal(map[string]any{"min": int64(0), "max": int64(5)}))
		Expect(spec["scaledownPeriod"]).To(Equal(int64(300)), "the default five idle minutes")

		By("sending the Gateway at the interceptor instead of the application")
		backend := route().Spec.Rules[0].BackendRefs[0]
		Expect(string(backend.Name)).To(Equal(interceptorService))
		Expect(string(*backend.Namespace)).To(Equal(PlatformNamespace))
		Expect(int32(*backend.Port)).To(Equal(int32(8080)))

		By("granting the application's namespace permission to route there")
		grant := &gatewayv1beta1.ReferenceGrant{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: interceptorGrantName(appNS), Namespace: PlatformNamespace,
		}, grant)).To(Succeed())
		Expect(string(grant.Spec.From[0].Namespace)).To(Equal(appNS))
		Expect(string(*grant.Spec.To[0].Name)).To(Equal(interceptorService))

		By("leaving the replica count to KEDA")
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, childKey, deploy)).To(Succeed())
		Expect(deploy.Spec.Replicas).To(HaveValue(Equal(int32(1))),
			"created at the API server's default, then scaled down by KEDA")

		By("saying so on the Environment")
		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		cond := meta.FindStatusCondition(env.Status.Conditions, condScaleToZero)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("IdlesToZero"))
	})

	It("never writes the replica count back while the environment idles", func() {
		reconcileOnce()
		reconcileOnce()

		By("letting KEDA park the workload")
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, childKey, deploy)).To(Succeed())
		deploy.Spec.Replicas = ptr.To(int32(0))
		Expect(k8sClient.Update(ctx, deploy)).To(Succeed())

		reconcileOnce()

		Expect(k8sClient.Get(ctx, childKey, deploy)).To(Succeed())
		Expect(deploy.Spec.Replicas).To(HaveValue(Equal(int32(0))),
			"reconciling a parked environment must not wake it up")
	})

	It("keeps production on its own replicas, and idles it when asked", func() {
		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		env.Spec.Type = kitchenv1alpha1.EnvironmentProduction
		env.Spec.Preview = nil
		Expect(k8sClient.Update(ctx, env)).To(Succeed())

		reconcileOnce()
		reconcileOnce()

		By("running the release's replicas behind a route that names the application")
		Expect(scaledObject()).To(BeNil())
		Expect(string(route().Spec.Rules[0].BackendRefs[0].Name)).To(Equal(envName))
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, childKey, deploy)).To(Succeed())
		Expect(deploy.Spec.Replicas).To(HaveValue(Equal(int32(3))))

		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		cond := meta.FindStatusCondition(env.Status.Conditions, condScaleToZero)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("AlwaysOn"))

		By("opting production in explicitly")
		setPolicy(kitchenv1alpha1.ScaleToZeroPolicy{Mode: kitchenv1alpha1.ScaleToZeroAlways})
		reconcileOnce()

		scaled := scaledObject()
		Expect(scaled).NotTo(BeNil())
		replicas, _, err := unstructured.NestedMap(scaled.Object, "spec", "replicas")
		Expect(err).NotTo(HaveOccurred())
		Expect(replicas["max"]).To(Equal(int64(5)))
		Expect(replicas["min"]).To(Equal(int64(0)))
	})

	It("never lets the ceiling fall below the replicas the environment runs", func() {
		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		env.Spec.Type = kitchenv1alpha1.EnvironmentProduction
		env.Spec.Preview = nil
		Expect(k8sClient.Update(ctx, env)).To(Succeed())
		setPolicy(kitchenv1alpha1.ScaleToZeroPolicy{
			Mode:        kitchenv1alpha1.ScaleToZeroAlways,
			MaxReplicas: ptr.To(int32(1)),
		})

		reconcileOnce()
		reconcileOnce()

		max, _, err := unstructured.NestedInt64(scaledObject().Object, "spec", "replicas", "max")
		Expect(err).NotTo(HaveOccurred())
		Expect(max).To(Equal(int64(3)), "the release runs three, so idling cannot cap it at one")
	})

	It("takes the idle window from the Project", func() {
		setPolicy(kitchenv1alpha1.ScaleToZeroPolicy{
			IdleAfter: &metav1.Duration{Duration: 90 * 1e9},
		})

		reconcileOnce()
		reconcileOnce()

		period, _, err := unstructured.NestedInt64(scaledObject().Object, "spec", "scaledownPeriod")
		Expect(err).NotTo(HaveOccurred())
		Expect(period).To(Equal(int64(90)))
	})

	It("points the preview gate at the interceptor rather than the application", func() {
		By("protecting the preview on a platform that has a gate")
		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
		project.Spec.Previews.Protected = ptr.To(true)
		Expect(k8sClient.Update(ctx, project)).To(Succeed())

		kitchen := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, kitchenKey, kitchen)).To(Succeed())
		kitchen.Spec.Auth = kitchenv1alpha1.AuthSpec{
			Enabled:     true,
			PreviewGate: kitchenv1alpha1.PreviewGateSpec{Enabled: true},
		}
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		reconcileOnce()
		reconcileOnce()

		By("still routing the Gateway at the gate")
		rule := route().Spec.Rules[0]
		Expect(string(rule.BackendRefs[0].Name)).To(Equal(PreviewGateName))

		By("but telling it to forward to the interceptor, which would otherwise " +
			"reach a Service with no endpoints")
		Expect(rule.Filters[0].RequestHeaderModifier.Set).To(ConsistOf(gatewayv1.HTTPHeader{
			Name:  previewgate.UpstreamHeader,
			Value: interceptorService + "." + PlatformNamespace + ".svc.cluster.local:8080",
		}))
		Expect(scaledObject()).NotTo(BeNil())
	})

	It("returns environments to plain routing when the platform switch goes off", func() {
		reconcileOnce()
		reconcileOnce()
		Expect(scaledObject()).NotTo(BeNil())

		By("turning scale-to-zero off underneath a parked environment")
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, childKey, deploy)).To(Succeed())
		deploy.Spec.Replicas = ptr.To(int32(0))
		Expect(k8sClient.Update(ctx, deploy)).To(Succeed())
		setPlatform(false)

		reconcileOnce()

		By("leaving no scaled object behind")
		Expect(scaledObject()).To(BeNil())

		By("naming the application on the route again")
		backend := route().Spec.Rules[0].BackendRefs[0]
		Expect(string(backend.Name)).To(Equal(envName))
		Expect(backend.Namespace).To(BeNil())

		By("and starting the pods the environment is meant to have")
		Expect(k8sClient.Get(ctx, childKey, deploy)).To(Succeed())
		Expect(deploy.Spec.Replicas).To(HaveValue(Equal(int32(1))), "a preview runs one")

		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		Expect(meta.FindStatusCondition(env.Status.Conditions, condScaleToZero)).To(BeNil(),
			"a platform that idles nothing says nothing on every environment it has")
	})

	It("takes the scaled object with it when the environment is deleted", func() {
		reconcileOnce()
		reconcileOnce()
		Expect(scaledObject()).NotTo(BeNil())

		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		Expect(k8sClient.Delete(ctx, env)).To(Succeed())
		reconcileOnce()

		Expect(scaledObject()).To(BeNil())
	})
})

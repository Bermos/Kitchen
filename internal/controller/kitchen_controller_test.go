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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

var _ = Describe("Kitchen Controller", func() {
	Context("When reconciling the singleton", func() {
		ctx := context.Background()

		singletonKey := types.NamespacedName{Name: KitchenSingletonName}
		gatewayKey := types.NamespacedName{Name: SharedGatewayName, Namespace: PlatformNamespace}
		cloudflaredKey := types.NamespacedName{Name: "kitchen-cloudflared", Namespace: PlatformNamespace}

		var reconciler *KitchenReconciler

		reconcileOnce := func(name string) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		BeforeEach(func() {
			reconciler = &KitchenReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			kitchen := &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
				Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com"},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())
		})

		AfterEach(func() {
			for _, obj := range []client.Object{
				&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: SharedGatewayName, Namespace: PlatformNamespace}},
				&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "kitchen-cloudflared", Namespace: PlatformNamespace}},
				&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
				&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: "other"}},
			} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
			// The kitchen-system namespace is intentionally NOT deleted: envtest
			// namespaces never finish terminating and would poison later specs.
		})

		It("creates the shared gateway with wildcard listeners", func() {
			reconcileOnce(KitchenSingletonName)

			gw := &gatewayv1.Gateway{}
			Expect(k8sClient.Get(ctx, gatewayKey, gw)).To(Succeed())
			Expect(string(gw.Spec.GatewayClassName)).To(Equal("cilium"))

			// Default TLS mode is acme: expect both listeners.
			Expect(gw.Spec.Listeners).To(HaveLen(2))
			Expect(string(*gw.Spec.Listeners[0].Hostname)).To(Equal("*.apps.example.com"))
			Expect(gw.Spec.Listeners[0].Protocol).To(Equal(gatewayv1.HTTPProtocolType))
			https := gw.Spec.Listeners[1]
			Expect(https.Protocol).To(Equal(gatewayv1.HTTPSProtocolType))
			Expect(https.TLS).NotTo(BeNil())
			Expect(string(https.TLS.CertificateRefs[0].Name)).To(Equal(WildcardTLSSecretName))

			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(kitchen.Status.Conditions, condReady)).To(BeTrue())
			Expect(meta.IsStatusConditionFalse(kitchen.Status.Conditions, condGatewayProgrammed)).To(BeTrue(),
				"no gateway controller runs in envtest, so the gateway must report unprogrammed")
		})

		It("deploys cloudflared when enabled and removes it when disabled", func() {
			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			kitchen.Spec.TLS.Mode = kitchenv1alpha1.TLSModeCloudflared
			kitchen.Spec.Ingress.Cloudflared = kitchenv1alpha1.CloudflaredSpec{
				Enabled:         true,
				TunnelSecretRef: &kitchenv1alpha1.LocalObjectReference{Name: "cloudflared-creds"},
			}
			Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

			reconcileOnce(KitchenSingletonName)

			By("checking the tunnel deployment")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, cloudflaredKey, deploy)).To(Succeed())
			container := deploy.Spec.Template.Spec.Containers[0]
			Expect(container.Image).To(Equal(CloudflaredImage))
			Expect(container.Env[0].Name).To(Equal("TUNNEL_TOKEN"))
			Expect(container.Env[0].ValueFrom.SecretKeyRef.Name).To(Equal("cloudflared-creds"))
			Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))

			By("checking the gateway has no https listener in cloudflared mode")
			gw := &gatewayv1.Gateway{}
			Expect(k8sClient.Get(ctx, gatewayKey, gw)).To(Succeed())
			Expect(gw.Spec.Listeners).To(HaveLen(1))
			Expect(gw.Spec.Listeners[0].Protocol).To(Equal(gatewayv1.HTTPProtocolType))

			By("disabling the tunnel")
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			kitchen.Spec.Ingress.Cloudflared.Enabled = false
			Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

			reconcileOnce(KitchenSingletonName)

			err := k8sClient.Get(ctx, cloudflaredKey, &appsv1.Deployment{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "cloudflared deployment should be removed")
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			Expect(meta.FindStatusCondition(kitchen.Status.Conditions, condTunnelConnected)).To(BeNil(),
				"tunnel condition should be dropped when disabled")
		})

		It("requires the tunnel secret when cloudflared is enabled", func() {
			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			kitchen.Spec.Ingress.Cloudflared = kitchenv1alpha1.CloudflaredSpec{Enabled: true}
			Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

			reconcileOnce(KitchenSingletonName)

			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			cond := meta.FindStatusCondition(kitchen.Status.Conditions, condTunnelConnected)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TunnelSecretMissing"))
		})

		It("refuses to reconcile a second Kitchen object", func() {
			other := &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: "other"},
				Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "elsewhere.example.com"},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, other))).To(Succeed())

			reconcileOnce("other")

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "other"}, other)).To(Succeed())
			cond := meta.FindStatusCondition(other.Status.Conditions, condReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NotTheSingleton"))
		})
	})
})

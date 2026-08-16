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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// telemetrySecretName mirrors what the chart writes for the release named
// "kitchen".
const telemetrySecretName = "kitchen-clickhouse"

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
				Spec: kitchenv1alpha1.KitchenSpec{
					BaseDomain: "apps.example.com",
					TLS:        acmeTLS(),
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())
		})

		AfterEach(func() {
			for _, obj := range []client.Object{
				&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: SharedGatewayName, Namespace: PlatformNamespace}},
				// Every reconcile here creates these, the singleton being in
				// acme mode. The ClusterIssuers are cluster-scoped, so leaving
				// them behind would leak into whatever runs next.
				acmeIssuerObject(),
				http01IssuerObject(),
				wildcardCertificateObject(),
				&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "kitchen-cloudflared", Namespace: PlatformNamespace}},
				&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
				&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: "other"}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: telemetrySecretName, Namespace: PlatformNamespace}},
			} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
			// The kitchen-system namespace is intentionally NOT deleted: envtest
			// namespaces never finish terminating and would poison later specs.
		})

		It("creates the shared gateway with its two listeners", func() {
			reconcileOnce(KitchenSingletonName)

			gw := &gatewayv1.Gateway{}
			Expect(k8sClient.Get(ctx, gatewayKey, gw)).To(Succeed())
			Expect(string(gw.Spec.GatewayClassName)).To(Equal("cilium"))

			// Default TLS mode is acme: expect both listeners.
			Expect(gw.Spec.Listeners).To(HaveLen(2))
			Expect(gw.Spec.Listeners[0].Hostname).To(BeNil(),
				"port 80 is deliberately catch-all: custom domains and their ACME HTTP-01 challenges "+
					"arrive there with hostnames outside the base domain")
			Expect(gw.Spec.Listeners[0].Protocol).To(Equal(gatewayv1.HTTPProtocolType))
			https := gw.Spec.Listeners[1]
			Expect(https.Protocol).To(Equal(gatewayv1.HTTPSProtocolType))
			Expect(string(*https.Hostname)).To(Equal("*.apps.example.com"))
			Expect(https.TLS).NotTo(BeNil())
			Expect(string(https.TLS.CertificateRefs[0].Name)).To(Equal(WildcardTLSSecretName))

			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(kitchen.Status.Conditions, condReady)).To(BeTrue())
			Expect(meta.IsStatusConditionFalse(kitchen.Status.Conditions, condGatewayProgrammed)).To(BeTrue(),
				"no gateway controller runs in envtest, so the gateway must report unprogrammed")
		})

		It("keeps port 80 for the redirect while edge TLS is on", func() {
			reconcileOnce(KitchenSingletonName)

			By("publishing a redirect bound to the http listener alone")
			redirectKey := types.NamespacedName{Name: httpsRedirectRouteName, Namespace: PlatformNamespace}
			route := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, redirectKey, route)).To(Succeed())

			parent := route.Spec.ParentRefs[0]
			Expect(string(parent.Name)).To(Equal(SharedGatewayName))
			Expect(parent.SectionName).NotTo(BeNil(),
				"without a section the redirect would also bind to https and loop")
			Expect(string(*parent.SectionName)).To(Equal(gatewayListenerHTTP))

			filter := route.Spec.Rules[0].Filters[0]
			Expect(filter.Type).To(Equal(gatewayv1.HTTPRouteFilterRequestRedirect))
			Expect(*filter.RequestRedirect.Scheme).To(Equal("https"))
			Expect(*filter.RequestRedirect.StatusCode).To(Equal(301))

			By("sending everything that actually serves to the https listener")
			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			Expect(string(*gatewaySection(kitchen))).To(Equal(gatewayListenerHTTPS))

			By("removing the redirect when there is no https listener to send anyone to")
			kitchen.Spec.TLS.Mode = kitchenv1alpha1.TLSModeNone
			Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

			reconcileOnce(KitchenSingletonName)

			err := k8sClient.Get(ctx, redirectKey, &gatewayv1.HTTPRoute{})
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"port 80 is where the platform answers without edge TLS, so redirecting it would loop")
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			Expect(string(*gatewaySection(kitchen))).To(Equal(gatewayListenerHTTP))
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

		It("applies the telemetry schema with the configured retention", func() {
			By("standing in for ClickHouse's HTTP interface")
			var statements []string
			store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				statements = append(statements, string(body))
				// No table exists yet, so system.tables answers nothing.
			}))
			defer store.Close()
			endpoint, err := url.Parse(store.URL)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: telemetrySecretName, Namespace: PlatformNamespace},
				StringData: map[string]string{
					"host":     endpoint.Hostname(),
					"httpPort": endpoint.Port(),
					"database": "kitchen",
					"username": "kitchen",
					"password": "hunter2",
				},
			})).To(Succeed())

			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			kitchen.Spec.Observability.ClickHouse = kitchenv1alpha1.ClickHouseSpec{
				RetentionDays: 7,
				SecretRef:     &kitchenv1alpha1.LocalObjectReference{Name: telemetrySecretName},
			}
			Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

			reconcileOnce(KitchenSingletonName)

			Expect(strings.Join(statements, "\n")).To(ContainSubstring("CREATE TABLE IF NOT EXISTS `kitchen`.`logs`"))
			Expect(strings.Join(statements, "\n")).To(ContainSubstring("toIntervalDay(7)"))

			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			cond := meta.FindStatusCondition(kitchen.Status.Conditions, condTelemetrySchema)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Message).To(ContainSubstring("7 days"))
		})

		It("reports an unreachable telemetry store without failing the reconcile", func() {
			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			kitchen.Spec.Observability.ClickHouse.SecretRef = &kitchenv1alpha1.LocalObjectReference{
				Name: telemetrySecretName,
			}
			Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

			// The secret the chart writes is not there — the reconcile still
			// has to program the gateway.
			reconcileOnce(KitchenSingletonName)

			Expect(k8sClient.Get(ctx, gatewayKey, &gatewayv1.Gateway{})).To(Succeed())
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			cond := meta.FindStatusCondition(kitchen.Status.Conditions, condTelemetrySchema)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ConnectionSecretMissing"))
		})

		It("says nothing about telemetry when there is no store", func() {
			reconcileOnce(KitchenSingletonName)

			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			Expect(meta.FindStatusCondition(kitchen.Status.Conditions, condTelemetrySchema)).To(BeNil())
		})

		It("switches sampling and traces on for a Kitchen that predates them", func() {
			// The upgrade case, and the one that would fail silently. The chart
			// applies the singleton as a post-install hook and does not
			// re-apply it on upgrade, so an installation that predates these
			// fields has an observability block in etcd with neither key in it.
			// Structural defaulting only descends into objects that are
			// present, which is why both carry an empty-object default of their
			// own — without it the platform would read `false` for both and
			// quietly collect nothing after the upgrade that taught it to.
			//
			// It is written as unstructured JSON on purpose: the Go type always
			// marshals `enabled`, so a typed create would send an explicit
			// `false` and test nothing at all.
			legacy := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": kitchenv1alpha1.GroupVersion.String(),
				"kind":       "Kitchen",
				"metadata":   map[string]any{"name": "legacy"},
				"spec": map[string]any{
					"baseDomain": "old.example.com",
					"tls":        map[string]any{"mode": "none"},
					"observability": map[string]any{
						"clickhouse": map[string]any{"retentionDays": int64(30)},
					},
				},
			}}
			legacy.SetGroupVersionKind(kitchenv1alpha1.GroupVersion.WithKind("Kitchen"))
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, legacy))).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, legacy))).To(Succeed())
			})

			stored := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "legacy"}, stored)).To(Succeed())

			Expect(stored.Spec.Observability.Metrics.Enabled).To(BeTrue())
			Expect(stored.Spec.Observability.Metrics.IntervalSeconds).To(Equal(int32(30)))
			Expect(stored.Spec.Observability.Traces.Enabled).To(BeTrue())
			// And an endpoint to hand to applications, rather than the empty
			// string the reconciler reads as "do not tell them anything".
			Expect(stored.Spec.Observability.Traces.Endpoint).NotTo(BeEmpty())
		})

		It("refuses to reconcile a second Kitchen object", func() {
			other := &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: "other"},
				Spec: kitchenv1alpha1.KitchenSpec{
					BaseDomain: "elsewhere.example.com",
					TLS:        acmeTLS(),
				},
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

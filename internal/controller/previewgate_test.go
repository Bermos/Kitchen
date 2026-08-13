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
	"encoding/json"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/idp"
	"github.com/Bermos/Kitchen/internal/previewgate"
)

// authSecretName mirrors what the chart writes for the release named
// "kitchen".
const authSecretName = "kitchen-auth"

// fakeIssuer serves just enough OpenID Connect for the operator to register a
// client: discovery, and a registration endpoint that only answers with a
// credential.
type fakeIssuer struct {
	*httptest.Server
	registrations int
	lastRedirects []string
}

func newFakeIssuer() *fakeIssuer {
	issuer := &fakeIssuer{}
	mux := http.NewServeMux()
	issuer.Server = httptest.NewServer(mux)

	mux.HandleFunc(idp.DiscoveryPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 issuer.URL,
			"authorization_endpoint": issuer.URL + "/oauth2/authorize",
			"token_endpoint":         issuer.URL + "/oauth2/token",
			"registration_endpoint":  issuer.URL + "/oauth2/register",
			"jwks_uri":               issuer.URL + "/jwks",
		})
	})
	mux.HandleFunc("/oauth2/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		issuer.registrations++
		issuer.lastRedirects = body.RedirectURIs
		_ = json.NewEncoder(w).Encode(map[string]string{
			"client_id":     "gate-client",
			"client_secret": "gate-secret",
		})
	})
	return issuer
}

var _ = Describe("Preview gate", func() {
	Context("When the platform runs one", func() {
		ctx := context.Background()

		singletonKey := types.NamespacedName{Name: KitchenSingletonName}
		gateKey := types.NamespacedName{Name: PreviewGateName, Namespace: PlatformNamespace}
		clientSecretKey := types.NamespacedName{Name: PreviewGateClientSecretName, Namespace: PlatformNamespace}

		var reconciler *KitchenReconciler
		var issuer *fakeIssuer

		reconcileOnce := func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: singletonKey})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		BeforeEach(func() {
			issuer = newFakeIssuer()
			reconciler = &KitchenReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				PreviewGateImage: "ghcr.io/bermos/kitchen:test",
			}

			// The reconciler creates the platform namespace, but the identity
			// provider's secret has to be there before the first reconcile —
			// the chart writes it, and the chart got there first.
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
			}))).To(Succeed())

			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
				Spec: kitchenv1alpha1.KitchenSpec{
					BaseDomain: "apps.example.com",
					Auth: kitchenv1alpha1.AuthSpec{
						Enabled:     true,
						PreviewGate: kitchenv1alpha1.PreviewGateSpec{Enabled: true},
					},
				},
			}))).To(Succeed())

			// The identity provider's details, as the chart writes them.
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: authSecretName, Namespace: PlatformNamespace},
				StringData: map[string]string{
					idp.SecretKeyIssuer:     issuer.URL,
					idp.SecretKeyServiceKey: "the-service-key",
				},
			}))).To(Succeed())

			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			kitchen.Spec.Auth.SecretRef = &kitchenv1alpha1.LocalObjectReference{Name: authSecretName}
			Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())
		})

		AfterEach(func() {
			issuer.Close()
			for _, obj := range []client.Object{
				&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: PreviewGateName, Namespace: PlatformNamespace}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: PreviewGateName, Namespace: PlatformNamespace}},
				&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: PreviewGateName, Namespace: PlatformNamespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: PreviewGateSecretName, Namespace: PlatformNamespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: PreviewGateClientSecretName, Namespace: PlatformNamespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: authSecretName, Namespace: PlatformNamespace}},
				&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: SharedGatewayName, Namespace: PlatformNamespace}},
				&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
			} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		It("registers an OAuth client and deploys the gate", func() {
			reconcileOnce()

			By("registering exactly one client, for the gate's own callback")
			Expect(issuer.registrations).To(Equal(1))
			Expect(issuer.lastRedirects).To(ConsistOf(
				"https://previews.apps.example.com" + previewgate.CallbackPath))

			By("keeping the credentials where the gate can read them")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, clientSecretKey, secret)).To(Succeed())
			Expect(string(secret.Data[gateSecretKeyClientID])).To(Equal("gate-client"))
			Expect(string(secret.Data[gateSecretKeyClientSecret])).To(Equal("gate-secret"))
			Expect(string(secret.Data[gateSecretKeyIssuer])).To(Equal(issuer.URL))

			By("generating a signing key for sessions")
			signing := &corev1.Secret{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: PreviewGateSecretName, Namespace: PlatformNamespace}, signing)).To(Succeed())
			Expect(string(signing.Data[gateSecretKeyCookie])).NotTo(BeEmpty())

			By("running the gate from the operator's own image")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, gateKey, deploy)).To(Succeed())
			container := deploy.Spec.Template.Spec.Containers[0]
			Expect(container.Image).To(Equal("ghcr.io/bermos/kitchen:test"))
			Expect(container.Command).To(ConsistOf("/gate"))
			Expect(container.Env).To(ContainElement(corev1.EnvVar{
				Name: "KITCHEN_GATE_COOKIE_SECURE", Value: "true"}))
			Expect(container.Env).To(ContainElement(corev1.EnvVar{
				Name: "KITCHEN_GATE_SESSION_TTL", Value: "8h0m0s"}))

			By("publishing the gate's own hostname")
			route := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, gateKey, route)).To(Succeed())
			Expect(route.Spec.Hostnames).To(ConsistOf(gatewayv1.Hostname("previews.apps.example.com")))
			Expect(string(route.Spec.Rules[0].BackendRefs[0].Name)).To(Equal(PreviewGateName))

			By("reporting that the gate is not available yet")
			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			cond := meta.FindStatusCondition(kitchen.Status.Conditions, condPreviewGateReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("GatePending"), "no deployment goes available in envtest")
		})

		It("registers the client once and not again on every reconcile", func() {
			reconcileOnce()
			reconcileOnce()
			reconcileOnce()
			Expect(issuer.registrations).To(Equal(1),
				"the stored credentials are the ones that matter: the issuer only hands them out once")
		})

		It("registers again when the callback moves", func() {
			reconcileOnce()

			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			kitchen.Spec.Auth.PreviewGate.Host = "gate.example.com"
			Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

			reconcileOnce()
			Expect(issuer.registrations).To(Equal(2))
			Expect(issuer.lastRedirects).To(ConsistOf("https://gate.example.com" + previewgate.CallbackPath))
		})

		It("reports an identity provider it cannot register with", func() {
			issuer.Close()
			reconcileOnce()

			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			cond := meta.FindStatusCondition(kitchen.Status.Conditions, condPreviewGateReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ClientNotRegistered"))
			Expect(k8sClient.Get(ctx, gateKey, &appsv1.Deployment{})).NotTo(Succeed())
		})

		It("takes the gate down when the platform stops wanting one", func() {
			reconcileOnce()
			Expect(k8sClient.Get(ctx, gateKey, &appsv1.Deployment{})).To(Succeed())

			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			kitchen.Spec.Auth.PreviewGate.Enabled = false
			Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

			reconcileOnce()

			Expect(errors.IsNotFound(k8sClient.Get(ctx, gateKey, &appsv1.Deployment{}))).To(BeTrue())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, gateKey, &corev1.Service{}))).To(BeTrue())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, gateKey, &gatewayv1.HTTPRoute{}))).To(BeTrue())

			By("keeping the credentials, which a gate turned back on would need")
			Expect(k8sClient.Get(ctx, clientSecretKey, &corev1.Secret{})).To(Succeed())

			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			Expect(meta.FindStatusCondition(kitchen.Status.Conditions, condPreviewGateReady)).To(BeNil())
		})
	})
})

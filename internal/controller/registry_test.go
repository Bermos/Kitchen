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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider"
)

// registryChartSecretName mirrors what the chart writes for the release named
// "kitchen".
const registryChartSecretName = "kitchen-registry"

var _ = Describe("The bundled registry", func() {
	ctx := context.Background()

	singletonKey := types.NamespacedName{Name: KitchenSingletonName}
	routeKey := types.NamespacedName{Name: RegistryRouteName, Namespace: PlatformNamespace}
	connectionKey := types.NamespacedName{Name: RegistryConnectionName, Namespace: PlatformNamespace}
	credentialKey := types.NamespacedName{Name: RegistryCredentialsSecretName, Namespace: PlatformNamespace}

	var reconciler *KitchenReconciler

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: singletonKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	singleton := func() *kitchenv1alpha1.Kitchen {
		kitchen := &kitchenv1alpha1.Kitchen{}
		ExpectWithOffset(1, k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
		return kitchen
	}

	// registrySpec is what the chart writes into the Kitchen singleton.
	registrySpec := func() kitchenv1alpha1.ImageRegistrySpec {
		return kitchenv1alpha1.ImageRegistrySpec{
			Enabled:   true,
			Service:   "kitchen-registry",
			Port:      5000,
			SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: registryChartSecretName},
		}
	}

	// Resolved the way the build reconciler resolves it, so that a seeded
	// connection is asserted to be one a build could actually push through
	// rather than merely one that stores the right string.
	connectionURL := func(conn *kitchenv1alpha1.Connection) string {
		target, err := provider.Registry(conn)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return target.Prefix
	}

	BeforeEach(func() {
		reconciler = &KitchenReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		// The reconciler creates the platform namespace, but the chart's
		// registry credential has to be there before the first reconcile —
		// so the namespace has to exist before this spec writes it, whether
		// or not another one got there first.
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: registryChartSecretName, Namespace: PlatformNamespace},
			StringData: map[string]string{"username": "kitchen", "password": "hunter2"},
		}))).To(Succeed())

		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Registry:   registrySpec(),
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())
	})

	AfterEach(func() {
		for _, obj := range []client.Object{
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: RegistryRouteName, Namespace: PlatformNamespace}},
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: RegistryConnectionName, Namespace: PlatformNamespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: RegistryCredentialsSecretName, Namespace: PlatformNamespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: registryChartSecretName, Namespace: PlatformNamespace}},
			&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: SharedGatewayName, Namespace: PlatformNamespace}},
			acmeIssuerObject(),
			http01IssuerObject(),
			wildcardCertificateObject(),
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("publishes the registry and seeds a connection pointing at it", func() {
		reconcileOnce()

		By("routing registry.<baseDomain> at the registry service")
		route := &gatewayv1.HTTPRoute{}
		Expect(k8sClient.Get(ctx, routeKey, route)).To(Succeed())
		Expect(route.Spec.Hostnames).To(ConsistOf(gatewayv1.Hostname("registry.apps.example.com")))
		Expect(string(*route.Spec.ParentRefs[0].SectionName)).To(Equal(gatewayListenerHTTPS),
			"in acme mode port 80 carries only the redirect, so a route bound to both would answer over cleartext")
		backend := route.Spec.Rules[0].BackendRefs[0]
		Expect(string(backend.Name)).To(Equal("kitchen-registry"))
		Expect(int32(*backend.Port)).To(Equal(int32(5000)))
		// The rule names no match, and the API server defaults that to the
		// catch-all: a registry's API is the whole of its host.
		Expect(route.Spec.Rules[0].Matches).To(HaveLen(1))
		Expect(*route.Spec.Rules[0].Matches[0].Path.Value).To(Equal("/"))

		By("seeding a dockerRegistry connection a project can pick")
		conn := &kitchenv1alpha1.Connection{}
		Expect(k8sClient.Get(ctx, connectionKey, conn)).To(Succeed())
		Expect(conn.Spec.Provider).To(Equal("dockerRegistry"))
		Expect(connectionURL(conn)).To(Equal("registry.apps.example.com"))
		Expect(conn.Labels).To(HaveKeyWithValue(labelManagedByKey, labelManagedByValue))

		By("writing the credential in the shape every registry consumer reads")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, credentialKey, secret)).To(Succeed())
		Expect(secret.Type).To(Equal(corev1.SecretTypeDockerConfigJson))
		var cfg struct {
			Auths map[string]struct {
				Username string `json:"username"`
				Password string `json:"password"`
			} `json:"auths"`
		}
		Expect(json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &cfg)).To(Succeed())
		Expect(cfg.Auths).To(HaveKey("registry.apps.example.com"))
		Expect(cfg.Auths["registry.apps.example.com"].Username).To(Equal("kitchen"))
		Expect(cfg.Auths["registry.apps.example.com"].Password).To(Equal("hunter2"))
		Expect(secret.Labels).To(HaveKeyWithValue(labelManagedByKey, labelManagedByValue),
			"deleting the connection from the connections page has to take its credential with it")

		By("recording what was seeded, so it is seeded once")
		kitchen := singleton()
		Expect(kitchen.Status.Registry).NotTo(BeNil())
		Expect(kitchen.Status.Registry.Host).To(Equal("registry.apps.example.com"))
		Expect(kitchen.Status.Registry.Connection).To(Equal(RegistryConnectionName))
		Expect(meta.IsStatusConditionTrue(kitchen.Status.Conditions, condRegistryReady)).To(BeTrue())
	})

	It("leaves a seeded connection deleted rather than reinstating it", func() {
		reconcileOnce()

		conn := &kitchenv1alpha1.Connection{}
		Expect(k8sClient.Get(ctx, connectionKey, conn)).To(Succeed())
		Expect(k8sClient.Delete(ctx, conn)).To(Succeed())

		reconcileOnce()

		err := k8sClient.Get(ctx, connectionKey, conn)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"an installation that wants only its own connections has to be able to end up with only its own")

		By("still publishing the registry, and saying the seed is gone")
		Expect(k8sClient.Get(ctx, routeKey, &gatewayv1.HTTPRoute{})).To(Succeed())
		kitchen := singleton()
		Expect(meta.IsStatusConditionTrue(kitchen.Status.Conditions, condRegistryReady)).To(BeTrue())
		Expect(kitchen.Status.Registry.Connection).To(BeEmpty())
	})

	It("keeps a seeded connection pointing at the registry when the base domain moves", func() {
		reconcileOnce()

		kitchen := singleton()
		kitchen.Spec.BaseDomain = "apps.example.net"
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		reconcileOnce()

		conn := &kitchenv1alpha1.Connection{}
		Expect(k8sClient.Get(ctx, connectionKey, conn)).To(Succeed())
		Expect(connectionURL(conn)).To(Equal("registry.apps.example.net"))

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, credentialKey, secret)).To(Succeed())
		Expect(string(secret.Data[corev1.DockerConfigJsonKey])).To(ContainSubstring("registry.apps.example.net"))
	})

	It("refuses to overwrite a connection of the same name it did not create", func() {
		Expect(k8sClient.Create(ctx, &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: RegistryConnectionName, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "dockerRegistry",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "someone-elses-secret"},
				Config:               &runtime.RawExtension{Raw: []byte(`{"url":"harbor.example.com/kitchen"}`)},
			},
		})).To(Succeed())

		reconcileOnce()

		conn := &kitchenv1alpha1.Connection{}
		Expect(k8sClient.Get(ctx, connectionKey, conn)).To(Succeed())
		Expect(connectionURL(conn)).To(Equal("harbor.example.com/kitchen"))
		Expect(conn.Spec.CredentialsSecretRef.Name).To(Equal("someone-elses-secret"))

		kitchen := singleton()
		Expect(meta.IsStatusConditionFalse(kitchen.Status.Conditions, condRegistryReady)).To(BeTrue())
		Expect(meta.FindStatusCondition(kitchen.Status.Conditions, condRegistryReady).Reason).
			To(Equal("ConnectionFailed"))
	})

	It("publishes nothing in TLS mode none, and says why", func() {
		kitchen := singleton()
		kitchen.Spec.TLS = kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeNone}
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		reconcileOnce()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, routeKey, &gatewayv1.HTTPRoute{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, connectionKey, &kitchenv1alpha1.Connection{}))).To(BeTrue())

		condition := meta.FindStatusCondition(singleton().Status.Conditions, condRegistryReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal("TLSModeNone"))
		Expect(condition.Message).To(ContainSubstring("container runtime"))
	})

	It("takes the route and the seeded connection down when it is switched off", func() {
		reconcileOnce()
		Expect(k8sClient.Get(ctx, connectionKey, &kitchenv1alpha1.Connection{})).To(Succeed())

		kitchen := singleton()
		kitchen.Spec.Registry.Enabled = false
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		reconcileOnce()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, routeKey, &gatewayv1.HTTPRoute{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, connectionKey, &kitchenv1alpha1.Connection{}))).To(BeTrue(),
			"a connection naming a registry nothing serves is a picker entry that fails every build chosen with it")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, credentialKey, &corev1.Secret{}))).To(BeTrue())

		kitchen = singleton()
		Expect(kitchen.Status.Registry).To(BeNil())
		Expect(meta.FindStatusCondition(kitchen.Status.Conditions, condRegistryReady)).To(BeNil())
	})

	It("waits for the credential the chart generates", func() {
		Expect(k8sClient.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: registryChartSecretName, Namespace: PlatformNamespace},
		})).To(Succeed())

		reconcileOnce()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, connectionKey, &kitchenv1alpha1.Connection{}))).To(BeTrue())
		condition := meta.FindStatusCondition(singleton().Status.Conditions, condRegistryReady)
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal("CredentialUnavailable"))
	})
})

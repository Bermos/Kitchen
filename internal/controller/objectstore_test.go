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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

// What the chart writes for the release named "kitchen": the Secret holding
// the store's root credential, the Service every bucket is reached at, and
// the name that Service's certificate is issued for. Beside them, the address
// the platform publishes the store at for a base domain of
// apps.example.com — the one a presigned URL is signed against (#601).
const (
	objectStoreChartSecretName = "kitchen-objectstore"
	objectStoreServiceName     = "kitchen-objectstore"
	objectStoreServiceHost     = "kitchen-objectstore.kitchen-system.svc"
	objectStorePublicHostname  = "objectstore.apps.example.com"
	objectStorePublicEndpoint  = "https://" + objectStorePublicHostname
)

var _ = Describe("The bundled object store", func() {
	ctx := context.Background()

	singletonKey := types.NamespacedName{Name: KitchenSingletonName}
	connectionKey := types.NamespacedName{Name: ObjectStoreConnectionName, Namespace: PlatformNamespace}
	credentialKey := types.NamespacedName{Name: ObjectStoreCredentialsSecretName, Namespace: PlatformNamespace}

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

	routeKey := types.NamespacedName{Name: ObjectStoreRouteName, Namespace: PlatformNamespace}
	adminRouteKey := types.NamespacedName{Name: ObjectStoreAdminRouteName, Namespace: PlatformNamespace}
	backendTLSKey := types.NamespacedName{Name: ObjectStoreBackendTLSName, Namespace: PlatformNamespace}

	backendTLSPolicy := func() *unstructured.Unstructured {
		policy := &unstructured.Unstructured{}
		policy.SetGroupVersionKind(backendTLSPolicyGVK)
		ExpectWithOffset(1, k8sClient.Get(ctx, backendTLSKey, policy)).To(Succeed())
		return policy
	}

	// serveTLS makes the chart's secret say what it says on an installation
	// where the store was issued a certificate from the platform's own CA.
	serveTLS := func() {
		secret := &corev1.Secret{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{
			Name: objectStoreChartSecretName, Namespace: PlatformNamespace,
		}, secret)).To(Succeed())
		secret.StringData = map[string]string{
			objectstore.SecretKeyHost:              objectStoreServiceHost,
			objectstore.SecretKeyScheme:            objectstore.SchemeHTTPS,
			objectstore.SecretKeyCAFile:            "/etc/kitchen/internal-ca/ca.crt",
			objectstore.SecretKeyCertificateSecret: "kitchen-objectstore-tls",
		}
		ExpectWithOffset(1, k8sClient.Update(ctx, secret)).To(Succeed())
	}

	connectionConfig := func(conn *kitchenv1alpha1.Connection) objectstore.Config {
		cfg, err := objectstore.ConfigOf(conn)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return cfg
	}

	BeforeEach(func() {
		reconciler = &KitchenReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: objectStoreChartSecretName, Namespace: PlatformNamespace},
			StringData: map[string]string{
				objectstore.CredentialKeyAccessKeyID:     "kitchen",
				objectstore.CredentialKeySecretAccessKey: "hunter2hunter2",
			},
		}))).To(Succeed())

		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Registry:   kitchenv1alpha1.ImageRegistrySpec{Enabled: false},
				ObjectStore: kitchenv1alpha1.ObjectStoreSpec{
					Enabled:   true,
					Service:   objectStoreServiceName,
					Port:      9000,
					SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: objectStoreChartSecretName},
				},
			},
		}
		ensureSingleton(ctx, kitchen)
	})

	AfterEach(func() {
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: ObjectStoreConnectionName, Namespace: PlatformNamespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ObjectStoreCredentialsSecretName, Namespace: PlatformNamespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: objectStoreChartSecretName, Namespace: PlatformNamespace}},
			&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: SharedGatewayName, Namespace: PlatformNamespace}},
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: ObjectStoreRouteName, Namespace: PlatformNamespace}},
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: ObjectStoreAdminRouteName, Namespace: PlatformNamespace}},
			backendTLSPolicyObject(),
			acmeIssuerObject(),
			http01IssuerObject(),
			wildcardCertificateObject(),
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("seeds an s3 connection pointing at the store's service", func() {
		reconcileOnce()

		conn := &kitchenv1alpha1.Connection{}
		Expect(k8sClient.Get(ctx, connectionKey, conn)).To(Succeed())
		Expect(conn.Spec.Provider).To(Equal(objectstore.ProviderS3))
		Expect(conn.Labels).To(HaveKeyWithValue(labelManagedByKey, labelManagedByValue))
		cfg := connectionConfig(conn)
		Expect(cfg.Endpoint).To(Equal("http://kitchen-objectstore.kitchen-system.svc.cluster.local:9000"))
		Expect(cfg.ForcePathStyle).To(BeTrue(), "MinIO needs path style")
		Expect(cfg.InCluster).To(BeTrue(), "which is what refuses a publicly readable bucket")
		Expect(cfg.Scoped()).To(BeTrue(), "the root credential mints one per bucket")

		By("writing the credential in the two keys every s3 connection carries")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, credentialKey, secret)).To(Succeed())
		Expect(string(secret.Data[objectstore.CredentialKeyAccessKeyID])).To(Equal("kitchen"))
		Expect(string(secret.Data[objectstore.CredentialKeySecretAccessKey])).To(Equal("hunter2hunter2"))
		Expect(secret.Labels).To(HaveKeyWithValue(labelManagedByKey, labelManagedByValue))

		By("recording what was seeded, so it is seeded once")
		kitchen := singleton()
		Expect(kitchen.Status.ObjectStore).NotTo(BeNil())
		Expect(kitchen.Status.ObjectStore.Connection).To(Equal(ObjectStoreConnectionName))
		Expect(kitchen.Status.ObjectStore.Endpoint).To(Equal(cfg.Endpoint))
		Expect(meta.IsStatusConditionTrue(kitchen.Status.Conditions, condObjectStoreReady)).To(BeTrue())
	})

	It("seeds an https connection, with the CA, when the chart says the store serves TLS", func() {
		serveTLS()

		reconcileOnce()

		conn := &kitchenv1alpha1.Connection{}
		Expect(k8sClient.Get(ctx, connectionKey, conn)).To(Succeed())
		cfg := connectionConfig(conn)
		Expect(cfg.Endpoint).To(Equal("https://kitchen-objectstore.kitchen-system.svc.cluster.local:9000"),
			"the scheme is the store's own, read from the secret the chart wrote")
		Expect(cfg.CAFile).To(Equal("/etc/kitchen/internal-ca/ca.crt"),
			"without it the connection is encrypted and unverified, which is not what the "+
				"platform's own clients settle for")

		By("saying where the store is, on the singleton, in the scheme it answers on")
		Expect(singleton().Status.ObjectStore.Endpoint).To(Equal(cfg.Endpoint))
	})

	It("refuses a scheme that is neither of the two the store is reached on", func() {
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: objectStoreChartSecretName, Namespace: PlatformNamespace,
		}, secret)).To(Succeed())
		secret.StringData = map[string]string{objectstore.SecretKeyScheme: "s3"}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		reconcileOnce()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, connectionKey, &kitchenv1alpha1.Connection{}))).
			To(BeTrue(), "a connection built from a scheme nothing understands would be a "+
				"store nothing can reach and a claim that never says why")
		Expect(meta.FindStatusCondition(singleton().Status.Conditions, condObjectStoreReady).Reason).
			To(Equal("CredentialUnavailable"))
	})

	It("leaves a seeded connection deleted rather than reinstating it", func() {
		reconcileOnce()
		conn := &kitchenv1alpha1.Connection{}
		Expect(k8sClient.Get(ctx, connectionKey, conn)).To(Succeed())
		Expect(k8sClient.Delete(ctx, conn)).To(Succeed())

		reconcileOnce()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, connectionKey, conn))).To(BeTrue())
		kitchen := singleton()
		Expect(meta.IsStatusConditionTrue(kitchen.Status.Conditions, condObjectStoreReady)).To(BeTrue())
		Expect(kitchen.Status.ObjectStore.Connection).To(BeEmpty())
	})

	It("refuses to overwrite a connection of the same name it did not create", func() {
		Expect(k8sClient.Create(ctx, &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: ObjectStoreConnectionName, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             objectstore.ProviderS3,
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "someone-elses-secret"},
				Config:               &runtime.RawExtension{Raw: []byte(`{"endpoint":"https://s3.example.com"}`)},
			},
		})).To(Succeed())

		reconcileOnce()

		conn := &kitchenv1alpha1.Connection{}
		Expect(k8sClient.Get(ctx, connectionKey, conn)).To(Succeed())
		Expect(connectionConfig(conn).Endpoint).To(Equal("https://s3.example.com"))
		Expect(conn.Spec.CredentialsSecretRef.Name).To(Equal("someone-elses-secret"))
		Expect(meta.FindStatusCondition(singleton().Status.Conditions, condObjectStoreReady).Reason).
			To(Equal("ConnectionFailed"))
	})

	It("takes the seeded connection down when it is switched off", func() {
		reconcileOnce()
		Expect(k8sClient.Get(ctx, connectionKey, &kitchenv1alpha1.Connection{})).To(Succeed())

		kitchen := singleton()
		kitchen.Spec.ObjectStore.Enabled = false
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		reconcileOnce()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, connectionKey, &kitchenv1alpha1.Connection{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, credentialKey, &corev1.Secret{}))).To(BeTrue())
		kitchen = singleton()
		Expect(kitchen.Status.ObjectStore).To(BeNil())
		Expect(meta.FindStatusCondition(kitchen.Status.Conditions, condObjectStoreReady)).To(BeNil())
	})

	It("publishes the store on the shared Gateway, and puts that address in the binding", func() {
		reconcileOnce()

		By("an HTTPRoute of the operator's, on the HTTPS listener, for the reserved label")
		route := &gatewayv1.HTTPRoute{}
		Expect(k8sClient.Get(ctx, routeKey, route)).To(Succeed())
		Expect(route.Spec.Hostnames).To(Equal([]gatewayv1.Hostname{objectStorePublicHostname}))
		Expect(route.Spec.ParentRefs).To(HaveLen(1))
		Expect(route.Spec.ParentRefs[0].Name).To(Equal(gatewayv1.ObjectName(SharedGatewayName)))
		Expect(route.Spec.ParentRefs[0].SectionName).NotTo(BeNil())
		Expect(string(*route.Spec.ParentRefs[0].SectionName)).To(Equal(gatewayListenerHTTPS),
			"the public address is https or it is not published at all")
		Expect(route.Spec.Rules).To(HaveLen(1))
		Expect(route.Spec.Rules[0].BackendRefs).To(HaveLen(1))
		Expect(route.Spec.Rules[0].BackendRefs[0].Name).To(Equal(gatewayv1.ObjectName(objectStoreServiceName)))
		Expect(*route.Spec.Rules[0].BackendRefs[0].Port).To(Equal(gatewayv1.PortNumber(9000)))

		By("a second address on the seeded connection, beside the in-cluster one and not replacing it")
		conn := &kitchenv1alpha1.Connection{}
		Expect(k8sClient.Get(ctx, connectionKey, conn)).To(Succeed())
		cfg := connectionConfig(conn)
		Expect(cfg.Endpoint).To(Equal("http://kitchen-objectstore.kitchen-system.svc.cluster.local:9000"),
			"the address the application's own reads and writes use does not move")
		Expect(cfg.PublicEndpoint).To(Equal(objectStorePublicEndpoint),
			"and this is the one a presigned URL is signed against")

		By("and saying both on the singleton, where the platform reports itself")
		kitchen := singleton()
		Expect(kitchen.Status.ObjectStore.Endpoint).To(Equal(cfg.Endpoint))
		Expect(kitchen.Status.ObjectStore.PublicEndpoint).To(Equal(objectStorePublicEndpoint))
		Expect(kitchen.Status.ObjectStore.Unpublished).To(BeEmpty())
		Expect(meta.FindStatusCondition(kitchen.Status.Conditions, condObjectStoreReady).Message).
			To(ContainSubstring(objectStorePublicEndpoint))
	})

	// Publishing port 9000 publishes the S3 API and MinIO's admin API with
	// it, because the S3 API is the whole URL space and the route in front
	// of it can only match `/`. The admin API is carved back out by a more
	// specific route, which is the only shape Gateway API has for it.
	It("keeps the store's admin API off the published address", func() {
		reconcileOnce()

		admin := &gatewayv1.HTTPRoute{}
		Expect(k8sClient.Get(ctx, adminRouteKey, admin)).To(Succeed())
		Expect(admin.Spec.Hostnames).To(Equal([]gatewayv1.Hostname{objectStorePublicHostname}),
			"the same hostname, or it out-precedences nothing")
		Expect(admin.Spec.Rules).To(HaveLen(1))

		By("matching a longer prefix than the store's route, which is what wins")
		Expect(admin.Spec.Rules[0].Matches).To(HaveLen(1))
		adminPath := admin.Spec.Rules[0].Matches[0].Path
		Expect(adminPath).NotTo(BeNil())
		Expect(*adminPath.Type).To(Equal(gatewayv1.PathMatchPathPrefix))
		Expect(*adminPath.Value).To(Equal("/minio/admin/"))

		store := &gatewayv1.HTTPRoute{}
		Expect(k8sClient.Get(ctx, routeKey, store)).To(Succeed())
		Expect(store.Spec.Rules[0].Matches).To(HaveLen(1))
		storePath := store.Spec.Rules[0].Matches[0].Path
		Expect(storePath).NotTo(BeNil())
		Expect(*storePath.Type).To(Equal(gatewayv1.PathMatchPathPrefix))
		Expect(*storePath.Value).To(Equal("/"))
		Expect(len(*adminPath.Value)).To(BeNumerically(">", len(*storePath.Value)),
			"path specificity is the whole of the precedence argument here, since the "+
				"hostnames are equal and hostname is weighed first")

		By("answering it itself rather than reaching the store")
		Expect(admin.Spec.Rules[0].BackendRefs).To(BeEmpty(),
			"a backend here would be the store, which is the thing being kept out of reach")
		Expect(admin.Spec.Rules[0].Filters).To(HaveLen(1),
			"and with no response-producing filter the rule answers 500, which would "+
				"report a platform fault for a deliberate refusal")
		Expect(admin.Spec.Rules[0].Filters[0].Type).To(Equal(gatewayv1.HTTPRouteFilterRequestRedirect))
		Expect(admin.Spec.Rules[0].Filters[0].RequestRedirect).NotTo(BeNil())
		Expect(*admin.Spec.Rules[0].Filters[0].RequestRedirect.Path.ReplaceFullPath).To(Equal("/"))
	})

	It("takes the admin carve-out down with the route it carves out of", func() {
		reconcileOnce()
		Expect(k8sClient.Get(ctx, adminRouteKey, &gatewayv1.HTTPRoute{})).To(Succeed())

		kitchen := singleton()
		kitchen.Spec.TLS = kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeNone}
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		reconcileOnce()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, adminRouteKey, &gatewayv1.HTTPRoute{}))).To(BeTrue(),
			"left behind it redirects a hostname the platform no longer answers for")
	})

	It("tells the Gateway to reach the store over TLS, on the platform's own CA", func() {
		serveTLS()

		reconcileOnce()

		// The two trust paths are different and must not be conflated: the
		// public leg rides the platform's wildcard certificate, this one the
		// internal CA, on the `.svc` name that certificate is issued for.
		policy := backendTLSPolicy()
		hostname, found, err := unstructured.NestedString(policy.Object, "spec", "validation", "hostname")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(hostname).To(Equal(objectStoreServiceHost),
			"the name the store's certificate is issued for, not the one in front of the Gateway")
		refs, found, err := unstructured.NestedSlice(policy.Object, "spec", "validation", "caCertificateRefs")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(refs).To(Equal([]any{map[string]any{
			"group": "", "kind": "ConfigMap", "name": InternalCAConfigMapName,
		}}))
		targets, _, err := unstructured.NestedSlice(policy.Object, "spec", "targetRefs")
		Expect(err).NotTo(HaveOccurred())
		Expect(targets).To(Equal([]any{map[string]any{
			"group": "", "kind": "Service", "name": objectStoreServiceName,
		}}))

		By("and leaving the private CA off the public address, which no client outside has")
		conn := &kitchenv1alpha1.Connection{}
		Expect(k8sClient.Get(ctx, connectionKey, conn)).To(Succeed())
		cfg := connectionConfig(conn)
		Expect(cfg.PublicEndpoint).To(Equal(objectStorePublicEndpoint))
		Expect(cfg.CAFile).To(Equal("/etc/kitchen/internal-ca/ca.crt"),
			"which belongs to the in-cluster endpoint alone")
	})

	It("writes no backend TLS policy for a store left in the clear", func() {
		reconcileOnce()

		policy := &unstructured.Unstructured{}
		policy.SetGroupVersionKind(backendTLSPolicyGVK)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, backendTLSKey, policy))).To(BeTrue(),
			"there is nothing to verify, and a policy saying otherwise would break the hop")
	})

	It("publishes nothing in tls mode none, and says why", func() {
		kitchen := singleton()
		kitchen.Spec.TLS = kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeNone}
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		reconcileOnce()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, routeKey, &gatewayv1.HTTPRoute{}))).To(BeTrue(),
			"publishing the store there would carry every presigned request in the clear")

		conn := &kitchenv1alpha1.Connection{}
		Expect(k8sClient.Get(ctx, connectionKey, conn)).To(Succeed())
		Expect(connectionConfig(conn).PublicEndpoint).To(BeEmpty())

		kitchen = singleton()
		Expect(kitchen.Status.ObjectStore.PublicEndpoint).To(BeEmpty())
		Expect(kitchen.Status.ObjectStore.Unpublished).To(ContainSubstring("tls.mode none"))
		// Still a running store, so the condition is True: not being
		// published is a fact about it rather than a fault.
		Expect(meta.IsStatusConditionTrue(kitchen.Status.Conditions, condObjectStoreReady)).To(BeTrue())
		Expect(meta.FindStatusCondition(kitchen.Status.Conditions, condObjectStoreReady).Message).
			To(ContainSubstring("tls.mode none"))
	})

	It("takes the published address down when the store is switched off", func() {
		serveTLS()
		reconcileOnce()
		Expect(k8sClient.Get(ctx, routeKey, &gatewayv1.HTTPRoute{})).To(Succeed())
		Expect(backendTLSPolicy()).NotTo(BeNil())

		kitchen := singleton()
		kitchen.Spec.ObjectStore.Enabled = false
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		reconcileOnce()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, routeKey, &gatewayv1.HTTPRoute{}))).To(BeTrue(),
			"a route to a store the chart has stopped rendering answers 503 under the platform's own name")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, adminRouteKey, &gatewayv1.HTTPRoute{}))).To(BeTrue())
		policy := &unstructured.Unstructured{}
		policy.SetGroupVersionKind(backendTLSPolicyGVK)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, backendTLSKey, policy))).To(BeTrue())
	})

	It("waits for the credential the chart generates", func() {
		Expect(k8sClient.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: objectStoreChartSecretName, Namespace: PlatformNamespace},
		})).To(Succeed())

		reconcileOnce()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, connectionKey, &kitchenv1alpha1.Connection{}))).To(BeTrue())
		Expect(meta.FindStatusCondition(singleton().Status.Conditions, condObjectStoreReady).Reason).
			To(Equal("CredentialUnavailable"))
	})
})

// backendTLSPolicyObject is the policy as a bare object, for a cleanup list.
func backendTLSPolicyObject() *unstructured.Unstructured {
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(backendTLSPolicyGVK)
	policy.SetName(ObjectStoreBackendTLSName)
	policy.SetNamespace(PlatformNamespace)
	return policy
}

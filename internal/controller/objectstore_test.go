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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

// objectStoreChartSecretName mirrors what the chart writes for the release
// named "kitchen".
const objectStoreChartSecretName = "kitchen-objectstore"

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
					Service:   "kitchen-objectstore",
					Port:      9000,
					SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: objectStoreChartSecretName},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())
	})

	AfterEach(func() {
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: ObjectStoreConnectionName, Namespace: PlatformNamespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ObjectStoreCredentialsSecretName, Namespace: PlatformNamespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: objectStoreChartSecretName, Namespace: PlatformNamespace}},
			&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: SharedGatewayName, Namespace: PlatformNamespace}},
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

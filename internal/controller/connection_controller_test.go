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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

var _ = Describe("Connection Controller", func() {
	Context("When reconciling a connection", func() {
		const namespace = "default"

		ctx := context.Background()

		var reconciler *ConnectionReconciler

		reconcileConn := func(name string) {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
			})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		getConn := func(name string) *kitchenv1alpha1.Connection {
			conn := &kitchenv1alpha1.Connection{}
			ExpectWithOffset(1, k8sClient.Get(ctx,
				types.NamespacedName{Name: name, Namespace: namespace}, conn)).To(Succeed())
			return conn
		}

		BeforeEach(func() {
			reconciler = &ConnectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		})

		AfterEach(func() {
			for _, obj := range []client.Object{
				&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "conn-gh", Namespace: namespace}},
				&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "conn-infisical", Namespace: namespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "conn-creds", Namespace: namespace}},
			} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		It("publishes the provider's capabilities once credentials are in place", func() {
			creds := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "conn-creds", Namespace: namespace},
				StringData: map[string]string{"token": "t"},
			}
			Expect(k8sClient.Create(ctx, creds)).To(Succeed())

			conn := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "conn-gh", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "github",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "conn-creds"},
				},
			}
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			reconcileConn("conn-gh")

			conn = getConn("conn-gh")
			Expect(conn.Status.Capabilities).To(ConsistOf(
				kitchenv1alpha1.CapabilityGitSource, kitchenv1alpha1.CapabilityStatusChecks))
			Expect(meta.IsStatusConditionTrue(conn.Status.Conditions, condConnected)).To(BeTrue())
		})

		It("gives the infisical provider the secretStore capability", func() {
			creds := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "conn-creds", Namespace: namespace},
				StringData: map[string]string{"clientId": "id", "clientSecret": "hush"},
			}
			Expect(k8sClient.Create(ctx, creds)).To(Succeed())

			conn := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "conn-infisical", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "infisical",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "conn-creds"},
				},
			}
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			reconcileConn("conn-infisical")

			conn = getConn("conn-infisical")
			Expect(conn.Status.Capabilities).To(ConsistOf(kitchenv1alpha1.CapabilitySecretStore))
			Expect(meta.IsStatusConditionTrue(conn.Status.Conditions, condConnected)).To(BeTrue())
		})

		It("reports missing credentials but still publishes capabilities", func() {
			conn := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "conn-gh", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "github",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "conn-creds"},
				},
			}
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())

			reconcileConn("conn-gh")

			conn = getConn("conn-gh")
			Expect(conn.Status.Capabilities).To(ConsistOf(
				kitchenv1alpha1.CapabilityGitSource, kitchenv1alpha1.CapabilityStatusChecks))
			cond := meta.FindStatusCondition(conn.Status.Conditions, condConnected)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("CredentialsMissing"))
		})
	})
})

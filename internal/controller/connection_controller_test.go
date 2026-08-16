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
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

var _ = Describe("Connection Controller", func() {
	Context("When validating a connection's credential", func() {
		const (
			namespace = "default"
			goodToken = "the-good-token"
		)

		ctx := context.Background()

		var (
			reconciler *ConnectionReconciler
			github     *httptest.Server
		)

		BeforeEach(func() {
			reconciler = &ConnectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// A stand-in GitHub that accepts exactly one token — reached
			// through the connection config's apiUrl, the same override GitHub
			// Enterprise uses, so the tests exercise the real probe.
			github = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+goodToken {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"message": "Bad credentials"}`))
					return
				}
				w.Header().Set("X-OAuth-Scopes", "repo, admin:repo_hook")
				_, _ = w.Write([]byte(`{"login": "octocat"}`))
			}))
			DeferCleanup(github.Close)
		})

		createConnection := func(name, providerName, token, apiURL string) types.NamespacedName {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-creds", Namespace: namespace},
				StringData: map[string]string{"token": token},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			conn := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             providerName,
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: name + "-creds"},
				},
			}
			if apiURL != "" {
				conn.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"apiUrl": "` + apiURL + `"}`)}
			}
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Delete(ctx, conn)).To(Succeed())
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			})
			return types.NamespacedName{Namespace: namespace, Name: name}
		}

		createGitHubConnection := func(name, token, apiURL string) types.NamespacedName {
			return createConnection(name, "github", token, apiURL)
		}

		reconcileOnce := func(key types.NamespacedName) reconcile.Result {
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			return result
		}

		getConnection := func(key types.NamespacedName) *kitchenv1alpha1.Connection {
			conn := &kitchenv1alpha1.Connection{}
			ExpectWithOffset(1, k8sClient.Get(ctx, key, conn)).To(Succeed())
			return conn
		}

		It("populates capabilities and marks a working credential valid", func() {
			key := createGitHubConnection("gh-good", goodToken, github.URL)
			result := reconcileOnce(key)

			conn := getConnection(key)
			Expect(conn.Status.Capabilities).To(ConsistOf(
				kitchenv1alpha1.CapabilityGitSource, kitchenv1alpha1.CapabilityStatusChecks))

			connected := meta.FindStatusCondition(conn.Status.Conditions, condConnected)
			Expect(connected).NotTo(BeNil())
			Expect(connected.Status).To(Equal(metav1.ConditionTrue))

			valid := meta.FindStatusCondition(conn.Status.Conditions, condCredentialsValid)
			Expect(valid).NotTo(BeNil())
			Expect(valid.Status).To(Equal(metav1.ConditionTrue))
			Expect(valid.Message).To(ContainSubstring("octocat"))
			Expect(valid.Message).To(ContainSubstring("admin:repo_hook"))

			// The credential itself must never surface in status.
			for _, condition := range conn.Status.Conditions {
				Expect(condition.Message).NotTo(ContainSubstring(goodToken))
			}

			Expect(result.RequeueAfter).To(Equal(connectionRecheckInterval))
		})

		It("marks a rejected credential invalid on a provider that answered", func() {
			key := createGitHubConnection("gh-bad", "the-wrong-token", github.URL)
			reconcileOnce(key)

			conn := getConnection(key)
			connected := meta.FindStatusCondition(conn.Status.Conditions, condConnected)
			Expect(connected.Status).To(Equal(metav1.ConditionTrue), "a wrong password is not an outage")

			valid := meta.FindStatusCondition(conn.Status.Conditions, condCredentialsValid)
			Expect(valid.Status).To(Equal(metav1.ConditionFalse))
			Expect(valid.Reason).To(Equal(reasonCredentialsRejected))
			Expect(valid.Message).To(ContainSubstring("Bad credentials"))
		})

		It("distinguishes an unreachable provider from a bad credential", func() {
			down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			down.Close() // The address exists; nothing answers on it.

			key := createGitHubConnection("gh-down", goodToken, down.URL)
			result := reconcileOnce(key)

			conn := getConnection(key)
			connected := meta.FindStatusCondition(conn.Status.Conditions, condConnected)
			Expect(connected.Status).To(Equal(metav1.ConditionFalse))
			Expect(connected.Reason).To(Equal(reasonProviderUnreachable))

			valid := meta.FindStatusCondition(conn.Status.Conditions, condCredentialsValid)
			Expect(valid.Status).To(Equal(metav1.ConditionUnknown),
				"an unanswered probe must not condemn the credential")

			Expect(result.RequeueAfter).To(Equal(connectionRetryInterval))
		})

		It("reports a provider nothing implements as Unknown, with no capabilities", func() {
			key := createConnection("gitea-conn", "gitea", "whatever", "")
			result := reconcileOnce(key)

			conn := getConnection(key)
			Expect(conn.Status.Capabilities).To(BeEmpty())
			for _, condType := range []string{condConnected, condCredentialsValid} {
				condition := meta.FindStatusCondition(conn.Status.Conditions, condType)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionUnknown))
				Expect(condition.Reason).To(Equal(reasonProviderNotImplemented))
			}
			// Time will not implement gitea; only a new operator does.
			Expect(result.RequeueAfter).To(BeZero())
		})

		It("marks a connection with a missing credentials secret", func() {
			conn := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "gh-orphan", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "github",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "does-not-exist"},
				},
			}
			Expect(k8sClient.Create(ctx, conn)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, conn)).To(Succeed()) })

			key := types.NamespacedName{Namespace: namespace, Name: conn.Name}
			reconcileOnce(key)

			got := getConnection(key)
			valid := meta.FindStatusCondition(got.Status.Conditions, condCredentialsValid)
			Expect(valid.Status).To(Equal(metav1.ConditionFalse))
			Expect(valid.Reason).To(Equal(reasonCredentialsMissing))
		})

		It("revalidates when the credentials secret is rotated", func() {
			key := createGitHubConnection("gh-rotate", "retired-token", github.URL)
			reconcileOnce(key)
			valid := meta.FindStatusCondition(getConnection(key).Status.Conditions, condCredentialsValid)
			Expect(valid.Status).To(Equal(metav1.ConditionFalse))
			firstTransition := valid.LastTransitionTime

			// The watch turns the secret write into this reconcile request.
			secret := &corev1.Secret{}
			secretKey := types.NamespacedName{Namespace: namespace, Name: "gh-rotate-creds"}
			Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
			requests := reconciler.mapSecretToConnections(ctx, secret)
			Expect(requests).To(ConsistOf(reconcile.Request{NamespacedName: key}))

			secret.Data["token"] = []byte(goodToken)
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			reconcileOnce(key)

			valid = meta.FindStatusCondition(getConnection(key).Status.Conditions, condCredentialsValid)
			Expect(valid.Status).To(Equal(metav1.ConditionTrue))
			Expect(valid.LastTransitionTime.Time).To(BeTemporally(">=", firstTransition.Time))
		})

		It("does not enqueue connections for unrelated secrets", func() {
			createGitHubConnection("gh-unrelated", goodToken, github.URL)
			other := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "some-other-secret", Namespace: namespace},
			}
			Expect(reconciler.mapSecretToConnections(ctx, other)).To(BeEmpty())
		})

		It("recovers once a provider outage ends", func() {
			// One server address, two lives: down for the first probe, up for
			// the second — the requeue interval is what carries the retry.
			key := createGitHubConnection("gh-flaky", goodToken, github.URL)
			github.CloseClientConnections()
			github.Close()
			reconcileOnce(key)
			Expect(meta.FindStatusCondition(getConnection(key).Status.Conditions, condConnected).Status).
				To(Equal(metav1.ConditionFalse))

			revived := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+goodToken {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_, _ = w.Write([]byte(`{"login": "octocat"}`))
			}))
			DeferCleanup(revived.Close)
			conn := getConnection(key)
			conn.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"apiUrl": "` + revived.URL + `"}`)}
			Expect(k8sClient.Update(ctx, conn)).To(Succeed())

			reconcileOnce(key)
			refreshed := getConnection(key)
			Expect(meta.FindStatusCondition(refreshed.Status.Conditions, condConnected).Status).
				To(Equal(metav1.ConditionTrue))
			Expect(meta.FindStatusCondition(refreshed.Status.Conditions, condCredentialsValid).Status).
				To(Equal(metav1.ConditionTrue))
		})
	})
})

var _ = Describe("Connection Controller timing", func() {
	It("rechecks less often than it retries", func() {
		// The retry interval exists to see an outage end quickly; if it ever
		// exceeds the steady-state recheck it has lost its purpose.
		Expect(connectionRetryInterval).To(BeNumerically("<", connectionRecheckInterval))
		Expect(connectionRecheckInterval).To(Equal(10 * time.Minute))
	})
})

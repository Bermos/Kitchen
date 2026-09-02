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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/idp"
)

// directorySecretName mirrors what the chart writes for the release named
// "kitchen": the same Secret the preview gate's client registration reads.
const directorySecretName = "kitchen-auth-directory"

// fakeDirectory is the identity provider's account directory and nothing
// else. The accounts it holds are the whole input to seeding, and the count
// of reads is how a test says "this was never asked".
type fakeDirectory struct {
	*httptest.Server
	accounts []idp.Account
	reads    int

	// serve404 makes this a federated issuer: one that has never heard of
	// Kitchen's directory prefix, which is what every issuer but the bundled
	// one is. It is not an outage, and the difference is the whole of what
	// separates "try again" from "name your operators yourself".
	serve404 bool
}

func newFakeDirectory(accounts ...idp.Account) *fakeDirectory {
	directory := &fakeDirectory{accounts: accounts}
	mux := http.NewServeMux()
	mux.HandleFunc(idp.AccountsPath, func(w http.ResponseWriter, _ *http.Request) {
		directory.reads++
		if directory.serve404 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": directory.accounts})
	})
	directory.Server = httptest.NewServer(mux)
	return directory
}

func account(subject, email string) idp.Account {
	return idp.Account{Subject: subject, Email: email, Name: email, EmailVerified: true}
}

var _ = Describe("The platform's operator list", func() {
	Context("When nobody has said who the operators are", func() {
		ctx := context.Background()

		singletonKey := types.NamespacedName{Name: KitchenSingletonName}

		var reconciler *KitchenReconciler
		var directory *fakeDirectory

		reconcileOnce := func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: singletonKey})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		// operators re-reads the singleton, since the point of every
		// assertion here is what reached the API server rather than what the
		// reconciler was holding.
		operators := func() []kitchenv1alpha1.AccessSubject {
			kitchen := &kitchenv1alpha1.Kitchen{}
			ExpectWithOffset(1, k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			return kitchen.Spec.Access.Operators
		}

		// reconcileAccessDirectly answers the one thing a full Reconcile
		// cannot be asked in envtest: whether *this* piece wants to be come
		// back to. The Result the controller returns is the whole platform's
		// — no Gateway controller runs here, so it always asks for a retry —
		// and the requeue is exactly what a permanently absent endpoint must
		// not cause.
		reconcileAccessDirectly := func() bool {
			kitchen := &kitchenv1alpha1.Kitchen{}
			ExpectWithOffset(1, k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			return reconciler.reconcileAccess(ctx, kitchen,
				func(string, metav1.ConditionStatus, string, string) {})
		}

		condition := func() *metav1.Condition {
			kitchen := &kitchenv1alpha1.Kitchen{}
			ExpectWithOffset(1, k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			return meta.FindStatusCondition(kitchen.Status.Conditions, condOperatorsConfigured)
		}

		// start brings the platform up with an identity provider holding
		// these accounts, the way the chart leaves it: the auth Secret
		// exists, and the Kitchen singleton has never had an operator list.
		start := func(accounts ...idp.Account) {
			directory = newFakeDirectory(accounts...)
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
			}))).To(Succeed())
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: directorySecretName, Namespace: PlatformNamespace},
				StringData: map[string]string{
					idp.SecretKeyIssuer:     directory.URL,
					idp.SecretKeyServiceKey: "the-service-key",
				},
			}))).To(Succeed())
			ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
				Spec: kitchenv1alpha1.KitchenSpec{
					BaseDomain: "apps.example.com",
					TLS:        acmeTLS(),
					Auth: kitchenv1alpha1.AuthSpec{
						Enabled:   true,
						SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: directorySecretName},
					},
				},
			})
		}

		BeforeEach(func() {
			reconciler = &KitchenReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		})

		AfterEach(func() {
			directory.Close()
			for _, obj := range []client.Object{
				&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: SharedGatewayName, Namespace: PlatformNamespace}},
				acmeIssuerObject(),
				http01IssuerObject(),
				wildcardCertificateObject(),
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: directorySecretName, Namespace: PlatformNamespace}},
				&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
			} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		It("makes the bootstrap account the first operator on a fresh install", func() {
			// A fresh install has exactly one account: the one the bootstrap
			// link created. The same rule that grandfathers an upgrade seeds
			// exactly that one here.
			start(account("user_anna", "anna@example.com"))
			reconcileOnce()

			Expect(operators()).To(HaveLen(1))
			Expect(operators()[0].Subject).To(Equal("user_anna"))
			Expect(operators()[0].Email).To(Equal("anna@example.com"),
				"the address rides along so the list reads")

			cond := condition()
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("OperatorsSeeded"))
			Expect(cond.Message).To(ContainSubstring("anna@example.com"))
		})

		It("grandfathers every account on an installation upgrading into enforcement", func() {
			// Before enforcement every account could call every route, so
			// every account read honestly is an operator. Seeding fewer than
			// all of them locks somebody out on a minor version bump.
			start(
				account("user_anna", "anna@example.com"),
				account("user_bo", "bo@example.com"),
				account("user_cy", "cy@example.com"),
			)
			reconcileOnce()

			Expect(operators()).To(HaveLen(3))
			Expect(operators()[0].Subject).To(Equal("user_anna"), "oldest account first")
			subjects := []string{}
			for _, operator := range operators() {
				subjects = append(subjects, operator.Subject)
			}
			Expect(subjects).To(ConsistOf("user_anna", "user_bo", "user_cy"))
		})

		It("seeds once and leaves the list alone afterwards", func() {
			start(account("user_anna", "anna@example.com"))
			reconcileOnce()

			// Somebody narrows it to somebody else. A second reconcile must
			// not put the seed back.
			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			kitchen.Spec.Access.Operators = []kitchenv1alpha1.AccessSubject{
				{Subject: "user_bo", Email: "bo@example.com"},
			}
			Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

			reconcileOnce()
			Expect(operators()).To(HaveLen(1))
			Expect(operators()[0].Subject).To(Equal("user_bo"))
			Expect(condition().Reason).To(Equal("OperatorsNamed"))
		})

		It("leaves an empty list exactly as it is", func() {
			// An empty list is somebody narrowing the platform to nobody on
			// purpose. It is not the absence the seeding is for, and the
			// accounts that exist are not even consulted.
			start(account("user_anna", "anna@example.com"))

			// Written as raw JSON on purpose. The field is `omitempty`, so a
			// typed Go client cannot express an empty list at all — it
			// marshals one away and the object comes back absent, which is
			// the other case entirely. kubectl and a JSON patch can, which is
			// how somebody narrows the platform to nobody today.
			kitchen := &kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}}
			Expect(k8sClient.Patch(ctx, kitchen,
				client.RawPatch(types.MergePatchType, []byte(`{"spec":{"access":{"operators":[]}}}`)))).To(Succeed())
			Expect(operators()).NotTo(BeNil(), "an empty list is not an absent one")

			reconcileOnce()

			Expect(operators()).To(BeEmpty())
			Expect(directory.reads).To(Equal(0), "a decision somebody took needs no accounts to second-guess it")

			cond := condition()
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("NobodyIsAnOperator"))
		})

		It("writes nothing while there are no accounts, and seeds on the next reconcile", func() {
			// A fresh install reconciles before anybody has followed the
			// bootstrap link. Writing `operators: []` there would turn
			// "nobody has said yet" into "somebody said nobody", and the
			// bootstrap account would never become an operator.
			start()
			reconcileOnce()

			Expect(operators()).To(BeNil(), "an absent list is what keeps the platform seedable")
			cond := condition()
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("AwaitingFirstAccount"))

			By("seeding as soon as somebody follows the bootstrap link")
			directory.accounts = []idp.Account{account("user_anna", "anna@example.com")}
			reconcileOnce()

			Expect(operators()).To(HaveLen(1))
			Expect(operators()[0].Subject).To(Equal("user_anna"))
			Expect(condition().Reason).To(Equal("OperatorsSeeded"))
		})

		It("reports an identity provider it cannot read the accounts from, and comes back", func() {
			start(account("user_anna", "anna@example.com"))
			directory.Close()
			reconcileOnce()

			Expect(operators()).To(BeNil())
			cond := condition()
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("DirectoryUnavailable"))
			Expect(cond.Message).To(ContainSubstring("This is retried"),
				"a directory that did not answer is asked again")
			Expect(cond.Message).To(ContainSubstring(chartOperatorsValue),
				"and if it never answers, the message still names the way out")

			// A failure that might clear is worth coming back for.
			Expect(reconcileAccessDirectly()).To(BeFalse())
		})

		It("names the way out on an issuer that serves no account directory", func() {
			// A federated issuer — Keycloak, Auth0 — answers 404 on Kitchen's
			// own directory prefix, because OIDC defines no such endpoint.
			// Nothing can be seeded from one, so an installation on it has to
			// have named its operators at install time; a condition that only
			// reported the 404 would leave the way out to be guessed.
			start(account("user_anna", "anna@example.com"))
			directory.serve404 = true
			reconcileOnce()

			Expect(operators()).To(BeNil(), "there is nothing honest to write")
			cond := condition()
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NoAccountDirectory"))
			Expect(cond.Message).To(ContainSubstring("serves no account directory"), "what is wrong")
			Expect(cond.Message).To(ContainSubstring(chartOperatorsValue), "and what would fix it")
			Expect(cond.Message).To(ContainSubstring("PATCH /settings"),
				"including why the platform cannot talk its own way out of this")

			By("not asking a permanently absent endpoint again on a timer")
			Expect(reconcileAccessDirectly()).To(BeTrue(),
				"an absent endpoint does not become present by being polled every 30 seconds")
		})

		It("leaves an operator list the chart wrote exactly as the chart wrote it", func() {
			// kitchen.access.operators renders into spec.access.operators at
			// install time, which is the only answer a federated installation
			// has. It is the same statement as a hand-written one and gets
			// the same treatment: present means somebody has said, so the
			// accounts that exist are not consulted and nothing is re-seeded.
			directory = newFakeDirectory(
				account("user_anna", "anna@example.com"),
				account("user_bo", "bo@example.com"),
				account("user_cy", "cy@example.com"),
			)
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
			}))).To(Succeed())
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: directorySecretName, Namespace: PlatformNamespace},
				StringData: map[string]string{
					idp.SecretKeyIssuer:     directory.URL,
					idp.SecretKeyServiceKey: "the-service-key",
				},
			}))).To(Succeed())
			Expect(k8sClient.Create(ctx, &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
				Spec: kitchenv1alpha1.KitchenSpec{
					BaseDomain: "apps.example.com",
					TLS:        acmeTLS(),
					Auth: kitchenv1alpha1.AuthSpec{
						Enabled:   true,
						SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: directorySecretName},
					},
					Access: kitchenv1alpha1.AccessSpec{
						Operators: []kitchenv1alpha1.AccessSubject{
							{Subject: "anna@example.com", Email: "anna@example.com"},
						},
					},
				},
			})).To(Succeed())

			reconcileOnce()

			Expect(operators()).To(HaveLen(1), "the chart's list is not widened to the three accounts")
			Expect(operators()[0].Subject).To(Equal("anna@example.com"))
			Expect(directory.reads).To(Equal(0), "a decision somebody took needs no accounts to second-guess it")
			Expect(condition().Reason).To(Equal("OperatorsNamed"))
		})

		It("seeds without disturbing anything else on the object", func() {
			// The seed is a merge patch of the one field, so everything else
			// the singleton says — and the status this reconcile assembled —
			// survives it.
			start(account("user_anna", "anna@example.com"))
			reconcileOnce()

			kitchen := &kitchenv1alpha1.Kitchen{}
			Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
			Expect(kitchen.Spec.BaseDomain).To(Equal("apps.example.com"))
			Expect(kitchen.Spec.Auth.SecretRef.Name).To(Equal(directorySecretName))
			Expect(meta.FindStatusCondition(kitchen.Status.Conditions, condReady)).NotTo(BeNil(),
				"the status update at the end of the reconcile still lands after a spec patch")
		})
	})
})

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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The message cert-manager writes on a Certificate it has issued. The point of
// mirroring it rather than reducing it to a boolean is that the interesting
// ones are the failures — a token without the right scopes, a zone it cannot
// see — and those are only ever spelled out here.
const certManagerReadyMessage = "Certificate is up to date and has not expired"

// certManagerObject addresses one of cert-manager's kinds the way the operator
// does: unstructured, so nothing in the test knows more about its schema than
// the code under test does.
func certManagerObject(kind, name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(certManagerGVK(kind))
	obj.SetName(name)
	obj.SetNamespace(namespace)
	return obj
}

func acmeIssuerObject() *unstructured.Unstructured {
	return certManagerObject("ClusterIssuer", acmeClusterIssuerName, "")
}

func http01IssuerObject() *unstructured.Unstructured {
	return certManagerObject("ClusterIssuer", acmeHTTP01ClusterIssuerName, "")
}

func wildcardCertificateObject() *unstructured.Unstructured {
	return certManagerObject("Certificate", wildcardCertificateName, PlatformNamespace)
}

// certManagerAbsent answers for cert-manager's kinds the way an API server
// that is not serving them yet does: with a no-match error, because the CRDs
// are not registered. Everything else is passed through, so a reconcile runs
// exactly as it would on a cluster where cert-manager is still starting.
type certManagerAbsent struct {
	client.Client
}

func certManagerNoMatch(obj client.Object) error {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Group != "cert-manager.io" {
		return nil
	}
	return &meta.NoKindMatchError{
		GroupKind:        gvk.GroupKind(),
		SearchedVersions: []string{gvk.Version},
	}
}

func (c certManagerAbsent) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if err := certManagerNoMatch(obj); err != nil {
		return err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c certManagerAbsent) Create(
	ctx context.Context, obj client.Object, opts ...client.CreateOption,
) error {
	if err := certManagerNoMatch(obj); err != nil {
		return err
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c certManagerAbsent) Update(
	ctx context.Context, obj client.Object, opts ...client.UpdateOption,
) error {
	if err := certManagerNoMatch(obj); err != nil {
		return err
	}
	return c.Client.Update(ctx, obj, opts...)
}

var _ = Describe("Kitchen edge TLS", func() {
	ctx := context.Background()

	singletonKey := types.NamespacedName{Name: KitchenSingletonName}
	issuerKey := types.NamespacedName{Name: acmeClusterIssuerName}
	certKey := types.NamespacedName{Name: wildcardCertificateName, Namespace: PlatformNamespace}

	var reconciler *KitchenReconciler

	// conditionOn re-reads the singleton, so an assertion is never made
	// against the copy the reconcile happened to be holding.
	conditionOn := func(key types.NamespacedName) *metav1.Condition {
		kitchen := &kitchenv1alpha1.Kitchen{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, kitchen)).To(Succeed())
		return meta.FindStatusCondition(kitchen.Status.Conditions, condCertificateReady)
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
			acmeIssuerObject(),
			http01IssuerObject(),
			wildcardCertificateObject(),
			&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{
				Name: SharedGatewayName, Namespace: PlatformNamespace,
			}},
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
				Name: httpsRedirectRouteName, Namespace: PlatformNamespace,
			}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("builds the issuer and the wildcard certificate from spec.tls.acme", func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: singletonKey})
		Expect(err).NotTo(HaveOccurred())

		By("registering an ACME account against the configured directory")
		issuer := acmeIssuerObject()
		Expect(k8sClient.Get(ctx, issuerKey, issuer)).To(Succeed())
		acme, found, err := unstructured.NestedMap(issuer.Object, "spec", "acme")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(acme).To(HaveKeyWithValue("server", "https://acme-v02.api.letsencrypt.org/directory"))
		Expect(acme).To(HaveKeyWithValue("email", "platform@example.com"))
		Expect(acme).To(HaveKeyWithValue("privateKeySecretRef",
			map[string]any{"name": acmeAccountKeySecretName}))

		By("solving over DNS-01, the only challenge that issues a wildcard")
		solvers, found, err := unstructured.NestedSlice(issuer.Object, "spec", "acme", "solvers")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(solvers).To(HaveLen(1))
		Expect(solvers[0]).To(HaveKeyWithValue("dns01", map[string]any{
			"cloudflare": map[string]any{
				"apiTokenSecretRef": map[string]any{
					"name": "cloudflare-api-token",
					"key":  "api-token",
				},
			},
		}))

		By("registering a second account for custom domains, solved over HTTP-01")
		http01 := http01IssuerObject()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: acmeHTTP01ClusterIssuerName}, http01)).To(Succeed())
		acme, found, err = unstructured.NestedMap(http01.Object, "spec", "acme")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(acme).To(HaveKeyWithValue("email", "platform@example.com"))
		Expect(acme).To(HaveKeyWithValue("privateKeySecretRef",
			map[string]any{"name": acmeHTTP01AccountKeySecretName}),
			"its own account key: two registrations must not race over one")
		solvers, found, err = unstructured.NestedSlice(http01.Object, "spec", "acme", "solvers")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(solvers).To(HaveLen(1))
		Expect(solvers[0]).To(HaveKeyWithValue("http01", map[string]any{
			"gatewayHTTPRoute": map[string]any{
				"parentRefs": []any{map[string]any{
					"group":     "gateway.networking.k8s.io",
					"kind":      "Gateway",
					"name":      SharedGatewayName,
					"namespace": PlatformNamespace,
				}},
			},
		}), "challenges are published as HTTPRoutes on the shared Gateway — the one "+
			"challenge that works for a zone the platform's DNS token cannot write to")

		By("requesting the wildcard into the secret the HTTPS listener reads")
		cert := wildcardCertificateObject()
		Expect(k8sClient.Get(ctx, certKey, cert)).To(Succeed())
		spec, found, err := unstructured.NestedMap(cert.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(spec).To(HaveKeyWithValue("secretName", WildcardTLSSecretName))
		Expect(spec).To(HaveKeyWithValue("dnsNames", []any{"*.apps.example.com"}))
		Expect(spec).To(HaveKeyWithValue("issuerRef", map[string]any{
			"name":  acmeClusterIssuerName,
			"kind":  "ClusterIssuer",
			"group": "cert-manager.io",
		}))

		By("reporting the request as filed but not yet issued")
		cond := conditionOn(singletonKey)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("Issuing"))
		Expect(cond.Message).To(Equal("waiting for cert-manager to issue the certificate"))
	})

	It("mirrors the Ready condition cert-manager writes, message and all", func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: singletonKey})
		Expect(err).NotTo(HaveOccurred())

		writeReady := func(status, message string) {
			cert := wildcardCertificateObject()
			ExpectWithOffset(1, k8sClient.Get(ctx, certKey, cert)).To(Succeed())
			ExpectWithOffset(1, unstructured.SetNestedSlice(cert.Object, []any{
				map[string]any{
					"type":               "Ready",
					"status":             status,
					"reason":             "TestWritten",
					"message":            message,
					"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
				},
			}, "status", "conditions")).To(Succeed())
			ExpectWithOffset(1, k8sClient.Status().Update(ctx, cert)).To(Succeed())
		}

		By("carrying an issued certificate through to the Kitchen object")
		writeReady("True", certManagerReadyMessage)
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: singletonKey})
		Expect(err).NotTo(HaveOccurred())

		cond := conditionOn(singletonKey)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Issued"))
		Expect(cond.Message).To(Equal(certManagerReadyMessage))

		By("carrying a failure through as the only place it is ever explained")
		const refused = "propagation check failed: DNS record for \"apps.example.com\" not yet propagated"
		writeReady("False", refused)
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: singletonKey})
		Expect(err).NotTo(HaveOccurred())

		cond = conditionOn(singletonKey)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("Issuing"))
		Expect(cond.Message).To(Equal(refused),
			"the reason a certificate is not issued is reported here and nowhere else")
	})

	It("waits for the cert-manager API rather than failing the reconcile", func() {
		// The case the whole design exists for: on a first install neither
		// object can be admitted until cert-manager's webhook is serving, so
		// the reconcile has to come back later instead of erroring out and
		// leaving the Gateway unbuilt.
		reconciler.Client = certManagerAbsent{k8sClient}

		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: singletonKey})
		Expect(err).NotTo(HaveOccurred(),
			"an API that is not served yet is a state to wait through, not a failure")
		Expect(result.RequeueAfter).To(Equal(30 * time.Second))

		cond := conditionOn(singletonKey)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("CertManagerUnavailable"))
		Expect(cond.Message).To(ContainSubstring("waiting for the cert-manager API"))

		By("still building everything that does not go through cert-manager")
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: SharedGatewayName, Namespace: PlatformNamespace,
		}, &gatewayv1.Gateway{})).To(Succeed(),
			"the Gateway must not wait on an API only the certificate needs")
	})

	It("leaves issued certificates alone when the mode changes", func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: singletonKey})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, issuerKey, acmeIssuerObject())).To(Succeed())

		kitchen := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
		kitchen.Spec.TLS.Mode = kitchenv1alpha1.TLSModeCloudflared
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: singletonKey})
		Expect(err).NotTo(HaveOccurred())

		Expect(conditionOn(singletonKey)).To(BeNil(),
			"the platform manages no certificate in this mode, so it reports on none")

		// ACME limits how often the same names may be re-issued, so a mode
		// flipped back has to find the certificate it left behind.
		Expect(k8sClient.Get(ctx, issuerKey, acmeIssuerObject())).To(Succeed())
		Expect(k8sClient.Get(ctx, certKey, wildcardCertificateObject())).To(Succeed())
	})
})

// The two states below are refused at admission now, so they are only
// reachable on an object written before the CRD carried those rules, or on a
// cluster whose CRDs are managed out of band and left behind. The conditions
// are what a reconciler running against a schema older than itself can still
// say, so they are exercised against the reconciler directly rather than
// through an API server that would not accept the object at all.
var _ = Describe("Kitchen edge TLS on an older schema", func() {
	ctx := context.Background()

	var (
		kitchen    *kitchenv1alpha1.Kitchen
		reconciler *KitchenReconciler
	)

	setCond := func(condType string, status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&kitchen.Status.Conditions, metav1.Condition{
			Type: condType, Status: status, Reason: reason, Message: message,
		})
	}

	condition := func() *metav1.Condition {
		return meta.FindStatusCondition(kitchen.Status.Conditions, condCertificateReady)
	}

	BeforeEach(func() {
		reconciler = &KitchenReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		kitchen = &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeACME},
			},
		}
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, acmeIssuerObject()))).To(Succeed())
	})

	It("says an acme mode with no acme block manages no certificate", func() {
		Expect(reconciler.reconcileTLS(ctx, kitchen, setCond)).To(BeFalse())

		cond := condition()
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("ACMEConfigMissing"))
		Expect(cond.Message).To(ContainSubstring("spec.tls.acme is unset"))
	})

	It("says an acme block with no solver cannot issue a wildcard", func() {
		kitchen.Spec.TLS.ACME = &kitchenv1alpha1.ACMESpec{Email: "platform@example.com"}

		Expect(reconciler.reconcileTLS(ctx, kitchen, setCond)).To(BeFalse())

		cond := condition()
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("SolverMissing"))
		Expect(cond.Message).To(ContainSubstring("DNS-01"))
	})

	It("falls back to the production directory for an issuer with no server", func() {
		// spec.tls.acme.server defaults in the CRD, so only an object written
		// before the field existed reaches the operator without one.
		acme := acmeTLS().ACME
		Expect(acme.Server).To(BeEmpty(), "the CRD default is what fills this in")

		Expect(reconciler.applyACMEClusterIssuer(ctx, acme)).To(Succeed())

		issuer := acmeIssuerObject()
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: acmeClusterIssuerName}, issuer)).To(Succeed())
		server, found, err := unstructured.NestedString(issuer.Object, "spec", "acme", "server")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(server).To(Equal(defaultACMEServer))
	})
})

// What the CRD refuses outright, so that a Kitchen which cannot work is a
// failed `kubectl apply` rather than a condition on an object the API server
// accepted. The reconcile-time conditions above stay for the states admission
// cannot see — cert-manager not being installed is the obvious one.
var _ = Describe("Kitchen admission", func() {
	ctx := context.Background()

	const name = "admission-probe"

	kitchenWith := func(tls kitchenv1alpha1.TLSSpec) *kitchenv1alpha1.Kitchen {
		return &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        tls,
			},
		}
	}

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx,
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: name}}))).To(Succeed())
	})

	It("refuses acme mode with no acme block", func() {
		// The whole tls block is left at its zero value: mode defaults to
		// acme, which is exactly the object someone gets by asking for the
		// least configuration possible.
		err := k8sClient.Create(ctx, kitchenWith(kitchenv1alpha1.TLSSpec{}))
		Expect(errors.IsInvalid(err)).To(BeTrue(), "expected a validation failure, got %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.tls.acme is required when tls.mode is acme"))
	})

	It("refuses an acme block that names no solver", func() {
		err := k8sClient.Create(ctx, kitchenWith(kitchenv1alpha1.TLSSpec{
			Mode: kitchenv1alpha1.TLSModeACME,
			ACME: &kitchenv1alpha1.ACMESpec{Email: "platform@example.com"},
		}))
		Expect(errors.IsInvalid(err)).To(BeTrue(), "expected a validation failure, got %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.tls.acme.dns01 needs a solver"))
	})

	It("admits the modes that manage no certificate without one", func() {
		for _, mode := range []kitchenv1alpha1.TLSMode{
			kitchenv1alpha1.TLSModeNone,
			kitchenv1alpha1.TLSModeCloudflared,
		} {
			kitchen := kitchenWith(kitchenv1alpha1.TLSSpec{Mode: mode})
			Expect(k8sClient.Create(ctx, kitchen)).To(Succeed(), "mode %q", mode)
			Expect(k8sClient.Delete(ctx, kitchen)).To(Succeed())
		}
	})

	It("admits the documented acme install", func() {
		Expect(k8sClient.Create(ctx, kitchenWith(acmeTLS()))).To(Succeed())
	})
})

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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The address the chart writes into the connection secret for a release named
// "kitchen": one Service name, which is also the name the certificate has to
// be issued for or nothing verifies.
const telemetryHost = "kitchen-clickhouse.kitchen-system.svc"

// storeCertificateName is what the chart asks the operator to fill.
const storeCertificateName = "kitchen-clickhouse-tls"

// A certificate is issued for the address the client dialled, and for the
// shortenings of it a cluster resolver also accepts — because verify-full
// fails on the name, and the name is whichever of these the client happened
// to use.
func TestServiceDNSNamesCoverEveryWayTheStoreIsAddressed(t *testing.T) {
	names := serviceDNSNames(telemetryHost)
	want := []string{
		telemetryHost,
		"kitchen-clickhouse.kitchen-system.svc.cluster.local",
		"kitchen-clickhouse.kitchen-system",
		"kitchen-clickhouse",
	}
	if len(names) != len(want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	for i, name := range want {
		if names[i] != name {
			t.Errorf("name %d is %q, want %q", i, names[i], name)
		}
	}
	if names[0] != telemetryHost {
		t.Error("the configured address is not the certificate's first name, so a certificate " +
			"cannot be read as being for the address it was issued for")
	}

	// An external store is addressed however its own operator addresses it,
	// and a SAN nobody asked for is worse than one fewer.
	if got := serviceDNSNames("clickhouse.example.com"); len(got) != 1 || got[0] != "clickhouse.example.com" {
		t.Errorf("names for an address outside the cluster: %v, want the address alone", got)
	}
	if got := serviceDNSNames("10.0.0.4"); len(got) != 1 {
		// An address is not a DNS name, but it parses as a DNS-1123
		// subdomain; nothing is invented around it either way.
		t.Errorf("names for a literal address: %v", got)
	}
	if got := serviceDNSNames("   "); got != nil {
		t.Errorf("names for an empty host: %v, want none", got)
	}
}

var _ = Describe("The platform's internal CA", func() {
	ctx := context.Background()

	singletonKey := types.NamespacedName{Name: KitchenSingletonName}
	caCertKey := types.NamespacedName{Name: InternalCACertificateName, Namespace: PlatformNamespace}
	storeCertKey := types.NamespacedName{Name: storeCertificateName, Namespace: PlatformNamespace}
	bundleKey := types.NamespacedName{Name: InternalCAConfigMapName, Namespace: PlatformNamespace}

	var reconciler *KitchenReconciler
	var tlsIssuer *InternalTLSReconciler

	// The two halves, in the order the cluster runs them: the Secret-driven
	// controller issues, and the Kitchen singleton reports. They are separate
	// controllers because the singleton is a Helm post-install hook and the
	// store's pod waits for its certificate — one reconciler doing both would
	// deadlock a first install.
	issueOnce := func() {
		_, err := tlsIssuer.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: PlatformNamespace, Name: telemetrySecretName,
		}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	reportOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: singletonKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	reconcileOnce := func() {
		issueOnce()
		reportOnce()
	}

	certificate := func(name string) *unstructured.Unstructured {
		return certManagerObject("Certificate", name, PlatformNamespace)
	}
	issuerObject := func(name string) *unstructured.Unstructured {
		return certManagerObject("Issuer", name, PlatformNamespace)
	}

	conditionOn := func() *metav1.Condition {
		kitchen := &kitchenv1alpha1.Kitchen{}
		ExpectWithOffset(1, k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
		return meta.FindStatusCondition(kitchen.Status.Conditions, condInternalCAReady)
	}

	// writeIssued puts cert-manager's own Ready condition on a Certificate,
	// which is the only thing that tells the operator a certificate exists.
	writeIssued := func(key types.NamespacedName) {
		cert := certificate(key.Name)
		ExpectWithOffset(1, k8sClient.Get(ctx, key, cert)).To(Succeed())
		ExpectWithOffset(1, unstructured.SetNestedSlice(cert.Object, []any{
			map[string]any{
				"type":               "Ready",
				"status":             "True",
				"reason":             "Ready",
				"message":            certManagerReadyMessage,
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
			},
		}, "status", "conditions")).To(Succeed())
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, cert)).To(Succeed())
	}

	// connectionSecret is what the chart writes: where the store is, and —
	// when the chart deployed it — which Secret its certificate belongs in.
	connectionSecret := func(data map[string]string) {
		// The platform namespace is the reconciler's to create, and this runs
		// before the first reconcile of the spec under test.
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: telemetrySecretName, Namespace: PlatformNamespace},
			StringData: data,
		}))).To(Succeed())
	}

	BeforeEach(func() {
		reconciler = &KitchenReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		tlsIssuer = &InternalTLSReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				// Deliberately not acme: the platform's internal CA signs
				// itself and asks nobody for anything, so an installation
				// that publishes nothing over TLS still encrypts what happens
				// inside its own namespace.
				TLS: kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeNone},
				Observability: kitchenv1alpha1.ObservabilitySpec{
					ClickHouse: kitchenv1alpha1.ClickHouseSpec{
						SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: telemetrySecretName},
					},
				},
			},
		})
	})

	AfterEach(func() {
		for _, obj := range []client.Object{
			certificate(InternalCACertificateName),
			certificate(storeCertificateName),
			issuerObject(internalSelfSignedIssuerName),
			issuerObject(internalCAIssuerName),
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: InternalCAConfigMapName, Namespace: PlatformNamespace,
			}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: InternalCASecretName, Namespace: PlatformNamespace,
			}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: telemetrySecretName, Namespace: PlatformNamespace,
			}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("builds a CA out of cert-manager and issues the store a certificate from it", func() {
		connectionSecret(map[string]string{
			clickhouse.SecretKeyHost:              telemetryHost,
			clickhouse.SecretKeyHTTPPort:          "8443",
			clickhouse.SecretKeyDatabase:          "kitchen",
			clickhouse.SecretKeyUsername:          "kitchen",
			clickhouse.SecretKeyPassword:          "hunter2",
			clickhouse.SecretKeyScheme:            clickhouse.SchemeHTTPS,
			clickhouse.SecretKeyCAFile:            "/etc/kitchen/internal-ca/ca.crt",
			clickhouse.SecretKeyCertificateSecret: storeCertificateName,
		})

		reconcileOnce()

		By("signing the CA's own certificate with an issuer that signs nothing else")
		selfSigned := issuerObject(internalSelfSignedIssuerName)
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: internalSelfSignedIssuerName, Namespace: PlatformNamespace,
		}, selfSigned)).To(Succeed())
		spec, found, err := unstructured.NestedMap(selfSigned.Object, "spec", "selfSigned")
		Expect(spec).To(BeEmpty(), "selfSigned takes no configuration and must stay empty")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())

		By("requesting a CA certificate, which is what the rest of it hangs from")
		ca := certificate(InternalCACertificateName)
		Expect(k8sClient.Get(ctx, caCertKey, ca)).To(Succeed())
		spec, found, err = unstructured.NestedMap(ca.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(spec).To(HaveKeyWithValue("isCA", true),
			"without isCA cert-manager issues a leaf, and the CA Issuer below signs nothing")
		Expect(spec).To(HaveKeyWithValue("secretName", InternalCASecretName))
		Expect(spec).To(HaveKeyWithValue("issuerRef", map[string]any{
			"name":  internalSelfSignedIssuerName,
			"kind":  "Issuer",
			"group": "cert-manager.io",
		}))

		By("holding the store's certificate back until the CA exists")
		Expect(k8sClient.Get(ctx, storeCertKey, certificate(storeCertificateName))).NotTo(Succeed())
		cond := conditionOn()
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("Issuing"))

		By("publishing the CA certificate, and not its key, once cert-manager has issued it")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: InternalCASecretName, Namespace: PlatformNamespace},
			Type:       corev1.SecretTypeTLS,
			StringData: map[string]string{
				"ca.crt":  "-- the platform's CA --",
				"tls.crt": "-- the platform's CA --",
				"tls.key": "-- the key nothing but cert-manager may hold --",
			},
		})).To(Succeed())
		writeIssued(caCertKey)

		reconcileOnce()

		bundle := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, bundleKey, bundle)).To(Succeed())
		Expect(bundle.Data).To(HaveKeyWithValue(InternalCABundleKey, "-- the platform's CA --"))
		Expect(bundle.Data).NotTo(HaveKey("tls.key"),
			"the bundle is mounted into every client of a store; the authority for the whole "+
				"namespace must not travel with it")

		By("signing with the CA from then on")
		caIssuer := issuerObject(internalCAIssuerName)
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: internalCAIssuerName, Namespace: PlatformNamespace,
		}, caIssuer)).To(Succeed())
		signsWith, found, err := unstructured.NestedString(caIssuer.Object, "spec", "ca", "secretName")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(signsWith).To(Equal(InternalCASecretName))

		By("issuing the store a certificate for every name a client reaches it by")
		store := certificate(storeCertificateName)
		Expect(k8sClient.Get(ctx, storeCertKey, store)).To(Succeed())
		spec, found, err = unstructured.NestedMap(store.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(spec).To(HaveKeyWithValue("secretName", storeCertificateName))
		Expect(spec).To(HaveKeyWithValue("dnsNames", []any{
			telemetryHost,
			"kitchen-clickhouse.kitchen-system.svc.cluster.local",
			"kitchen-clickhouse.kitchen-system",
			"kitchen-clickhouse",
		}))
		Expect(spec).To(HaveKeyWithValue("issuerRef", map[string]any{
			"name":  internalCAIssuerName,
			"kind":  "Issuer",
			"group": "cert-manager.io",
		}))

		By("reporting the namespace encrypted only once the store's certificate is issued")
		Expect(conditionOn().Status).To(Equal(metav1.ConditionFalse))
		writeIssued(storeCertKey)
		reconcileOnce()

		cond = conditionOn()
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Issued"))
		Expect(cond.Message).To(ContainSubstring(telemetryHost))

		By("putting it in the list an operator already reads")
		kitchen := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
		var row *kitchenv1alpha1.ComponentStatus
		for i := range kitchen.Status.Components {
			if kitchen.Status.Components[i].Name == internalCAComponentName {
				row = &kitchen.Status.Components[i]
			}
		}
		Expect(row).NotTo(BeNil(),
			"a CA that never issued leaves the store's pod waiting for a Secret and every "+
				"telemetry condition reading \"cannot connect\"; the survey is where that is visible")
		Expect(row.Healthy).To(BeTrue())
	})

	It("says so when the store is left answering in the clear", func() {
		connectionSecret(map[string]string{
			clickhouse.SecretKeyHost:     telemetryHost,
			clickhouse.SecretKeyHTTPPort: "8123",
			clickhouse.SecretKeyDatabase: "kitchen",
			clickhouse.SecretKeyUsername: "kitchen",
			clickhouse.SecretKeyPassword: "hunter2",
			clickhouse.SecretKeyScheme:   clickhouse.SchemeHTTP,
		})

		reconcileOnce()

		cond := conditionOn()
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("StoreInTheClear"))
		Expect(cond.Message).To(ContainSubstring(PlatformNamespace))

		By("issuing nothing, because nothing asked it to")
		Expect(k8sClient.Get(ctx, caCertKey, certificate(InternalCACertificateName))).NotTo(Succeed())

		By("not holding the platform short of Ready over a choice somebody made")
		kitchen := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
		for _, component := range kitchen.Status.Components {
			Expect(component.Name).NotTo(Equal(internalCAComponentName))
		}
	})

	It("issues nothing for a store whose certificate is somebody else's", func() {
		connectionSecret(map[string]string{
			clickhouse.SecretKeyHost:     "clickhouse.telemetry.example.com",
			clickhouse.SecretKeyHTTPPort: "8443",
			clickhouse.SecretKeyDatabase: "kitchen",
			clickhouse.SecretKeyUsername: "kitchen",
			clickhouse.SecretKeyPassword: "hunter2",
			clickhouse.SecretKeyScheme:   clickhouse.SchemeHTTPS,
		})

		reconcileOnce()

		Expect(conditionOn()).To(BeNil(),
			"an external store over TLS is not the internal CA's business, and a condition "+
				"about a CA nothing uses is noise in the one list this is said in")
		Expect(k8sClient.Get(ctx, caCertKey, certificate(InternalCACertificateName))).NotTo(Succeed())
	})
})

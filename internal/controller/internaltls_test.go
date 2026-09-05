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
	"os"
	"path/filepath"
	"strings"
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
	"github.com/Bermos/Kitchen/internal/accountsdb"
	"github.com/Bermos/Kitchen/internal/backup"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

// The address the chart writes into the connection secret for a release named
// "kitchen": one Service name, which is also the name the certificate has to
// be issued for or nothing verifies.
const telemetryHost = "kitchen-clickhouse.kitchen-system.svc"

// storeCertificateName is what the chart asks the operator to fill.
const storeCertificateName = "kitchen-clickhouse-tls"

// The same two facts for the accounts database and for the object store, whose
// secrets say them in the same words — one vocabulary, so one controller
// issues for all three.
const (
	accountsHost            = "kitchen-postgres.kitchen-system.svc"
	accountsCertificateName = "kitchen-postgres-tls"

	objectStoreHost            = "kitchen-objectstore.kitchen-system.svc"
	objectStoreCertificateName = "kitchen-objectstore-tls"
)

// Every bundled store's connection secret spells the two keys this controller
// reads the same way, which is what lets one controller serve all of them and
// the chart add a store by writing a secret. A rename on one side alone would
// be a store that is silently never issued for.
func TestEveryStoreSpeaksOneConnectionSecretVocabulary(t *testing.T) {
	for _, spelling := range []struct{ what, key, want string }{
		{"the telemetry store's host", clickhouse.SecretKeyHost, connectionSecretKeyHost},
		{"the accounts database's host", accountsdb.SecretKeyHost, connectionSecretKeyHost},
		{"the object store's host", objectstore.SecretKeyHost, connectionSecretKeyHost},
		{
			"the telemetry store's certificate request",
			clickhouse.SecretKeyCertificateSecret, connectionSecretKeyCertificateSecret,
		},
		{
			"the accounts database's certificate request",
			accountsdb.SecretKeyCertificateSecret, connectionSecretKeyCertificateSecret,
		},
		{
			"the object store's certificate request",
			objectstore.SecretKeyCertificateSecret, connectionSecretKeyCertificateSecret,
		},
	} {
		if spelling.key != spelling.want {
			t.Errorf("%s is spelled %q, and this controller reads %q: nothing would ever be "+
				"issued for that store, and nothing would say so", spelling.what,
				spelling.key, spelling.want)
		}
	}
}

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

// The operator writes a backup run's pod itself, so the path it mounts the CA
// bundle at is a Go constant — and the path every connection secret names is
// the chart's helper. A rename on one side alone is a scheduled backup that
// cannot reach the accounts database, on a night nobody is watching.
func TestTheCABundleIsMountedWhereTheChartSaysItIs(t *testing.T) {
	path := filepath.Join("..", "..", "charts", "kitchen", "templates", "_helpers.tpl")
	helpers, err := os.ReadFile(path) //nolint:gosec // a fixed path inside the repository
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !strings.Contains(string(helpers), `{{- "`+InternalCAMountPath+`" }}`) {
		t.Errorf("the chart does not mount the CA bundle at %s, which is where every pod the "+
			"operator writes looks for it", InternalCAMountPath)
	}
	// And the file inside it that a backup run verifies a destination in this
	// cluster against, which is a constant of its own in a package that cannot
	// import this one.
	if want := InternalCAMountPath + "/" + InternalCABundleKey; backup.InternalCAFile != want {
		t.Errorf("a backup run reads its CA at %s and the pod mounts one at %s: the upload "+
			"fails on a file that is not there", backup.InternalCAFile, want)
	}
}

var _ = Describe("The platform's internal CA", func() {
	ctx := context.Background()

	singletonKey := types.NamespacedName{Name: KitchenSingletonName}
	caCertKey := types.NamespacedName{Name: InternalCACertificateName, Namespace: PlatformNamespace}
	storeCertKey := types.NamespacedName{Name: storeCertificateName, Namespace: PlatformNamespace}
	accountsCertKey := types.NamespacedName{
		Name: accountsCertificateName, Namespace: PlatformNamespace,
	}
	bundleKey := types.NamespacedName{Name: InternalCAConfigMapName, Namespace: PlatformNamespace}

	var reconciler *KitchenReconciler
	var tlsIssuer *InternalTLSReconciler

	// The two halves, in the order the cluster runs them: the Secret-driven
	// controller issues, and the Kitchen singleton reports. They are separate
	// controllers because the singleton is a Helm post-install hook and the
	// store's pod waits for its certificate — one reconciler doing both would
	// deadlock a first install.
	issueFor := func(secretName string) {
		_, err := tlsIssuer.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: PlatformNamespace, Name: secretName,
		}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	issueOnce := func() {
		issueFor(telemetrySecretName)
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

	// accountsSecret is the identity provider's connection secret, which says
	// the same two things about the accounts database in the same two keys.
	accountsSecret := func(data map[string]string) {
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: accountsdb.DefaultSecretName, Namespace: PlatformNamespace,
			},
			StringData: data,
		}))).To(Succeed())
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

	// objectStoreSecret is the other secret the chart writes, and the other
	// store one controller issues for. The singleton has to name the store
	// too: nothing is issued for a store the platform does not deploy.
	objectStoreSecret := func(data map[string]string) {
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: objectStoreChartSecretName, Namespace: PlatformNamespace},
			StringData: data,
		}))).To(Succeed())

		kitchen := &kitchenv1alpha1.Kitchen{}
		ExpectWithOffset(1, k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
		kitchen.Spec.ObjectStore = kitchenv1alpha1.ObjectStoreSpec{
			Enabled:   true,
			Service:   "kitchen-objectstore",
			Port:      9000,
			SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: objectStoreChartSecretName},
		}
		ExpectWithOffset(1, k8sClient.Update(ctx, kitchen)).To(Succeed())
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
			certificate(accountsCertificateName),
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: accountsdb.DefaultSecretName, Namespace: PlatformNamespace,
			}},
			certificate(objectStoreCertificateName),
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: objectStoreChartSecretName, Namespace: PlatformNamespace,
			}},
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{
				Name: ObjectStoreConnectionName, Namespace: PlatformNamespace,
			}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: ObjectStoreCredentialsSecretName, Namespace: PlatformNamespace,
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

	It("issues the object store a certificate for the name applications reach it by", func() {
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
		objectStoreSecret(map[string]string{
			objectstore.CredentialKeyAccessKeyID:     "kitchen",
			objectstore.CredentialKeySecretAccessKey: "hunter2hunter2",
			objectstore.SecretKeyHost:                objectStoreHost,
			objectstore.SecretKeyScheme:              objectstore.SchemeHTTPS,
			objectstore.SecretKeyCAFile:              "/etc/kitchen/internal-ca/ca.crt",
			objectstore.SecretKeyCertificateSecret:   objectStoreCertificateName,
		})

		By("getting there the same way the telemetry store does: the CA first")
		reconcileOnce()
		issueFor(objectStoreChartSecretName)
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
		issueOnce()
		issueFor(objectStoreChartSecretName)

		By("issuing for every name a client in the cluster dials, applications included")
		cert := certificate(objectStoreCertificateName)
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: objectStoreCertificateName, Namespace: PlatformNamespace,
		}, cert)).To(Succeed())
		spec, found, err := unstructured.NestedMap(cert.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(spec).To(HaveKeyWithValue("secretName", objectStoreCertificateName))
		Expect(spec).To(HaveKeyWithValue("dnsNames", []any{
			objectStoreHost,
			// The one an application's binding carries, which is the whole
			// reason the store's SANs are not just the address the operator
			// happens to use.
			"kitchen-objectstore.kitchen-system.svc.cluster.local",
			"kitchen-objectstore.kitchen-system",
			"kitchen-objectstore",
		}))

		By("holding the condition until both stores' certificates are issued")
		reportOnce()
		Expect(conditionOn().Status).To(Equal(metav1.ConditionFalse))
		writeIssued(storeCertKey)
		reportOnce()
		Expect(conditionOn().Status).To(Equal(metav1.ConditionFalse),
			"one condition covers every bundled store, and it is as good as the weakest")
		writeIssued(types.NamespacedName{
			Name: objectStoreCertificateName, Namespace: PlatformNamespace,
		})
		reportOnce()

		cond := conditionOn()
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Issued"))
		Expect(cond.Message).To(ContainSubstring(telemetryHost))
		Expect(cond.Message).To(ContainSubstring(objectStoreHost))
	})

	It("names the object store when it is the one left in the clear", func() {
		// A telemetry store of somebody else's, over TLS: not the internal
		// CA's business, so the object store is the only store with anything
		// to say and the message is entirely about it.
		connectionSecret(map[string]string{
			clickhouse.SecretKeyHost:     "clickhouse.telemetry.example.com",
			clickhouse.SecretKeyHTTPPort: "8443",
			clickhouse.SecretKeyDatabase: "kitchen",
			clickhouse.SecretKeyUsername: "kitchen",
			clickhouse.SecretKeyPassword: "hunter2",
			clickhouse.SecretKeyScheme:   clickhouse.SchemeHTTPS,
		})
		objectStoreSecret(map[string]string{
			objectstore.CredentialKeyAccessKeyID:     "kitchen",
			objectstore.CredentialKeySecretAccessKey: "hunter2hunter2",
			objectstore.SecretKeyHost:                objectStoreHost,
			objectstore.SecretKeyScheme:              objectstore.SchemeHTTP,
		})

		reconcileOnce()

		cond := conditionOn()
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("StoreInTheClear"))
		Expect(cond.Message).To(ContainSubstring("objectStore.tls.enabled"))
		Expect(cond.Message).To(ContainSubstring("application namespace"),
			"the object store is the one bundled store applications reach, so what crosses "+
				"in the clear crosses between namespaces too")
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

	It("issues the accounts database a certificate from the same CA", func() {
		// Both stores, because the platform's report is about the namespace
		// rather than about one store in it: a namespace with one encrypted
		// store and one in the clear is a readable namespace.
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
		accountsSecret(map[string]string{
			accountsdb.SecretKeyHost:              accountsHost,
			accountsdb.SecretKeyPort:              "5432",
			accountsdb.SecretKeyDatabase:          "kitchen_auth",
			accountsdb.SecretKeyUsername:          "kitchen",
			accountsdb.SecretKeyPassword:          "hunter2",
			accountsdb.SecretKeySSLMode:           accountsdb.SSLModeVerifyFull,
			accountsdb.SecretKeyCAFile:            "/etc/kitchen/internal-ca/ca.crt",
			accountsdb.SecretKeyCertificateSecret: accountsCertificateName,
		})

		By("building the CA once, however many stores ask for one")
		issueFor(telemetrySecretName)
		issueFor(accountsdb.DefaultSecretName)
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
		issueFor(telemetrySecretName)
		issueFor(accountsdb.DefaultSecretName)

		By("issuing it for every name a client in the cluster reaches the Service by")
		accounts := certificate(accountsCertificateName)
		Expect(k8sClient.Get(ctx, accountsCertKey, accounts)).To(Succeed())
		spec, found, err := unstructured.NestedMap(accounts.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(spec).To(HaveKeyWithValue("secretName", accountsCertificateName))
		Expect(spec).To(HaveKeyWithValue("dnsNames", []any{
			accountsHost,
			"kitchen-postgres.kitchen-system.svc.cluster.local",
			"kitchen-postgres.kitchen-system",
			"kitchen-postgres",
		}))
		Expect(spec).To(HaveKeyWithValue("issuerRef", map[string]any{
			"name":  internalCAIssuerName,
			"kind":  "Issuer",
			"group": "cert-manager.io",
		}))

		By("holding the report back while either store's certificate is still coming")
		writeIssued(storeCertKey)
		reportOnce()
		cond := conditionOn()
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("Issuing"))
		Expect(cond.Message).To(ContainSubstring("accounts database"),
			"a namespace is as encrypted as its weakest store, and the condition has to say "+
				"which one is holding it up")

		By("reporting the namespace encrypted once both are issued")
		writeIssued(accountsCertKey)
		reportOnce()
		cond = conditionOn()
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Issued"))
		Expect(cond.Message).To(ContainSubstring(telemetryHost))
		Expect(cond.Message).To(ContainSubstring(accountsHost))
	})

	It("says so when the accounts database is left in the clear beside an encrypted store", func() {
		connectionSecret(map[string]string{
			clickhouse.SecretKeyHost:              telemetryHost,
			clickhouse.SecretKeyHTTPPort:          "8443",
			clickhouse.SecretKeyDatabase:          "kitchen",
			clickhouse.SecretKeyUsername:          "kitchen",
			clickhouse.SecretKeyPassword:          "hunter2",
			clickhouse.SecretKeyScheme:            clickhouse.SchemeHTTPS,
			clickhouse.SecretKeyCertificateSecret: storeCertificateName,
		})
		// postgres.tls.enabled=false: a DSN with no sslmode, which for both
		// drivers in front of this database is a connection in the clear.
		accountsSecret(map[string]string{
			accountsdb.SecretKeyHost:     accountsHost,
			accountsdb.SecretKeyPort:     "5432",
			accountsdb.SecretKeyDatabase: "kitchen_auth",
			accountsdb.SecretKeyUsername: "kitchen",
			accountsdb.SecretKeyPassword: "hunter2",
		})

		issueFor(telemetrySecretName)
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: InternalCASecretName, Namespace: PlatformNamespace},
			Type:       corev1.SecretTypeTLS,
			StringData: map[string]string{
				"ca.crt": "-- the platform's CA --", "tls.crt": "-- the platform's CA --",
				"tls.key": "-- the key nothing but cert-manager may hold --",
			},
		})).To(Succeed())
		writeIssued(caCertKey)
		issueFor(telemetrySecretName)
		writeIssued(storeCertKey)
		reportOnce()

		cond := conditionOn()
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("StoreInTheClear"))
		Expect(cond.Message).To(ContainSubstring("accounts database"),
			"one encrypted store must not be allowed to speak for the namespace")
		Expect(cond.Message).To(ContainSubstring(PlatformNamespace))

		By("issuing nothing for it, because nothing asked")
		Expect(k8sClient.Get(ctx, accountsCertKey,
			certificate(accountsCertificateName))).NotTo(Succeed())

		By("not holding the platform short of Ready over a choice somebody made")
		kitchen := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
		for _, component := range kitchen.Status.Components {
			Expect(component.Name).NotTo(Equal(internalCAComponentName))
		}
	})

	It("issues nothing for an accounts database whose certificate is somebody else's", func() {
		// An external Postgres with `postgres.external.sslmode` set: verified
		// against the host's roots, which the platform's CA is not in.
		accountsSecret(map[string]string{
			accountsdb.SecretKeyHost:     "postgres.databases.example.com",
			accountsdb.SecretKeyPort:     "5432",
			accountsdb.SecretKeyDatabase: "kitchen_auth",
			accountsdb.SecretKeyUsername: "kitchen",
			accountsdb.SecretKeyPassword: "hunter2",
			accountsdb.SecretKeySSLMode:  accountsdb.SSLModeVerifyFull,
		})
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
			"neither store's certificate is the platform's, so there is nothing here it can "+
				"say anything true about")
		Expect(k8sClient.Get(ctx, caCertKey, certificate(InternalCACertificateName))).NotTo(Succeed())
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

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

package database

import (
	"context"
	"net/url"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testPlatformIssuer is the ClusterIssuer the operator writes for the
// platform's internal CA. Its name is the operator's constant; spelling it
// here rather than importing it is what keeps this package free of a
// dependency on internal/controller, which imports this one.
const testPlatformIssuer = "kitchen-internal-ca"

// testServerSecret is what a certificate for the one cluster these tests
// provision is issued into.
const testServerSecret = testCluster + serverCertificateSuffix

// testPlatformCAPEM stands in for the platform's own root, told apart from
// the per-cluster one by its bytes alone.
const testPlatformCAPEM = "-----BEGIN CERTIFICATE-----\nkitchen-internal-ca\n-----END CERTIFICATE-----\n"

// cnpgTLSScheme is cnpgScheme plus the two cert-manager kinds this file
// writes and reads, registered the same way: unstructured, so that neither
// operator's Go module is in the build.
func cnpgTLSScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := cnpgScheme(t)
	for _, gvk := range []schema.GroupVersionKind{certificateGVK(), clusterIssuerGVK()} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
		metav1.AddToGroupVersion(scheme, gvk.GroupVersion())
	}
	return scheme
}

// platformCAIssuer is the ClusterIssuer as the operator writes it: backed by
// the CA Secret in the platform namespace, which nothing here ever reads.
func platformCAIssuer() *unstructured.Unstructured {
	issuer := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"ca": map[string]any{"secretName": testPlatformIssuer}},
	}}
	issuer.SetGroupVersionKind(clusterIssuerGVK())
	issuer.SetName(testPlatformIssuer)
	return issuer
}

// cnpgWithPlatformCA is a provisioner pointed at that issuer.
func cnpgWithPlatformCA(t *testing.T, objects ...client.Object) *CNPG {
	t.Helper()
	return &CNPG{
		Client: fake.NewClientBuilder().
			WithScheme(cnpgTLSScheme(t)).WithObjects(objects...).Build(),
		Namespace:      testDatabaseNamespace,
		Images:         DefaultPostgresImages,
		StorageSize:    DefaultStorageSize,
		Instances:      DefaultInstances,
		ServerCAIssuer: testPlatformIssuer,
	}
}

func getCertificate(t *testing.T, c *CNPG, name string) *unstructured.Unstructured {
	t.Helper()
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: name}
	if err := c.Client.Get(context.Background(), key, cert); err != nil {
		t.Fatalf("reading certificate %s: %v", name, err)
	}
	return cert
}

// The whole of step 5(a): a claim's database is signed by the one authority
// everything else this platform runs is signed by, and the private key of
// that authority stays where cert-manager put it.
func TestAClaimDatabaseAsksThePlatformCAForItsServerCertificate(t *testing.T) {
	cnpg := cnpgWithPlatformCA(t, platformCAIssuer())

	if _, err := cnpg.Provision(context.Background(), shopDB); err == nil {
		t.Fatal("a freshly created cluster reported itself ready")
	}

	cert := getCertificate(t, cnpg, testServerSecret)
	if got := nestedString(cert, "spec", "secretName"); got != testServerSecret {
		t.Fatalf("secretName %q, want %q", got, testServerSecret)
	}
	if got := nestedString(cert, "spec", "issuerRef", "kind"); got != "ClusterIssuer" {
		t.Fatalf("issuerRef kind %q — a namespaced Issuer cannot sign into %s without the CA's "+
			"private key being copied there, which is what #443 refuses", got, testDatabaseNamespace)
	}
	if got := nestedString(cert, "spec", "issuerRef", "name"); got != testPlatformIssuer {
		t.Fatalf("issuerRef name %q, want %q", got, testPlatformIssuer)
	}

	// Every name a client inside the cluster reaches the database by, so that
	// `verify-full` on the binding's host is not the only connection that can
	// be made to work.
	names, _, err := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		testCluster + "-rw",
		testCluster + "-rw." + testDatabaseNamespace + ".svc",
		testCluster + "-rw." + testDatabaseNamespace + ".svc.cluster.local",
		testCluster + "-ro." + testDatabaseNamespace + ".svc",
		testCluster + "-r." + testDatabaseNamespace + ".svc",
	} {
		if !slices.Contains(names, want) {
			t.Fatalf("the certificate does not cover %q; it covers %v", want, names)
		}
	}

	// And the Cluster names it under both halves of CloudNativePG's schema:
	// one Secret carries `ca.crt` for the first and the `tls.*` pair for the
	// second, which is exactly what cert-manager writes.
	cluster := getCluster(t, cnpg, testCluster)
	if got := nestedString(cluster, "spec", "certificates", "serverCASecret"); got != testServerSecret {
		t.Fatalf("spec.certificates.serverCASecret %q, want %q", got, testServerSecret)
	}
	if got := nestedString(cluster, "spec", "certificates", "serverTLSSecret"); got != testServerSecret {
		t.Fatalf("spec.certificates.serverTLSSecret %q, want %q", got, testServerSecret)
	}

	// The client half is deliberately untouched: CloudNativePG authenticates
	// its own `streaming_replica` with a client certificate it rotates for
	// itself, and naming a client CA without a replication certificate would
	// take that over.
	if got := nestedString(cluster, "spec", "certificates", "clientCASecret"); got != "" {
		t.Fatalf("clientCASecret %q — the replication identity is CloudNativePG's", got)
	}
	if got := nestedString(cluster, "spec", "certificates", "replicationTLSSecret"); got != "" {
		t.Fatalf("replicationTLSSecret %q — the replication identity is CloudNativePG's", got)
	}
}

// An installation with no platform CA — no cert-manager, or one that has not
// issued yet — provisions exactly what it provisioned before. A database that
// cannot start because its certificate is late is a worse failure than one
// signed by an authority of its own.
func TestWithoutThePlatformCATheDatabaseKeepsItsOwn(t *testing.T) {
	cnpg := cnpgWithPlatformCA(t)

	if _, err := cnpg.Provision(context.Background(), shopDB); err == nil {
		t.Fatal("a freshly created cluster reported itself ready")
	}

	cluster := getCluster(t, cnpg, testCluster)
	if _, found, err := unstructured.NestedMap(cluster.Object, "spec", "certificates"); err != nil || found {
		t.Fatalf("the cluster names certificates with no issuer to sign them (found=%v, err=%v)", found, err)
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK())
	err := cnpg.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testDatabaseNamespace, Name: testServerSecret}, cert)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a certificate was requested from an issuer that does not exist: %v", err)
	}
}

// The claim reports which of the two it got, so that "one CA" is readable
// without going and looking at a Cluster.
func TestTheInstanceReportsWhichAuthoritySignedIt(t *testing.T) {
	ours := readyCluster()
	ours.SetLabels(map[string]string{managedByLabel: managedByValue})
	withCA := cnpgWithPlatformCA(t, platformCAIssuer(), ours, appSecret(testCluster),
		platformIssuedSecret())
	instance, err := withCA.Provision(context.Background(), shopDB)
	if err != nil {
		t.Fatal(err)
	}
	if instance.CertificateAuthority != CAPlatform {
		t.Fatalf("certificate authority %q, want %q", instance.CertificateAuthority, CAPlatform)
	}

	theirs := readyCluster()
	theirs.SetLabels(map[string]string{managedByLabel: managedByValue})
	without := cnpgWithPlatformCA(t, theirs, appSecret(testCluster), caSecret(testCluster+"-ca"))
	instance, err = without.Provision(context.Background(), shopDB)
	if err != nil {
		t.Fatal(err)
	}
	if instance.CertificateAuthority != CAProvider {
		t.Fatalf("certificate authority %q, want %q", instance.CertificateAuthority, CAProvider)
	}
}

// platformIssuedSecret is what cert-manager writes: the server pair and the
// authority that signed it, under the key CloudNativePG reads a server CA by.
func platformIssuedSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testServerSecret, Namespace: testDatabaseNamespace},
		Data: map[string][]byte{
			caCertificateKey:        []byte(testPlatformCAPEM),
			corev1.TLSCertKey:       []byte("the server certificate"),
			corev1.TLSPrivateKeyKey: []byte("the server key"),
		},
	}
}

// The migration, and the whole of what an upgrade does to a database that
// already exists: the block is patched on once — CloudNativePG rolls the
// instances to pick the certificate up — and every reconcile after that
// leaves the Cluster alone.
func TestAnExistingDatabaseIsReissuedOnceAndThenLeftAlone(t *testing.T) {
	existing := readyCluster()
	existing.SetLabels(map[string]string{managedByLabel: managedByValue})
	cnpg := cnpgWithPlatformCA(t, platformCAIssuer(), existing, appSecret(testCluster),
		platformIssuedSecret())

	if _, err := cnpg.Provision(context.Background(), shopDB); err != nil {
		t.Fatal(err)
	}
	cluster := getCluster(t, cnpg, testCluster)
	if got := serverCertificatesOf(cluster); got != testServerSecret {
		t.Fatalf("the existing cluster still names %q, so an installation upgrading into this "+
			"would never move off CloudNativePG's own CA", got)
	}
	first := cluster.GetResourceVersion()

	if _, err := cnpg.Provision(context.Background(), shopDB); err != nil {
		t.Fatal(err)
	}
	if got := getCluster(t, cnpg, testCluster).GetResourceVersion(); got != first {
		t.Fatal("a second reconcile rewrote the certificates block, which is a rolling restart " +
			"of every claim database on every pass")
	}
}

// A Cluster this platform did not create is never written to, here as
// everywhere else: an operator who handed the platform a database is not
// having its server identity replaced underneath them.
func TestADatabaseThePlatformDidNotCreateIsNotReissued(t *testing.T) {
	adopted := readyCluster()
	adopted.SetLabels(nil)
	cnpg := cnpgWithPlatformCA(t, platformCAIssuer(), adopted, appSecret(testCluster),
		caSecret(testCluster+"-ca"))

	if _, err := cnpg.Provision(context.Background(), shopDB); err != nil {
		t.Fatal(err)
	}
	if got := serverCertificatesOf(getCluster(t, cnpg, testCluster)); got != "" {
		t.Fatalf("a cluster the platform does not manage was given %q", got)
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK())
	err := cnpg.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testDatabaseNamespace, Name: testServerSecret}, cert)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a certificate was issued for a database the platform does not manage "+
			"and would never name it: %v", err)
	}
}

// Nothing an application reads moves. The binding is composed from the
// Cluster's published CA Secret whichever authority it is, so the URL, the
// mounted path and the key the certificate travels in are the same as before
// — which is what makes this a `feat` and not a breaking change.
func TestThePlatformCAChangesTheAuthorityAndNotTheBinding(t *testing.T) {
	cluster := readyCluster()
	cluster.SetLabels(map[string]string{managedByLabel: managedByValue})
	// CloudNativePG publishes whichever Secret it is using; here that is the
	// one cert-manager issued into.
	if err := unstructured.SetNestedField(cluster.Object, testServerSecret,
		"status", "certificates", "serverCASecret"); err != nil {
		t.Fatal(err)
	}
	cnpg := cnpgWithPlatformCA(t, platformCAIssuer(), cluster, appSecret(testCluster),
		platformIssuedSecret())

	instance, err := cnpg.Provision(context.Background(), shopDB)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Binding.CA != testPlatformCAPEM {
		t.Fatalf("binding CA %q, want the platform's root", instance.Binding.CA)
	}
	parsed, err := url.Parse(instance.Binding.URL)
	if err != nil {
		t.Fatalf("connection URL %q does not parse: %v", instance.Binding.URL, err)
	}
	if mode := parsed.Query().Get("sslmode"); mode != "require" {
		t.Fatalf("sslmode %q, want require — the claim contract is what raises it to verify-full", mode)
	}
	if parsed.Host != testCluster+"-rw."+testDatabaseNamespace+".svc:5432" {
		t.Fatalf("host %q moved", parsed.Host)
	}
}

// Nothing owner-references the Certificate — it is written before the Cluster
// exists, because the Cluster has to name the Secret it will be issued into —
// so deleting the database has to take it, exactly as it takes the base
// backup schedule. Otherwise cert-manager renews a certificate for a database
// nobody has, forever.
func TestDestroyingTheDatabaseTakesItsCertificateWithIt(t *testing.T) {
	cnpg := cnpgWithPlatformCA(t, platformCAIssuer(), platformIssuedSecret())

	if _, err := cnpg.Provision(context.Background(), shopDB); err == nil {
		t.Fatal("a freshly created cluster reported itself ready")
	}
	getCertificate(t, cnpg, testServerSecret)

	if err := cnpg.Deprovision(context.Background(),
		testDatabaseNamespace+"/"+testCluster); err != nil {
		t.Fatal(err)
	}

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: testServerSecret}
	if err := cnpg.Client.Get(context.Background(), key, cert); !apierrors.IsNotFound(err) {
		t.Fatalf("the certificate outlived the database it was issued for: %v", err)
	}
	// And the Secret with it: cert-manager leaves that behind on its own,
	// which is right for a certificate somebody may reissue and wrong for one
	// whose database has been destroyed.
	secret := &corev1.Secret{}
	if err := cnpg.Client.Get(context.Background(), key, secret); !apierrors.IsNotFound(err) {
		t.Fatalf("the issued Secret outlived the database: %v", err)
	}
}

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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// One CA for the whole platform, and a claim's database on it (#443, #468
// step 5a).
//
// Until this, there were two CA stories. The platform minted one — a
// self-signed root, a CA certificate, an issuer — and signed ClickHouse, the
// identity provider's Postgres and the object store with it; and every
// CloudNativePG Cluster a claim provisioned minted **a second one of its
// own**, because `desiredCluster` set no `certificates:` block and the
// operator generates a per-cluster CA when nothing names one. So an
// application handed `sslmode=verify-full&sslrootcert=…` (#456) verified
// against an authority that vouched for exactly one database and for nothing
// else the platform runs, and "inject the platform's CA and trust it" was not
// a sentence anybody could act on.
//
// What makes that fixable without widening the blast radius is the shape of
// the fix rather than the fix itself. The naive version copies the CA Secret
// — key included — into `kitchen-databases` so a namespaced Issuer there can
// sign; that puts the authority for the whole platform in a namespace whose
// contents are provisioned on a developer's request, and the issue refuses it
// outright. Instead the operator writes a **ClusterIssuer** backed by the CA
// Secret where it already is, and cert-manager — which is the only thing that
// ever reads the key — issues a certificate into the database namespace. The
// key does not move. What moves is a certificate, signed, which is the entire
// job of a certificate authority.
//
// Two smaller decisions follow from CloudNativePG's schema and are worth
// stating, because both look like omissions:
//
//   - **One Secret answers both refs.** `spec.certificates.serverCASecret`
//     wants a `ca.crt` and `serverTLSSecret` wants a `kubernetes.io/tls`
//     pair; cert-manager writes all three keys into the Secret it issues,
//     with `ca.crt` being the authority that signed it. A second Secret
//     holding a copy of the CA certificate would be a second thing to keep in
//     step across a rotation, for no reader that the first does not already
//     satisfy.
//   - **Nothing is said about the client CA.** CloudNativePG authenticates
//     `streaming_replica` with a client certificate of its own, from a client
//     CA it generates. That is the operator's internal traffic and no
//     application ever presents a certificate to this database, so
//     `clientCASecret` and `replicationTLSSecret` are left alone — the schema
//     pairs them with each other and not with the server's, and supplying a
//     client CA without a replication certificate would take over a rotation
//     CloudNativePG currently does for itself.
//
// The whole of it is conditional, and deliberately so: a platform CA that has
// not issued yet, a cert-manager that is not serving, or an installation
// running neither leaves the Cluster on CloudNativePG's own CA exactly as
// before. A database that cannot start because its certificate is late is a
// worse failure than one signed by the wrong authority, and the claim says
// which of the two it got — see Instance.CertificateAuthority.

const (
	// serverCertificateSuffix names the Certificate, and the Secret it is
	// issued into, after the Cluster it is for. It is deliberately not
	// `-server`, which is what CloudNativePG calls the Secret it generates
	// itself: the two coexist on a database that was provisioned before this
	// existed, and one silently overwriting the other would be a rotation
	// nobody could unpick.
	serverCertificateSuffix = "-server-tls"

	// serverCertificateDuration is ninety days, renewed at sixty — the same
	// numbers the bundled stores are issued on, for the same reason: short
	// enough that rotation is exercised rather than theoretical.
	//
	// CloudNativePG reloads a changed server certificate into the running
	// Postgres rather than restarting it, so the renewal costs nothing.
	serverCertificateDuration    = "2160h"
	serverCertificateRenewBefore = "720h"

	// clusterDomain is the suffix a Service's fully qualified name carries.
	// It is Kubernetes' compiled-in default and is not read from anywhere: a
	// cluster with a different one still resolves the three shorter names,
	// which is what the binding actually uses.
	clusterDomain = "cluster.local"

	// certificateComponentLabel says what one of these Certificates is for,
	// beside the managed-by pair every object the platform writes carries. It
	// is what tells a claim's server certificate apart from anything else
	// cert-manager holds in the database namespace.
	certificateComponentLabel = "app.kubernetes.io/component"
	certificateComponentValue = "database-server-tls"
)

// certificateGVK is cert-manager's Certificate, addressed as an unstructured
// object for the reason CloudNativePG's Cluster is: importing an operator's
// module ties this build to its release cadence.
func certificateGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}
}

// clusterIssuerGVK is the issuer a claim's certificate is signed by.
func clusterIssuerGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuer"}
}

// serverCertificateName is the Certificate and Secret one Cluster's server
// certificate lives in.
func serverCertificateName(cluster string) string {
	return naming.Truncate(cluster+serverCertificateSuffix, maxClusterName+len(serverCertificateSuffix))
}

// serverDNSNames is every name a client inside the cluster reaches this
// database by, which is what the certificate has to carry for `verify-full`
// to mean anything.
//
// All three Services, because a Cluster publishes three — `-rw` to the
// primary, `-ro` to the replicas, `-r` to any instance — and the binding uses
// one of them today. Issuing for the name in the binding alone would make a
// read-only connection somebody writes by hand fail hostname verification for
// no reason the platform could explain.
//
// Four forms of each, because a resolver inside the cluster shortens by
// search domain and a client may have been handed any of them: the bare
// Service name, with the namespace, with `.svc`, and fully qualified. The
// order is stable so that nothing rewrites the Certificate on every reconcile.
func serverDNSNames(cluster, namespace string) []string {
	names := make([]string, 0, 12)
	for _, service := range []string{cluster + "-rw", cluster + "-ro", cluster + "-r"} {
		names = append(names,
			service,
			service+"."+namespace,
			service+"."+namespace+".svc",
			service+"."+namespace+".svc."+clusterDomain,
		)
	}
	return names
}

// serverCertificateSecret issues this Cluster's server certificate from the
// platform's CA and answers the Secret it lands in — or "" where the platform
// has no CA to issue from, which is not an error.
//
// The three ways it answers "" are all states an installation is legitimately
// in and none of them is a fault:
//
//   - No issuer was configured, which is every provisioner built outside the
//     operator: the unit tests', and anything embedding this package.
//   - cert-manager is not serving, or is not installed at all. `helm install`
//     on a first run is in this state for about a minute.
//   - The ClusterIssuer is not there yet, because the platform's CA has not
//     issued. The same reconcile that writes it wakes this one.
//
// A Cluster provisioned in any of them keeps CloudNativePG's own CA and picks
// this up on a later reconcile, which is also the migration path for every
// database that already exists.
func (c *CNPG) serverCertificateSecret(ctx context.Context, cluster string) (string, error) {
	if c.ServerCAIssuer == "" {
		return "", nil
	}

	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(clusterIssuerGVK())
	switch err := c.Client.Get(ctx, types.NamespacedName{Name: c.ServerCAIssuer}, issuer); {
	case err == nil:
	case meta.IsNoMatchError(err), apierrors.IsNotFound(err):
		return "", nil
	default:
		return "", err
	}

	name := serverCertificateName(cluster)
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK())
	cert.SetName(name)
	cert.SetNamespace(c.Namespace)

	dnsNames := make([]any, 0, 12)
	for _, dnsName := range serverDNSNames(cluster, c.Namespace) {
		dnsNames = append(dnsNames, dnsName)
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, c.Client, cert, func() error {
		cert.SetLabels(map[string]string{
			managedByLabel:            managedByValue,
			certificateComponentLabel: certificateComponentValue,
			// Which database it is for, so that the claim's own labels find
			// it the way they find everything else the claim caused.
			clusterLabel: cluster,
		})
		return unstructured.SetNestedMap(cert.Object, map[string]any{
			"secretName": name,
			// The name the connection string uses, so that a client which
			// checks only the subject still checks the right thing.
			"commonName":  cluster + "-rw",
			"dnsNames":    dnsNames,
			"duration":    serverCertificateDuration,
			"renewBefore": serverCertificateRenewBefore,
			// A server certificate and nothing else: it may not sign, and it
			// is not a client identity. CloudNativePG's replication client
			// certificate is its own and stays its own.
			"usages": []any{"digital signature", "key encipherment", "server auth"},
			"issuerRef": map[string]any{
				"name":  c.ServerCAIssuer,
				"kind":  "ClusterIssuer",
				"group": "cert-manager.io",
			},
		}, "spec")
	}); err != nil {
		if meta.IsNoMatchError(err) {
			return "", nil
		}
		return "", fmt.Errorf("requesting a server certificate for database %s: %w", cluster, err)
	}
	return name, nil
}

// clusterCertificates is the `spec.certificates` block naming that Secret, or
// nil where there is none to name.
func clusterCertificates(secret string) map[string]any {
	if secret == "" {
		return nil
	}
	return map[string]any{
		// One Secret under both keys: cert-manager writes `ca.crt`,
		// `tls.crt` and `tls.key` into it, which is exactly one of each of
		// what the two fields ask for. See the file comment.
		"serverCASecret":  secret,
		"serverTLSSecret": secret,
	}
}

// ensureServerCertificates puts the block on a Cluster that already exists.
//
// It is the second of the two things this provisioner rewrites on a found
// Cluster — `ensurePgHBA` is the first — and for the same reason: a database
// is created once and found by every reconcile after that, so a certificate
// named only at creation would never reach an installation that already has
// databases, which is every installation upgrading into this.
//
// **It costs one rolling restart per database, once.** CloudNativePG replaces
// the server certificate and rolls the instances to pick it up; a single-
// instance Cluster is briefly unavailable while it does. That happens on the
// first reconcile after the upgrade and never again, and it is the whole of
// the migration — no binding changes, no DSN changes, and an application
// already verifying `verify-full` against the mounted authority goes on
// verifying against the file at the same path, whose contents are now the
// platform's CA.
//
// A Cluster somebody else created is left alone, the way every other write
// here is: the managed-by label is what says whose it is.
func (c *CNPG) ensureServerCertificates(
	ctx context.Context,
	cluster *unstructured.Unstructured,
	secret string,
) error {
	wanted := clusterCertificates(secret)
	if wanted == nil {
		return nil
	}
	if cluster.GetLabels()[managedByLabel] != managedByValue {
		return nil
	}
	if serverCertificatesOf(cluster) == secret {
		return nil
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	for field, value := range wanted {
		if err := unstructured.SetNestedField(cluster.Object, value, "spec", "certificates", field); err != nil {
			return err
		}
	}
	return c.Client.Patch(ctx, cluster, patch)
}

// serverCertificatesOf is the Secret a Cluster's spec names for both halves
// of its server identity, and "" where it names none or names two different
// ones — the second being a Cluster an operator configured by hand, which
// this leaves exactly as it found it.
func serverCertificatesOf(cluster *unstructured.Unstructured) string {
	ca := nestedString(cluster, "spec", "certificates", "serverCASecret")
	tls := nestedString(cluster, "spec", "certificates", "serverTLSSecret")
	if ca == "" || ca != tls {
		return ""
	}
	return ca
}

// certificateAuthorityOf reads back which authority this Cluster's server
// certificate came from, as the claim reports it.
//
// It is read off the Cluster rather than remembered from the write, because
// the write may have been three reconciles ago and an operator may have
// changed it since. `platform` is only claimed where the Cluster names the
// Secret this file issues into; anything else — CloudNativePG's own CA, or a
// certificate an installation supplied itself — is the provider's as far as
// the claim is concerned, which is exactly what the condition should say.
func (c *CNPG) certificateAuthorityOf(cluster *unstructured.Unstructured) CertificateAuthority {
	if serverCertificatesOf(cluster) == serverCertificateName(cluster.GetName()) {
		return CAPlatform
	}
	return CAProvider
}

// deleteServerCertificate removes the Certificate a Cluster was issued, and
// the Secret with it — cert-manager deletes neither on its own, and a
// certificate renewing forever for a database that is gone is the same shape
// of leak as a ScheduledBackup of one.
//
// Absent is already gone, and a cluster whose cert-manager has been
// uninstalled is too: a claim has to be deletable after either, since wedging
// a finalizer on it is worse than an object nobody can find.
func (c *CNPG) deleteServerCertificate(ctx context.Context, namespace, cluster string) error {
	name := serverCertificateName(cluster)
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK())
	cert.SetNamespace(namespace)
	cert.SetName(name)
	err := c.Client.Delete(ctx, cert)
	if err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return err
	}
	// cert-manager leaves the Secret behind when its Certificate goes, which
	// is right for a certificate somebody may reissue and wrong for one whose
	// database has been destroyed.
	secret := &corev1.Secret{}
	secret.SetNamespace(namespace)
	secret.SetName(name)
	err = c.Client.Delete(ctx, secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

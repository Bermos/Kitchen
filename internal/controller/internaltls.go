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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/accountsdb"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The platform's own certificate authority, and the certificates it issues to
// the stores Kitchen bundles.
//
// It exists because of the security review's finding (#323, and #382 after
// it): the bundled stores answer in plaintext inside `kitchen-system`. The
// NetworkPolicy that came out of #379 closed the namespace to applications,
// which moved the exposure rather than removing it — a pod that lands in the
// platform namespace, or the node under it, still reads every log line, every
// query and every password on the wire.
//
// The machinery is cert-manager's, which the chart already bundles and which
// already issues the platform's edge certificate. Three objects make a CA out
// of it: a self-signed Issuer, a Certificate with `isCA` that the self-signed
// Issuer signs, and a second Issuer that signs with the result. Everything
// after that is one Certificate per store.
//
// It is created here rather than by the chart for the reason the edge issuer
// is (see reconcileTLS): cert-manager's own webhook admits these kinds, so on
// a first install they cannot exist until it is serving. A reconcile loop can
// requeue where a Helm release simply fails.
//
// It has nothing to do with `spec.tls.mode`. That mode decides how the
// platform is published to the internet — what the shared Gateway terminates
// and what scheme a generated URL carries — and this decides whether two pods
// in one namespace talk in the clear. An installation on `tls.mode: none`,
// which has no ACME account and no wildcard, still gets all of this: the CA
// signs itself and asks nobody for anything.
const (
	// internalSelfSignedIssuerName signs one certificate ever: the CA's own.
	internalSelfSignedIssuerName = "kitchen-internal-selfsigned"

	// InternalCACertificateName is the platform's CA, and InternalCASecretName
	// the Secret cert-manager puts its key and certificate in. They share a
	// name with the Issuer that signs with them and with the ConfigMap the
	// certificate alone is published in, because they are one thing under
	// four kinds.
	InternalCACertificateName = "kitchen-internal-ca"
	InternalCASecretName      = "kitchen-internal-ca"
	internalCAIssuerName      = "kitchen-internal-ca"

	// InternalCAConfigMapName holds the CA certificate and nothing else, for
	// the platform's own components to verify against. It is a ConfigMap
	// rather than the CA Secret because the CA Secret also holds the CA's
	// private key, and a client needs the certificate: the operator, the API
	// and the telemetry agent mount this, and none of them can sign with it.
	InternalCAConfigMapName = "kitchen-internal-ca"

	// InternalCABundleKey is the file the bundle appears as, in the Secret
	// cert-manager writes and in the ConfigMap published from it.
	InternalCABundleKey = "ca.crt"

	// caCertificateDuration is ten years, renewed a year out. A CA nothing
	// outside this cluster trusts is not on anybody's revocation path, and
	// the cost of a short one is an expiry that takes the telemetry store
	// offline on a morning nobody expected it to.
	caCertificateDuration    = "87600h"
	caCertificateRenewBefore = "8760h"

	// storeCertificateDuration is ninety days, renewed at sixty. Short
	// enough that rotation is exercised rather than theoretical, long enough
	// that it is never the reason a store restarts.
	storeCertificateDuration    = "2160h"
	storeCertificateRenewBefore = "720h"

	condInternalCAReady = "InternalCAReady"

	// The two keys this controller reads out of a connection secret,
	// whichever store the secret belongs to. Every bundled store's secret
	// spells them the same way — clickhouse.SecretKeyHost and
	// accountsdb.SecretKeyHost and objectstore.SecretKeyHost are these
	// strings, and
	// TestEveryStoreSpeaksOneConnectionSecretVocabulary holds them to it — so
	// one controller issues for all of them and the chart adds a store by
	// writing a secret rather than by teaching this file a name.
	connectionSecretKeyHost              = "host"
	connectionSecretKeyCertificateSecret = "certificateSecret"

	// internalCAComponentName is the row this appears as in
	// status.components, beside the workloads. A CA that never issued is
	// exactly the kind of failure that is invisible everywhere else: the
	// store's pod sits waiting for a Secret, and every condition about
	// telemetry reads "cannot connect".
	internalCAComponentName = "internal-ca"
)

// InternalTLSReconciler issues the platform's CA and a certificate for every
// bundled store that asks for one.
//
// It is a controller of its own, driven by the connection secrets the chart
// writes, and that is not tidiness — it is the install ordering. The Kitchen
// singleton is a Helm **post-install hook**, so on a first install it does not
// exist until the release is otherwise ready; the store's pod does not start
// until its certificate Secret exists. A KitchenReconciler that issued
// certificates would therefore be waiting for an object that is waiting for
// the store that is waiting for the certificate, and `helm install --wait`
// would time out on all three.
//
// A Secret needs nothing from the platform's configuration, and the operator
// syncs every one that matches the moment it starts, which is inside the same
// install. So the work lives here and the Kitchen singleton only *reports* on
// it — which is the honest split anyway: the condition then describes the
// world rather than the last thing one reconcile happened to do.
type InternalTLSReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Nothing here watches cert-manager's kinds: a watch on a CRD that is absent
// stops the whole manager from starting, and an installation may be running
// its own cert-manager, or none. So progress comes from requeueing.
//
// The short interval is what the first install runs on, and it has to be short:
// cert-manager is coming up in the same release, the store's pod is waiting for
// a certificate, and `helm install --wait` is counting. The long one only
// applies once everything is issued, where all it does is keep the published CA
// bundle in step with a CA cert-manager has rotated.
const (
	internalTLSRetryInterval  = 15 * time.Second
	internalTLSResyncInterval = 5 * time.Minute
)

// The namespaced Issuer is this file's alone; the ClusterIssuers and the
// Certificates are shared with the edge TLS the KitchenReconciler owns.
// +kubebuilder:rbac:groups=cert-manager.io,resources=issuers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch

// Reconcile issues from one connection secret.
func (r *InternalTLSReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	secret := &corev1.Secret{}
	if err := r.Get(ctx, req.NamespacedName, secret); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	certSecret := strings.TrimSpace(string(secret.Data[connectionSecretKeyCertificateSecret]))
	if certSecret == "" {
		// Either an external store, whose certificate is somebody else's, or
		// one an installation has deliberately left in the clear. Both are
		// reported on the Kitchen singleton; neither is issued for.
		return ctrl.Result{}, nil
	}
	if errs := validation.IsDNS1123Subdomain(certSecret); len(errs) > 0 {
		// Nothing to retry: the secret has to change first, and a change to it
		// wakes this controller.
		log.Error(nil, "the connection secret asks for a certificate in an unusable Secret name",
			"secret", req.NamespacedName, "certificateSecret", certSecret,
			"problem", strings.Join(errs, "; "))
		return ctrl.Result{}, nil
	}

	names := serviceDNSNames(string(secret.Data[connectionSecretKeyHost]))
	if len(names) == 0 {
		log.Error(nil, "the connection secret's host is not a DNS name, so there is nothing "+
			"a certificate could be issued for", "secret", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Everything below is reported on the Kitchen singleton by
	// KitchenReconciler.reconcileInternalTLS, which reads these same objects.
	// A failure here is retried rather than surfaced, because on a first
	// install this runs before there is a singleton to surface it on — and
	// the commonest failure is cert-manager not serving yet, which is the
	// ordering rather than a fault.
	pending := ctrl.Result{RequeueAfter: internalTLSRetryInterval}

	if err := r.applySelfSignedIssuer(ctx); err != nil {
		return pending, ignoreNoMatch(err)
	}
	ca, err := r.applyInternalCACertificate(ctx)
	if err != nil {
		return pending, ignoreNoMatch(err)
	}
	if ready, message := certificateReady(ca); !ready {
		log.V(1).Info("waiting for the platform's internal CA", "reason", message)
		return pending, nil
	}

	if err := r.publishInternalCABundle(ctx); err != nil {
		return pending, err
	}
	if err := r.applyInternalCAIssuer(ctx); err != nil {
		return pending, ignoreNoMatch(err)
	}
	if _, err := r.applyStoreCertificate(ctx, certSecret, names); err != nil {
		return pending, ignoreNoMatch(err)
	}
	return ctrl.Result{RequeueAfter: internalTLSResyncInterval}, nil
}

// ignoreNoMatch drops the error a cluster whose cert-manager is not serving
// yet answers with. It is not a fault and it is not worth a stack trace on
// every reconcile of a fresh install; the requeue is what fixes it, and the
// Kitchen singleton is where it is reported.
func ignoreNoMatch(err error) error {
	if meta.IsNoMatchError(err) {
		return nil
	}
	return err
}

// SetupWithManager watches the connection secrets the chart writes.
//
// Every Secret in the platform namespace is offered and all but the ones
// carrying a certificateSecret key are dropped, which is one predicate rather
// than a label the chart would have to remember to write. The initial sync is
// what makes this run during `helm install`, before anything has changed.
func (r *InternalTLSReconciler) SetupWithManager(mgr ctrl.Manager) error {
	asksForACertificate := func(obj client.Object) bool {
		secret, ok := obj.(*corev1.Secret)
		if !ok || secret.Namespace != PlatformNamespace {
			return false
		}
		return len(secret.Data[connectionSecretKeyCertificateSecret]) > 0
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("internaltls").
		For(&corev1.Secret{}, builder.WithPredicates(
			predicate.NewPredicateFuncs(asksForACertificate))).
		Complete(r)
}

// internalTLSStore is one bundled store, as the singleton reports on it.
//
// Only stores with something to say end up here: one whose certificate the
// platform issues, and one that is reached in the clear. A store whose
// certificate is somebody else's — an external ClickHouse over TLS, an
// external Postgres with an sslmode of its own, an s3 Connection to a store on
// the internet — is not the internal CA's business and contributes nothing,
// because a condition about a CA that is not in the path would be noise in the
// one list this is said in.
type internalTLSStore struct {
	// certificate is the Secret the store's certificate is issued into, and
	// empty for a store that is reached in the clear.
	certificate string
	// issued describes the finished state, for the condition's message.
	issued string
	// clear describes what is readable, for a store nobody asked to encrypt.
	clear string
	// what names the certificate while it is being waited for.
	what string
}

// reconcileInternalTLS reports whether the platform namespace is encrypted.
//
// It writes none of the objects it reads: InternalTLSReconciler above owns
// those, for the install-ordering reason given there. What is left here is the
// half that belongs on the singleton — the one place an operator reads what
// the platform thinks of itself — and a condition derived from live objects
// cannot drift from what is actually true.
//
// One condition covers every bundled store, rather than one condition each.
// The question an operator is asking is whether this namespace is readable,
// and the answer is as good as its weakest store; a list of conditions that
// all say the same thing on a healthy platform is a list nobody reads.
func (r *KitchenReconciler) reconcileInternalTLS(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	stores := []internalTLSStore{}
	if store, ok := r.telemetryStoreTLS(ctx, kitchen); ok {
		stores = append(stores, store)
	}
	if store, ok := r.accountsDatabaseTLS(ctx, kitchen); ok {
		stores = append(stores, store)
	}
	if store, ok := r.objectStoreTLS(ctx, kitchen); ok {
		stores = append(stores, store)
	}
	if len(stores) == 0 {
		// Nothing the platform runs, or nothing whose certificate is the
		// platform's. The CA is not created for its own sake.
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condInternalCAReady)
		return true
	}

	requests := []struct{ name, what string }{}
	for _, store := range stores {
		if store.certificate != "" {
			requests = append(requests, struct{ name, what string }{store.certificate, store.what})
		}
	}
	if len(requests) > 0 {
		// The CA first: everything else is signed by it, and "the CA is not
		// issued yet" is the more useful half of the same wait.
		requests = append([]struct{ name, what string }{
			{InternalCACertificateName, "the platform's internal CA"},
		}, requests...)
	}

	for _, request := range requests {
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certManagerGVK("Certificate"))
		key := types.NamespacedName{Namespace: PlatformNamespace, Name: request.name}
		if err := r.Get(ctx, key, cert); err != nil {
			if meta.IsNoMatchError(err) {
				setCond(condInternalCAReady, metav1.ConditionFalse, "CertManagerUnavailable",
					"waiting for the cert-manager API to be served, which is what issues "+
						"every certificate inside the platform namespace: "+err.Error())
				return false
			}
			setCond(condInternalCAReady, metav1.ConditionFalse, "Issuing",
				"waiting for "+request.what+" to be requested: "+err.Error())
			return false
		}
		if ready, message := certificateReady(cert); !ready {
			setCond(condInternalCAReady, metav1.ConditionFalse, "Issuing",
				request.what+" is not issued yet: "+message)
			return false
		}
	}

	// A store somebody left in the clear is said last, because it is the
	// answer only once nothing is still being issued.
	//
	// It deliberately does not hold the platform short of Ready: nothing is
	// broken, and a condition that could never go true would train an
	// operator to ignore the one place this is said. It is said at all
	// because a platform quietly shipping every log line, every session, every
	// object and its own passwords across its namespace is the finding this
	// file exists for.
	readable := []string{}
	issued := []string{}
	for _, store := range stores {
		if store.certificate == "" {
			readable = append(readable, store.clear)
			continue
		}
		issued = append(issued, store.issued)
	}
	if len(readable) > 0 {
		setCond(condInternalCAReady, metav1.ConditionFalse, "StoreInTheClear",
			strings.Join(readable, "; and ")+
				". Anything that can watch traffic inside "+PlatformNamespace+
				", or the node under it, reads all of it")
		return true
	}

	setCond(condInternalCAReady, metav1.ConditionTrue, "Issued",
		"the platform's internal CA is issued, and "+strings.Join(issued, ", and "))
	return true
}

// telemetryStoreTLS reads the telemetry store's connection secret, which is
// what the chart says the store with rather than anything on this object:
// whether the bundled ClickHouse serves TLS is a chart value, and what
// reaches the operator is a secret that either names a Secret for the store's
// certificate or does not.
func (r *KitchenReconciler) telemetryStoreTLS(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
) (internalTLSStore, bool) {
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		// No telemetry store at all.
		return internalTLSStore{}, false
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		// TelemetrySchemaReady already says the store cannot be reached at
		// all, which is a bigger fact than its certificate. Saying it twice
		// would put the same cause under two names in the list an operator
		// reads.
		return internalTLSStore{}, false
	}

	host := string(secret.Data[connectionSecretKeyHost])
	certSecret := strings.TrimSpace(string(secret.Data[connectionSecretKeyCertificateSecret]))
	if certSecret != "" {
		return internalTLSStore{
			certificate: certSecret,
			what:        "the telemetry store's certificate",
			issued:      "the telemetry store serves " + host + " with a certificate signed by it",
		}, true
	}
	if scheme := string(secret.Data[clickhouse.SecretKeyScheme]); scheme == clickhouse.SchemeHTTPS {
		// An external store with a certificate of its own. The platform's CA
		// is not in that path and has nothing to report about it.
		return internalTLSStore{}, false
	}
	return internalTLSStore{
		clear: "the telemetry store is reached over plain HTTP, so its queries, its rows and " +
			"its password are readable (set clickhouse.tls.enabled to have the platform issue " +
			"it a certificate)",
	}, true
}

// accountsDatabaseTLS reads the identity provider's connection secret, on the
// same terms: `sslmode` is what every client of it asks for, and
// `certificateSecret` is the chart asking the operator to issue one.
//
// The database holds every session, every OAuth client's secret and every
// passkey on the platform, and the identity provider is the busiest client of
// anything in this namespace — so it is the store where "readable inside
// kitchen-system" costs the most.
func (r *KitchenReconciler) accountsDatabaseTLS(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
) (internalTLSStore, bool) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: accountsdb.SecretName(kitchen)}
	if err := r.Get(ctx, key, secret); err != nil {
		// An installation with no identity provider, or one whose secret is
		// somebody else's to write. Either way there is nothing here the
		// platform can say anything true about.
		return internalTLSStore{}, false
	}

	host := string(secret.Data[connectionSecretKeyHost])
	certSecret := strings.TrimSpace(string(secret.Data[connectionSecretKeyCertificateSecret]))
	if certSecret != "" {
		return internalTLSStore{
			certificate: certSecret,
			what:        "the accounts database's certificate",
			issued: "the accounts database serves " + host +
				" with a certificate signed by it, and refuses anything unencrypted",
		}, true
	}
	// An external database asked for over TLS is verified against the host's
	// roots rather than against this CA, so the platform has nothing to
	// report — and nothing to be quiet about either.
	switch strings.TrimSpace(string(secret.Data[accountsdb.SecretKeySSLMode])) {
	case accountsdb.SSLModeVerifyFull, "verify-ca", "require":
		return internalTLSStore{}, false
	}
	return internalTLSStore{
		clear: "the accounts database is reached without TLS, so every session, every OAuth " +
			"client's secret and the database's own password are readable (set " +
			"postgres.tls.enabled, or postgres.external.sslmode for a database of your own)",
	}, true
}

// objectStoreTLS reads the bundled object store's secret, on the same terms:
// `certificateSecret` is the chart asking the operator to issue one, and
// `scheme` is what every client of the store is told.
//
// It is the one bundled store application namespaces reach — the NetworkPolicy
// opens its port to them, because an objectStore claim hands a pod this
// address — so "readable inside kitchen-system" understates it: an object
// crossing in the clear crosses between two namespaces, and the credential
// scoped to that bucket crosses with the request that uses it.
func (r *KitchenReconciler) objectStoreTLS(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
) (internalTLSStore, bool) {
	store := resolveObjectStore(kitchen)
	if store == nil {
		// No bundled store. An installation with an s3 Connection of its own
		// is reaching somebody else's store, whose certificate is theirs.
		return internalTLSStore{}, false
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: store.SecretName}
	if err := r.Get(ctx, key, secret); err != nil {
		// ObjectStoreReady already says the store's credential cannot be
		// read, which is a bigger fact than its certificate.
		return internalTLSStore{}, false
	}

	host := string(secret.Data[connectionSecretKeyHost])
	if host == "" {
		host = store.host()
	}
	certSecret := strings.TrimSpace(string(secret.Data[connectionSecretKeyCertificateSecret]))
	if certSecret != "" {
		return internalTLSStore{
			certificate: certSecret,
			what:        "the object store's certificate",
			issued: "the object store serves " + host + " with a certificate signed by it, which " +
				"every application that claimed a bucket is handed in its binding",
		}, true
	}
	return internalTLSStore{
		clear: "the object store is reached over plain HTTP, so every object, every upload and " +
			"every bucket credential is readable — by this namespace and by every application " +
			"namespace, which is the one namespace the store is open to (set " +
			"objectStore.tls.enabled to have the platform issue it a certificate)",
	}, true
}

// serviceDNSNames turns the address the connection secret carries into every
// name a client inside the cluster might dial it by, so that verification
// succeeds on the one the client actually used.
//
// The address the chart writes is `<service>.<namespace>.svc`, which is what
// the operator and the telemetry agent both connect to; the other three are
// the same Service under the shortenings the cluster's resolver accepts. The
// configured host comes first so that a certificate is always readable as
// being for the address it was issued for.
//
// Anything that is not a Service address in that shape gets its own name and
// nothing invented around it — a certificate with a wrong SAN on it is worse
// than a certificate with one.
func serviceDNSNames(host string) []string {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" || len(validation.IsDNS1123Subdomain(host)) > 0 {
		return nil
	}

	names := []string{host}
	labels := strings.Split(strings.TrimSuffix(host, ".cluster.local"), ".")
	if len(labels) == 3 && labels[2] == "svc" {
		for _, name := range []string{
			labels[0] + "." + labels[1] + ".svc.cluster.local",
			labels[0] + "." + labels[1],
			labels[0],
		} {
			if name != host {
				names = append(names, name)
			}
		}
	}
	return names
}

// applySelfSignedIssuer writes the issuer that signs the CA's own
// certificate, and nothing else in the platform.
func (r *InternalTLSReconciler) applySelfSignedIssuer(ctx context.Context) error {
	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(certManagerGVK("Issuer"))
	issuer.SetName(internalSelfSignedIssuerName)
	issuer.SetNamespace(PlatformNamespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, issuer, func() error {
		issuer.SetLabels(map[string]string{
			labelComponentKey: internalSelfSignedIssuerName,
			labelManagedByKey: labelManagedByValue,
		})
		return unstructured.SetNestedMap(issuer.Object, map[string]any{}, "spec", "selfSigned")
	})
	return err
}

// applyInternalCACertificate requests the platform's CA into the Secret the CA
// Issuer signs with, and returns it so the caller can report on its progress.
func (r *InternalTLSReconciler) applyInternalCACertificate(
	ctx context.Context,
) (*unstructured.Unstructured, error) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerGVK("Certificate"))
	cert.SetName(InternalCACertificateName)
	cert.SetNamespace(PlatformNamespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		cert.SetLabels(map[string]string{
			labelComponentKey: InternalCACertificateName,
			labelManagedByKey: labelManagedByValue,
		})
		return unstructured.SetNestedMap(cert.Object, map[string]any{
			"isCA":        true,
			"commonName":  "Kitchen platform internal CA",
			"secretName":  InternalCASecretName,
			"duration":    caCertificateDuration,
			"renewBefore": caCertificateRenewBefore,
			"privateKey": map[string]any{
				"algorithm": "ECDSA",
				"size":      int64(256),
				// A rotated key that reuses the same Secret is what lets a
				// renewal happen without anything having to be told.
				"rotationPolicy": "Always",
			},
			"issuerRef": map[string]any{
				"name":  internalSelfSignedIssuerName,
				"kind":  "Issuer",
				"group": "cert-manager.io",
			},
		}, "spec")
	})
	if err != nil {
		return nil, err
	}
	return cert, nil
}

// applyInternalCAIssuer writes the issuer every in-cluster certificate is
// signed by.
func (r *InternalTLSReconciler) applyInternalCAIssuer(ctx context.Context) error {
	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(certManagerGVK("Issuer"))
	issuer.SetName(internalCAIssuerName)
	issuer.SetNamespace(PlatformNamespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, issuer, func() error {
		issuer.SetLabels(map[string]string{
			labelComponentKey: internalCAIssuerName,
			labelManagedByKey: labelManagedByValue,
		})
		return unstructured.SetNestedMap(issuer.Object, map[string]any{
			"secretName": InternalCASecretName,
		}, "spec", "ca")
	})
	return err
}

// applyStoreCertificate requests one bundled store's certificate, for every
// name a client in the cluster reaches it by.
func (r *InternalTLSReconciler) applyStoreCertificate(
	ctx context.Context,
	name string,
	dnsNames []string,
) (*unstructured.Unstructured, error) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerGVK("Certificate"))
	cert.SetName(name)
	cert.SetNamespace(PlatformNamespace)

	names := make([]any, 0, len(dnsNames))
	for _, dnsName := range dnsNames {
		names = append(names, dnsName)
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		cert.SetLabels(map[string]string{
			labelComponentKey: name,
			labelManagedByKey: labelManagedByValue,
		})
		return unstructured.SetNestedMap(cert.Object, map[string]any{
			"secretName":  name,
			"dnsNames":    names,
			"duration":    storeCertificateDuration,
			"renewBefore": storeCertificateRenewBefore,
			// A server certificate, and only that: it may not sign anything
			// and it is not a client identity.
			"usages": []any{"digital signature", "key encipherment", "server auth"},
			"issuerRef": map[string]any{
				"name":  internalCAIssuerName,
				"kind":  "Issuer",
				"group": "cert-manager.io",
			},
		}, "spec")
	})
	if err != nil {
		return nil, err
	}
	return cert, nil
}

// publishInternalCABundle copies the CA certificate — and not its key — into a
// ConfigMap the platform's own components mount.
//
// cert-manager writes the CA's certificate and its private key into one
// Secret, which is right for the Issuer that signs with it and wrong for
// everything that merely has to verify against it. A client needs the
// certificate; handing it the key as well would put the authority for the
// whole namespace into every pod that talks to a store.
func (r *InternalTLSReconciler) publishInternalCABundle(ctx context.Context) error {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: InternalCASecretName}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the CA reports itself issued but %s/%s does not exist yet",
				PlatformNamespace, InternalCASecretName)
		}
		return err
	}

	// `ca.crt` is what cert-manager writes for a CA it signed; for a
	// self-signed root the certificate is its own issuer, so `tls.crt` is the
	// same bytes and is the fallback for a cert-manager that stops writing
	// the first.
	bundle := secret.Data[InternalCABundleKey]
	if len(bundle) == 0 {
		bundle = secret.Data[corev1.TLSCertKey]
	}
	if len(bundle) == 0 {
		return fmt.Errorf("%s/%s holds neither %s nor %s, so there is no bundle to publish",
			PlatformNamespace, InternalCASecretName, InternalCABundleKey, corev1.TLSCertKey)
	}

	configMap := &corev1.ConfigMap{}
	configMap.SetName(InternalCAConfigMapName)
	configMap.SetNamespace(PlatformNamespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		configMap.Labels = map[string]string{
			labelComponentKey: InternalCAConfigMapName,
			labelManagedByKey: labelManagedByValue,
		}
		configMap.Data = map[string]string{InternalCABundleKey: string(bundle)}
		return nil
	})
	return err
}

// internalTLSComponent puts the CA in the list an operator already reads.
//
// It is not a workload, and it is here for the same reason the scheduled
// backup's row is: the survey is what somebody looks at, and "the platform's
// stores are encrypted" is a fact about the platform that has no pod to be
// unhealthy. It is derived from the condition rather than carried separately
// so that the two can never disagree.
func internalTLSComponent(kitchen *kitchenv1alpha1.Kitchen) *kitchenv1alpha1.ComponentStatus {
	cond := meta.FindStatusCondition(kitchen.Status.Conditions, condInternalCAReady)
	if cond == nil {
		return nil
	}
	// A store somebody chose to leave in the clear is reported by the
	// condition and is not a broken component: an unhealthy row here would
	// hold ComponentsHealthy false for as long as that choice stands.
	if cond.Status == metav1.ConditionFalse && cond.Reason == "StoreInTheClear" {
		return nil
	}
	healthy := cond.Status == metav1.ConditionTrue
	available := int32(0)
	if healthy {
		available = 1
	}
	return &kitchenv1alpha1.ComponentStatus{
		Name:      internalCAComponentName,
		Kind:      "Certificate",
		Healthy:   healthy,
		Available: available,
		Desired:   1,
		Message:   cond.Message,
	}
}

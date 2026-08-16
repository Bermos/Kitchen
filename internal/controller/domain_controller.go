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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	domainFinalizer = "kitchen.bermos.dev/domain-cleanup"

	condVerified = "Verified"

	// verificationRecordPrefix is where ownership of a hostname is proven: a
	// TXT record at _kitchen-challenge.<hostname>. An underscore prefix keeps
	// the name out of the way of anything the zone actually serves.
	verificationRecordPrefix = "_kitchen-challenge."

	// domainChildPrefix names everything a Domain materializes in the platform
	// namespace: the cert-manager Certificate and, with the -tls suffix, the
	// secret it fills and the Gateway listener reads.
	domainChildPrefix    = "kitchen-domain-"
	domainTLSNameSuffix  = "-tls"
	domainListenerPrefix = "dom-"
)

// DNSResolver is the slice of net.Resolver that domain verification needs. It
// is an interface so envtest can answer the lookups instead of the network.
type DNSResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
}

// DomainReconciler reconciles a Domain: it verifies ownership of the hostname
// over DNS, requests the per-domain certificate where the TLS mode calls for
// one, and reports whether the platform is actually routing the name. The
// routing itself belongs to others — KitchenReconciler alone writes the shared
// Gateway's listeners and EnvironmentReconciler alone writes each
// Environment's HTTPRoute — so this reconciler only ever observes them.
type DomainReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Resolver answers the verification lookups. Nil uses the system
	// resolver; tests inject a fake.
	Resolver DNSResolver
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=domains,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=domains/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=domains/finalizers,verbs=update
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments;kitchens,verbs=get;list;watch
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;delete

// Reconcile drives one custom domain towards being served.
func (r *DomainReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	domain := &kitchenv1alpha1.Domain{}
	if err := r.Get(ctx, req.NamespacedName, domain); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !domain.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, domain)
	}

	if controllerutil.AddFinalizer(domain, domainFinalizer) {
		if err := r.Update(ctx, domain); err != nil {
			return ctrl.Result{}, err
		}
	}

	setCond := func(condType string, status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&domain.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: domain.Generation,
		})
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		setCond(condVerified, metav1.ConditionFalse, "PlatformConfigMissing", err.Error())
		if err := r.Status().Update(ctx, domain); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// The environment the hostname routes to. Its absence blocks routing, not
	// verification — the DNS instructions are useful either way.
	env := &kitchenv1alpha1.Environment{}
	envErr := r.Get(ctx, types.NamespacedName{
		Namespace: domain.Namespace, Name: domain.Spec.EnvironmentRef.Name,
	}, env)
	if envErr != nil && !apierrors.IsNotFound(envErr) {
		return ctrl.Result{}, envErr
	}

	domain.Status.Verification = verificationInstructions(domain, kitchen, env, envErr == nil)
	domain.Status.TLSMode = domainTLSMode(domain, kitchen)

	r.verify(ctx, domain, kitchen, setCond)
	certReady := r.reconcileCertificate(ctx, domain, kitchen, setCond)
	routed := r.observeRoute(ctx, domain, kitchen, env, envErr, setCond)

	if err := r.Status().Update(ctx, domain); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled domain", "hostname", domain.Spec.Hostname,
		"verified", domain.Status.Verified, "certificateReady", certReady, "routed", routed)

	// DNS propagation, certificate issuance and route acceptance all finish
	// outside anything this controller watches, so an unfinished domain polls.
	if !domain.Status.Verified {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if !certReady || !routed {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// finalize removes what the domain materialized: the per-domain Certificate
// and the secret it filled. The Gateway listener and the route hostname are
// not touched here — their writers watch Domains and rebuild from the live,
// verified ones, so this deletion is what removes them.
func (r *DomainReconciler) finalize(ctx context.Context, domain *kitchenv1alpha1.Domain) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(domain, domainFinalizer) {
		return ctrl.Result{}, nil
	}

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerGVK("Certificate"))
	cert.SetName(domainCertificateName(domain.Name))
	cert.SetNamespace(PlatformNamespace)
	// NoMatch: a platform without cert-manager never created one.
	if err := r.Delete(ctx, cert); err != nil &&
		!apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return ctrl.Result{}, err
	}

	// cert-manager does not delete a Certificate's secret with it. It carries
	// the platform's managed-by label (set through secretTemplate); anything
	// else that owns a secret of the same name is left alone.
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: DomainTLSSecretName(domain.Name)}
	if err := r.Get(ctx, key, secret); err == nil && secret.Labels[labelManagedByKey] == labelManagedByValue {
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(domain, domainFinalizer)
	return ctrl.Result{}, r.Update(ctx, domain)
}

// verify checks DNS ownership of the hostname: the TXT record, or a CNAME
// pointing the hostname at the platform — either one is the zone owner's own
// action. A domain that verified once stays verified: routing must not follow
// a record the owner deletes after setup, and the token is deterministic from
// the UID, so a wiped status re-runs the same check.
func (r *DomainReconciler) verify(
	ctx context.Context,
	domain *kitchenv1alpha1.Domain,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) {
	if domain.Status.Verified {
		return
	}

	resolver := r.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	record := verificationRecordName(domain.Spec.Hostname)
	expected := verificationToken(domain.UID)

	values, txtErr := resolver.LookupTXT(ctx, record)
	for _, value := range values {
		if strings.TrimSpace(value) == expected {
			domain.Status.Verified = true
			setCond(condVerified, metav1.ConditionTrue, "TXTRecordFound",
				fmt.Sprintf("the TXT record at %s carries the expected token", record))
			return
		}
	}

	// The CNAME alternative: the hostname itself pointing into the base
	// domain. LookupCNAME follows to the canonical name, which for a name
	// with no CNAME is the name itself.
	cname, cnameErr := resolver.LookupCNAME(ctx, domain.Spec.Hostname)
	target := strings.TrimSuffix(strings.TrimSpace(cname), ".")
	if cnameErr == nil && target != "" && !strings.EqualFold(target, domain.Spec.Hostname) &&
		hostUnderDomain(target, kitchen.Spec.BaseDomain) {
		domain.Status.Verified = true
		setCond(condVerified, metav1.ConditionTrue, "CNAMEPointsAtPlatform",
			fmt.Sprintf("%s is a CNAME to %s, inside the platform's base domain", domain.Spec.Hostname, target))
		return
	}

	// Not verified: say which of the real failure modes this is, because they
	// call for different fixes — creating the record, fixing its value, or
	// waiting out propagation.
	switch {
	case len(values) > 0:
		setCond(condVerified, metav1.ConditionFalse, "RecordMismatch", fmt.Sprintf(
			"the TXT record at %s exists but carries the wrong value: expected %q", record, expected))
	case txtErr == nil || dnsNotFound(txtErr):
		setCond(condVerified, metav1.ConditionFalse, "RecordMissing", fmt.Sprintf(
			"no TXT record at %s (and no CNAME from %s into %s): create either — "+
				"a fresh record can take a few minutes to propagate",
			record, domain.Spec.Hostname, kitchen.Spec.BaseDomain))
	default:
		setCond(condVerified, metav1.ConditionFalse, "LookupFailed", fmt.Sprintf(
			"the DNS lookup for %s failed: %v — this is not the record being absent; "+
				"check propagation, or whether the operator's resolver serves a split-horizon zone",
			record, txtErr))
	}
}

// reconcileCertificate owns the per-domain certificate where the TLS mode in
// effect calls for one, and reports the mode's truth where it does not.
func (r *DomainReconciler) reconcileCertificate(
	ctx context.Context,
	domain *kitchenv1alpha1.Domain,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	switch domainTLSMode(domain, kitchen) {
	case kitchenv1alpha1.TLSModeNone:
		// Served over plain HTTP by choice; there is no certificate to
		// report on. An already-issued certificate from an earlier mode is
		// left alone, for the same rate-limit reason the platform's own
		// reconcileTLS leaves the wildcard.
		meta.RemoveStatusCondition(&domain.Status.Conditions, condCertificateReady)
		return true
	case kitchenv1alpha1.TLSModeCloudflared:
		// A custom hostname on a tunnel is an ingress rule in Cloudflare's
		// control plane, not a certificate in this cluster. The tunnel is
		// token-managed — the operator holds no Cloudflare API credential in
		// this mode — so it can neither create that rule nor observe it.
		// Unknown is the honest status: not failed, and not verifiable.
		setCond(condCertificateReady, metav1.ConditionUnknown, "TunnelManagedExternally", fmt.Sprintf(
			"TLS for %s terminates at the Cloudflare edge. Add the hostname as a public hostname "+
				"of the tunnel in the Cloudflare dashboard; the tunnel is token-managed, so the "+
				"operator cannot do this for you or see whether it is done", domain.Spec.Hostname))
		return true
	}

	// Mode acme from here on.
	if kitchen.Spec.TLS.Mode != kitchenv1alpha1.TLSModeACME {
		setCond(condCertificateReady, metav1.ConditionFalse, "NoACMEAccount", fmt.Sprintf(
			"this domain asks for an ACME certificate, but the platform's TLS mode is %q and so "+
				"no ACME account is configured; run the platform in acme mode, or set this "+
				"domain's tls to the platform's own mode", kitchen.Spec.TLS.Mode))
		return false
	}
	if !domain.Status.Verified {
		setCond(condCertificateReady, metav1.ConditionFalse, "AwaitingVerification",
			"the certificate is requested once the domain is verified")
		return false
	}

	cert, err := r.applyDomainCertificate(ctx, domain)
	if err != nil {
		if meta.IsNoMatchError(err) {
			setCond(condCertificateReady, metav1.ConditionFalse, "CertManagerUnavailable",
				"waiting for the cert-manager API to be served: "+err.Error())
			return false
		}
		setCond(condCertificateReady, metav1.ConditionFalse, "CertificateNotApplied", err.Error())
		return false
	}

	ready, message := certificateReady(cert)
	if !ready {
		// HTTP-01 is the one challenge the platform can solve for a zone it
		// does not control, and it only works once the hostname resolves to
		// the shared Gateway. That dependency is this message's most likely
		// cause, so it is named rather than left to cert-manager's text.
		setCond(condCertificateReady, metav1.ConditionFalse, "Issuing", fmt.Sprintf(
			"%s — issuance is solved over HTTP-01 through the shared Gateway, so %s must "+
				"resolve to the platform (the CNAME in status.verification) for it to finish",
			message, domain.Spec.Hostname))
		return false
	}
	setCond(condCertificateReady, metav1.ConditionTrue, "Issued", message)
	return true
}

// applyDomainCertificate requests the hostname's certificate into the secret
// the per-domain Gateway listener reads. It goes through the HTTP-01 issuer:
// the platform's DNS-01 solver writes challenge records with a Cloudflare
// token scoped to the *base* domain's zone, which by definition is not the
// zone a custom domain lives in.
func (r *DomainReconciler) applyDomainCertificate(
	ctx context.Context,
	domain *kitchenv1alpha1.Domain,
) (*unstructured.Unstructured, error) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerGVK("Certificate"))
	cert.SetName(domainCertificateName(domain.Name))
	cert.SetNamespace(PlatformNamespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		cert.SetLabels(map[string]string{
			labelComponentKey: "kitchen-domain",
			labelManagedByKey: labelManagedByValue,
		})
		return unstructured.SetNestedMap(cert.Object, map[string]any{
			"secretName": DomainTLSSecretName(domain.Name),
			// The managed-by label on the filled secret is what lets the
			// finalizer delete it, and what enqueues the Kitchen singleton to
			// add the listener the moment the secret appears.
			"secretTemplate": map[string]any{
				"labels": map[string]any{
					labelComponentKey: "kitchen-domain",
					labelManagedByKey: labelManagedByValue,
				},
			},
			"dnsNames": []any{domain.Spec.Hostname},
			"issuerRef": map[string]any{
				"name":  acmeHTTP01ClusterIssuerName,
				"kind":  "ClusterIssuer",
				"group": "cert-manager.io",
			},
		}, "spec")
	})
	if err != nil {
		return nil, err
	}
	return cert, nil
}

// observeRoute reports whether the platform is routing the hostname, reading
// the state the other reconcilers wrote: the Environment's HTTPRoute and its
// acceptance by the Gateway.
func (r *DomainReconciler) observeRoute(
	ctx context.Context,
	domain *kitchenv1alpha1.Domain,
	kitchen *kitchenv1alpha1.Kitchen,
	env *kitchenv1alpha1.Environment,
	envErr error,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	if envErr != nil {
		setCond(condRouteProgrammed, metav1.ConditionFalse, "EnvironmentMissing", envErr.Error())
		return false
	}
	if !domain.Status.Verified {
		setCond(condRouteProgrammed, metav1.ConditionFalse, "AwaitingVerification",
			"the hostname joins the environment's route once the domain is verified")
		return false
	}

	route := &gatewayv1.HTTPRoute{}
	key := types.NamespacedName{Namespace: appNamespace(env.Spec.ProjectRef.Name), Name: env.Name}
	if err := r.Get(ctx, key, route); err != nil {
		setCond(condRouteProgrammed, metav1.ConditionFalse, "RouteMissing",
			"the environment has no route yet: "+err.Error())
		return false
	}

	carried := false
	for _, hostname := range route.Spec.Hostnames {
		if string(hostname) == domain.Spec.Hostname {
			carried = true
		}
	}
	if !carried {
		setCond(condRouteProgrammed, metav1.ConditionFalse, "HostnamePending",
			"the environment's route does not carry the hostname yet")
		return false
	}

	// Accepted, on the listener this domain's traffic actually uses. In acme
	// mode that is the per-domain listener; the other modes share the HTTP one.
	section := gatewayListenerHTTP
	if domainTLSMode(domain, kitchen) == kitchenv1alpha1.TLSModeACME {
		section = domainListenerName(domain.Name)
	}
	for _, parent := range route.Status.Parents {
		ref := parent.ParentRef
		if string(ref.Name) != SharedGatewayName ||
			ref.SectionName == nil || string(*ref.SectionName) != section {
			continue
		}
		for _, cond := range parent.Conditions {
			if cond.Type == string(gatewayv1.RouteConditionAccepted) {
				if cond.Status == metav1.ConditionTrue {
					setCond(condRouteProgrammed, metav1.ConditionTrue, "Accepted",
						fmt.Sprintf("the gateway accepted the route for %s", domain.Spec.Hostname))
					return true
				}
				setCond(condRouteProgrammed, metav1.ConditionFalse, "RouteNotAccepted", cond.Message)
				return false
			}
		}
	}
	setCond(condRouteProgrammed, metav1.ConditionFalse, "AwaitingGatewayAcceptance", fmt.Sprintf(
		"the route carries the hostname; waiting for the gateway controller to accept it on listener %q", section))
	return false
}

// verificationInstructions is what the owner of the zone has to do, spelled
// out for the API and the dashboard. The CNAME target is the environment's own
// generated hostname where it is known — the record the user needs for routing
// anyway — and the bare base domain otherwise.
func verificationInstructions(
	domain *kitchenv1alpha1.Domain,
	kitchen *kitchenv1alpha1.Kitchen,
	env *kitchenv1alpha1.Environment,
	envFound bool,
) *kitchenv1alpha1.DomainVerification {
	target := kitchen.Spec.BaseDomain
	if envFound {
		target = hostname(env.Spec.ProjectRef.Name, env, kitchen.Spec.BaseDomain)
	}
	return &kitchenv1alpha1.DomainVerification{
		TXTRecord:   verificationRecordName(domain.Spec.Hostname),
		TXTValue:    verificationToken(domain.UID),
		CNAMETarget: target,
	}
}

func verificationRecordName(hostname string) string {
	return verificationRecordPrefix + hostname
}

// verificationToken derives the expected TXT value from the Domain's UID:
// stable for the object's lifetime, recomputable from nothing but the object,
// and useless for any other Domain.
func verificationToken(uid types.UID) string {
	sum := sha256.Sum256([]byte("kitchen-domain-ownership:" + string(uid)))
	return "kitchen-verify=" + hex.EncodeToString(sum[:16])
}

// dnsNotFound tells NXDOMAIN — the record simply is not there — from a lookup
// that failed to complete, which is a different message to the user.
func dnsNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// hostUnderDomain reports whether host is the domain itself or a name under it.
func hostUnderDomain(host, domain string) bool {
	if domain == "" {
		return false
	}
	host, domain = strings.ToLower(host), strings.ToLower(domain)
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// domainTLSMode is the mode in effect: the Domain's own, or the platform
// default it inherits.
func domainTLSMode(domain *kitchenv1alpha1.Domain, kitchen *kitchenv1alpha1.Kitchen) kitchenv1alpha1.TLSMode {
	if domain.Spec.TLS != "" {
		return domain.Spec.TLS
	}
	if kitchen.Spec.TLS.Mode != "" {
		return kitchen.Spec.TLS.Mode
	}
	return kitchenv1alpha1.TLSModeACME
}

// domainChildName derives a child object's name from the Domain's, keeping it
// deterministic under the 253-character limit: an over-long name is truncated
// and disambiguated with a hash of the full one.
func domainChildName(prefix, name, suffix string) string {
	full := prefix + name + suffix
	if len(full) <= 253 {
		return full
	}
	sum := sha256.Sum256([]byte(name))
	keep := 253 - len(prefix) - len(suffix) - 9
	return prefix + name[:keep] + "-" + hex.EncodeToString(sum[:4]) + suffix
}

// domainCertificateName and DomainTLSSecretName name a Domain's Certificate
// and the secret it fills. The secret name is exported for the API's Gateway
// views; the Gateway listener references it directly.
func domainCertificateName(name string) string {
	return domainChildName(domainChildPrefix, name, "")
}

// DomainTLSSecretName is the secret a Domain's certificate lands in.
func DomainTLSSecretName(name string) string {
	return domainChildName(domainChildPrefix, name, domainTLSNameSuffix)
}

// domainListenerName is the shared Gateway listener a Domain's HTTPS traffic
// terminates on. HTTPS needs one listener per hostname — each carries its own
// certificate — where plain HTTP shares the catch-all listener.
func domainListenerName(name string) string {
	return domainChildName(domainListenerPrefix, name, "")
}

// domainListenerReady says whether the shared Gateway should carry (and the
// environment's route should bind) an HTTPS listener for this domain: it is
// verified, its mode terminates TLS at the Gateway, and the certificate
// secret the listener would reference actually exists — a listener pointing
// at a missing or deleted secret must never be written. Both writers gate on
// this one predicate so they converge without watching each other.
func domainListenerReady(
	ctx context.Context,
	c client.Client,
	domain *kitchenv1alpha1.Domain,
	kitchen *kitchenv1alpha1.Kitchen,
) bool {
	if !domain.DeletionTimestamp.IsZero() || !domain.Status.Verified {
		return false
	}
	if domainTLSMode(domain, kitchen) != kitchenv1alpha1.TLSModeACME {
		return false
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: DomainTLSSecretName(domain.Name)}
	return c.Get(ctx, key, secret) == nil
}

// domainRouting is what an Environment's route carries for its custom
// domains: their hostnames, and the extra Gateway listeners to bind.
type domainRouting struct {
	hostnames []gatewayv1.Hostname
	sections  []gatewayv1.SectionName
}

// domainRoutingFor collects the verified Domains routed at an Environment.
// TLS-terminating domains bind their own listener once it can exist; the
// others ride the shared HTTP listener — which, on a platform whose own routes
// all live on the HTTPS listener, the route has to bind explicitly.
func domainRoutingFor(
	ctx context.Context,
	c client.Client,
	env *kitchenv1alpha1.Environment,
	kitchen *kitchenv1alpha1.Kitchen,
) (domainRouting, error) {
	routing := domainRouting{}

	domains := &kitchenv1alpha1.DomainList{}
	if err := c.List(ctx, domains, client.InNamespace(env.Namespace)); err != nil {
		return routing, err
	}
	sort.Slice(domains.Items, func(i, j int) bool {
		return domains.Items[i].Name < domains.Items[j].Name
	})

	needsHTTP := false
	for i := range domains.Items {
		domain := &domains.Items[i]
		if domain.Spec.EnvironmentRef.Name != env.Name ||
			!domain.DeletionTimestamp.IsZero() || !domain.Status.Verified {
			continue
		}
		routing.hostnames = append(routing.hostnames, gatewayv1.Hostname(domain.Spec.Hostname))
		if domainTLSMode(domain, kitchen) == kitchenv1alpha1.TLSModeACME {
			if domainListenerReady(ctx, c, domain, kitchen) {
				routing.sections = append(routing.sections, gatewayv1.SectionName(domainListenerName(domain.Name)))
			}
			continue
		}
		// none and cloudflared serve over port 80 at the Gateway.
		needsHTTP = true
	}
	if needsHTTP && string(*gatewaySection(kitchen)) != gatewayListenerHTTP {
		routing.sections = append(routing.sections, gatewayv1.SectionName(gatewayListenerHTTP))
	}
	return routing, nil
}

// mapEnvironmentToDomains enqueues the Domains attached to an Environment, so
// route changes reflect into RouteProgrammed promptly.
func (r *DomainReconciler) mapEnvironmentToDomains(ctx context.Context, obj client.Object) []ctrl.Request {
	domains := &kitchenv1alpha1.DomainList{}
	if err := r.List(ctx, domains, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "could not list domains for an environment change")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(domains.Items))
	for i := range domains.Items {
		if domains.Items[i].Spec.EnvironmentRef.Name != obj.GetName() {
			continue
		}
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: domains.Items[i].Namespace, Name: domains.Items[i].Name,
		}})
	}
	return requests
}

// mapPlatformToDomains enqueues every Domain when the platform configuration
// changes: the TLS mode a Domain inherits and the base domain its CNAME check
// compares against both live on the singleton.
func (r *DomainReconciler) mapPlatformToDomains(ctx context.Context, _ client.Object) []ctrl.Request {
	domains := &kitchenv1alpha1.DomainList{}
	if err := r.List(ctx, domains); err != nil {
		logf.FromContext(ctx).Error(err, "could not list domains after a platform change")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(domains.Items))
	for i := range domains.Items {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: domains.Items[i].Namespace, Name: domains.Items[i].Name,
		}})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *DomainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Domain{}).
		Watches(&kitchenv1alpha1.Environment{}, handler.EnqueueRequestsFromMapFunc(r.mapEnvironmentToDomains)).
		Watches(&kitchenv1alpha1.Kitchen{}, handler.EnqueueRequestsFromMapFunc(r.mapPlatformToDomains)).
		Named("domain").
		Complete(r)
}

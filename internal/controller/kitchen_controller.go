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
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

const (
	// CloudflaredImage runs the optional tunnel.
	CloudflaredImage = "cloudflare/cloudflared:2025.8.1"

	cloudflaredDeploymentName = "kitchen-cloudflared"

	// WildcardTLSSecretName holds the wildcard certificate for the base
	// domain when TLS mode is acme. The Gateway's HTTPS listener reads it, and
	// in acme mode the operator asks cert-manager to fill it.
	WildcardTLSSecretName = "kitchen-wildcard-tls"

	// acmeClusterIssuerName is the issuer the operator creates from
	// spec.tls.acme: the platform's source of certificates for names inside
	// the base domain, solving over Cloudflare DNS-01.
	acmeClusterIssuerName = "kitchen-acme"

	// acmeHTTP01ClusterIssuerName is the second issuer acme mode creates, for
	// custom domains: their zones are by definition not the one the DNS-01
	// token can write to, so they are solved over HTTP-01 through the shared
	// Gateway instead. Same ACME account details, its own registration.
	acmeHTTP01ClusterIssuerName = "kitchen-acme-http01"

	// acmeAccountKeySecretName holds the ACME account key cert-manager
	// generates on registration; acmeHTTP01AccountKeySecretName the HTTP-01
	// issuer's own, so the two registrations never race over one key. Losing
	// either means re-registering, not losing issued certificates.
	acmeAccountKeySecretName       = "kitchen-acme-account"
	acmeHTTP01AccountKeySecretName = "kitchen-acme-http01-account"

	// wildcardCertificateName is the Certificate requesting *.<baseDomain>.
	wildcardCertificateName = "kitchen-wildcard"

	// The shared Gateway's listeners. Routes name one explicitly, so that in
	// acme mode the HTTP listener can be left to the redirect alone.
	gatewayListenerHTTP  = "http"
	gatewayListenerHTTPS = "https"

	// httpsRedirectRouteName is the only route bound to the HTTP listener when
	// edge TLS is on.
	httpsRedirectRouteName = "kitchen-https-redirect"

	// defaultACMEServer matches the CRD default, for Kitchen objects written
	// before the field existed.
	defaultACMEServer = "https://acme-v02.api.letsencrypt.org/directory"

	// tunnelTokenKey is the key in the cloudflared credentials secret.
	tunnelTokenKey = "token"

	condGatewayProgrammed = "GatewayProgrammed"
	condTunnelConnected   = "TunnelConnected"
	condTelemetrySchema   = "TelemetrySchemaReady"
	condPreviewGateReady  = "PreviewGateReady"
	condCertificateReady  = "CertificateReady"
	condComponentsHealthy = "ComponentsHealthy"

	// defaultRetentionDays matches the CRD default, for Kitchen objects
	// written before the field existed.
	defaultRetentionDays = 30

	labelComponentKey = "app.kubernetes.io/name"
)

// KitchenReconciler reconciles the Kitchen singleton: it owns the platform
// namespace, the shared Gateway all Environments route through, and the
// optional cloudflared tunnel in front of it.
type KitchenReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// PreviewGateImage is the image the forward-auth gate runs. It is the
	// operator's own image — the gate is a second binary in it — and the
	// chart passes it in, because a pod cannot read its own image back.
	// Without it, protected previews have nothing to route through.
	PreviewGateImage string

	// APIReader reads straight from the API server, bypassing the cache.
	// The component survey needs it for events and pods: field selectors are
	// not served by the cache, and caching every event and pod in the cluster
	// to answer an occasional question would cost far more than it saves.
	// SetupWithManager fills it in; a nil reader only costs the survey the
	// explanatory half of its message.
	APIReader client.Reader

	// Audit is the platform's audit recorder, read here for the sequence
	// number the platform publishes as the chain's external anchor. This
	// reconciler records nothing itself: the Kitchen singleton is
	// configuration, and its own edits are recorded by the API that made
	// them.
	Audit *audit.Recorder
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=connections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=clusterissuers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives the platform's shared infrastructure.
func (r *KitchenReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, req.NamespacedName, kitchen); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	setCond := func(condType string, status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&kitchen.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: kitchen.Generation,
		})
	}

	// The shared Gateway makes a second Kitchen object meaningless; only
	// the singleton is reconciled.
	if kitchen.Name != KitchenSingletonName {
		setCond(condReady, metav1.ConditionFalse, "NotTheSingleton",
			"only the Kitchen object named \""+KitchenSingletonName+"\" is reconciled")
		return ctrl.Result{}, r.Status().Update(ctx, kitchen)
	}

	if err := r.ensurePlatformNamespace(ctx); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.applyGateway(ctx, kitchen); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.applyHTTPSRedirect(ctx, kitchen); err != nil {
		return ctrl.Result{}, err
	}

	r.reconcileCloudflared(ctx, kitchen, setCond)

	certReady := r.reconcileTLS(ctx, kitchen, setCond)
	schemaReady := r.reconcileTelemetrySchema(ctx, kitchen, setCond)
	complianceReady := r.reconcileCompliance(ctx, kitchen, setCond)
	gateReady := r.reconcilePreviewGate(ctx, kitchen, setCond)
	registryReady := r.reconcileRegistry(ctx, kitchen, setCond)
	programmed := r.observeGateway(ctx, kitchen, setCond)
	componentsHealthy := r.surveyComponents(ctx, kitchen, setCond)
	setCond(condReady, metav1.ConditionTrue, "Reconciled", "platform infrastructure is in place")

	if err := r.Status().Update(ctx, kitchen); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled kitchen",
		"gatewayProgrammed", programmed,
		"telemetrySchemaReady", schemaReady,
		"previewGateReady", gateReady,
		"registryReady", registryReady,
		"certificateReady", certReady,
		"complianceReady", complianceReady,
		"componentsHealthy", componentsHealthy)
	if !programmed || !schemaReady || !gateReady || !registryReady || !certReady ||
		!complianceReady || !componentsHealthy {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileTelemetrySchema creates the telemetry tables in ClickHouse and
// keeps their TTL in step with spec.observability.clickhouse.retentionDays.
// The store is where the collectors ship to and where the operator API reads
// logs from, but nothing in the request path depends on it, so a store that
// is still starting up (or gone) surfaces as a condition and a retry rather
// than a failed reconcile.
func (r *KitchenReconciler) reconcileTelemetrySchema(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		// Installed without a telemetry store. Nothing to manage, and
		// nothing to complain about on every reconcile.
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condTelemetrySchema)
		return true
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		setCond(condTelemetrySchema, metav1.ConditionFalse, "ConnectionSecretMissing", err.Error())
		return false
	}
	cfg, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		setCond(condTelemetrySchema, metav1.ConditionFalse, "ConnectionSecretInvalid", err.Error())
		return false
	}

	retention := kitchen.Spec.Observability.ClickHouse.RetentionDays
	if retention < 1 {
		retention = defaultRetentionDays
	}
	if err := clickhouse.New(cfg).EnsureTelemetrySchema(ctx, retention); err != nil {
		setCond(condTelemetrySchema, metav1.ConditionFalse, "SchemaNotApplied", err.Error())
		return false
	}

	setCond(condTelemetrySchema, metav1.ConditionTrue, "SchemaApplied",
		fmt.Sprintf("telemetry schema is in place, retaining %d days", retention))
	return true
}

func (r *KitchenReconciler) ensurePlatformNamespace(ctx context.Context) error {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: PlatformNamespace}, ns)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   PlatformNamespace,
		Labels: map[string]string{labelManagedByKey: labelManagedByValue},
	}}
	if err := r.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// applyGateway ensures the shared Gateway: a catch-all HTTP listener, in acme
// TLS mode an HTTPS listener terminating with the wildcard certificate, and
// one HTTPS listener per custom domain that is ready for one. In cloudflared
// mode the tunnel carries edge TLS, so no wildcard HTTPS listener exists.
//
// The HTTP listener deliberately carries no hostname: custom domains and
// their ACME HTTP-01 challenges arrive on port 80 with names outside the base
// domain, and a *.<baseDomain> listener would refuse their routes outright.
// Hostname scoping lives on the routes, which all declare theirs.
func (r *KitchenReconciler) applyGateway(ctx context.Context, kitchen *kitchenv1alpha1.Kitchen) error {
	wildcard := gatewayv1.Hostname("*." + kitchen.Spec.BaseDomain)
	allowAll := &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromAll)},
	}

	listeners := []gatewayv1.Listener{{
		Name:          gatewayListenerHTTP,
		Port:          80,
		Protocol:      gatewayv1.HTTPProtocolType,
		AllowedRoutes: allowAll,
	}}
	if kitchen.Spec.TLS.Mode == kitchenv1alpha1.TLSModeACME {
		listeners = append(listeners, gatewayv1.Listener{
			Name:          gatewayListenerHTTPS,
			Port:          443,
			Protocol:      gatewayv1.HTTPSProtocolType,
			Hostname:      &wildcard,
			AllowedRoutes: allowAll,
			TLS: &gatewayv1.GatewayTLSConfig{
				Mode: ptr.To(gatewayv1.TLSModeTerminate),
				CertificateRefs: []gatewayv1.SecretObjectReference{{
					Name: WildcardTLSSecretName,
				}},
			},
		})
	}

	domainListeners, err := r.domainListeners(ctx, kitchen, allowAll)
	if err != nil {
		return err
	}
	listeners = append(listeners, domainListeners...)

	className := kitchen.Spec.Ingress.GatewayClassName
	if className == "" {
		className = "cilium"
	}

	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{
		Name:      SharedGatewayName,
		Namespace: PlatformNamespace,
	}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, gw, func() error {
		gw.Labels = map[string]string{
			labelComponentKey: "kitchen-gateway",
			labelManagedByKey: labelManagedByValue,
		}
		gw.Spec.GatewayClassName = gatewayv1.ObjectName(className)
		gw.Spec.Listeners = listeners
		return nil
	})
	return err
}

// domainListeners builds one HTTPS listener per custom domain that is ready
// for one: verified, TLS-terminating, and with its certificate secret in
// place — the predicate domainListenerReady shares with the route writer. A
// domain whose secret is not issued yet simply has no listener, and one being
// deleted loses its listener before the finalizer removes the secret, so a
// listener never references a secret that is gone.
func (r *KitchenReconciler) domainListeners(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	allowAll *gatewayv1.AllowedRoutes,
) ([]gatewayv1.Listener, error) {
	domains := &kitchenv1alpha1.DomainList{}
	if err := r.List(ctx, domains, client.InNamespace(PlatformNamespace)); err != nil {
		return nil, err
	}
	// Name order keeps the spec stable across reconciles, and decides which
	// of two Domains claiming one hostname gets the listener: listeners on
	// one port must have distinct hostnames, so the duplicate is dropped
	// rather than writing a Gateway that conflicts with itself.
	sort.Slice(domains.Items, func(i, j int) bool {
		return domains.Items[i].Name < domains.Items[j].Name
	})

	listeners := make([]gatewayv1.Listener, 0, len(domains.Items))
	claimed := map[string]bool{}
	for i := range domains.Items {
		domain := &domains.Items[i]
		if claimed[domain.Spec.Hostname] || !domainListenerReady(ctx, r.Client, domain, kitchen) {
			continue
		}
		claimed[domain.Spec.Hostname] = true
		listeners = append(listeners, gatewayv1.Listener{
			Name:          gatewayv1.SectionName(domainListenerName(domain.Name)),
			Port:          443,
			Protocol:      gatewayv1.HTTPSProtocolType,
			Hostname:      ptr.To(gatewayv1.Hostname(domain.Spec.Hostname)),
			AllowedRoutes: allowAll,
			TLS: &gatewayv1.GatewayTLSConfig{
				Mode: ptr.To(gatewayv1.TLSModeTerminate),
				CertificateRefs: []gatewayv1.SecretObjectReference{{
					Name: gatewayv1.ObjectName(DomainTLSSecretName(domain.Name)),
				}},
			},
		})
	}
	return listeners, nil
}

// reconcileCloudflared deploys or removes the tunnel. Failures surface as
// conditions; the tunnel never blocks the Gateway.
func (r *KitchenReconciler) reconcileCloudflared(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) {
	deployKey := types.NamespacedName{Namespace: PlatformNamespace, Name: cloudflaredDeploymentName}
	cf := kitchen.Spec.Ingress.Cloudflared

	if !cf.Enabled {
		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: deployKey.Name, Namespace: deployKey.Namespace,
		}}
		if err := r.Delete(ctx, deploy); err != nil && !apierrors.IsNotFound(err) {
			setCond(condTunnelConnected, metav1.ConditionFalse, "CleanupFailed", err.Error())
			return
		}
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condTunnelConnected)
		return
	}

	if cf.TunnelSecretRef == nil {
		setCond(condTunnelConnected, metav1.ConditionFalse, "TunnelSecretMissing",
			"spec.ingress.cloudflared.tunnelSecretRef is required when cloudflared is enabled")
		return
	}

	labels := platformLabels("cloudflared", "cloudflared")
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: deployKey.Name, Namespace: deployKey.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = labels
		deploy.Spec.Replicas = ptr.To(int32(2))
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{labelComponentKey: "cloudflared"}}
		deploy.Spec.Template.Labels = labels
		deploy.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  "cloudflared",
			Image: CloudflaredImage,
			Args:  []string{"tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run"},
			Env: []corev1.EnvVar{{
				Name: "TUNNEL_TOKEN",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cf.TunnelSecretRef.Name},
					Key:                  tunnelTokenKey,
				}},
			}},
		}}
		return nil
	})
	if err != nil {
		setCond(condTunnelConnected, metav1.ConditionFalse, "DeployFailed", err.Error())
		return
	}

	available := false
	current := &appsv1.Deployment{}
	if err := r.Get(ctx, deployKey, current); err == nil {
		for _, c := range current.Status.Conditions {
			if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
				available = true
			}
		}
	}
	if available {
		setCond(condTunnelConnected, metav1.ConditionTrue, "TunnelRunning", "cloudflared is available")
	} else {
		setCond(condTunnelConnected, metav1.ConditionFalse, "TunnelPending", "cloudflared is not available yet")
	}
}

// gatewaySection names the listener a route should attach to. In acme mode the
// HTTP listener is left to the redirect alone, so everything that actually
// serves binds to the HTTPS one; in the other modes there is no HTTPS listener
// to bind to, and port 80 is where the platform answers.
func gatewaySection(kitchen *kitchenv1alpha1.Kitchen) *gatewayv1.SectionName {
	if kitchen.Spec.TLS.Mode == kitchenv1alpha1.TLSModeACME {
		return ptr.To(gatewayv1.SectionName(gatewayListenerHTTPS))
	}
	return ptr.To(gatewayv1.SectionName(gatewayListenerHTTP))
}

// applyHTTPSRedirect publishes the only route the HTTP listener carries in acme
// mode: a permanent redirect to the same URL over HTTPS.
//
// Gateway API has no listener-level redirect, so this has to be a route — and
// it only works because every other route names the HTTPS listener explicitly
// (see gatewaySection), leaving port 80 to this one. A route bound to both
// listeners would otherwise win on hostname specificity and serve the real
// thing over cleartext.
//
// In the other TLS modes port 80 is where the platform actually answers, so the
// redirect is removed rather than left to loop.
func (r *KitchenReconciler) applyHTTPSRedirect(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
) error {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Name:      httpsRedirectRouteName,
		Namespace: PlatformNamespace,
	}}

	if kitchen.Spec.TLS.Mode != kitchenv1alpha1.TLSModeACME {
		if err := r.Delete(ctx, route); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		route.Labels = map[string]string{
			labelComponentKey: httpsRedirectRouteName,
			labelManagedByKey: labelManagedByValue,
		}
		route.Spec.CommonRouteSpec = gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{
				Name:        SharedGatewayName,
				Namespace:   ptr.To(gatewayv1.Namespace(PlatformNamespace)),
				SectionName: ptr.To(gatewayv1.SectionName(gatewayListenerHTTP)),
			}},
		}
		// No hostnames: the listener's own *.<baseDomain> already scopes this,
		// and anything else arriving on port 80 should be redirected too.
		route.Spec.Hostnames = nil
		route.Spec.Rules = []gatewayv1.HTTPRouteRule{{
			Filters: []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Scheme: ptr.To("https"),
					// Permanent: the HTTP address is not one the platform ever
					// intends to serve while edge TLS is on.
					StatusCode: ptr.To(301),
				},
			}},
		}}
		return nil
	})
	return err
}

// certManagerGVK builds a reference to one of cert-manager's kinds. They are
// addressed as unstructured objects rather than through cert-manager's Go
// types: the operator only ever writes two small specs, and importing
// cert-manager would tie the platform's build to its release cadence.
func certManagerGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: kind}
}

// reconcileTLS owns edge TLS in acme mode: the ClusterIssuer the platform
// requests certificates from, and the wildcard certificate the shared
// Gateway's HTTPS listener terminates with.
//
// Both objects are admitted by cert-manager's own webhook, so on a first
// install they cannot exist until it is serving. That is exactly why they are
// created here rather than by the chart that installs cert-manager beside
// them: an API that is not up yet surfaces as a condition and a retry, and the
// next reconcile succeeds on its own.
func (r *KitchenReconciler) reconcileTLS(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	if kitchen.Spec.TLS.Mode != kitchenv1alpha1.TLSModeACME {
		// Anything already issued is deliberately left alone. ACME limits how
		// often the same names may be re-issued, so tearing certificates down
		// on a mode change would make changing back expensive.
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condCertificateReady)
		return true
	}

	// Both of the next two states are refused at admission by the CRD's own
	// validation rules, so they are only reachable on an object written before
	// those rules existed, or on a cluster whose CRDs are managed out of band
	// (crds.install=false) and left behind. They stay because a reconciler that
	// trusts the schema it was compiled against reports nothing at all when the
	// schema in the cluster is older than it is.
	acme := kitchen.Spec.TLS.ACME
	if acme == nil {
		setCond(condCertificateReady, metav1.ConditionFalse, "ACMEConfigMissing",
			"spec.tls.acme is unset, so the platform manages no certificate and the "+
				"Gateway's HTTPS listener has nothing to terminate with")
		return false
	}

	if acme.DNS01.Cloudflare == nil {
		setCond(condCertificateReady, metav1.ConditionFalse, "SolverMissing",
			"spec.tls.acme.dns01 needs a solver: a wildcard covers every generated URL, "+
				"and ACME issues wildcards over DNS-01 only")
		return false
	}

	if err := r.applyACMEClusterIssuer(ctx, acme); err != nil {
		if meta.IsNoMatchError(err) {
			setCond(condCertificateReady, metav1.ConditionFalse, "CertManagerUnavailable",
				"waiting for the cert-manager API to be served: "+err.Error())
			return false
		}
		setCond(condCertificateReady, metav1.ConditionFalse, "IssuerNotApplied", err.Error())
		return false
	}

	if err := r.applyHTTP01ClusterIssuer(ctx, acme); err != nil {
		if meta.IsNoMatchError(err) {
			setCond(condCertificateReady, metav1.ConditionFalse, "CertManagerUnavailable",
				"waiting for the cert-manager API to be served: "+err.Error())
			return false
		}
		setCond(condCertificateReady, metav1.ConditionFalse, "IssuerNotApplied", err.Error())
		return false
	}

	cert, err := r.applyWildcardCertificate(ctx, kitchen)
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
		setCond(condCertificateReady, metav1.ConditionFalse, "Issuing", message)
		return false
	}
	setCond(condCertificateReady, metav1.ConditionTrue, "Issued", message)
	return true
}

// applyACMEClusterIssuer writes the platform's issuer from spec.tls.acme.
func (r *KitchenReconciler) applyACMEClusterIssuer(
	ctx context.Context,
	acme *kitchenv1alpha1.ACMESpec,
) error {
	server := acme.Server
	if server == "" {
		server = defaultACMEServer
	}
	token := acme.DNS01.Cloudflare.APITokenSecretRef

	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(certManagerGVK("ClusterIssuer"))
	issuer.SetName(acmeClusterIssuerName)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, issuer, func() error {
		issuer.SetLabels(map[string]string{
			labelComponentKey: "kitchen-acme",
			labelManagedByKey: labelManagedByValue,
		})
		return unstructured.SetNestedMap(issuer.Object, map[string]any{
			"server":              server,
			"email":               acme.Email,
			"privateKeySecretRef": map[string]any{"name": acmeAccountKeySecretName},
			"solvers": []any{map[string]any{
				"dns01": map[string]any{
					"cloudflare": map[string]any{
						"apiTokenSecretRef": map[string]any{
							"name": token.Name,
							"key":  token.Key,
						},
					},
				},
			}},
		}, "spec", "acme")
	})
	return err
}

// applyHTTP01ClusterIssuer writes the issuer custom domains request from. It
// solves over HTTP-01 through the shared Gateway — cert-manager publishes each
// challenge as an HTTPRoute on it — because the DNS-01 solver's token only
// writes to the base domain's zone, and a custom domain's zone is someone
// else's. cert-manager needs its Gateway API support switched on for this;
// the chart does that (config.gatewayAPI.enabled on the sub-chart).
func (r *KitchenReconciler) applyHTTP01ClusterIssuer(
	ctx context.Context,
	acme *kitchenv1alpha1.ACMESpec,
) error {
	server := acme.Server
	if server == "" {
		server = defaultACMEServer
	}

	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(certManagerGVK("ClusterIssuer"))
	issuer.SetName(acmeHTTP01ClusterIssuerName)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, issuer, func() error {
		issuer.SetLabels(map[string]string{
			labelComponentKey: acmeHTTP01ClusterIssuerName,
			labelManagedByKey: labelManagedByValue,
		})
		return unstructured.SetNestedMap(issuer.Object, map[string]any{
			"server":              server,
			"email":               acme.Email,
			"privateKeySecretRef": map[string]any{"name": acmeHTTP01AccountKeySecretName},
			"solvers": []any{map[string]any{
				"http01": map[string]any{
					"gatewayHTTPRoute": map[string]any{
						"parentRefs": []any{map[string]any{
							"group":     gatewayv1.GroupName,
							"kind":      "Gateway",
							"name":      SharedGatewayName,
							"namespace": PlatformNamespace,
						}},
					},
				},
			}},
		}, "spec", "acme")
	})
	return err
}

// applyWildcardCertificate requests *.<baseDomain> into the secret the
// Gateway's HTTPS listener reads, and returns the object as it now stands so
// the caller can report on its progress.
func (r *KitchenReconciler) applyWildcardCertificate(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
) (*unstructured.Unstructured, error) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerGVK("Certificate"))
	cert.SetName(wildcardCertificateName)
	cert.SetNamespace(PlatformNamespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		cert.SetLabels(map[string]string{
			labelComponentKey: "kitchen-wildcard",
			labelManagedByKey: labelManagedByValue,
		})
		return unstructured.SetNestedMap(cert.Object, map[string]any{
			"secretName": WildcardTLSSecretName,
			"dnsNames":   []any{"*." + kitchen.Spec.BaseDomain},
			"issuerRef": map[string]any{
				"name":  acmeClusterIssuerName,
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

// certificateReady reads the Ready condition cert-manager writes on a
// Certificate, so the platform reports whether TLS actually works rather than
// only whether the request was filed. A DNS-01 order takes a while, and its
// failures — a bad API token, a zone the token cannot see — are reported here
// and nowhere else the operator can see.
func certificateReady(cert *unstructured.Unstructured) (bool, string) {
	const pending = "waiting for cert-manager to issue the certificate"

	conditions, found, err := unstructured.NestedSlice(cert.Object, "status", "conditions")
	if err != nil || !found {
		return false, pending
	}
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok || condition["type"] != "Ready" {
			continue
		}
		message, _ := condition["message"].(string)
		if message == "" {
			message = pending
		}
		return condition["status"] == "True", message
	}
	return false, pending
}

// observeGateway mirrors the Gateway's data-plane state into Kitchen status.
func (r *KitchenReconciler) observeGateway(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	gw := &gatewayv1.Gateway{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: SharedGatewayName}
	if err := r.Get(ctx, key, gw); err != nil {
		setCond(condGatewayProgrammed, metav1.ConditionFalse, "GatewayMissing", err.Error())
		return false
	}

	kitchen.Status.GatewayAddress = ""
	if len(gw.Status.Addresses) > 0 {
		kitchen.Status.GatewayAddress = gw.Status.Addresses[0].Value
	}

	for _, c := range gw.Status.Conditions {
		if c.Type == string(gatewayv1.GatewayConditionProgrammed) && c.Status == metav1.ConditionTrue {
			setCond(condGatewayProgrammed, metav1.ConditionTrue, "Programmed", "gateway is programmed")
			return true
		}
	}
	setCond(condGatewayProgrammed, metav1.ConditionFalse, "Pending",
		"waiting for the gateway controller to program the gateway")
	return false
}

// mapToSingleton enqueues the Kitchen singleton for owned infrastructure.
func (r *KitchenReconciler) mapToSingleton(_ context.Context, obj client.Object) []ctrl.Request {
	if obj.GetNamespace() != PlatformNamespace {
		return nil
	}
	if obj.GetLabels()[labelManagedByKey] != labelManagedByValue {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: KitchenSingletonName}}}
}

// mapDomainToSingleton enqueues the singleton for any Domain in the platform
// namespace. Unlike mapToSingleton it needs no managed-by label: Domains are
// user objects, not something this reconciler created.
func (r *KitchenReconciler) mapDomainToSingleton(_ context.Context, obj client.Object) []ctrl.Request {
	if obj.GetNamespace() != PlatformNamespace {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: KitchenSingletonName}}}
}

// mapPlatformWorkload enqueues the singleton for anything the component survey
// reports on. Unlike mapToSingleton this deliberately does not require the
// operator to have created the object: most platform workloads come from the
// chart and are labelled managed-by Helm, and their health is exactly what the
// survey exists to notice.
func (r *KitchenReconciler) mapPlatformWorkload(_ context.Context, obj client.Object) []ctrl.Request {
	if obj.GetNamespace() != PlatformNamespace {
		return nil
	}
	if obj.GetLabels()[labelPartOfKey] != labelPartOfValue {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: KitchenSingletonName}}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *KitchenReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Events are read through this rather than the cache; see APIReader.
	r.APIReader = mgr.GetAPIReader()

	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Kitchen{}).
		Watches(&gatewayv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.mapToSingleton)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapToSingleton)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapPlatformWorkload)).
		Watches(&appsv1.StatefulSet{}, handler.EnqueueRequestsFromMapFunc(r.mapPlatformWorkload)).
		Watches(&appsv1.DaemonSet{}, handler.EnqueueRequestsFromMapFunc(r.mapPlatformWorkload)).
		// Domains feed the Gateway's per-domain listeners; a listener also
		// waits on its certificate secret, which cert-manager labels
		// managed-by kitchen (through the Certificate's secretTemplate), so
		// mapToSingleton picks the secret up the moment it is issued.
		Watches(&kitchenv1alpha1.Domain{}, handler.EnqueueRequestsFromMapFunc(r.mapDomainToSingleton)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapToSingleton)).
		Named("kitchen").
		Complete(r)
}

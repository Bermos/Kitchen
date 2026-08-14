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
	// spec.tls.acme. It is cluster-scoped because it is the platform's one
	// source of certificates.
	acmeClusterIssuerName = "kitchen-acme"

	// acmeAccountKeySecretName holds the ACME account key cert-manager
	// generates on registration. Losing it means re-registering, not losing
	// issued certificates.
	acmeAccountKeySecretName = "kitchen-acme-account"

	// wildcardCertificateName is the Certificate requesting *.<baseDomain>.
	wildcardCertificateName = "kitchen-wildcard"

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
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
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

	r.reconcileCloudflared(ctx, kitchen, setCond)

	certReady := r.reconcileTLS(ctx, kitchen, setCond)
	schemaReady := r.reconcileTelemetrySchema(ctx, kitchen, setCond)
	gateReady := r.reconcilePreviewGate(ctx, kitchen, setCond)
	programmed := r.observeGateway(ctx, kitchen, setCond)
	setCond(condReady, metav1.ConditionTrue, "Reconciled", "platform infrastructure is in place")

	if err := r.Status().Update(ctx, kitchen); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled kitchen",
		"gatewayProgrammed", programmed,
		"telemetrySchemaReady", schemaReady,
		"previewGateReady", gateReady,
		"certificateReady", certReady)
	if !programmed || !schemaReady || !gateReady || !certReady {
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
	if err := clickhouse.New(cfg).EnsureLogsSchema(ctx, retention); err != nil {
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

// applyGateway ensures the shared Gateway with a wildcard HTTP listener and,
// in acme TLS mode, an HTTPS listener terminating with the wildcard
// certificate. In cloudflared mode the tunnel carries edge TLS, so only the
// HTTP listener exists.
func (r *KitchenReconciler) applyGateway(ctx context.Context, kitchen *kitchenv1alpha1.Kitchen) error {
	wildcard := gatewayv1.Hostname("*." + kitchen.Spec.BaseDomain)
	allowAll := &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromAll)},
	}

	listeners := []gatewayv1.Listener{{
		Name:          "http",
		Port:          80,
		Protocol:      gatewayv1.HTTPProtocolType,
		Hostname:      &wildcard,
		AllowedRoutes: allowAll,
	}}
	if kitchen.Spec.TLS.Mode == kitchenv1alpha1.TLSModeACME {
		listeners = append(listeners, gatewayv1.Listener{
			Name:          "https",
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

	className := kitchen.Spec.Ingress.GatewayClassName
	if className == "" {
		className = "cilium"
	}

	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{
		Name:      SharedGatewayName,
		Namespace: PlatformNamespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, gw, func() error {
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

	labels := map[string]string{
		labelComponentKey: "cloudflared",
		labelManagedByKey: labelManagedByValue,
	}
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

// SetupWithManager sets up the controller with the Manager.
func (r *KitchenReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Kitchen{}).
		Watches(&gatewayv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.mapToSingleton)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapToSingleton)).
		Named("kitchen").
		Complete(r)
}

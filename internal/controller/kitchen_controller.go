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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	// CloudflaredImage runs the optional tunnel.
	CloudflaredImage = "cloudflare/cloudflared:2025.8.1"

	cloudflaredDeploymentName = "kitchen-cloudflared"

	// WildcardTLSSecretName holds the wildcard certificate for the base
	// domain when TLS mode is acme (issued by cert-manager, integration
	// pending).
	WildcardTLSSecretName = "kitchen-wildcard-tls"

	// tunnelTokenKey is the key in the cloudflared credentials secret.
	tunnelTokenKey = "token"

	condGatewayProgrammed = "GatewayProgrammed"
	condTunnelConnected   = "TunnelConnected"

	labelComponentKey = "app.kubernetes.io/name"
)

// KitchenReconciler reconciles the Kitchen singleton: it owns the platform
// namespace, the shared Gateway all Environments route through, and the
// optional cloudflared tunnel in front of it.
type KitchenReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create

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

	programmed := r.observeGateway(ctx, kitchen, setCond)
	setCond(condReady, metav1.ConditionTrue, "Reconciled", "platform infrastructure is in place")

	if err := r.Status().Update(ctx, kitchen); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled kitchen", "gatewayProgrammed", programmed)
	if !programmed {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
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

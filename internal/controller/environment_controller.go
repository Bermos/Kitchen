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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/previewgate"
)

const (
	// PlatformNamespace is where Kitchen CRs and the shared Gateway live.
	PlatformNamespace = "kitchen-system"
	// SharedGatewayName is the Gateway all HTTPRoutes attach to.
	SharedGatewayName = "kitchen"
	// KitchenSingletonName is the name of the cluster-wide Kitchen object.
	KitchenSingletonName = "default"

	// LabelEnvironment marks everything an Environment materializes, and is
	// what its Deployment and Service select their pods on. It is exported
	// because the API finds those pods the same way, and a second spelling of
	// a selector is a bug waiting for the first rename.
	LabelEnvironment = "kitchen.bermos.dev/environment"

	// AppContainerName is the container an Environment's pod runs the
	// application in. The API reads the workload's image and resources back
	// off it.
	AppContainerName = "app"

	labelProject        = "kitchen.bermos.dev/project"
	labelEnvironment    = LabelEnvironment
	labelEnvironmentNS  = "kitchen.bermos.dev/environment-namespace"
	labelManagedByKey   = "app.kubernetes.io/managed-by"
	labelManagedByValue = "kitchen"

	environmentFinalizer = "kitchen.bermos.dev/environment-cleanup"

	condReady             = "Ready"
	condWorkloadAvailable = "WorkloadAvailable"
	condRouteProgrammed   = "RouteProgrammed"
	condPreviewProtected  = "PreviewProtected"
)

// EnvironmentReconciler reconciles an Environment: it materializes the
// referenced Release as a Deployment, Service and HTTPRoute in the project's
// application namespace and reports the resulting URL.
type EnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Activity feeds the dashboard's recent-activity feed. May be nil.
	Activity *activity.Recorder
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments/finalizers,verbs=update
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=projects;releases;resourceclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives an Environment towards its Release.
func (r *EnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	env := &kitchenv1alpha1.Environment{}
	if err := r.Get(ctx, req.NamespacedName, env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !env.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, env)
	}

	if controllerutil.AddFinalizer(env, environmentFinalizer) {
		if err := r.Update(ctx, env); err != nil {
			return ctrl.Result{}, err
		}
	}

	project := &kitchenv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: env.Spec.ProjectRef.Name}, project); err != nil {
		return r.notReady(ctx, env, "ProjectMissing", err)
	}

	release := &kitchenv1alpha1.Release{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: env.Spec.ReleaseRef.Name}, release); err != nil {
		return r.notReady(ctx, env, "ReleaseMissing", err)
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return r.notReady(ctx, env, "PlatformConfigMissing", err)
	}

	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}

	podEnv, requeue, err := r.resolveEnv(ctx, env, release)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeue {
		return r.notReady(ctx, env, "ClaimNotBound",
			fmt.Errorf("a referenced ResourceClaim is not bound yet"))
	}

	labels := childLabels(project.Name, env)

	if err := r.applyDeployment(ctx, env, release, appNS, labels, podEnv); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyService(ctx, env, release, appNS, labels); err != nil {
		return ctrl.Result{}, err
	}

	// Only previews are ever gated: a production environment is the
	// application's public address.
	protected := env.Spec.Type == kitchenv1alpha1.EnvironmentPreview && project.Spec.Previews.IsProtected()
	gate := previewGate(kitchen)
	if protected && gate == nil {
		// Asked to protect a preview on a platform with no gate. Publishing
		// it anyway would be the one outcome the Project explicitly did not
		// ask for, so it gets no route at all until the platform grows a gate
		// or the Project opts out of protection.
		if err := r.deleteRoute(ctx, appNS, env.Name); err != nil {
			return ctrl.Result{}, err
		}
		return r.unprotectable(ctx, env)
	}
	if !protected {
		gate = nil
	}

	host := hostname(project.Name, env, kitchen.Spec.BaseDomain)
	if err := r.applyHTTPRoute(ctx, env, appNS, labels, host, gate, gatewaySection(kitchen)); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("reconciled environment", "namespace", appNS, "host", host, "protected", protected)
	return r.updateStatus(ctx, env, release, kitchen, appNS, host, protected)
}

// finalize deletes the Environment's children and releases the finalizer. The
// children live in another namespace, so owner references cannot garbage
// collect them for us.
func (r *EnvironmentReconciler) finalize(ctx context.Context, env *kitchenv1alpha1.Environment) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(env, environmentFinalizer) {
		return ctrl.Result{}, nil
	}

	appNS := appNamespace(env.Spec.ProjectRef.Name)
	objectMeta := metav1.ObjectMeta{Name: env.Name, Namespace: appNS}
	children := []client.Object{
		&appsv1.Deployment{ObjectMeta: objectMeta},
		&corev1.Service{ObjectMeta: objectMeta},
		&gatewayv1.HTTPRoute{ObjectMeta: objectMeta},
	}
	for _, child := range children {
		if err := r.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(env, environmentFinalizer)
	if err := r.Update(ctx, env); err != nil {
		return ctrl.Result{}, err
	}
	if env.Spec.Type == kitchenv1alpha1.EnvironmentPreview && env.Spec.Preview != nil {
		r.Activity.Record(ctx, clickhouse.Event{
			Type:        clickhouse.EventPreviewRemoved,
			Project:     env.Spec.ProjectRef.Name,
			Environment: env.Name,
			Message:     fmt.Sprintf("preview for PR #%d removed", env.Spec.Preview.PullRequest),
		})
	}
	return ctrl.Result{}, nil
}

func ensureNamespace(ctx context.Context, c client.Client, name, projectName string) error {
	ns := &corev1.Namespace{}
	err := c.Get(ctx, types.NamespacedName{Name: name}, ns)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			labelProject:      projectName,
			labelManagedByKey: labelManagedByValue,
		},
	}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// resolveEnv turns the Release's config snapshot into container env vars for
// this environment type. It returns requeue=true when a referenced
// ResourceClaim exists but has no binding secret yet.
func (r *EnvironmentReconciler) resolveEnv(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
) ([]corev1.EnvVar, bool, error) {
	isPreview := env.Spec.Type == kitchenv1alpha1.EnvironmentPreview
	var out []corev1.EnvVar
	for _, v := range release.Spec.ConfigSnapshot.Env {
		switch {
		case v.FromResourceClaim != nil:
			claim := &kitchenv1alpha1.ResourceClaim{}
			key := types.NamespacedName{Namespace: env.Namespace, Name: v.FromResourceClaim.Name}
			if err := r.Get(ctx, key, claim); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, true, nil
				}
				return nil, false, err
			}
			if claim.Status.SecretName == "" {
				return nil, true, nil
			}
			out = append(out, corev1.EnvVar{
				Name: v.Name,
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: claim.Status.SecretName},
					Key:                  v.FromResourceClaim.Key,
				}},
			})
		case v.SecretRef != nil:
			out = append(out, corev1.EnvVar{
				Name: v.Name,
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: v.SecretRef.Name},
					Key:                  v.SecretRef.Key,
				}},
			})
		default:
			value := v.Value
			if isPreview && v.PreviewValue != "" {
				value = v.PreviewValue
			}
			out = append(out, corev1.EnvVar{Name: v.Name, Value: value})
		}
	}
	return out, false, nil
}

func (r *EnvironmentReconciler) applyDeployment(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	appNS string,
	labels map[string]string,
	podEnv []corev1.EnvVar,
) error {
	runtimeSpec := release.Spec.ConfigSnapshot.Runtime
	port := runtimeSpec.Port
	if port == 0 {
		port = 3000
	}
	replicas := int32(1)
	if env.Spec.Type != kitchenv1alpha1.EnvironmentPreview && runtimeSpec.Replicas != nil {
		replicas = *runtimeSpec.Replicas
	}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: env.Name, Namespace: appNS}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = labels
		deploy.Spec.Replicas = ptr.To(replicas)
		deploy.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{labelEnvironment: env.Name},
		}
		deploy.Spec.Template.Labels = labels
		deploy.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:      AppContainerName,
			Image:     release.Spec.Image,
			Ports:     []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
			Env:       podEnv,
			Resources: runtimeSpec.Resources,
		}}
		return nil
	})
	return err
}

func (r *EnvironmentReconciler) applyService(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	appNS string,
	labels map[string]string,
) error {
	port := release.Spec.ConfigSnapshot.Runtime.Port
	if port == 0 {
		port = 3000
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: env.Name, Namespace: appNS}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = map[string]string{labelEnvironment: env.Name}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       80,
			TargetPort: intstr.FromInt32(port),
		}}
		return nil
	})
	return err
}

// applyHTTPRoute publishes the Environment on the shared Gateway. A gate
// turns the same route into a protected one: traffic goes to the gate
// instead, carrying the application's address in a header the Gateway sets —
// so one gate serves every preview on the platform without knowing about any
// of them in advance.
func (r *EnvironmentReconciler) applyHTTPRoute(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	appNS string,
	labels map[string]string,
	host string,
	gate *previewGateBackend,
	// section is the Gateway listener to attach to. With edge TLS on, port 80
	// carries only the redirect, so an application route that also bound there
	// would serve the app over cleartext.
	section *gatewayv1.SectionName,
) error {
	if gate != nil {
		// The route and the gate live in different namespaces, and Gateway
		// API only allows that with the target namespace's permission.
		if err := r.allowGateBackend(ctx, appNS, gate); err != nil {
			return err
		}
	}

	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: env.Name, Namespace: appNS}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		route.Labels = labels
		route.Spec.CommonRouteSpec = gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{
				Name:        SharedGatewayName,
				Namespace:   ptr.To(gatewayv1.Namespace(PlatformNamespace)),
				SectionName: section,
			}},
		}
		route.Spec.Hostnames = []gatewayv1.Hostname{gatewayv1.Hostname(host)}

		rule := gatewayv1.HTTPRouteRule{
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(env.Name),
						Port: ptr.To(gatewayv1.PortNumber(80)),
					},
				},
			}},
		}
		if gate != nil {
			rule.Filters = []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
				RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
					// Set, not Add: whatever the client sent under this name
					// is overwritten before the gate ever sees it.
					Set: []gatewayv1.HTTPHeader{{
						Name:  previewgate.UpstreamHeader,
						Value: upstreamAddress(appNS, env.Name, 80),
					}},
				},
			}}
			rule.BackendRefs = []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name:      gatewayv1.ObjectName(gate.Service),
						Namespace: ptr.To(gatewayv1.Namespace(PlatformNamespace)),
						Port:      ptr.To(gatewayv1.PortNumber(gate.Port)),
					},
				},
			}}
		}
		route.Spec.Rules = []gatewayv1.HTTPRouteRule{rule}
		return nil
	})
	return err
}

// allowGateBackend grants the project's namespace permission to route to the
// gate. One grant covers every protected Environment of that namespace, and
// it names the single Service — a cross-namespace reference is exactly as
// wide as it has to be.
//
// It outlives the Environments that needed it, like the application namespace
// itself does: a project whose previews are all closed will want it again on
// the next pull request.
func (r *EnvironmentReconciler) allowGateBackend(ctx context.Context, appNS string, gate *previewGateBackend) error {
	grant := &gatewayv1beta1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{
		Name:      referenceGrantName(appNS),
		Namespace: PlatformNamespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, grant, func() error {
		grant.Labels = map[string]string{
			labelComponentKey: "kitchen-preview-gate",
			labelManagedByKey: labelManagedByValue,
		}
		grant.Spec.From = []gatewayv1beta1.ReferenceGrantFrom{{
			Group:     gatewayv1.GroupName,
			Kind:      "HTTPRoute",
			Namespace: gatewayv1beta1.Namespace(appNS),
		}}
		grant.Spec.To = []gatewayv1beta1.ReferenceGrantTo{{
			Group: "",
			Kind:  "Service",
			Name:  ptr.To(gatewayv1beta1.ObjectName(gate.Service)),
		}}
		return nil
	})
	return err
}

// referenceGrantName is stable per application namespace, so the grant is
// created once and found again by every Environment in it.
func referenceGrantName(appNS string) string {
	return "kitchen-preview-gate-" + appNS
}

// deleteRoute takes an Environment off the Gateway.
func (r *EnvironmentReconciler) deleteRoute(ctx context.Context, appNS, name string) error {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}}
	if err := r.Delete(ctx, route); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *EnvironmentReconciler) updateStatus(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	kitchen *kitchenv1alpha1.Kitchen,
	appNS string,
	host string,
	protected bool,
) (ctrl.Result, error) {
	scheme := platformScheme(kitchen)

	deploy := &appsv1.Deployment{}
	available := false
	if err := r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: env.Name}, deploy); err == nil {
		for _, c := range deploy.Status.Conditions {
			if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
				available = true
			}
		}
	}

	// The release the workload ran until now stops being current here. The
	// spec writers (auto-promotion, the API) record the move themselves with
	// cause and caller; this records the ones nobody did — kubectl on the
	// spec, or a writer whose status update was lost. RecordReleaseMove
	// skips a move whose entry already exists.
	if outgoing := env.Status.ObservedRelease; outgoing != release.Name {
		env.RecordReleaseMove(outgoing, kitchenv1alpha1.ReleaseMoveSuperseded, "")
	}

	env.Status.URL = fmt.Sprintf("%s://%s", scheme, host)
	env.Status.ObservedRelease = release.Name
	if available {
		env.Status.Phase = kitchenv1alpha1.EnvironmentLive
	} else {
		env.Status.Phase = kitchenv1alpha1.EnvironmentDeploying
	}

	setCond := func(condType string, status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: env.Generation,
		})
	}
	setCond(condRouteProgrammed, metav1.ConditionTrue, "Applied", "HTTPRoute applied")
	switch {
	case protected:
		setCond(condPreviewProtected, metav1.ConditionTrue, "GatedByPlatformLogin",
			fmt.Sprintf("requests are gated behind platform login at %s", previewGateHost(kitchen)))
	case env.Spec.Type == kitchenv1alpha1.EnvironmentPreview:
		setCond(condPreviewProtected, metav1.ConditionFalse, "Public",
			"spec.previews.protected is off for this Project: anyone with the URL can reach this preview")
	default:
		// Production environments are public by definition; the condition
		// would only ever be noise on them.
		meta.RemoveStatusCondition(&env.Status.Conditions, condPreviewProtected)
	}
	if available {
		setCond(condWorkloadAvailable, metav1.ConditionTrue, "DeploymentAvailable", "workload is available")
		setCond(condReady, metav1.ConditionTrue, "Reconciled", "environment is live")
	} else {
		setCond(condWorkloadAvailable, metav1.ConditionFalse, "DeploymentUnavailable", "workload is not available yet")
		setCond(condReady, metav1.ConditionFalse, "WorkloadPending", "waiting for workload to become available")
	}

	if err := r.Status().Update(ctx, env); err != nil {
		return ctrl.Result{}, err
	}
	if !available {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// unprotectable records a preview that asked to be gated on a platform with
// no gate to route it through. The workload is deployed either way — it is
// the URL that is withheld, and only until the platform can protect it.
func (r *EnvironmentReconciler) unprotectable(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
) (ctrl.Result, error) {
	const message = "the Project asks for protected previews, but the platform runs no forward-auth gate " +
		"(spec.auth.enabled and spec.auth.previewGate.enabled on the Kitchen object). " +
		"No route is published: set spec.previews.protected=false on the Project to serve this preview openly."

	env.Status.Phase = kitchenv1alpha1.EnvironmentPending
	env.Status.URL = ""
	for _, condition := range []metav1.Condition{
		{Type: condPreviewProtected, Status: metav1.ConditionFalse, Reason: "PreviewGateUnavailable", Message: message},
		{Type: condRouteProgrammed, Status: metav1.ConditionFalse, Reason: "PreviewGateUnavailable", Message: message},
		{Type: condReady, Status: metav1.ConditionFalse, Reason: "PreviewGateUnavailable", Message: message},
	} {
		condition.ObservedGeneration = env.Generation
		meta.SetStatusCondition(&env.Status.Conditions, condition)
	}
	if err := r.Status().Update(ctx, env); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// notReady records a Ready=False condition with the given reason and retries.
func (r *EnvironmentReconciler) notReady(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	reason string,
	cause error,
) (ctrl.Result, error) {
	env.Status.Phase = kitchenv1alpha1.EnvironmentPending
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               condReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		ObservedGeneration: env.Generation,
	})
	if err := r.Status().Update(ctx, env); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// AppNamespace is the namespace a project's workloads run in. It is exported
// for the API, which joins per-namespace telemetry back to project names.
func AppNamespace(projectName string) string {
	return appNamespace(projectName)
}

func appNamespace(projectName string) string {
	return "kitchen-" + projectName
}

// hostname computes the environment's generated host. Production gets the
// project slug; previews get <project>-pr-<n>.
func hostname(projectName string, env *kitchenv1alpha1.Environment, baseDomain string) string {
	slug := projectName
	if env.Spec.Type == kitchenv1alpha1.EnvironmentPreview && env.Spec.Preview != nil {
		slug = fmt.Sprintf("%s-pr-%d", projectName, env.Spec.Preview.PullRequest)
	}
	return fmt.Sprintf("%s.%s", slug, baseDomain)
}

func childLabels(projectName string, env *kitchenv1alpha1.Environment) map[string]string {
	return map[string]string{
		labelProject:       projectName,
		labelEnvironment:   env.Name,
		labelEnvironmentNS: env.Namespace,
		labelManagedByKey:  labelManagedByValue,
	}
}

// mapChildToEnvironment enqueues the owning Environment for a labeled child.
func (r *EnvironmentReconciler) mapChildToEnvironment(_ context.Context, obj client.Object) []ctrl.Request {
	labels := obj.GetLabels()
	name, ok := labels[labelEnvironment]
	if !ok {
		return nil
	}
	ns, ok := labels[labelEnvironmentNS]
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *EnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Environment{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapChildToEnvironment)).
		Named("environment").
		Complete(r)
}

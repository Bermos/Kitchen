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
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/gitprovider"
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

	// LabelProject names the project everything an Environment materializes
	// belongs to. The usage collector attributes a container's resources with
	// it, which is the join a metrics scrape of the kubelet cannot make.
	LabelProject = "kitchen.bermos.dev/project"

	// AppContainerName is the container an Environment's pod runs the
	// application in. The API reads the workload's image and resources back
	// off it.
	AppContainerName = "app"

	// servicePort is the port an Environment's Service is published on,
	// whatever the container listens on behind it. Everything that addresses
	// the application — the Gateway's backend, the preview gate's upstream,
	// the interceptor's scale target — names this one.
	servicePort = int32(80)

	// defaultContainerPort matches the CRD default, for Releases whose
	// snapshot predates the field.
	defaultContainerPort = int32(3000)

	labelProject        = LabelProject
	labelEnvironment    = LabelEnvironment
	labelEnvironmentNS  = "kitchen.bermos.dev/environment-namespace"
	labelManagedByKey   = "app.kubernetes.io/managed-by"
	labelManagedByValue = "kitchen"

	// The Pod Security Standards labels, as written on every namespace the
	// operator creates for a project.
	labelPodSecurityEnforce = "pod-security.kubernetes.io/enforce"
	labelPodSecurityAudit   = "pod-security.kubernetes.io/audit"
	labelPodSecurityWarn    = "pod-security.kubernetes.io/warn"

	environmentFinalizer = "kitchen.bermos.dev/environment-cleanup"

	condReady             = "Ready"
	condWorkloadAvailable = "WorkloadAvailable"
	condRouteProgrammed   = "RouteProgrammed"
	condPreviewProtected  = "PreviewProtected"
	condScaleToZero       = "ScaleToZero"
)

// EnvironmentReconciler reconciles an Environment: it materializes the
// referenced Release as a Deployment, Service and HTTPRoute in the project's
// application namespace and reports the resulting URL.
type EnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Activity feeds the dashboard's recent-activity feed. May be nil.
	Activity *activity.Recorder
	// GitProviders resolves a Provider for a Connection, for reporting the
	// deployment (and a preview's URL) back to the pull request. Defaults to
	// gitprovider.Default; tests inject fakes.
	GitProviders gitprovider.Factory
	// Audit appends this reconciler's state transitions to the tamper-evident
	// log. Unlike Activity it is waited on: a transition it refuses is a
	// transition this reconciler does not make. May be nil.
	Audit *audit.Recorder
}

// git reports deploy status back to the repository the Environment's commit
// came from. Posting is best effort: it never fails a deployment.
func (r *EnvironmentReconciler) git() gitReporting {
	return gitReporting{Client: r.Client, Factory: r.GitProviders}
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments/finalizers,verbs=update
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=projects;releases;resourceclaims;domains;builds;connections,verbs=get;list;watch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// `deletecollection` is what DeleteAllOf asks for, and it is not implied by
// `delete`: tearing an environment down removes its runs by label rather than
// by name, because a run started by hand is owned by nothing and would
// otherwise be the one thing left in the namespace.
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete;deletecollection
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=http.keda.sh,resources=httpscaledobjects,verbs=get;list;watch;create;update;patch;delete

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

	// The finalizer is added exactly once, on the first reconcile after the
	// object appears, which makes it the one place an Environment's creation
	// can be recorded no matter who created it — a build, the API, or
	// somebody with kubectl.
	if controllerutil.AddFinalizer(env, environmentFinalizer) {
		if err := r.Audit.Record(ctx, audit.Transition{
			Object:     env,
			Kind:       audit.KindEnvironment,
			Operation:  clickhouse.AuditCreate,
			Controller: actorEnvironmentController,
			To:         env.Spec.ReleaseRef.Name,
			Project:    env.Spec.ProjectRef.Name,
			Reason:     fmt.Sprintf("environment %s appeared", env.Name),
			Details:    map[string]any{"type": string(env.Spec.Type), "release": env.Spec.ReleaseRef.Name},
		}); err != nil {
			return ctrl.Result{}, err
		}
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

	labels := childLabels(project.Name, env)
	host := hostname(project.Name, env, kitchen.Spec.BaseDomain)

	// Only previews are ever gated: a production environment is the
	// application's public address. A preview asked to be protected on a
	// platform with no gate gets no route at all — publishing it anyway would
	// be the one outcome the Project explicitly did not ask for.
	protected := env.Spec.Type == kitchenv1alpha1.EnvironmentPreview && project.Spec.Previews.IsProtected()
	gate := previewGate(kitchen)
	unprotectable := protected && gate == nil
	if !protected {
		gate = nil
	}

	podEnv, requeue, err := r.resolveEnv(ctx, env, release)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeue {
		return r.notReady(ctx, env, "ClaimNotBound",
			fmt.Errorf("a referenced ResourceClaim is not bound yet"))
	}
	// The platform's own variables go first, so that a project setting one of
	// them wins: the kubelet takes the last value of a repeated name, and an
	// application that has been told where to send its spans knows something
	// the platform does not.
	//
	// The address is passed in rather than read off the status, because the
	// status is written at the end of this reconcile: an environment would
	// otherwise spend its first deployment not knowing its own URL and roll
	// again once it did. It is empty exactly when the environment gets no
	// route, which is a preview the platform will not publish.
	publicURL := ""
	if !unprotectable {
		publicURL = fmt.Sprintf("%s://%s", platformScheme(kitchen), host)
	}
	podEnv = append(platformEnv(kitchen, project.Name, env, release, publicURL), podEnv...)

	runtimeSpec := release.Spec.ConfigSnapshot.Runtime
	idle, idleCond, err := r.reconcileScaleToZero(ctx, env, project, kitchen, appNS, host, labels,
		servicePort, desiredReplicas(env, runtimeSpec), !unprotectable)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.applyDeployment(ctx, env, release, project, appNS, labels, podEnv, idle != nil); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyService(ctx, env, release, appNS, labels); err != nil {
		return ctrl.Result{}, err
	}

	// The processes are materialized before the route, and before the
	// unpublishable branch below, because they have nothing to do with the
	// route: a worker is not addressed and a scheduled job is not either. A
	// preview the platform will not publish still runs whatever its project
	// asked it to run.
	processes, err := r.reconcileProcesses(ctx, env, project, release, appNS, labels, podEnv)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Assigned after the recording pass inside, which compares against what
	// the status still holds from the previous reconcile.
	env.Status.Processes = processes

	if unprotectable {
		if err := r.deleteRoute(ctx, appNS, env.Name); err != nil {
			return ctrl.Result{}, err
		}
		return r.unprotectable(ctx, env)
	}

	// Verified custom domains ride this environment's route; see
	// domainRoutingFor. The Domain reconciler owns everything else about them.
	domains, err := domainRoutingFor(ctx, r.Client, env, kitchen)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.applyHTTPRoute(ctx, env, appNS, labels, host, gate, idle, gatewaySection(kitchen), domains); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("reconciled environment", "namespace", appNS, "host", host,
		"protected", protected, "idlesToZero", idle != nil)
	return r.updateStatus(ctx, env, project, release, kitchen, appNS, host, protected, idleCond)
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
	// The scaled object goes too, and separately: an environment that never
	// idled has none, and a platform without the HTTP add-on has no API to
	// ask, neither of which is a failure to tear down.
	if err := r.deleteHTTPScaledObject(ctx, appNS, env.Name); err != nil {
		return ctrl.Result{}, err
	}
	// So do the workers and the scheduled jobs, which are named after the
	// environment and the process rather than after the environment alone and
	// so are found by their label instead of being listed above.
	if err := r.deleteProcesses(ctx, env, appNS); err != nil {
		return ctrl.Result{}, err
	}

	// Tell the pull request before the finalizer goes: once the Environment
	// is gone, so is the record of what was posted about it, and the comment
	// would go on advertising a URL that no longer answers.
	r.git().retireEnvironment(ctx, env, env.Status.GitReport)

	if err := r.Audit.Record(ctx, audit.Transition{
		Object:     env,
		Kind:       audit.KindEnvironment,
		Operation:  clickhouse.AuditDelete,
		Controller: actorEnvironmentController,
		From:       env.Spec.ReleaseRef.Name,
		Project:    env.Spec.ProjectRef.Name,
		Reason:     fmt.Sprintf("environment %s and everything it was running were torn down", env.Name),
		Details:    map[string]any{"type": string(env.Spec.Type), "release": env.Spec.ReleaseRef.Name},
	}); err != nil {
		return ctrl.Result{}, err
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

// appNamespacePodSecurity is the Pod Security level application namespaces are
// labelled with, read off the platform singleton.
//
// A cluster with no singleton yet — a project reconciling against a chart
// whose post-install hook has not landed, or a test — gets the CRD's own
// default rather than an error: refusing to make the namespace would leave the
// project with nowhere to build, over a level that is about to be the default
// anyway.
func appNamespacePodSecurity(ctx context.Context, c client.Client) (kitchenv1alpha1.PodSecurityLevel, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := c.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		if apierrors.IsNotFound(err) {
			return kitchenv1alpha1.PodSecurityPrivileged, nil
		}
		return "", err
	}
	if kitchen.Spec.AppNamespaces.PodSecurity == "" {
		return kitchenv1alpha1.PodSecurityPrivileged, nil
	}
	return kitchen.Spec.AppNamespaces.PodSecurity, nil
}

// appNamespaceLabels is the metadata the operator owns on an application
// namespace: whose project it is, that the platform made it, and the Pod
// Security level its pods are admitted against.
//
// The level is written rather than inherited because the cluster default
// decides whether half the platform works: Talos defaults to baseline, which
// refuses the Dockerfile builder's pods outright, and a Job whose pods are
// refused creates no pod at all — the build sits in Running with nothing
// behind it. The platform namespace has been labelled explicitly for the same
// kind of reason since the log collector arrived; these namespaces are the
// other half of it.
func appNamespaceLabels(projectName string, level kitchenv1alpha1.PodSecurityLevel) map[string]string {
	return map[string]string{
		labelProject:            projectName,
		labelManagedByKey:       labelManagedByValue,
		labelPodSecurityEnforce: string(level),
		labelPodSecurityAudit:   string(level),
		labelPodSecurityWarn:    string(level),
	}
}

func ensureNamespace(ctx context.Context, c client.Client, name, projectName string) error {
	level, err := appNamespacePodSecurity(ctx, c)
	if err != nil {
		return err
	}
	labels := appNamespaceLabels(projectName, level)

	ns := &corev1.Namespace{}
	err = c.Get(ctx, types.NamespacedName{Name: name}, ns)
	if err == nil {
		// An existing namespace is relabelled rather than left alone. A
		// namespace is created once and every reconcile after that finds it,
		// so a level that were only written at creation would never reach an
		// installation that already has projects — which is every
		// installation upgrading into this.
		patch := client.MergeFrom(ns.DeepCopy())
		changed := false
		for key, value := range labels {
			if ns.Labels[key] == value {
				continue
			}
			if ns.Labels == nil {
				ns.Labels = map[string]string{}
			}
			ns.Labels[key] = value
			changed = true
		}
		if !changed {
			return nil
		}
		return c.Patch(ctx, ns, patch)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
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
			secretName := claim.Status.SecretName
			// A branching claim gives every preview its own database branch;
			// the preview reads the branch's binding, and waits for it the
			// same way an unbound claim is waited for.
			if isPreview && claim.PreviewBranching() {
				secretName = ""
				for _, branch := range claim.Status.Branches {
					if branch.Environment == env.Name {
						secretName = branch.SecretName
					}
				}
				if secretName == "" {
					return nil, true, nil
				}
			}
			out = append(out, corev1.EnvVar{
				Name: v.Name,
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
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

// platformEnv is everything the platform tells an application about itself.
//
// PORT comes first because it is the one variable an application is expected
// to have read before it can serve anything at all: a buildpacks-built image
// starts whatever process the buildpack chose, and every buildpack's answer to
// "which port" is $PORT. A Dockerfile build that ignores it is unaffected —
// the value is the port the Service already routes to.
func platformEnv(
	kitchen *kitchenv1alpha1.Kitchen,
	projectName string,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	publicURL string,
) []corev1.EnvVar {
	port := containerPort(release.Spec.ConfigSnapshot.Runtime)
	vars := []corev1.EnvVar{
		{Name: "PORT", Value: strconv.Itoa(int(port))},
	}
	if publicURL != "" {
		// Where this environment is published, which the application cannot
		// work out for itself: a preview's hostname carries a pull request
		// number nothing in the repository knows. It is what an OIDC client
		// builds its redirect URI from — the address half of what a
		// ResourceClaim of type oidcClient hands over — and what every
		// framework that needs to write an absolute link is asking for when
		// it demands a base URL.
		vars = append(vars, corev1.EnvVar{Name: "KITCHEN_URL", Value: publicURL})
	}
	return append(vars, telemetryEnv(kitchen, projectName, env)...)
}

// telemetryEnv is what the platform tells an application about itself: where
// the trace receiver is, and who is calling.
//
// This is the whole of Kitchen's tracing setup as far as an application is
// concerned. Every OpenTelemetry SDK reads these variables, so instrumenting
// an application is adding the SDK — there is no endpoint to look up, no
// collector to deploy, and no resource attributes to remember to set. The
// project and environment go in as attributes because the application has no
// way of knowing them and the trace view needs them to say where a span ran.
//
// Nothing is injected when the platform does not collect traces: an
// application pointed at an endpoint that refuses its exports would spend the
// rest of its life retrying them.
func telemetryEnv(
	kitchen *kitchenv1alpha1.Kitchen,
	projectName string,
	env *kitchenv1alpha1.Environment,
) []corev1.EnvVar {
	traces := kitchen.Spec.Observability.Traces
	endpoint := strings.TrimSpace(traces.Endpoint)
	if !traces.Enabled || endpoint == "" {
		return nil
	}
	// One service per project, not per environment: production and a preview
	// of the same application are the same service seen at two moments, and
	// splitting them would make every preview a new entry in the service list
	// that is never seen again. Which is which is an attribute — the one
	// semantic conventions define for exactly this.
	attributes := []string{
		"service.name=" + projectName,
		"kitchen.project=" + projectName,
		"kitchen.environment=" + env.Name,
		"deployment.environment.name=" + env.Name,
	}
	return []corev1.EnvVar{
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: endpoint},
		{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "http/protobuf"},
		{Name: "OTEL_SERVICE_NAME", Value: projectName},
		{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: strings.Join(attributes, ",")},
	}
}

// desiredReplicas is how many pods an Environment runs when it is not idling.
// Previews always run one; production runs what the Release was built with.
func desiredReplicas(env *kitchenv1alpha1.Environment, runtimeSpec kitchenv1alpha1.RuntimeSpec) int32 {
	if env.Spec.Type == kitchenv1alpha1.EnvironmentPreview || runtimeSpec.Replicas == nil {
		return 1
	}
	return *runtimeSpec.Replicas
}

// containerPort is the port the application listens on inside its pod.
func containerPort(runtimeSpec kitchenv1alpha1.RuntimeSpec) int32 {
	if runtimeSpec.Port == 0 {
		return defaultContainerPort
	}
	return runtimeSpec.Port
}

func (r *EnvironmentReconciler) applyDeployment(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	project *kitchenv1alpha1.Project,
	appNS string,
	labels map[string]string,
	podEnv []corev1.EnvVar,
	// idles says KEDA owns the replica count on this Deployment, because the
	// environment is allowed to park at zero.
	idles bool,
) error {
	runtimeSpec := release.Spec.ConfigSnapshot.Runtime
	port := containerPort(runtimeSpec)

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: env.Name, Namespace: appNS}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = labels
		// While the environment idles the replica count is left exactly as it
		// stands: KEDA is what parks the pods and what brings them back, and
		// writing a number here every reconcile would undo whichever it had
		// just set — including the zero the whole feature exists for. A
		// Deployment created in that state starts at the API server's default
		// of one and is scaled down as soon as it goes quiet.
		if !idles {
			deploy.Spec.Replicas = ptr.To(desiredReplicas(env, runtimeSpec))
		}
		deploy.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{labelEnvironment: env.Name},
		}
		deploy.Spec.Template.Labels = labels
		// A registry that wanted a credential to push wants one to pull. The
		// build already put that docker config in this namespace, under a
		// name derived from the same Connection, so the Deployment only has
		// to name it — without which the pods sit in ImagePullBackOff while
		// every other part of the environment reads as healthy.
		deploy.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: registrySecretName(project.Spec.Registry.ConnectionRef.Name)},
		}
		app := corev1.Container{
			Name:      AppContainerName,
			Image:     release.Spec.Image,
			Ports:     []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
			Env:       podEnv,
			Resources: runtimeSpec.Resources,
		}
		// Every environment is probed, whether or not the project declared a
		// health check: absent one it is a TCP connect to the container port,
		// which is a weaker claim than an HTTP 200 and a far better one than
		// the readiness nothing used to establish. Previews inherit the
		// check with the rest of the snapshot.
		applyProbes(&app, runtimeSpec.Health, port)
		deploy.Spec.Template.Spec.Containers = []corev1.Container{app}
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
	port := containerPort(release.Spec.ConfigSnapshot.Runtime)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: env.Name, Namespace: appNS}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = map[string]string{labelEnvironment: env.Name}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       servicePort,
			TargetPort: intstr.FromInt32(port),
		}}
		return nil
	})
	return err
}

// applyHTTPRoute publishes the Environment on the shared Gateway. Two things
// can stand between the Gateway and the application, and they compose:
//
//   - A gate turns the route into a protected one: traffic goes to the gate
//     instead, carrying the application's address in a header the Gateway sets,
//     so one gate serves every preview on the platform without knowing about
//     any of them in advance.
//   - An idling environment is addressed through the interceptor, which
//     cold-starts it. It replaces the application wherever the route would
//     otherwise name it — as the Gateway's backend on an open environment, and
//     as the gate's upstream on a protected one. Both keep the visitor's Host
//     header, which is how the interceptor knows which application a request
//     that arrived at it belongs to.
func (r *EnvironmentReconciler) applyHTTPRoute(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	appNS string,
	labels map[string]string,
	host string,
	gate *previewGateBackend,
	idle *idleBackend,
	// section is the Gateway listener to attach to. With edge TLS on, port 80
	// carries only the redirect, so an application route that also bound there
	// would serve the app over cleartext.
	section *gatewayv1.SectionName,
	// domains adds the environment's verified custom hostnames and the extra
	// listeners they bind — each parentRef names its section explicitly, so
	// hostname intersection alone never puts a host on a listener it should
	// not serve from.
	domains domainRouting,
) error {
	// A route and a backend in different namespaces need the backend
	// namespace's permission, which is what a ReferenceGrant is. It is only
	// needed where the route itself names the other namespace: a gated route
	// reaches the interceptor through the gate, which is an ordinary
	// connection Gateway API has no say over.
	switch {
	case gate != nil:
		if err := r.allowBackendService(ctx, appNS, PlatformNamespace, gate.Service,
			referenceGrantName(appNS), PreviewGateName); err != nil {
			return err
		}
	case idle != nil:
		if err := r.allowBackendService(ctx, appNS, idle.Namespace, idle.Service,
			interceptorGrantName(appNS), interceptorComponentName); err != nil {
			return err
		}
	}

	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: env.Name, Namespace: appNS}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		route.Labels = labels
		parents := []gatewayv1.ParentReference{{
			Name:        SharedGatewayName,
			Namespace:   ptr.To(gatewayv1.Namespace(PlatformNamespace)),
			SectionName: section,
		}}
		for _, extra := range domains.sections {
			if section != nil && extra == *section {
				continue
			}
			parents = append(parents, gatewayv1.ParentReference{
				Name:        SharedGatewayName,
				Namespace:   ptr.To(gatewayv1.Namespace(PlatformNamespace)),
				SectionName: ptr.To(extra),
			})
		}
		route.Spec.CommonRouteSpec = gatewayv1.CommonRouteSpec{ParentRefs: parents}
		route.Spec.Hostnames = append([]gatewayv1.Hostname{gatewayv1.Hostname(host)}, domains.hostnames...)

		// Where the application is reached: itself, or the interceptor that
		// wakes it. Whoever forwards the request last uses this address.
		application := upstreamAddress(appNS, env.Name, servicePort)
		applicationRef := gatewayv1.BackendObjectReference{
			Name: gatewayv1.ObjectName(env.Name),
			Port: ptr.To(gatewayv1.PortNumber(servicePort)),
		}
		if idle != nil {
			application = idle.Address()
			applicationRef = gatewayv1.BackendObjectReference{
				Name:      gatewayv1.ObjectName(idle.Service),
				Namespace: ptr.To(gatewayv1.Namespace(idle.Namespace)),
				Port:      ptr.To(gatewayv1.PortNumber(idle.Port)),
			}
		}

		rule := gatewayv1.HTTPRouteRule{
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{BackendObjectReference: applicationRef},
			}},
		}
		if gate != nil {
			rule.Filters = []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
				RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
					// Set, not Add: whatever the client sent under these
					// names is overwritten before the gate ever sees them.
					//
					// The project is the other half of what the gate needs:
					// the upstream says where to forward, and this says whose
					// preview it is, which is what turns "signed in" into "on
					// this project". The gate does not take it on trust — it
					// checks the route it arrived on really is this project's
					// (see previewgate.routedFor).
					Set: []gatewayv1.HTTPHeader{{
						Name:  previewgate.UpstreamHeader,
						Value: application,
					}, {
						Name:  previewgate.ProjectHeader,
						Value: env.Spec.ProjectRef.Name,
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

// allowBackendService grants one application namespace permission to route to
// one Service in another. It lives in the Service's namespace, because that is
// the side being asked, and it names the single Service — a cross-namespace
// reference is exactly as wide as it has to be. One grant covers every
// Environment of the application namespace that routes there.
//
// It outlives the Environments that needed it, like the application namespace
// itself does: a project whose previews are all closed will want it again on
// the next pull request.
func (r *EnvironmentReconciler) allowBackendService(
	ctx context.Context,
	appNS, backendNS, service, name, component string,
) error {
	grant := &gatewayv1beta1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: backendNS,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, grant, func() error {
		grant.Labels = map[string]string{
			labelComponentKey: component,
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
			Name:  ptr.To(gatewayv1beta1.ObjectName(service)),
		}}
		return nil
	})
	return err
}

// referenceGrantName and interceptorGrantName are stable per application
// namespace, so each grant is created once and found again by every
// Environment in it.
func referenceGrantName(appNS string) string {
	return PreviewGateName + "-" + appNS
}

func interceptorGrantName(appNS string) string {
	return interceptorComponentName + "-" + appNS
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
	project *kitchenv1alpha1.Project,
	release *kitchenv1alpha1.Release,
	kitchen *kitchenv1alpha1.Kitchen,
	appNS string,
	host string,
	protected bool,
	// idleCond is the ScaleToZero condition to record, or nil on a platform
	// that idles nothing — where the condition would be the same line on every
	// Environment and say nothing.
	idleCond *metav1.Condition,
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
	// Live is the Deployment's availability, which is its ready replicas,
	// which is the readiness probe applyDeployment writes. That chain is why
	// the probes are worth having: before them this number said the process
	// had started, and the platform reported a health it had never checked.
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
	recordScaleToZero(env, idleCond)
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

	r.reportDeployStatus(ctx, env, project, release, protected)

	if err := r.Status().Update(ctx, env); err != nil {
		return ctrl.Result{}, err
	}
	if !available {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// reportDeployStatus posts the deployment — and a preview's URL, onto its
// pull request — back to the git provider, and records what it said in
// status.gitReport so the next reconcile stays quiet unless something moved.
// It is called with the status already in its final shape and before it is
// written, so the report and the status it describes land together.
func (r *EnvironmentReconciler) reportDeployStatus(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	project *kitchenv1alpha1.Project,
	release *kitchenv1alpha1.Release,
	protected bool,
) {
	revision, ok := r.revisionOf(ctx, env, release)
	if !ok {
		// Without the commit there is nothing to report against: the
		// Build the Release came from is gone, or was never named.
		return
	}

	state := deploymentStateFor(env.Status.Phase)
	// A report that repeats the last one is not posted. The comparison
	// carries the empty error, so a report that failed last time is retried
	// on the next pass rather than remembered as done.
	candidate := &kitchenv1alpha1.GitReport{
		Revision: revision.SHA,
		State:    string(state),
		URL:      env.Status.URL,
	}
	if candidate.Matches(env.Status.GitReport) {
		return
	}

	env.Status.GitReport = r.git().reportEnvironment(
		ctx, project, env, revision, state, protected, env.Status.GitReport)
}

// revisionOf is the commit an Environment is running: the Release names the
// Build, and the Build names the commit.
func (r *EnvironmentReconciler) revisionOf(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
) (kitchenv1alpha1.GitRevision, bool) {
	if release.Spec.BuildRef.Name == "" {
		return kitchenv1alpha1.GitRevision{}, false
	}
	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Namespace: env.Namespace, Name: release.Spec.BuildRef.Name}
	if err := r.Get(ctx, key, build); err != nil {
		return kitchenv1alpha1.GitRevision{}, false
	}
	if build.Spec.Git.SHA == "" {
		return kitchenv1alpha1.GitRevision{}, false
	}
	return build.Spec.Git, true
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
	// An environment with no route has nothing that could cold-start it, so it
	// is not idling and does not report on it.
	recordScaleToZero(env, nil)
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
	// Spelled in previewgate, because the preview gate checks a route's
	// project header against the namespace of the address it is told to
	// forward to. A second spelling here would be the two of them agreeing
	// only until one of them was renamed.
	return previewgate.AppNamespace(projectName)
}

// hostname computes the environment's generated host. Production gets the
// project slug; previews get <project>-pr-<n>.
func hostname(projectName string, env *kitchenv1alpha1.Environment, baseDomain string) string {
	if env.Spec.Type == kitchenv1alpha1.EnvironmentPreview && env.Spec.Preview != nil {
		return fmt.Sprintf("%s-pr-%d.%s", projectName, env.Spec.Preview.PullRequest, baseDomain)
	}
	return projectHost(projectName, baseDomain)
}

// projectHost is where a project's production environment is published, and
// it is derived rather than observed: it follows from the project's name and
// the platform's base domain, so it is knowable before the Environment
// exists. That is what lets an oidcClient claim register a working redirect
// URI for production on a project that has never been deployed — which it has
// to, since the deployment is waiting on the claim's binding.
func projectHost(projectName, baseDomain string) string {
	return fmt.Sprintf("%s.%s", projectName, baseDomain)
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

// mapDomainToEnvironment enqueues the Environment a Domain routes to, so its
// route gains and loses the hostname with the Domain rather than on the next
// unrelated reconcile.
func (r *EnvironmentReconciler) mapDomainToEnvironment(_ context.Context, obj client.Object) []ctrl.Request {
	domain, ok := obj.(*kitchenv1alpha1.Domain)
	if !ok || domain.Spec.EnvironmentRef.Name == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{
		Namespace: domain.Namespace, Name: domain.Spec.EnvironmentRef.Name,
	}}}
}

// mapPlatformToEnvironments enqueues every Environment when the platform
// configuration changes.
//
// Routing follows the Kitchen object — the base domain, whether there is a gate
// to protect previews with, whether environments may idle to zero — and a live
// Environment has nothing else that would reconcile it again. Without this, a
// platform switch would only reach the environments that happened to change
// after it was flipped.
func (r *EnvironmentReconciler) mapPlatformToEnvironments(ctx context.Context, _ client.Object) []ctrl.Request {
	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := r.List(ctx, environments); err != nil {
		logf.FromContext(ctx).Error(err, "could not list environments after a platform change")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(environments.Items))
	for _, env := range environments.Items {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: env.Namespace, Name: env.Name,
		}})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *EnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Environment{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapChildToEnvironment)).
		// A run finishing is the event this feature is about, and a Job is
		// where it lands. Both carry the environment label the mapper reads,
		// because the CronJob stamps it on the Jobs it creates.
		Watches(&batchv1.CronJob{}, handler.EnqueueRequestsFromMapFunc(r.mapChildToEnvironment)).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(r.mapChildToEnvironment)).
		Watches(&kitchenv1alpha1.Kitchen{}, handler.EnqueueRequestsFromMapFunc(r.mapPlatformToEnvironments)).
		Watches(&kitchenv1alpha1.Domain{}, handler.EnqueueRequestsFromMapFunc(r.mapDomainToEnvironment)).
		Named("environment").
		Complete(r)
}

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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

const (
	projectFinalizer = "kitchen.bermos.dev/project-cleanup"

	condSourceConnected   = "SourceConnected"
	condRegistryConnected = "RegistryConnected"
	condWebhookRegistered = "WebhookRegistered"
	condInitialBuild      = "InitialBuild"
	// condPreviews says whether this project gets preview environments, and
	// for one with no repository says why it cannot.
	condPreviews = "Previews"

	// reasonNoRepository is a repository's answer asked of a project that has
	// none, because its source is an image somebody else built (#307).
	reasonNoRepository = "NoRepository"

	// initialBuildAnnotation says a Build was the platform's own idea rather
	// than a push or a request, which is the difference between "nobody has
	// deployed this yet" and "this is what connecting the repository did".
	initialBuildAnnotation = "kitchen.bermos.dev/initial-build"

	// gitCredentialsTokenKey is the key in a git Connection's credentials
	// secret holding the API token.
	gitCredentialsTokenKey = "token"

	webhookSecretKey = "secret"
)

// ProjectReconciler reconciles a Project: it prepares the application
// namespace, validates the project's Connections, registers the git webhook,
// and keeps status references current.
type ProjectReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// GitProviders resolves a Provider for a Connection. Defaults to
	// gitprovider.Default; tests inject fakes.
	GitProviders gitprovider.Factory
	// Audit appends this reconciler's state transitions to the tamper-evident
	// log. Unlike Activity it is waited on: a transition it refuses is a
	// transition this reconciler does not make. May be nil.
	Audit *audit.Recorder
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=projects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=projects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=projects/finalizers,verbs=update
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=connections;kitchens,verbs=get;list;watch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=builds,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=releases;environments;domains;resourceclaims;promotions;exceptions,verbs=get;list;watch;delete
// The application namespace is relabelled as well as created: its Pod Security
// level is read off the platform singleton and reconciled onto namespaces that
// already exist, which needs update and patch.
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// The project's own secrets are mirrored into the application namespace and
// deleted with the project, so this reconciler writes and removes Secrets as
// well as reading them.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile prepares everything a Project needs before its first build.
func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	project := &kitchenv1alpha1.Project{}
	if err := r.Get(ctx, req.NamespacedName, project); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !project.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, project)
	}

	if controllerutil.AddFinalizer(project, projectFinalizer) {
		if err := r.Audit.Record(ctx, audit.Transition{
			Object:     project,
			Kind:       audit.KindProject,
			Operation:  clickhouse.AuditCreate,
			Controller: actorProjectController,
			Project:    project.Name,
			Reason:     fmt.Sprintf("project %s appeared", project.Name),
			Details: map[string]any{
				"source":           projectSourceDescription(project),
				"repo":             project.Spec.Source.GitSource().Repo,
				"productionBranch": project.Spec.Source.GitSource().ProductionBranch,
			},
		}); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Update(ctx, project); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := ensureNamespace(ctx, r.Client, appNamespace(project.Name), project.Name); err != nil {
		return ctrl.Result{}, err
	}

	// The project's own secrets, mirrored from the copy the API writes into
	// the platform namespace. It happens on every reconcile rather than at
	// namespace creation, so a namespace that already exists — which is every
	// namespace after the first reconcile — gets them too.
	if err := r.mirrorProjectSecrets(ctx, project); err != nil {
		return ctrl.Result{}, err
	}

	setCond := func(condType string, status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&project.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: project.Generation,
		})
	}

	// A project whose source is an image has no repository to read, nothing
	// to push, and no webhook to register — so it is asked none of the three
	// (#307). Its own connection question is the pull credential, and that is
	// asked of the image the workloads name rather than of the project.
	var (
		sourceConn        *kitchenv1alpha1.Connection
		sourceOK          = true
		registryOK        = true
		retryInitialBuild bool
		err               error
	)
	r.setPreviewsCondition(project, setCond)
	if project.Spec.Source.HasRepository() {
		sourceConn, err = r.checkConnection(ctx, project, project.Spec.Source.GitSource().ConnectionRef.Name,
			kitchenv1alpha1.CapabilityGitSource, condSourceConnected, setCond)
		sourceOK = err == nil
		_, err = r.checkConnection(ctx, project, project.Spec.RegistryConnection(),
			kitchenv1alpha1.CapabilityImageStore, condRegistryConnected, setCond)
		registryOK = err == nil

		if sourceOK {
			r.ensureWebhook(ctx, project, sourceConn, setCond)
		}
	} else {
		meta.RemoveStatusCondition(&project.Status.Conditions, condSourceConnected)
		meta.RemoveStatusCondition(&project.Status.Conditions, condRegistryConnected)
		meta.RemoveStatusCondition(&project.Status.Conditions, condWebhookRegistered)
	}

	// The first build waits for both connections: a build with nowhere to
	// push its image is a failed build, and the project is about to be
	// requeued anyway. A project that builds nothing waits for neither.
	if sourceOK && registryOK {
		retryInitialBuild = r.ensureInitialBuild(ctx, project, sourceConn, setCond)
	}

	r.updateReferences(ctx, project)

	// Ready is what everything summarising a project reads, so it answers for
	// the repository as well as for the connections: a project whose source
	// the platform cannot see will never build, and reporting that as ready
	// left "I fixed the token, why is my project still broken" with nothing to
	// look at but a sub-condition.
	initialBuild := meta.FindStatusCondition(project.Status.Conditions, condInitialBuild)
	switch {
	case !sourceOK || !registryOK:
		setCond(condReady, metav1.ConditionFalse, "ConnectionsNotReady", "one or more connections are not ready")
	case initialBuild != nil && initialBuild.Status == metav1.ConditionFalse &&
		initialBuild.Reason == reasonRepositoryUnreadable:
		setCond(condReady, metav1.ConditionFalse, reasonRepositoryUnreadable, initialBuild.Message)
	default:
		setCond(condReady, metav1.ConditionTrue, "Reconciled", "project is ready")
	}

	if err := r.Status().Update(ctx, project); err != nil {
		return ctrl.Result{}, err
	}
	if !sourceOK || !registryOK || retryInitialBuild {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	log.Info("reconciled project", "project", project.Name)
	return ctrl.Result{}, nil
}

// finalize deregisters the git webhook (best effort), garbage-collects the
// records the platform derived from the project, and deletes the application
// namespace with everything in it.
func (r *ProjectReconciler) finalize(ctx context.Context, project *kitchenv1alpha1.Project) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(project, projectFinalizer) {
		return ctrl.Result{}, nil
	}

	if project.Spec.Source.HasRepository() && project.Status.WebhookID != "" {
		if provider, err := r.resolveProvider(ctx, project, nil); err == nil {
			if err := provider.DeleteWebhook(ctx, project.Spec.Source.GitSource().Repo, project.Status.WebhookID); err != nil {
				log.Error(err, "failed to delete webhook, continuing", "webhookID", project.Status.WebhookID)
			}
		}
	}

	remaining, err := r.deleteDependents(ctx, project)
	if err != nil {
		return ctrl.Result{}, err
	}
	if remaining > 0 {
		// Environments carry their own cleanup finalizer, so their deletion is
		// asynchronous; the namespace waits for them so their finalizers still
		// find the children they have to remove.
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: appNamespace(project.Name)}}
	if err := r.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// The application namespace took the mirrored copy of the project's
	// secrets with it; the source the API wrote is in the platform namespace,
	// and goes here.
	if err := r.deleteProjectSecrets(ctx, project); err != nil {
		return ctrl.Result{}, err
	}

	// Recorded here rather than where the deletion was requested, because
	// this is the point at which everything the project owned is actually
	// gone — which is the fact worth having a record of.
	if err := r.Audit.Record(ctx, audit.Transition{
		Object:     project,
		Kind:       audit.KindProject,
		Operation:  clickhouse.AuditDelete,
		Controller: actorProjectController,
		Project:    project.Name,
		Reason: fmt.Sprintf(
			"project %s deleted: its environments, builds, releases, domains, claims and namespace went with it",
			project.Name),
		Details: map[string]any{"source": projectSourceDescription(project)},
	}); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(project, projectFinalizer)
	return ctrl.Result{}, r.Update(ctx, project)
}

// deleteDependents removes everything in the platform namespace that
// references the project — builds, releases, environments, promotions, and
// the domains and resource claims hanging off them. They reference the
// project by name rather than by owner, so nothing garbage-collects them when
// the project goes. It returns how many are still around, which is nonzero
// while environment finalizers run.
func (r *ProjectReconciler) deleteDependents(ctx context.Context, project *kitchenv1alpha1.Project) (int, error) {
	inNamespace := client.InNamespace(project.Namespace)

	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := r.List(ctx, environments, inNamespace); err != nil {
		return 0, err
	}
	environmentNames := map[string]bool{}
	for i := range environments.Items {
		if environments.Items[i].Spec.ProjectRef.Name == project.Name {
			environmentNames[environments.Items[i].Name] = true
		}
	}

	doomed := []client.Object{}

	// Domains go first, while the environments they point at still exist to
	// say which project they belonged to.
	domains := &kitchenv1alpha1.DomainList{}
	if err := r.List(ctx, domains, inNamespace); err != nil {
		return 0, err
	}
	for i := range domains.Items {
		if environmentNames[domains.Items[i].Spec.EnvironmentRef.Name] {
			doomed = append(doomed, &domains.Items[i])
		}
	}
	for i := range environments.Items {
		if environmentNames[environments.Items[i].Name] {
			doomed = append(doomed, &environments.Items[i])
		}
	}

	builds := &kitchenv1alpha1.BuildList{}
	if err := r.List(ctx, builds, inNamespace); err != nil {
		return 0, err
	}
	for i := range builds.Items {
		if builds.Items[i].Spec.ProjectRef.Name == project.Name {
			doomed = append(doomed, &builds.Items[i])
		}
	}

	releases := &kitchenv1alpha1.ReleaseList{}
	if err := r.List(ctx, releases, inNamespace); err != nil {
		return 0, err
	}
	for i := range releases.Items {
		if releases.Items[i].Spec.ProjectRef.Name == project.Name {
			doomed = append(doomed, &releases.Items[i])
		}
	}

	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := r.List(ctx, claims, inNamespace); err != nil {
		return 0, err
	}
	for i := range claims.Items {
		if claims.Items[i].Spec.ProjectRef.Name == project.Name {
			doomed = append(doomed, &claims.Items[i])
		}
	}

	promotions := &kitchenv1alpha1.PromotionList{}
	if err := r.List(ctx, promotions, inNamespace); err != nil {
		return 0, err
	}
	for i := range promotions.Items {
		if promotions.Items[i].Spec.ProjectRef.Name == project.Name {
			doomed = append(doomed, &promotions.Items[i])
		}
	}

	// Exceptions are retained through their whole lifecycle — the register is
	// queryable historically — so project deletion is the one thing that
	// garbage-collects them.
	exceptions := &kitchenv1alpha1.ExceptionList{}
	if err := r.List(ctx, exceptions, inNamespace); err != nil {
		return 0, err
	}
	for i := range exceptions.Items {
		if exceptions.Items[i].Spec.ProjectRef.Name == project.Name {
			doomed = append(doomed, &exceptions.Items[i])
		}
	}

	for _, obj := range doomed {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	return len(doomed), nil
}

// checkConnection loads a Connection and records a condition. When the
// Connection reports capabilities, the required one must be present.
func (r *ProjectReconciler) checkConnection(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	name string,
	capability kitchenv1alpha1.Capability,
	condType string,
	setCond func(string, metav1.ConditionStatus, string, string),
) (*kitchenv1alpha1.Connection, error) {
	conn := &kitchenv1alpha1.Connection{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: project.Namespace, Name: name}, conn); err != nil {
		setCond(condType, metav1.ConditionFalse, "ConnectionMissing", err.Error())
		return nil, err
	}
	// A Connection whose reconciler has not looked at it yet claims nothing,
	// which is not the same as claiming it cannot: the project waits for the
	// verdict rather than being refused on an empty one.
	if len(conn.Status.Capabilities) > 0 && !connectionProvides(conn, capability) {
		err := fmt.Errorf("connection %q does not provide the %s capability", name, capability)
		setCond(condType, metav1.ConditionFalse, "CapabilityMissing", err.Error())
		return nil, err
	}
	setCond(condType, metav1.ConditionTrue, "Connected", "connection is available")
	return conn, nil
}

// ensureWebhook registers (or refreshes) the git webhook for the project's
// repository. Failures are recorded as conditions, never as reconcile errors:
// a broken webhook shouldn't stop the rest of the project from working.
func (r *ProjectReconciler) ensureWebhook(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	conn *kitchenv1alpha1.Connection,
	setCond func(string, metav1.ConditionStatus, string, string),
) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		setCond(condWebhookRegistered, metav1.ConditionFalse, "PlatformConfigMissing", err.Error())
		return
	}

	provider, err := r.resolveProvider(ctx, project, conn)
	if err != nil {
		reason := "ProviderError"
		if errors.Is(err, gitprovider.ErrUnsupportedProvider) {
			reason = "ProviderUnsupported"
		} else if apierrors.IsNotFound(err) {
			reason = "CredentialsMissing"
		}
		setCond(condWebhookRegistered, metav1.ConditionFalse, reason, err.Error())
		return
	}

	secret, err := r.ensureWebhookSecret(ctx, project)
	if err != nil {
		setCond(condWebhookRegistered, metav1.ConditionFalse, "SecretError", err.Error())
		return
	}

	hookURL := fmt.Sprintf("%s/webhooks/git/%s", apiExternalURL(kitchen), conn.Name)
	id, err := provider.EnsureWebhook(ctx, project.Spec.Source.GitSource().Repo, gitprovider.WebhookSpec{
		URL:    hookURL,
		Secret: secret,
		Events: []string{"push", "pull_request"},
	})
	if err != nil {
		setCond(condWebhookRegistered, metav1.ConditionFalse, "WebhookFailed", err.Error())
		return
	}
	project.Status.WebhookID = id
	setCond(condWebhookRegistered, metav1.ConditionTrue, "Registered", fmt.Sprintf("webhook %s registered at %s", id, hookURL))
}

// resolveProvider builds the git provider for the project's source
// Connection, loading the Connection itself if not already provided.
func (r *ProjectReconciler) resolveProvider(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	conn *kitchenv1alpha1.Connection,
) (gitprovider.Provider, error) {
	if conn == nil {
		conn = &kitchenv1alpha1.Connection{}
		key := types.NamespacedName{Namespace: project.Namespace, Name: project.Spec.Source.GitSource().ConnectionRef.Name}
		if err := r.Get(ctx, key, conn); err != nil {
			return nil, err
		}
	}

	creds := &corev1.Secret{}
	key := types.NamespacedName{Namespace: conn.Namespace, Name: conn.Spec.CredentialsSecretRef.Name}
	if err := r.Get(ctx, key, creds); err != nil {
		return nil, err
	}
	token := string(creds.Data[gitCredentialsTokenKey])
	if token == "" {
		return nil, fmt.Errorf("credentials secret %q has no %q key", conn.Spec.CredentialsSecretRef.Name, gitCredentialsTokenKey)
	}

	factory := r.GitProviders
	if factory == nil {
		factory = gitprovider.Default
	}
	return factory(conn, token)
}

// ensureWebhookSecret creates (or reads) the per-project webhook signing
// secret and returns its value.
func (r *ProjectReconciler) ensureWebhookSecret(ctx context.Context, project *kitchenv1alpha1.Project) (string, error) {
	name := "kitchen-webhook-" + project.Name
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: project.Namespace, Name: name}, secret)
	if err == nil {
		return string(secret.Data[webhookSecretKey]), nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	value := hex.EncodeToString(raw)
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: project.Namespace,
			Labels:    map[string]string{labelProject: project.Name, labelManagedByKey: labelManagedByValue},
		},
		StringData: map[string]string{webhookSecretKey: value},
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", err
	}
	return value, nil
}

// updateReferences refreshes the convenience refs in status: the production
// Environment and the newest Build.
//
// Both are derived rather than accumulated, so a ref is cleared when the
// object behind it is gone — a Build the retention sweep pruned or an
// environment somebody deleted would otherwise leave the project pointing at
// a name nothing resolves. A read that failed for any reason other than "it
// is not there" leaves the ref alone: an unreadable object is not an absent
// one.
func (r *ProjectReconciler) updateReferences(ctx context.Context, project *kitchenv1alpha1.Project) {
	env := &kitchenv1alpha1.Environment{}
	envKey := types.NamespacedName{Namespace: project.Namespace, Name: project.Name + "-production"}
	switch err := r.Get(ctx, envKey, env); {
	case err == nil:
		project.Status.ProductionEnvironmentRef = &kitchenv1alpha1.LocalObjectReference{Name: env.Name}
	case apierrors.IsNotFound(err):
		project.Status.ProductionEnvironmentRef = nil
	}

	builds := &kitchenv1alpha1.BuildList{}
	if err := r.List(ctx, builds, client.InNamespace(project.Namespace)); err != nil {
		return
	}
	var latest *kitchenv1alpha1.Build
	for i := range builds.Items {
		b := &builds.Items[i]
		if b.Spec.ProjectRef.Name != project.Name {
			continue
		}
		if latest == nil || b.CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = b
		}
	}
	if latest == nil {
		project.Status.LatestBuildRef = nil
		return
	}
	project.Status.LatestBuildRef = &kitchenv1alpha1.LocalObjectReference{Name: latest.Name}
}

// apiExternalURL returns the operator API's public base URL. Its scheme
// follows the platform's TLS mode: a webhook URL handed to a git provider has
// to name a scheme the Gateway serves.
func apiExternalURL(kitchen *kitchenv1alpha1.Kitchen) string {
	if kitchen.Spec.API.ExternalURL != "" {
		return kitchen.Spec.API.ExternalURL
	}
	return platformScheme(kitchen) + "://kitchen." + kitchen.Spec.BaseDomain
}

// mapBuildToProject enqueues the project a Build belongs to, so that
// status.latestBuildRef follows the builds a push produced rather than only
// the ones a request to the API happened to make alongside a write to the
// Project.
func (r *ProjectReconciler) mapBuildToProject(_ context.Context, obj client.Object) []ctrl.Request {
	build, ok := obj.(*kitchenv1alpha1.Build)
	if !ok || build.Spec.ProjectRef.Name == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{
		Namespace: build.Namespace, Name: build.Spec.ProjectRef.Name,
	}}}
}

// mapEnvironmentToProject enqueues the project an Environment belongs to, so
// that status.productionEnvironmentRef appears with the environment rather
// than at the next time something wrote to the Project.
func (r *ProjectReconciler) mapEnvironmentToProject(_ context.Context, obj client.Object) []ctrl.Request {
	env, ok := obj.(*kitchenv1alpha1.Environment)
	if !ok || env.Spec.ProjectRef.Name == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{
		Namespace: env.Namespace, Name: env.Spec.ProjectRef.Name,
	}}}
}

// mapConnectionToProjects enqueues every project bound to a Connection, so a
// connection that has just become usable — a capability appearing, a
// credential accepted, a rotated secret revalidated — moves the projects
// waiting on it rather than leaving them until the next write to the Project.
// Both refs count: a project waits on its registry as much as on its source.
func (r *ProjectReconciler) mapConnectionToProjects(ctx context.Context, obj client.Object) []ctrl.Request {
	conn, ok := obj.(*kitchenv1alpha1.Connection)
	if !ok {
		return nil
	}
	projects := &kitchenv1alpha1.ProjectList{}
	if err := r.List(ctx, projects, client.InNamespace(conn.Namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "could not list projects after a connection change")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(projects.Items))
	for i := range projects.Items {
		project := &projects.Items[i]
		if project.Spec.Source.GitSource().ConnectionRef.Name != conn.Name &&
			project.Spec.RegistryConnection() != conn.Name {
			continue
		}
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: project.Namespace, Name: project.Name,
		}})
	}
	return requests
}

// existenceChanged passes creations and deletions and drops updates.
//
// Both refs the two watches below feed are questions about which objects
// exist — the newest Build by creation time, and whether the production
// Environment is there — so no update can change either answer. The
// distinction matters because a project reconcile re-registers the git
// webhook with the provider: an Environment's status moves with every replica
// KEDA parks or wakes and a Build's with every phase it passes through, and
// reconciling the project on each of those would spend the installation's
// GitHub rate limit on a status that could not have changed.
func existenceChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(event.UpdateEvent) bool { return false },
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Project{}).
		// The project's own secrets: the API writes them into the platform
		// namespace owner-referenced by the Project, and this is what carries
		// a new or rotated value into the application namespace on the spot
		// rather than on the next unrelated write to the Project.
		Owns(&corev1.Secret{}).
		// The Project's status is derived from Builds and Environments, and
		// nothing owner-references them, so without these watches it is
		// refreshed only when the Project object itself changes. A project
		// whose deploys all arrive by webhook never writes to its Project,
		// which is how one came to report a permanently failed latest build
		// and no production environment while it was serving traffic.
		Watches(&kitchenv1alpha1.Build{}, handler.EnqueueRequestsFromMapFunc(r.mapBuildToProject),
			builder.WithPredicates(existenceChanged())).
		Watches(&kitchenv1alpha1.Environment{}, handler.EnqueueRequestsFromMapFunc(r.mapEnvironmentToProject),
			builder.WithPredicates(existenceChanged())).
		// The connections are the other half of what a project is waiting on,
		// and the two watches above cannot help there: they map Builds and
		// Environments back to their project, and a project that has never
		// built has neither. No predicate, unlike those two — a Connection's
		// status moves only when the verdict itself moved, which is exactly
		// what the project reads off it.
		Watches(&kitchenv1alpha1.Connection{}, handler.EnqueueRequestsFromMapFunc(r.mapConnectionToProjects)).
		Named("project").
		Complete(r)
}

// setPreviewsCondition says, in words, whether this project gets preview
// environments — and for a project with no repository, why it does not.
//
// It is a condition rather than a refusal at admission because
// `previews.enabled` defaults to true: a vendored project that never mentioned
// previews would otherwise be refused outright. And it is written rather than
// left implicit because a preview that silently never appears reads as a
// fault — which is the whole of what #307 asked for here.
func (r *ProjectReconciler) setPreviewsCondition(
	project *kitchenv1alpha1.Project,
	setCond func(string, metav1.ConditionStatus, string, string),
) {
	switch {
	case !project.Spec.Source.HasRepository():
		setCond(condPreviews, metav1.ConditionFalse, reasonNoRepository, fmt.Sprintf(
			"previews are environments for pull requests, and this project has no repository to open one "+
				"against: its source is the image %s. Nothing about it changes until that image does.",
			project.Spec.Source.ImageSource().Reference()))
	case !project.Spec.Previews.IsEnabled():
		setCond(condPreviews, metav1.ConditionFalse, "Disabled",
			"previews are turned off for this project: a pull request against it gets no environment of its own")
	default:
		setCond(condPreviews, metav1.ConditionTrue, "Enabled",
			"a pull request against this project gets a preview environment of its own")
	}
}

// projectSourceDescription names where a project's software comes from, for
// the records that used to name a repository and now have two answers to
// choose between.
func projectSourceDescription(project *kitchenv1alpha1.Project) string {
	if project.Spec.Source.HasRepository() {
		return project.Spec.Source.GitSource().Repo
	}
	return project.Spec.Source.ImageSource().Reference()
}

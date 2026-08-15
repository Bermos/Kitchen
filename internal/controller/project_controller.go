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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

const (
	projectFinalizer = "kitchen.bermos.dev/project-cleanup"

	condSourceConnected   = "SourceConnected"
	condRegistryConnected = "RegistryConnected"
	condWebhookRegistered = "WebhookRegistered"

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
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=projects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=projects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=projects/finalizers,verbs=update
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=connections;kitchens,verbs=get;list;watch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=builds;releases;environments;domains;resourceclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

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
		if err := r.Update(ctx, project); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := ensureNamespace(ctx, r.Client, appNamespace(project.Name), project.Name); err != nil {
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

	sourceConn, err := r.checkConnection(ctx, project, project.Spec.Source.ConnectionRef.Name,
		kitchenv1alpha1.CapabilityGitSource, condSourceConnected, setCond)
	sourceOK := err == nil
	_, err = r.checkConnection(ctx, project, project.Spec.Registry.ConnectionRef.Name,
		kitchenv1alpha1.CapabilityImageStore, condRegistryConnected, setCond)
	registryOK := err == nil

	if sourceOK {
		r.ensureWebhook(ctx, project, sourceConn, setCond)
	}

	r.updateReferences(ctx, project)

	if sourceOK && registryOK {
		setCond(condReady, metav1.ConditionTrue, "Reconciled", "project is ready")
	} else {
		setCond(condReady, metav1.ConditionFalse, "ConnectionsNotReady", "one or more connections are not ready")
	}

	if err := r.Status().Update(ctx, project); err != nil {
		return ctrl.Result{}, err
	}
	if !sourceOK || !registryOK {
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

	if project.Status.WebhookID != "" {
		if provider, err := r.resolveProvider(ctx, project, nil); err == nil {
			if err := provider.DeleteWebhook(ctx, project.Spec.Source.Repo, project.Status.WebhookID); err != nil {
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

	controllerutil.RemoveFinalizer(project, projectFinalizer)
	return ctrl.Result{}, r.Update(ctx, project)
}

// deleteDependents removes everything in the platform namespace that
// references the project — builds, releases, environments, and the domains and
// resource claims hanging off them. They reference the project by name rather
// than by owner, so nothing garbage-collects them when the project goes. It
// returns how many are still around, which is nonzero while environment
// finalizers run.
func (r *ProjectReconciler) deleteDependents(ctx context.Context, project *kitchenv1alpha1.Project) (int, error) {
	remaining := 0
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

	// Domains go first, while the environments they point at still exist to
	// say which project they belonged to.
	domains := &kitchenv1alpha1.DomainList{}
	if err := r.List(ctx, domains, inNamespace); err != nil {
		return 0, err
	}
	for i := range domains.Items {
		if !environmentNames[domains.Items[i].Spec.EnvironmentRef.Name] {
			continue
		}
		remaining++
		if err := r.Delete(ctx, &domains.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}

	for i := range environments.Items {
		if !environmentNames[environments.Items[i].Name] {
			continue
		}
		remaining++
		if err := r.Delete(ctx, &environments.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}

	builds := &kitchenv1alpha1.BuildList{}
	if err := r.List(ctx, builds, inNamespace); err != nil {
		return 0, err
	}
	for i := range builds.Items {
		if builds.Items[i].Spec.ProjectRef.Name != project.Name {
			continue
		}
		remaining++
		if err := r.Delete(ctx, &builds.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}

	releases := &kitchenv1alpha1.ReleaseList{}
	if err := r.List(ctx, releases, inNamespace); err != nil {
		return 0, err
	}
	for i := range releases.Items {
		if releases.Items[i].Spec.ProjectRef.Name != project.Name {
			continue
		}
		remaining++
		if err := r.Delete(ctx, &releases.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}

	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := r.List(ctx, claims, inNamespace); err != nil {
		return 0, err
	}
	for i := range claims.Items {
		if claims.Items[i].Spec.ProjectRef.Name != project.Name {
			continue
		}
		remaining++
		if err := r.Delete(ctx, &claims.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}

	return remaining, nil
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
	if len(conn.Status.Capabilities) > 0 {
		found := false
		for _, c := range conn.Status.Capabilities {
			if c == capability {
				found = true
			}
		}
		if !found {
			err := fmt.Errorf("connection %q does not provide the %s capability", name, capability)
			setCond(condType, metav1.ConditionFalse, "CapabilityMissing", err.Error())
			return nil, err
		}
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
	id, err := provider.EnsureWebhook(ctx, project.Spec.Source.Repo, gitprovider.WebhookSpec{
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
		key := types.NamespacedName{Namespace: project.Namespace, Name: project.Spec.Source.ConnectionRef.Name}
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
func (r *ProjectReconciler) updateReferences(ctx context.Context, project *kitchenv1alpha1.Project) {
	env := &kitchenv1alpha1.Environment{}
	envKey := types.NamespacedName{Namespace: project.Namespace, Name: project.Name + "-production"}
	if err := r.Get(ctx, envKey, env); err == nil {
		project.Status.ProductionEnvironmentRef = &kitchenv1alpha1.LocalObjectReference{Name: env.Name}
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
	if latest != nil {
		project.Status.LatestBuildRef = &kitchenv1alpha1.LocalObjectReference{Name: latest.Name}
	}
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

// SetupWithManager sets up the controller with the Manager.
func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Project{}).
		Named("project").
		Complete(r)
}

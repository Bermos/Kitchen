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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/cache"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// The redis half of the ResourceClaim reconciler: a cache or a queue from a
// cache-capable Connection, provisioned through internal/provider/cache —
// one Valkey per claim in this cluster, or a keyspace at a server somebody
// else runs — with its binding written into a Secret in the application
// namespace and, since both providers declare previews get a fresh instance,
// one instance and one Secret per preview Environment.
//
// It is the fifth contract and the shape has not moved since the first: a
// Connection matched on a capability, a provisioner built from it, typed
// Requirements read off the claim's own slice of spec.config, three answers
// from the provider — bound, not ready yet, or refused with a message.
//
// The refusal is the one worth reading. `usage` decides whether the instance
// evicts under memory pressure or refuses the write, and a queue served by
// an evicting instance loses jobs and reports nothing. A provider that
// cannot honour what the claim asked answers ErrUnsatisfiable, and the claim
// fails with the provider's own words rather than binding something that
// will quietly drop work.

// The in-cluster cache provider: one Valkey per claim is a StatefulSet, a
// Service and a Secret the platform writes with its own account, plus the
// volume a queue keeps its jobs on — which a StatefulSet does not collect
// with itself, so the provisioner deletes it too.
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete

// The external cache provider provisions nothing, but it does write: which
// logical database of the server each claim and each preview holds is
// recorded on the Connection's own status, because that is the only place
// every claim through one server can see what the others were given.
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=connections/status,verbs=get;update;patch

// redisContract is the claimContract for type redis.
type redisContract struct{}

func (redisContract) reconcile(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
	project *kitchenv1alpha1.Project,
	conn *kitchenv1alpha1.Connection,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	provisioner, err := r.cacheProvisionerFor(ctx, conn)
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return r.pending(ctx, claim, "CredentialsMissing", err)
		case errors.Is(err, cache.ErrUnsupportedProvider):
			return r.failed(ctx, claim, "ProviderUnsupported", err)
		default:
			return r.failed(ctx, claim, "ProviderError", err)
		}
	}

	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}

	if result, done, err := r.provisionCache(ctx, claim, provisioner, appNS); done {
		return result, err
	}

	claimType, _ := claim.Type()
	mode := declare(claim, claimType, conn.Spec.Provider)
	branchErr := r.reconcileBranches(ctx, claim, project.Name, cacheBrancher{provisioner}, appNS,
		conn.Spec.Provider, mode.Isolated())

	reason := fmt.Sprintf("claim %s bound: %s via %s", claim.Name, claim.Spec.Type, conn.Name)
	if err := r.bind(ctx, claim, conn.Spec.Provider, reason, map[string]any{
		"type":           claim.Spec.Type,
		"connection":     conn.Name,
		"secret":         claim.Status.SecretName,
		"dataProvenance": claim.Status.DataProvenance,
		"residency":      claim.Status.Residency,
		"previewMode":    claim.Status.PreviewMode,
		"usage":          claim.Redis().Usage,
	}); err != nil {
		return ctrl.Result{}, err
	}
	if branchErr != nil {
		return ctrl.Result{RequeueAfter: claimRequeueDelay}, nil
	}
	log.Info("reconciled resource claim", "claim", claim.Name, "secret", claim.Status.SecretName)
	return ctrl.Result{}, nil
}

// finalize tears the preview instances down under either policy, releases
// the Environments they held, and destroys the claim's own instance under
// deletionPolicy Delete alone.
//
// Retain is the default here for the same reason it is everywhere else, and
// it is not academic for a queue: what is in one is work nobody has done
// yet, and deleting the claim in front of it must not be able to throw that
// away. A claim of the same name created later against the same connection
// finds the instance by name and rebinds to it.
func (redisContract) finalize(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
) error {
	log := logf.FromContext(ctx)
	appNS := appNamespace(claim.Spec.ProjectRef.Name)

	provisioner, err := r.cacheProvisionerForClaim(ctx, claim)
	if err != nil {
		log.Info("finalizing claim without its provider", "claim", claim.Name, "reason", err.Error())
	}
	var brancher claimBrancher
	if provisioner != nil {
		brancher = cacheBrancher{provisioner}
	}

	for _, branch := range claim.Status.Branches {
		if err := r.deleteBranch(ctx, claim, brancher, appNS, branch); err != nil {
			return err
		}
	}
	if err := r.releaseEnvironments(ctx, claim); err != nil {
		return err
	}

	if claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete &&
		claim.Status.InstanceID != "" && provisioner != nil {
		if err := provisioner.Deprovision(ctx, claim.Status.InstanceID); err != nil {
			return err
		}
	}
	return nil
}

// provisionCache creates the instance and the shared binding Secret when
// either is missing. done=true means the caller returns result and err as
// they are — provisioning failed, or has not finished, and the status
// already says which.
func (r *ResourceClaimReconciler) provisionCache(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner cache.Provisioner,
	appNS string,
) (ctrl.Result, bool, error) {
	secretName := claimSecretName(claim.Name)
	if claim.Status.InstanceID != "" {
		err := r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: secretName}, &corev1.Secret{})
		if err == nil {
			return ctrl.Result{}, false, nil
		}
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, true, err
		}
		// The instance exists and its Secret went missing: fall through and
		// provision again, which finds it by name and recovers the binding.
	}

	instance, err := provisionCacheInstance(ctx, claim, provisioner)
	switch {
	case errors.Is(err, cache.ErrNotReady):
		result, err := r.pending(ctx, claim, "Provisioning", err)
		return result, true, err
	case errors.Is(err, naming.ErrNotAdoptable):
		result, err := r.failed(ctx, claim, "InstanceNotAdoptable", err)
		return result, true, err
	case errors.Is(err, cache.ErrUnsatisfiable):
		result, err := r.failed(ctx, claim, "RequirementsUnsatisfiable", err)
		return result, true, err
	case err != nil:
		result, err := r.failed(ctx, claim, "ProvisionFailed", err)
		return result, true, err
	}
	if err := r.writeBindingSecret(ctx, claim, appNS, secretName, instance.Binding.Data()); err != nil {
		return ctrl.Result{}, true, err
	}
	claim.Status.InstanceID = instance.ID
	claim.Status.InstanceName = instance.Name
	claim.Status.SecretName = secretName
	claim.Status.DataProvenance = string(instance.Provenance)
	claim.Status.Residency = instance.Region
	setClaimCondition(claim, condProvisioned, metav1.ConditionTrue, "Provisioned",
		fmt.Sprintf("%s provisioned as %s", claim.Spec.Type, instance.ID))
	return ctrl.Result{}, false, nil
}

// provisionCacheInstance asks the provisioner for the claim's instance, with
// the claim's requirements where the provisioner can take them.
//
// A claim that asks for a usage, a memory limit or a version through a
// provisioner that cannot be asked any of them is refused rather than
// provisioned as though it had not asked. That is not a formality here: the
// difference between the two usages is whether work is dropped when memory
// runs out.
func provisionCacheInstance(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner cache.Provisioner,
) (cache.Instance, error) {
	resource := claimResource(claim)
	requirements := cacheRequirements(claim)
	if requirements.Empty() {
		return provisioner.Provision(ctx, resource)
	}
	capable, ok := provisioner.(cache.CapableProvisioner)
	if !ok {
		return cache.Instance{}, fmt.Errorf(
			"%w: this claim asks for a usage, a memory limit or a version, and connection %q cannot be asked "+
				"for any of them — claim through a %s connection, which provisions an instance of its own, "+
				"or drop config.redis from the claim",
			cache.ErrUnsatisfiable, claim.Connection(), cache.ProviderValkey)
	}
	return capable.ProvisionWith(ctx, resource, requirements)
}

// cacheRequirements reads the claim's spec.config into what the provisioner
// takes. The two vocabularies are kept apart on purpose: the CRD's is what
// somebody writes, the provider package's is what a provisioner answers.
func cacheRequirements(claim *kitchenv1alpha1.ResourceClaim) cache.Requirements {
	cfg := claim.Redis()
	return cache.Requirements{
		Usage:     cache.Usage(cfg.Usage),
		MaxMemory: cfg.MaxMemory,
		Version:   cfg.Version,
	}
}

// cacheBrancher is a cache provisioner as the branch machinery
// (resourceclaim_postgres.go) sees it: a preview's own instance, created and
// torn down by name, whose binding becomes the data of the preview's Secret.
type cacheBrancher struct{ provisioner cache.Provisioner }

func (b cacheBrancher) createBranch(ctx context.Context, instanceID, name string) (claimBranchResult, error) {
	branch, err := b.provisioner.CreateBranch(ctx, instanceID, name)
	if err != nil {
		return claimBranchResult{}, err
	}
	return claimBranchResult{
		ID:         branch.ID,
		Provenance: string(branch.Provenance),
		Data:       branch.Binding.Data(),
	}, nil
}

func (b cacheBrancher) deleteBranch(ctx context.Context, instanceID, branchID string) error {
	return b.provisioner.DeleteBranch(ctx, instanceID, branchID)
}

// cacheProvisionerFor builds the cache provisioner for a Connection.
//
// A Connection that names no credentials secret is not an error here: the
// in-cluster provider has none, because it provisions with the operator's
// own account. The CRD refuses an empty credentialsSecretRef for every other
// provider, which is what keeps that the exception rather than a hole.
func (r *ResourceClaimReconciler) cacheProvisionerFor(
	ctx context.Context,
	conn *kitchenv1alpha1.Connection,
) (cache.Provisioner, error) {
	serverURL := ""
	if secretName := conn.Spec.CredentialsSecretRef.Name; secretName != "" {
		creds := &corev1.Secret{}
		key := types.NamespacedName{Namespace: conn.Namespace, Name: secretName}
		if err := r.Get(ctx, key, creds); err != nil {
			return nil, err
		}
		serverURL = string(creds.Data[cache.CredentialKeyURL])
		if serverURL == "" {
			return nil, fmt.Errorf("credentials secret %q has no %q key", secretName, cache.CredentialKeyURL)
		}
	}

	factory := r.Caches
	if factory == nil {
		factory = cache.Default
	}
	// Where the in-cluster provisioner puts its instances is the
	// Connection's own `namespace` config, and kitchen-caches otherwise.
	// It is deliberately not a project's application namespace: that
	// namespace is deleted with its project, and a claim under
	// deletionPolicy Retain has to survive exactly that.
	return factory(cache.Options{
		Connection: conn,
		URL:        serverURL,
		Cluster:    r.Client,
		Namespace:  cache.DefaultCacheNamespace,
	})
}

// cacheProvisionerForClaim resolves the claim's Connection first; used by
// finalization, where the Connection may already be gone.
func (r *ResourceClaimReconciler) cacheProvisionerForClaim(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
) (cache.Provisioner, error) {
	conn := &kitchenv1alpha1.Connection{}
	key := types.NamespacedName{Namespace: claim.Namespace, Name: claim.Connection()}
	if err := r.Get(ctx, key, conn); err != nil {
		return nil, err
	}
	return r.cacheProvisionerFor(ctx, conn)
}

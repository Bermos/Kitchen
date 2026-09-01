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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/database"
)

// The postgres half of the ResourceClaim reconciler: a database from a
// database-capable Connection, provisioned through internal/provider/database
// — Neon, or CloudNativePG in this cluster — with its binding written into a
// Secret in the application namespace and, with previewBranching, one branch
// and one Secret per preview Environment.
//
// It is the first contract and the one the others are shaped after: a
// Connection matched on a capability, a provisioner built from it, a typed
// Requirements read off the claim's own slice of spec.config, and three
// answers from the provider — bound, not ready yet, or refused with a
// message.

const (
	// claimBranchFinalizer is this controller's hold on a preview Environment
	// it created a database branch for. The Environment's own reconciler
	// knows nothing about branches: the claim controller watches Environments,
	// tears the branch and its Secret down when one is deleted, and only then
	// releases this finalizer — the whole branch relationship stays in one
	// controller.
	claimBranchFinalizer = "kitchen.bermos.dev/resourceclaim-branch"

	// databaseTokenKey is the key in a database Connection's credentials
	// secret holding the provider API token.
	databaseTokenKey = "token"

	condBranchesReady = "PreviewBranchesReady"
)

// The self-hosted database provider: CloudNativePG's Clusters are what a
// postgres claim becomes in this cluster, and the pod and node reads are how
// the claim's residency is *reported* — the node its primary landed on is
// where the data actually is.
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// postgresContract is the claimContract for type postgres.
type postgresContract struct{}

// reconcile provisions the database through the Connection's plugin, keeps
// the preview branches in step, and binds.
func (postgresContract) reconcile(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
	project *kitchenv1alpha1.Project,
	conn *kitchenv1alpha1.Connection,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	provisioner, err := r.provisionerFor(ctx, conn)
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return r.pending(ctx, claim, "CredentialsMissing", err)
		case errors.Is(err, database.ErrUnsupportedProvider):
			return r.failed(ctx, claim, "ProviderUnsupported", err)
		default:
			return r.failed(ctx, claim, "ProviderError", err)
		}
	}

	// The binding Secret lives in the application namespace; a claim can be
	// bound before the project's first build creates it.
	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}

	if result, done, err := r.provision(ctx, claim, provisioner, appNS); done {
		return result, err
	}

	branchErr := r.reconcileBranches(ctx, claim, project.Name, provisioner, appNS, conn.Spec.Provider)

	// The declaration travels in the bind's audit record: what the data
	// derives from and where the provider put it are the two facts the
	// binding is answerable for. "" reads as undeclared/unreported — the
	// record states the absence rather than omitting it.
	reason := fmt.Sprintf("claim %s bound: %s via %s", claim.Name, claim.Spec.Type, conn.Name)
	if err := r.bind(ctx, claim, conn.Spec.Provider, reason, map[string]any{
		"type":           claim.Spec.Type,
		"connection":     conn.Name,
		"secret":         claim.Status.SecretName,
		"dataProvenance": claim.Status.DataProvenance,
		"residency":      claim.Status.Residency,
	}); err != nil {
		return ctrl.Result{}, err
	}
	if branchErr != nil {
		// The shared binding works either way; the branch condition carries
		// the provider's complaint and the requeue retries it.
		return ctrl.Result{RequeueAfter: claimRequeueDelay}, nil
	}
	log.Info("reconciled resource claim", "claim", claim.Name, "secret", claim.Status.SecretName)
	return ctrl.Result{}, nil
}

// finalize tears the branches down, releases the Environments they held, and
// deprovisions the database — the last only under deletionPolicy Delete:
// Retain is the default precisely so that deleting the claim in front of a
// production database cannot destroy its data.
func (postgresContract) finalize(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
) error {
	log := logf.FromContext(ctx)
	appNS := appNamespace(claim.Spec.ProjectRef.Name)

	// Best-effort: with the Connection or its credentials already gone there
	// is no provider to talk to, and blocking deletion on one would wedge the
	// claim (and any project teardown behind it) forever. Provider *errors*
	// below do return and retry — an unreachable API is transient, a deleted
	// Connection is not.
	provisioner, err := r.provisionerForClaim(ctx, claim)
	if err != nil {
		log.Info("finalizing claim without its provider", "claim", claim.Name, "reason", err.Error())
	}

	for _, branch := range claim.Status.Branches {
		if err := r.deleteBranch(ctx, claim, provisioner, appNS, branch); err != nil {
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

// provision creates the provider-side instance and the shared binding Secret
// when either is missing. done=true means the caller returns result and err
// as they are — provisioning failed and status already says why.
func (r *ResourceClaimReconciler) provision(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner database.Provisioner,
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
		// The instance exists but its Secret went missing: fall through and
		// provision again, which finds the instance by name and recovers the
		// binding.
	}

	instance, err := provisionInstance(ctx, claim, provisioner)
	switch {
	case errors.Is(err, database.ErrNotReady):
		// The database exists and is coming up. That is Pending and not
		// Failed: a database the platform runs itself takes minutes, and a
		// claim that read Failed for every one of them would teach everybody
		// to ignore the word.
		result, err := r.pending(ctx, claim, "Provisioning", err)
		return result, true, err
	case errors.Is(err, database.ErrUnsatisfiable):
		// The claim asked for something no image can supply. Nothing was
		// created and retrying will refuse again, so the message is the whole
		// answer — which is the point of asking for capabilities on the claim
		// rather than discovering them in a crash loop.
		result, err := r.failed(ctx, claim, "RequirementsUnsatisfiable", err)
		return result, true, err
	case err != nil:
		result, err := r.failed(ctx, claim, "ProvisionFailed", err)
		return result, true, err
	}
	if err := r.writeBindingSecret(ctx, claim, appNS, secretName, instance.Binding); err != nil {
		return ctrl.Result{}, true, err
	}
	claim.Status.InstanceID = instance.ID
	claim.Status.SecretName = secretName
	// The provider's own account of what it handed over, and where it put
	// it. Both may be empty — an undeclared provenance and an unreported
	// placement are states the status carries as absences, and the policy
	// engine and the inventory read them as such rather than guessing.
	claim.Status.DataProvenance = string(instance.Provenance)
	claim.Status.Residency = instance.Region
	setClaimCondition(claim, condProvisioned, metav1.ConditionTrue, "Provisioned",
		fmt.Sprintf("%s provisioned as %s", claim.Spec.Type, instance.ID))
	return ctrl.Result{}, false, nil
}

// provisionInstance asks the provisioner for the claim's database, with the
// claim's requirements where the provisioner can take them.
//
// A claim that asks for a Postgres version, an extension or a volume through
// a provisioner that cannot answer any of those is refused rather than
// provisioned as though it had not asked. The alternative — provision it and
// hope — is the exact failure this feature exists to remove, moved one
// provider along.
func provisionInstance(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner database.Provisioner,
) (database.Instance, error) {
	name := instanceName(claim)
	requirements := claimRequirements(claim)
	if requirements.Empty() {
		return provisioner.Provision(ctx, name)
	}
	capable, ok := provisioner.(database.CapableProvisioner)
	if !ok {
		return database.Instance{}, fmt.Errorf(
			"%w: this claim asks for a Postgres version, extensions or storage, and connection %q cannot be "+
				"asked for any of them — claim through a %s connection, which provisions into this cluster, "+
				"or drop config.postgres from the claim",
			database.ErrUnsatisfiable, claim.Connection(), database.ProviderCNPG)
	}
	return capable.ProvisionWith(ctx, name, requirements)
}

// claimRequirements reads the claim's spec.config into what the provisioner
// takes. The two vocabularies are kept apart on purpose: the CRD's is what
// somebody writes, the provider package's is what a provisioner answers, and
// neither should have to move because the other did.
func claimRequirements(claim *kitchenv1alpha1.ResourceClaim) database.Requirements {
	cfg := claim.Postgres()
	return database.Requirements{
		Version:      cfg.Version,
		Extensions:   cfg.Extensions,
		StorageSize:  cfg.Storage.Size,
		StorageClass: cfg.Storage.StorageClass,
	}
}

// reconcileBranches keeps the provider-side branches in step with the
// project's preview Environments: one branch and one binding Secret per live
// preview while previewBranching is on, none otherwise, and a deleted
// Environment's branch torn down before this controller's finalizer lets it
// go. A returned error is also recorded on the PreviewBranchesReady
// condition; the claim itself stays bound.
func (r *ResourceClaimReconciler) reconcileBranches(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	projectName string,
	provisioner database.Provisioner,
	appNS string,
	provider string,
) error {
	branching := claim.PreviewBranching()

	previous := map[string]kitchenv1alpha1.ClaimBranch{}
	for _, branch := range claim.Status.Branches {
		previous[branch.Environment] = branch
	}

	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := r.List(ctx, environments, client.InNamespace(claim.Namespace)); err != nil {
		return r.branchesNotReady(claim, "ListFailed", err)
	}

	kept := make([]kitchenv1alpha1.ClaimBranch, 0, len(claim.Status.Branches)+1)
	for i := range environments.Items {
		env := &environments.Items[i]
		if env.Spec.ProjectRef.Name != projectName || env.Spec.Type != kitchenv1alpha1.EnvironmentPreview {
			continue
		}
		if !env.DeletionTimestamp.IsZero() || !branching {
			// A preview on its way out, or branching switched off: the branch
			// and its Secret go, then the finalizer.
			if branch, ok := previous[env.Name]; ok {
				if err := r.deleteBranch(ctx, claim, provisioner, appNS, branch); err != nil {
					kept = append(kept, branch)
					claim.Status.Branches = kept
					return r.branchesNotReady(claim, "BranchTeardownFailed", err)
				}
				delete(previous, env.Name)
			}
			if controllerutil.RemoveFinalizer(env, claimBranchFinalizer) {
				if err := r.Update(ctx, env); err != nil {
					return r.branchesNotReady(claim, "BranchTeardownFailed", err)
				}
			}
			continue
		}

		// The finalizer goes on before the branch exists, so there is never a
		// branch whose Environment this controller has no hold on.
		if controllerutil.AddFinalizer(env, claimBranchFinalizer) {
			if err := r.Update(ctx, env); err != nil {
				return r.branchesNotReady(claim, "BranchFailed", err)
			}
		}
		_, existed := previous[env.Name]
		branch, err := r.ensureBranch(ctx, claim, provisioner, appNS, env.Name, previous)
		if err != nil {
			claim.Status.Branches = kept
			return r.branchesNotReady(claim, branchReason(err), err)
		}
		kept = append(kept, branch)
		delete(previous, env.Name)
		if !existed {
			// A branch this claim did not have before: sign and keep the
			// provider's declaration for it, naming the preview — the branch
			// is what that environment's workload reads, so the branch's
			// provenance is the one its policy is judged on.
			r.recordDataClassDeclaration(ctx, claim, env.Name, branch.Provenance, provider)
		}
	}

	// Whatever is left over belongs to Environments that no longer exist.
	for _, branch := range previous {
		if err := r.deleteBranch(ctx, claim, provisioner, appNS, branch); err != nil {
			kept = append(kept, branch)
			claim.Status.Branches = kept
			return r.branchesNotReady(claim, "BranchTeardownFailed", err)
		}
	}
	claim.Status.Branches = kept

	if !branching {
		meta.RemoveStatusCondition(&claim.Status.Conditions, condBranchesReady)
		return nil
	}
	setClaimCondition(claim, condBranchesReady, metav1.ConditionTrue, "BranchesReady",
		fmt.Sprintf("%d preview branch(es) in place", len(kept)))
	return nil
}

// ensureBranch makes sure one preview Environment has its branch and binding
// Secret, reusing what a previous reconcile recorded.
func (r *ResourceClaimReconciler) ensureBranch(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner database.Provisioner,
	appNS string,
	envName string,
	previous map[string]kitchenv1alpha1.ClaimBranch,
) (kitchenv1alpha1.ClaimBranch, error) {
	secretName := claimBranchSecretName(claim.Name, envName)
	if branch, ok := previous[envName]; ok {
		err := r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: branch.SecretName}, &corev1.Secret{})
		if err == nil {
			return branch, nil
		}
		if !apierrors.IsNotFound(err) {
			return kitchenv1alpha1.ClaimBranch{}, err
		}
		// Secret gone: CreateBranch below finds the branch by name again and
		// recovers the binding.
	}

	branch, err := provisioner.CreateBranch(ctx, claim.Status.InstanceID, envName)
	if err != nil {
		return kitchenv1alpha1.ClaimBranch{}, err
	}
	if err := r.writeBindingSecret(ctx, claim, appNS, secretName, branch.Binding); err != nil {
		return kitchenv1alpha1.ClaimBranch{}, err
	}
	return kitchenv1alpha1.ClaimBranch{
		Environment: envName,
		ID:          branch.ID,
		SecretName:  secretName,
		Provenance:  string(branch.Provenance),
	}, nil
}

// deleteBranch removes one branch at the provider and its binding Secret. A
// nil provisioner (only during finalization with the Connection gone) skips
// the provider call rather than wedging deletion forever.
func (r *ResourceClaimReconciler) deleteBranch(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner database.Provisioner,
	appNS string,
	branch kitchenv1alpha1.ClaimBranch,
) error {
	if provisioner != nil {
		if err := provisioner.DeleteBranch(ctx, claim.Status.InstanceID, branch.ID); err != nil {
			return err
		}
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: branch.SecretName, Namespace: appNS}}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// branchReason tells a preview database that is still coming up apart from one
// that failed. A database the platform runs itself takes minutes, and a
// condition that read "failed" for every one of them would teach everybody to
// ignore the word — the same distinction the claim's own phase makes.
func branchReason(err error) string {
	if errors.Is(err, database.ErrNotReady) {
		return "BranchProvisioning"
	}
	return "BranchFailed"
}

// branchesNotReady records why the preview branches are not in step and hands
// the error back for the caller's requeue.
func (r *ResourceClaimReconciler) branchesNotReady(claim *kitchenv1alpha1.ResourceClaim, reason string, err error) error {
	setClaimCondition(claim, condBranchesReady, metav1.ConditionFalse, reason, err.Error())
	return err
}

// releaseEnvironments drops this controller's finalizer from every preview
// Environment of the claim's project — the recorded branches and any stray a
// failed reconcile left behind.
func (r *ResourceClaimReconciler) releaseEnvironments(ctx context.Context, claim *kitchenv1alpha1.ResourceClaim) error {
	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := r.List(ctx, environments, client.InNamespace(claim.Namespace)); err != nil {
		return err
	}
	for i := range environments.Items {
		env := &environments.Items[i]
		if env.Spec.ProjectRef.Name != claim.Spec.ProjectRef.Name {
			continue
		}
		if controllerutil.RemoveFinalizer(env, claimBranchFinalizer) {
			if err := r.Update(ctx, env); err != nil {
				return err
			}
		}
	}
	return nil
}

// provisionerFor builds the database provisioner for a Connection, reading
// the API token from its credentials secret. The token never appears in any
// status, log line or error this controller writes.
//
// A Connection that names no credentials secret is not an error here: the
// self-hosted provider has no credential at all, because it provisions into
// this cluster with the operator's own account. That is the one provider it
// is true of, and the Connection CRD refuses an empty credentialsSecretRef
// for every other.
func (r *ResourceClaimReconciler) provisionerFor(ctx context.Context, conn *kitchenv1alpha1.Connection) (database.Provisioner, error) {
	token := ""
	if secretName := conn.Spec.CredentialsSecretRef.Name; secretName != "" {
		creds := &corev1.Secret{}
		key := types.NamespacedName{Namespace: conn.Namespace, Name: secretName}
		if err := r.Get(ctx, key, creds); err != nil {
			return nil, err
		}
		token = string(creds.Data[databaseTokenKey])
		if token == "" {
			return nil, fmt.Errorf("credentials secret %q has no %q key", secretName, databaseTokenKey)
		}
	}

	factory := r.Databases
	if factory == nil {
		factory = database.Default
	}
	return factory(database.Options{
		Connection: conn,
		Token:      token,
		Cluster:    r.Client,
		Namespace:  r.databaseNamespace(ctx),
	})
}

// databaseNamespace is where a self-hosted provisioner puts its databases —
// the singleton's spec.databases.namespace, and the compiled-in default when
// there is no singleton to read (a test cluster, or an operator running
// before the platform object lands). It is deliberately not a project's
// application namespace: that namespace is deleted with its project, and a
// claim under deletionPolicy Retain has to survive exactly that.
func (r *ResourceClaimReconciler) databaseNamespace(ctx context.Context) string {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return database.DefaultDatabaseNamespace
	}
	if namespace := kitchen.Spec.Databases.Namespace; namespace != "" {
		return namespace
	}
	return database.DefaultDatabaseNamespace
}

// provisionerForClaim resolves the claim's Connection first; used by
// finalization, where the Connection may already be gone.
func (r *ResourceClaimReconciler) provisionerForClaim(ctx context.Context, claim *kitchenv1alpha1.ResourceClaim) (database.Provisioner, error) {
	conn := &kitchenv1alpha1.Connection{}
	key := types.NamespacedName{Namespace: claim.Namespace, Name: claim.Connection()}
	if err := r.Get(ctx, key, conn); err != nil {
		return nil, err
	}
	return r.provisionerFor(ctx, conn)
}

// writeBindingSecret writes one binding into the project namespace. The keys
// are the vocabulary Project.spec.env's fromResourceClaim selects on.
func (r *ResourceClaimReconciler) writeBindingSecret(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	appNS, name string,
	binding database.Binding,
) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = map[string]string{
			labelProject:      claim.Spec.ProjectRef.Name,
			labelClaim:        claim.Name,
			labelManagedByKey: labelManagedByValue,
		}
		secret.Data = map[string][]byte{
			"url":      []byte(binding.URL),
			"host":     []byte(binding.Host),
			"port":     []byte(binding.Port),
			"user":     []byte(binding.User),
			"password": []byte(binding.Password),
			"database": []byte(binding.Database),
		}
		return nil
	})
	return err
}

// instanceName is the deterministic provider-side name for a claim's
// instance, which is what makes provisioning restartable: a lost status is
// recovered by looking the name up rather than by provisioning twice.
func instanceName(claim *kitchenv1alpha1.ResourceClaim) string {
	return "kitchen-" + claim.Name
}

func claimBranchSecretName(claim, environment string) string {
	return claim + "-binding-" + environment
}

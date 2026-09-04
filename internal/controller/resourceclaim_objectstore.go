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
	"github.com/Bermos/Kitchen/internal/provider/naming"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

// The objectStore half of the ResourceClaim reconciler: a bucket from an
// objectStore-capable Connection, provisioned through
// internal/provider/objectstore — the MinIO the chart runs, or any
// S3-compatible store somebody else runs — with its binding written into a
// Secret in the application namespace and, since the provider declares
// previews get a fresh bucket, one bucket and one Secret per preview
// Environment.
//
// It is the third contract, and it is the first one's shape exactly: a
// Connection matched on a capability, a provisioner built from it, typed
// Requirements read off the claim's own slice of spec.config, three answers
// from the provider. The branch machinery is the postgres contract's,
// reached through claimBrancher, because a preview's own bucket and a
// preview's own database are the same thing to the reconciler — a
// provider-side resource created with the Environment and torn down with
// it, under either deletion policy.

// objectStoreContract is the claimContract for type objectStore.
type objectStoreContract struct{}

func (objectStoreContract) reconcile(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
	project *kitchenv1alpha1.Project,
	conn *kitchenv1alpha1.Connection,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	provisioner, err := r.bucketProvisionerFor(ctx, conn)
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return r.pending(ctx, claim, "CredentialsMissing", err)
		case errors.Is(err, objectstore.ErrUnsupportedProvider):
			return r.failed(ctx, claim, "ProviderUnsupported", err)
		default:
			return r.failed(ctx, claim, "ProviderError", err)
		}
	}

	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}

	if result, done, err := r.provisionBucket(ctx, claim, provisioner, appNS); done {
		return result, err
	}

	claimType, _ := claim.Type()
	mode := declare(claim, claimType, conn.Spec.Provider)
	branchErr := r.reconcileBranches(ctx, claim, project.Name, bucketBrancher{provisioner}, appNS,
		conn.Spec.Provider, mode.Isolated())

	reason := fmt.Sprintf("claim %s bound: %s via %s", claim.Name, claim.Spec.Type, conn.Name)
	if err := r.bind(ctx, claim, conn.Spec.Provider, reason, map[string]any{
		"type":           claim.Spec.Type,
		"connection":     conn.Name,
		"secret":         claim.Status.SecretName,
		"dataProvenance": claim.Status.DataProvenance,
		"residency":      claim.Status.Residency,
		"previewMode":    claim.Status.PreviewMode,
	}); err != nil {
		return ctrl.Result{}, err
	}
	if branchErr != nil {
		return ctrl.Result{RequeueAfter: claimRequeueDelay}, nil
	}
	log.Info("reconciled resource claim", "claim", claim.Name, "secret", claim.Status.SecretName)
	return ctrl.Result{}, nil
}

// finalize tears the preview buckets down under either policy, releases the
// Environments they held, and removes the claim's own bucket — with its
// objects and its credential — under deletionPolicy Delete alone. Retain
// leaves the bucket, its objects and its user at the store, and a claim of
// the same name created later against the same connection finds the bucket
// by name and re-issues the credential.
func (objectStoreContract) finalize(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
) error {
	log := logf.FromContext(ctx)
	appNS := appNamespace(claim.Spec.ProjectRef.Name)

	provisioner, err := r.bucketProvisionerForClaim(ctx, claim)
	if err != nil {
		log.Info("finalizing claim without its provider", "claim", claim.Name, "reason", err.Error())
	}
	var brancher claimBrancher
	if provisioner != nil {
		brancher = bucketBrancher{provisioner}
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

// provisionBucket creates the bucket and the shared binding Secret when
// either is missing, on the same terms as provision does for a database.
func (r *ResourceClaimReconciler) provisionBucket(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner objectstore.Provisioner,
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
		// The bucket exists and its Secret went missing: provisioning again
		// finds the bucket by name and re-issues its credential.
	}

	instance, err := provisionBucketInstance(ctx, claim, provisioner)
	switch {
	case errors.Is(err, objectstore.ErrNotReady):
		result, err := r.pending(ctx, claim, "Provisioning", err)
		return result, true, err
	case errors.Is(err, naming.ErrNotAdoptable):
		result, err := r.failed(ctx, claim, "InstanceNotAdoptable", err)
		return result, true, err
	case errors.Is(err, objectstore.ErrUnsatisfiable):
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

// provisionBucketInstance asks the provisioner for the claim's bucket, with
// the claim's requirements where the provisioner can take them, and refuses
// requirements it cannot rather than provisioning as though they had not
// been written down.
func provisionBucketInstance(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner objectstore.Provisioner,
) (objectstore.Instance, error) {
	resource := claimResource(claim)
	requirements := bucketRequirements(claim)
	if requirements.Empty() {
		return provisioner.Provision(ctx, resource)
	}
	capable, ok := provisioner.(objectstore.CapableProvisioner)
	if !ok {
		return objectstore.Instance{}, fmt.Errorf(
			"%w: this claim asks for versioning, public reads or a size, and connection %q cannot be asked "+
				"for any of them — drop config.objectStore from the claim",
			objectstore.ErrUnsatisfiable, claim.Connection())
	}
	return capable.ProvisionWith(ctx, resource, requirements)
}

// bucketRequirements reads the claim's spec.config into what the provisioner
// takes, keeping the CRD's vocabulary and the provider package's apart.
func bucketRequirements(claim *kitchenv1alpha1.ResourceClaim) objectstore.Requirements {
	cfg := claim.ObjectStore()
	return objectstore.Requirements{
		Versioning: cfg.Versioning,
		PublicRead: cfg.PublicRead,
		Size:       cfg.Size,
	}
}

// bucketBrancher is an object store provisioner as reconcileBranches sees
// it: a preview's own bucket, created and torn down by name.
type bucketBrancher struct{ provisioner objectstore.Provisioner }

func (b bucketBrancher) createBranch(ctx context.Context, instanceID, name string) (claimBranchResult, error) {
	branch, err := b.provisioner.CreateBranch(ctx, instanceID, name)
	if err != nil {
		return claimBranchResult{}, err
	}
	return claimBranchResult{ID: branch.ID, Provenance: string(branch.Provenance), Data: branch.Binding.Data()}, nil
}

func (b bucketBrancher) deleteBranch(ctx context.Context, instanceID, branchID string) error {
	return b.provisioner.DeleteBranch(ctx, instanceID, branchID)
}

// idler is nil: a bucket is storage and no compute, so an idle preview's
// bucket already costs only what its objects cost. The claim's status says
// so in the provider's own words.
func (bucketBrancher) idler() claimIdler { return nil }

// bucketProvisionerFor builds the object store provisioner for a
// Connection, reading its access key pair from its credentials secret. The
// pair never appears in any status, log line or error this controller
// writes.
func (r *ResourceClaimReconciler) bucketProvisionerFor(
	ctx context.Context,
	conn *kitchenv1alpha1.Connection,
) (objectstore.Provisioner, error) {
	secretName := conn.Spec.CredentialsSecretRef.Name
	if secretName == "" {
		return nil, fmt.Errorf("connection %q names no credentials secret", conn.Name)
	}
	creds := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: conn.Namespace, Name: secretName}, creds); err != nil {
		return nil, err
	}
	accessKey := string(creds.Data[objectstore.CredentialKeyAccessKeyID])
	secretKey := string(creds.Data[objectstore.CredentialKeySecretAccessKey])
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("credentials secret %q needs both %q and %q", secretName,
			objectstore.CredentialKeyAccessKeyID, objectstore.CredentialKeySecretAccessKey)
	}

	factory := r.Buckets
	if factory == nil {
		factory = objectstore.Default
	}
	return factory(objectstore.Options{Connection: conn, AccessKeyID: accessKey, SecretAccessKey: secretKey})
}

// bucketProvisionerForClaim resolves the claim's Connection first; used by
// finalization, where the Connection may already be gone.
func (r *ResourceClaimReconciler) bucketProvisionerForClaim(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
) (objectstore.Provisioner, error) {
	conn := &kitchenv1alpha1.Connection{}
	key := types.NamespacedName{Namespace: claim.Namespace, Name: claim.Connection()}
	if err := r.Get(ctx, key, conn); err != nil {
		return nil, err
	}
	return r.bucketProvisionerFor(ctx, conn)
}

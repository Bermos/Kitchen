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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/backup"
	"github.com/Bermos/Kitchen/internal/provider/database"
)

// Backup policies for the databases this platform runs (#245 phase 2).
//
// The platform archive carries configuration, credentials and accounts, and
// deliberately carries no application data — docs/BACKUP.md has said so since
// before there was anything to do about it. This is the other half: a
// CloudNativePG claim's database, archived continuously to an object store
// with a base backup on a schedule, so that "restore the platform" and
// "recover the data" are two operations that both exist rather than one that
// does and one that is a sentence in a document.
//
// Four decisions are made here rather than left implicit:
//
//   - **The policy is inherited, not repeated.** A claim's own spec.backup
//     wins field by field over its Connection's default, which wins over the
//     platform's destination. An installation that configured a destination
//     once has said where its databases go.
//   - **Backups outlive the claim, under either deletion policy.** Nothing in
//     this file, and nothing the finalizer does, deletes an object at the
//     destination. `Delete` destroys the database; if it also destroyed the
//     backups then the policy meant to protect data would be the one that
//     removed the last copy of it.
//   - **Previews are never backed up.** A preview's database is a fresh,
//     empty Cluster declaring `dataProvenance: synthetic` — this runs against
//     the claim's own instance and never against a branch, which is what
//     "previews default off" means in code rather than in a default.
//   - **What a provider keeps is the provider's.** A Neon claim reports what
//     Neon keeps and takes no policy; the platform manages nothing it cannot.

// CloudNativePG's schedule object, which is what a base backup on a schedule
// is. The Cluster verbs the postgres contract already holds cover the object
// store half, which is a field on the Cluster itself.
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=scheduledbackups,verbs=get;list;watch;create;update;patch;delete

const (
	// claimBackupPrefix is appended to the destination's prefix for every
	// database backup, so that a bucket shared with the platform archive
	// reads as two things rather than one muddle. The platform's own prune
	// only ever considers objects named like an archive it wrote, so this is
	// legibility rather than protection — but a person looking into the
	// bucket during an incident is exactly who needs it.
	claimBackupPrefix = "databases"

	// backupCredentialRegionKey is where a region is put in the copied
	// credential. CloudNativePG takes its region by secret reference rather
	// than as a plain string, which is why there is a key for it at all.
	backupCredentialRegionKey = "region"
)

// claimBackupSecretName is the destination credential as this claim's
// database can read it: a copy in the namespace the Clusters live in, because
// CloudNativePG resolves a Secret reference in the Cluster's own namespace
// and the credential the API wrote is in the platform's.
//
// It is the same shape as the registry pull credential, and for the same
// reason — "a private registry needs the credential twice" — and it is named
// after the claim so that it goes when the claim does.
func claimBackupSecretName(claim string) string { return claim + "-backup-destination" }

// reconcileClaimBackup keeps one claim's backup policy in force and reports
// what the destination actually holds.
//
// A returned error is a transient one worth retrying. Everything a person
// would have to act on — no destination configured, a database the platform
// did not create, a provider version that has dropped the mechanism — lands
// on status.backup with the sentence that names the fix, because retrying it
// forever would change nothing and hide it.
func (r *ResourceClaimReconciler) reconcileClaimBackup(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner database.Provisioner,
	conn *kitchenv1alpha1.Connection,
) error {
	provider := ""
	if conn != nil {
		provider = conn.Spec.Provider
	}

	// A provider that backs itself up takes no policy. This is the honest
	// third state: such a claim is protected, by somebody else, and reporting
	// it as unprotected would be as wrong as reporting a bucket as protected.
	if selfBacking, ok := provisioner.(database.SelfBackingProvisioner); ok {
		claim.Status.Backup = &kitchenv1alpha1.ClaimBackupStatus{
			ProviderManaged: true,
			Reason:          selfBacking.ManagedBackupNote(),
		}
		return nil
	}
	backer, ok := provisioner.(database.BackupProvisioner)
	if !ok {
		claim.Status.Backup = &kitchenv1alpha1.ClaimBackupStatus{
			Reason: fmt.Sprintf("the %s provider neither keeps backups of its own nor takes a policy from "+
				"this platform, so nothing is backing this database up", provider),
		}
		return nil
	}
	if claim.Status.InstanceID == "" {
		claim.Status.Backup = &kitchenv1alpha1.ClaimBackupStatus{
			Reason: "the database is not provisioned yet, so there is nothing to back up",
		}
		return nil
	}

	policy, status, err := r.claimBackupPolicy(ctx, claim, conn)
	if err != nil {
		return err
	}
	claim.Status.Backup = status

	if err := backer.ConfigureBackup(ctx, claim.Status.InstanceID, policy); err != nil {
		switch {
		case errors.Is(err, database.ErrBackupNotManaged),
			errors.Is(err, database.ErrBackupUnsupported),
			errors.Is(err, database.ErrUnsatisfiable):
			// A refusal rather than a failure: retrying will refuse again, so
			// the message is the whole answer and it belongs where somebody
			// is already looking at the claim.
			status.Enabled = false
			status.Reason = err.Error()
			return nil
		case errors.Is(err, database.ErrNotReady):
			status.Reason = err.Error()
			return nil
		default:
			return err
		}
	}

	// Read back rather than echo: a policy that never landed must not be able
	// to report itself as in force, and the first recoverable point is a fact
	// about the destination that no spec can state.
	state, err := backer.BackupState(ctx, claim.Status.InstanceID)
	if err != nil {
		return err
	}
	applyBackupState(status, state)
	return nil
}

// applyBackupState folds what the provider reports into the claim's status.
func applyBackupState(status *kitchenv1alpha1.ClaimBackupStatus, state database.BackupState) {
	if state.Destination != "" {
		status.Destination = state.Destination
	}
	if state.Schedule != "" {
		status.Schedule = state.Schedule
	}
	if state.RetentionPolicy != "" {
		status.RetentionPolicy = state.RetentionPolicy
	}
	status.LastBackup = backupTimeOrNil(state.LastBackup)
	status.LastFailure = backupTimeOrNil(state.LastFailure)
	status.FirstRecoverablePoint = backupTimeOrNil(state.FirstRecoverablePoint)

	switch state.Archiving {
	case database.ArchivingHealthy:
		status.Archiving = kitchenv1alpha1.ClaimArchivingHealthy
	case database.ArchivingFailing:
		status.Archiving = kitchenv1alpha1.ClaimArchivingFailing
	default:
		status.Archiving = kitchenv1alpha1.ClaimArchivingUnknown
	}
	status.ArchivingMessage = state.ArchivingMessage

	// The sentence a reader of a working policy gets. It says what is true —
	// there is a schedule and a destination — without claiming a recovery
	// point that has not been reported yet.
	if status.Enabled && status.Reason == "" {
		if status.FirstRecoverablePoint == nil {
			status.Reason = "configured; no recovery point has been reported yet, which is the state a " +
				"database is in until its first base backup has been taken and read back"
		} else {
			status.Reason = "backed up to " + status.Destination
		}
	}
}

// timeOrNil converts a provider timestamp for the status.
func backupTimeOrNil(at *time.Time) *metav1.Time {
	if at == nil {
		return nil
	}
	when := metav1.NewTime(*at)
	return &when
}

// claimBackupPolicy resolves what this claim's policy actually is, and the
// status that goes with it.
//
// The inheritance is field by field and in one direction: the claim narrows
// its Connection's default, which narrows the platform's. That is the same
// operator-sets-defaults / developer-overrides split cnpgConfig already has
// for images and storage, and it is why the Connection is where an
// installation says "every database I provision goes to this bucket, nightly,
// kept 30 days" exactly once.
func (r *ResourceClaimReconciler) claimBackupPolicy(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	conn *kitchenv1alpha1.Connection,
) (database.BackupPolicy, *kitchenv1alpha1.ClaimBackupStatus, error) {
	wanted := mergeClaimBackupSpec(claim.Spec.Backup, database.ConnectionBackupPolicy(conn))
	status := &kitchenv1alpha1.ClaimBackupStatus{
		Schedule:        wanted.Schedule,
		RetentionPolicy: wanted.RetentionPolicy,
	}

	destination := wanted.Destination
	if destination == nil {
		kitchen := &kitchenv1alpha1.Kitchen{}
		if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
			if !apierrors.IsNotFound(err) {
				return database.BackupPolicy{}, nil, err
			}
		} else {
			destination = kitchen.Spec.Backup.Destination
		}
	}

	// No destination anywhere is not a half-configuration to report later. It
	// is the whole answer, and it names the one route that fixes it — there
	// is deliberately no local destination for a database's backups any more
	// than for the platform archive.
	if destination == nil {
		status.Enabled = false
		status.Reason = "this installation has nowhere to write backups to: set the platform's backup " +
			"destination (PUT /platform/backup/destination, or the Backup screen) and every database " +
			"claimed here is archived to it"
		return database.BackupPolicy{}, status, nil
	}
	status.Destination = backup.Describe(destination)

	// Enabled defaults to on once there is somewhere to write. An
	// installation that has gone to the trouble of configuring an off-cluster
	// destination has said what it wants done with its data; a claim that
	// wants out says so.
	if wanted.Enabled != nil && !*wanted.Enabled {
		status.Enabled = false
		status.Reason = "backups are switched off for this claim. What is already at the destination stays " +
			"there — switching a policy off never deletes an archive"
		return database.BackupPolicy{}, status, nil
	}
	status.Enabled = true

	resolved, err := r.claimBackupDestination(ctx, claim, destination)
	if err != nil {
		return database.BackupPolicy{}, nil, err
	}
	if !resolved.Configured() {
		status.Enabled = false
		status.Reason = fmt.Sprintf("the backup destination %q names no bucket, so there is nowhere for "+
			"this database's archives to go", status.Destination)
		return database.BackupPolicy{}, status, nil
	}
	return database.BackupPolicy{
		Enabled:         true,
		Schedule:        wanted.Schedule,
		RetentionPolicy: wanted.RetentionPolicy,
		Destination:     resolved,
	}, status, nil
}

// mergeClaimBackupSpec lays the claim's policy over the Connection's, field
// by field. Either may be nil; an unset field on the claim takes the
// Connection's answer, which is what makes a Connection a default rather than
// an all-or-nothing.
func mergeClaimBackupSpec(claim, connection *kitchenv1alpha1.ClaimBackupSpec) kitchenv1alpha1.ClaimBackupSpec {
	merged := kitchenv1alpha1.ClaimBackupSpec{}
	if connection != nil {
		merged = *connection.DeepCopy()
	}
	if claim == nil {
		return merged
	}
	if claim.Enabled != nil {
		enabled := *claim.Enabled
		merged.Enabled = &enabled
	}
	if strings.TrimSpace(claim.Schedule) != "" {
		merged.Schedule = claim.Schedule
	}
	if strings.TrimSpace(claim.RetentionPolicy) != "" {
		merged.RetentionPolicy = claim.RetentionPolicy
	}
	if claim.Destination != nil {
		merged.Destination = claim.Destination.DeepCopy()
	}
	return merged
}

// claimBackupDestination turns the CRD's destination into the one the
// provisioner takes, putting the credential where the database can read it.
//
// The copy is the point. CloudNativePG resolves a Secret reference in the
// Cluster's own namespace, and the credential the API wrote is in the
// platform's — so it is synced across, exactly as the registry pull
// credential is synced into each application namespace, and it carries the
// claim's labels so that it goes when the claim does.
func (r *ResourceClaimReconciler) claimBackupDestination(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	spec *kitchenv1alpha1.BackupDestination,
) (database.BackupDestination, error) {
	if spec.Type != kitchenv1alpha1.BackupDestinationS3 || spec.S3 == nil {
		return database.BackupDestination{}, nil
	}
	s3 := spec.S3
	resolved := database.BackupDestination{
		Bucket:               s3.Bucket,
		Prefix:               strings.Trim(strings.Trim(s3.Prefix, "/")+"/"+claimBackupPrefix, "/"),
		Region:               s3.Region,
		Endpoint:             s3.Endpoint,
		ServerSideEncryption: s3.ServerSideEncryption,
	}

	data := map[string][]byte{}
	if ref := s3.CredentialsSecretRef; ref != nil {
		source := &corev1.Secret{}
		key := types.NamespacedName{Namespace: claim.Namespace, Name: ref.Name}
		if err := r.Get(ctx, key, source); err != nil {
			return database.BackupDestination{}, fmt.Errorf(
				"the backup destination's credential %s/%s could not be read: %w", key.Namespace, key.Name, err)
		}
		data[backup.CredentialKeyAccessKeyID] = source.Data[backup.CredentialKeyAccessKeyID]
		data[backup.CredentialKeySecretAccessKey] = source.Data[backup.CredentialKeySecretAccessKey]
	}
	// The region travels with the credential because CloudNativePG takes it
	// by secret reference and not as a plain string — an oddity of that
	// operator's schema rather than a decision here.
	if region := strings.TrimSpace(s3.Region); region != "" {
		data[backupCredentialRegionKey] = []byte(region)
	}
	if len(data) == 0 {
		// Nothing to copy: the ambient credential chain, which is the better
		// answer where it is available because there is then no long-lived
		// key anywhere to leak.
		return resolved, nil
	}

	name := claimBackupSecretName(claim.Name)
	namespace := r.databaseNamespace(ctx)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = map[string]string{
			labelProject:      claim.Spec.ProjectRef.Name,
			labelClaim:        claim.Name,
			labelManagedByKey: labelManagedByValue,
		}
		secret.Data = data
		return nil
	}); err != nil {
		return database.BackupDestination{}, err
	}

	resolved.CredentialsSecret = name
	resolved.AccessKeyIDKey = backup.CredentialKeyAccessKeyID
	resolved.SecretAccessKeyKey = backup.CredentialKeySecretAccessKey
	if _, ok := data[backupCredentialRegionKey]; ok {
		resolved.RegionKey = backupCredentialRegionKey
	}
	return resolved, nil
}

// finalizeClaimBackup removes the platform's own bookkeeping and **nothing at
// the destination**.
//
// That is the decision this phase makes most deliberately. Under
// `deletionPolicy: Delete` the database itself is destroyed; if the backups
// went with it, "Delete" would quietly destroy the recovery point, which is
// the one thing deletion protection exists to prevent. What is in the bucket
// is pruned by the retention policy and by nothing else — including this.
func (r *ResourceClaimReconciler) finalizeClaimBackup(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
) error {
	copies := []types.NamespacedName{
		{Namespace: r.databaseNamespace(ctx), Name: claimBackupSecretName(claim.Name)},
		{Namespace: claim.Namespace, Name: claimBackupSecretName(claim.Name)},
	}
	for _, key := range copies {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, key, secret); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		// Only what this platform wrote. A Secret of that name somebody else
		// put there is somebody else's, and the rule that credentials the API
		// wrote go with their object is the same rule that says the ones it
		// did not write stay.
		if secret.Labels[labelManagedByKey] != labelManagedByValue {
			continue
		}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

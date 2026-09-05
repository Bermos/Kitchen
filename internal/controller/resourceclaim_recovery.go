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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/provider/database"
)

// Point-in-time recovery, where the provider can actually do it (#247).
//
// "Restore" is the wrong word and getting that right is most of the design.
// Neither supported provider rewinds a database in place: Neon branches at a
// parent timestamp, and both that and anything cnpg could do hand back a
// *second* database holding the old data. So the primitive is **recover to a
// sibling, then decide**, which is the better design anyway — the original is
// untouched while somebody looks at the copy, cutover is a separate act with
// its own confirmation, and getting the timestamp wrong costs another
// recovery rather than the database.
//
// Two operations, therefore, and this file is both of them:
//
//   - **Recover** is cheap and reversible. Every entry in spec.recoveries
//     becomes a sibling database with a binding Secret of its own, listed on
//     the claim's status. Removing the entry discards it.
//   - **Promote** is destructive, and is spec.promotedRecovery: the claim's
//     own binding Secret is rewritten with the recovery's binding, so every
//     environment reading the claim rolls onto it. What it displaced is
//     retained and recorded, never destroyed.
//
// The window is read from the provider on every pass and never declared —
// the same rule residency follows. A claim whose provisioner does not
// implement database.RecoverableProvisioner reports no window and offers no
// recovery, with the reason on the status, rather than offering one that
// fails when it is used.

const condRecoveries = "RecoveriesReady"

// recoveryBranchName is what a recovery is called at the provider. It is
// prefixed so that a recovery and a preview environment of the same name
// cannot be the same branch — a preview branch is named after its
// Environment, and "recovery" is not a name an Environment can have.
func recoveryBranchName(name string) string { return "recovery-" + name }

// recoverySecretName is the recovery's own binding Secret in the application
// namespace: a second address for the same application's data, which is what
// makes looking at the copy possible before anything is promoted.
func recoverySecretName(claim, recovery string) string {
	return claim + "-recovery-" + recovery
}

// reconcileRecoveries keeps the claim's recovered siblings in step with what
// it asks for, and its binding in step with what it promotes.
//
// A returned error is also recorded on the RecoveriesReady condition and the
// claim itself stays bound: a recovery the provider refused is a fact about
// one sibling, not about the database the application is reading.
func (r *ResourceClaimReconciler) reconcileRecoveries(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner database.Provisioner,
	appNS string,
	provider string,
) error {
	recoverable, ok := provisioner.(database.RecoverableProvisioner)
	if !ok {
		// The asymmetry said out loud rather than as a greyed-out button:
		// this provider cannot reconstruct data at a past moment, so the
		// claim offers nothing and says which provider it is.
		return r.recoveryUnavailable(claim, fmt.Sprintf(
			"%s cannot recover a database to a point in time: there is no history at the provider to "+
				"reach back into", provider))
	}
	if claim.Status.InstanceID == "" {
		return r.recoveryUnavailable(claim, "the database is not provisioned yet")
	}

	// unreadable is why the window could not be given, kept rather than
	// returned on the spot so that the siblings below are reconciled under it
	// anyway: a provider that stops answering does not un-make the copies it
	// has already made, and a recovery somebody discards while the window is
	// gone still has to go.
	var unreadable error
	unreadableReason := ""

	status := r.recoveryStatus(claim)
	window, err := recoverable.RecoveryWindow(ctx, claim.Status.InstanceID)
	switch {
	case err == nil && window.Empty():
		status.Window = nil
		status.Available = false
		status.Reason = fmt.Sprintf("%s keeps no history for this database, so there is nothing to "+
			"recover to", provider)
	case err == nil:
		status.Window = &kitchenv1alpha1.ClaimRecoveryWindow{
			Earliest:   metav1.NewTime(window.Earliest),
			Latest:     metav1.NewTime(window.Latest),
			ObservedAt: metav1.NewTime(time.Now()),
		}
		status.Available = true
		status.Reason = fmt.Sprintf("%s can reconstruct this database to any moment inside the window",
			provider)

	case errors.Is(err, database.ErrBackupNotManaged), errors.Is(err, database.ErrUnsatisfiable):
		// A refusal: the provider will not recover *this* database and has
		// said why. Retrying refuses again, so the provider's own sentence is
		// the whole answer — a database this platform did not create, or one
		// with no archive to bootstrap from, is a fact and not a fault.
		if failure := r.recoveryUnavailable(claim, err.Error()); failure != nil {
			return failure
		}

	case errors.Is(err, database.ErrNotReady):
		// Not yet: there is an archive and nothing in it to reach back to.
		// The claim says so in the provider's own words, and the requeue is
		// what makes the window appear without anybody nudging it.
		status.Window = nil
		status.Available = false
		status.Reason = err.Error()
		unreadable, unreadableReason = err, "WindowNotYet"

	default:
		// The window could not be read, which is not the same as there not
		// being one. Whatever was last observed stays on the status, marked
		// unavailable, so nothing offers a picker over a window nobody can
		// confirm.
		status.Available = false
		status.Reason = fmt.Sprintf("could not read what %s can recover to: %s", provider, err.Error())
		unreadable, unreadableReason = err, "WindowUnreadable"
	}

	if err := r.recoverSiblings(ctx, claim, recoverable, appNS); err != nil {
		return err
	}
	if err := r.promoteRecovery(ctx, claim, appNS); err != nil {
		return err
	}
	if unreadable != nil {
		return r.recoveriesNotReady(claim, unreadableReason, unreadable)
	}

	count := len(r.recoveryStatus(claim).Recoveries)
	if count == 0 {
		meta.RemoveStatusCondition(&claim.Status.Conditions, condRecoveries)
		return nil
	}
	setClaimCondition(claim, condRecoveries, metav1.ConditionTrue, "RecoveriesReady",
		fmt.Sprintf("%d recovered database(s) in place", count))
	return nil
}

// recoverSiblings makes each asked-for recovery exist, and discards the ones
// nobody asks for any more. Nothing here touches the claim's own binding:
// that is promote's, and keeping the two apart is the whole point of the two
// steps.
func (r *ResourceClaimReconciler) recoverSiblings(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	recoverable database.RecoverableProvisioner,
	appNS string,
) error {
	status := r.recoveryStatus(claim)
	previous := map[string]kitchenv1alpha1.ClaimRecovery{}
	for _, recovery := range status.Recoveries {
		previous[recovery.Name] = recovery
	}

	kept := make([]kitchenv1alpha1.ClaimRecovery, 0, len(claim.Spec.Recoveries))
	var failure error
	for _, request := range claim.Spec.Recoveries {
		recorded, existed := previous[request.Name]
		delete(previous, request.Name)
		if existed && recorded.Phase == kitchenv1alpha1.ClaimRecoveryReady && recorded.SecretName != "" {
			err := r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: recorded.SecretName}, &corev1.Secret{})
			if err == nil {
				kept = append(kept, recorded)
				continue
			}
			if !apierrors.IsNotFound(err) {
				return err
			}
			// The Secret went missing: RecoverTo below finds the database by
			// name again and recovers the binding, rather than taking a
			// second copy at a different moment.
		}
		if !existed {
			recorded = kitchenv1alpha1.ClaimRecovery{
				Name:      request.Name,
				At:        request.At,
				CreatedAt: metav1.NewTime(time.Now()),
				// A recovery is a new place the same data lives, so it
				// carries the claim's own classification down with it.
				DataClass: claim.Spec.DataClass,
			}
		}
		recorded.At = request.At

		branch, err := recoverable.RecoverTo(ctx, claim.Status.InstanceID,
			recoveryBranchName(request.Name), request.At.Time)
		if err != nil {
			// A provider that is still making it is Pending, not Failed. The
			// same distinction the claim's own phase makes, and for the same
			// reason: a word that reads "failed" for every minute of a normal
			// wait teaches everybody to ignore it.
			recorded.Phase = kitchenv1alpha1.ClaimRecoveryFailed
			if errors.Is(err, database.ErrNotReady) {
				recorded.Phase = kitchenv1alpha1.ClaimRecoveryPending
			}
			recorded.Message = err.Error()
			kept = append(kept, recorded)
			failure = err
			continue
		}
		if err := r.writeBindingSecret(ctx, claim, appNS,
			recoverySecretName(claim.Name, request.Name), databaseBindingData(branch.Binding)); err != nil {
			return err
		}
		recorded.ID = branch.ID
		recorded.SecretName = recoverySecretName(claim.Name, request.Name)
		recorded.Provenance = string(branch.Provenance)
		recorded.Phase = kitchenv1alpha1.ClaimRecoveryReady
		recorded.Message = ""
		kept = append(kept, recorded)
		if !existed {
			r.Activity.Record(ctx, clickhouse.Event{
				Type:    clickhouse.EventClaimBound,
				Project: claim.Spec.ProjectRef.Name,
				Claim:   claim.Name,
				Message: fmt.Sprintf("claim %s recovered to %s as %s", claim.Name,
					request.At.Time.UTC().Format(time.RFC3339), request.Name),
			})
		}
	}

	// Whatever is left over is a recovery nobody asks for any more: the
	// database and its binding go. A promoted recovery is never here — the
	// API refuses to discard the one the claim is bound to.
	for _, recovery := range previous {
		if recovery.ID != "" {
			if err := recoverable.DeleteBranch(ctx, claim.Status.InstanceID, recovery.ID); err != nil {
				kept = append(kept, recovery)
				failure = err
				continue
			}
		}
		if recovery.SecretName != "" {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: recovery.SecretName, Namespace: appNS}}
			if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	status.Recoveries = kept
	if failure != nil {
		return r.recoveriesNotReady(claim, "RecoveryFailed", failure)
	}
	return nil
}

// promoteRecovery makes the claim's binding the promoted recovery's, and
// records what that displaced.
//
// **Cutover ordering.** The binding Secret is rewritten in place and the
// environments reading it roll through their ordinary deploy — the platform
// does not stop them first. Taking every environment of a project to zero
// replicas would be an outage the platform started on its own, with no bound
// on how long it lasted if the roll then failed; and the thing a scale-down
// would protect against — writes landing in the displaced database during the
// cutover — is exactly what retaining that database keeps readable. So the
// window is one rolling update wide, it is documented (docs/api/claims.md),
// and nothing about it is silent.
func (r *ResourceClaimReconciler) promoteRecovery(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	appNS string,
) error {
	status := r.recoveryStatus(claim)
	promoted := claim.Spec.PromotedRecovery
	if promoted == "" {
		return nil
	}

	var recovery *kitchenv1alpha1.ClaimRecovery
	for i := range status.Recoveries {
		if status.Recoveries[i].Name == promoted {
			recovery = &status.Recoveries[i]
		}
	}
	if recovery == nil {
		return r.recoveriesNotReady(claim, "PromotionUnknown",
			fmt.Errorf("no recovery named %q to promote", promoted))
	}
	if recovery.Phase != kitchenv1alpha1.ClaimRecoveryReady || recovery.SecretName == "" {
		// Asked for before the sibling exists: the claim keeps the binding it
		// has, and the next pass promotes.
		return r.recoveriesNotReady(claim, "PromotionWaiting",
			fmt.Errorf("recovery %q is not ready to be promoted yet", promoted))
	}

	source := &corev1.Secret{}
	key := types.NamespacedName{Namespace: appNS, Name: recovery.SecretName}
	if err := r.Get(ctx, key, source); err != nil {
		return err
	}
	if claim.Status.SecretName == "" {
		return r.recoveriesNotReady(claim, "PromotionWaiting",
			fmt.Errorf("claim %s has no binding to promote over yet", claim.Name))
	}
	if err := r.writeBindingSecret(ctx, claim, appNS, claim.Status.SecretName, source.Data); err != nil {
		return err
	}

	if recovery.PromotedAt != nil {
		// Already the claim's binding; the Secret write above is what keeps
		// it that way through a reconcile that runs again.
		return nil
	}

	// What this displaced is kept, and recorded so that somebody can find it:
	// the previously promoted recovery, or — the first time — the instance's
	// own database, which is still there under status.instanceName.
	displaced := kitchenv1alpha1.ClaimRetainedDatabase{
		DisplacedBy: promoted,
		At:          metav1.NewTime(time.Now()),
	}
	for i := range status.Recoveries {
		if status.Recoveries[i].PromotedAt == nil || status.Recoveries[i].Name == promoted {
			continue
		}
		displaced.Recovery = status.Recoveries[i].Name
		displaced.ID = status.Recoveries[i].ID
		status.Recoveries[i].PromotedAt = nil
	}
	status.Retained = append(status.Retained, displaced)

	now := metav1.NewTime(time.Now())
	recovery.PromotedAt = &now
	r.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventClaimBound,
		Project: claim.Spec.ProjectRef.Name,
		Claim:   claim.Name,
		Message: fmt.Sprintf("claim %s promoted recovery %s: it now binds the database as it was at %s, "+
			"and what it displaced is retained", claim.Name, promoted,
			recovery.At.Time.UTC().Format(time.RFC3339)),
	})
	return nil
}

// recoveryUnavailable records a claim that offers no recovery, keeping any
// recovery it already has: a provider that stops answering does not make the
// siblings it already made disappear.
func (r *ResourceClaimReconciler) recoveryUnavailable(
	claim *kitchenv1alpha1.ResourceClaim,
	reason string,
) error {
	status := r.recoveryStatus(claim)
	status.Available = false
	status.Reason = reason
	status.Window = nil
	if len(status.Recoveries) == 0 {
		meta.RemoveStatusCondition(&claim.Status.Conditions, condRecoveries)
	}
	return nil
}

// recoveryStatus is the claim's recovery block, created empty the first time
// anything writes to it.
func (r *ResourceClaimReconciler) recoveryStatus(
	claim *kitchenv1alpha1.ResourceClaim,
) *kitchenv1alpha1.ClaimRecoveryStatus {
	if claim.Status.Recovery == nil {
		claim.Status.Recovery = &kitchenv1alpha1.ClaimRecoveryStatus{}
	}
	return claim.Status.Recovery
}

// recoveriesNotReady records why the recoveries are not in step and hands the
// error back for the caller's requeue.
func (r *ResourceClaimReconciler) recoveriesNotReady(
	claim *kitchenv1alpha1.ResourceClaim,
	reason string,
	err error,
) error {
	setClaimCondition(claim, condRecoveries, metav1.ConditionFalse, reason, err.Error())
	return err
}

// finalizeRecoveries discards the recovered siblings and their bindings when
// the claim goes.
//
// It follows the claim's own deletionPolicy rather than having one of its
// own: under Delete the instance is destroyed with everything in it anyway,
// and under Retain the recoveries are left where they are — the same reading
// that leaves the database itself behind. What always goes is the platform's
// own bookkeeping, which is the binding Secrets.
func (r *ResourceClaimReconciler) finalizeRecoveries(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner database.Provisioner,
	appNS string,
) error {
	if claim.Status.Recovery == nil {
		return nil
	}
	recoverable, _ := provisioner.(database.RecoverableProvisioner)
	for _, recovery := range claim.Status.Recovery.Recoveries {
		if claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete &&
			recoverable != nil && recovery.ID != "" {
			if err := recoverable.DeleteBranch(ctx, claim.Status.InstanceID, recovery.ID); err != nil {
				return err
			}
		}
		if recovery.SecretName == "" {
			continue
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: recovery.SecretName, Namespace: appNS}}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

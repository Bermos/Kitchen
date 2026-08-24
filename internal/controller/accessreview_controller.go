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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
)

// The recertification reconciler is the Exception reconciler's shape: it is
// the clock and the consequence, and nothing else.
//
// It stamps a new cycle Open, requeues itself for the exact moment the cycle
// comes due, stamps it Overdue when that passes, and — when the API has
// closed a cycle — carries out the two things a close is *for*: it takes off
// the grants somebody revoked, and it mints the retained artefact. Opening a
// cycle on the cadence is not here, because there is no object to reconcile
// before one exists; that is the sweep in accesssweep.go.
//
// What it deliberately does not do is refuse anything. An overdue cycle is a
// phase and a condition, not a consequence: an access review that stopped
// deployments when it ran late would be switched off within the month, and a
// control nobody leaves on is worth nothing. The consequence of a review is
// that somebody has to look.

// AccessReviewReconciler keeps every cycle honest against the clock, and
// finishes the ones somebody closed.
type AccessReviewReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Audit is waited on and fail-closed: an overdue stamp or a revocation
	// the log refuses to record is one this reconciler does not make. May be
	// nil.
	Audit *audit.Recorder

	// Records is where the closing artefact's envelope is kept. Nil is the
	// real store, resolved off the singleton.
	Records SignedRecordStoreFactory

	// Now is the clock, for tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=accessreviews,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=accessreviews/status,verbs=get;update;patch

func (r *AccessReviewReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// Reconcile drives one cycle: Open on first sight, Overdue when the clock
// says so, and finished once somebody has closed it.
func (r *AccessReviewReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	review := &kitchenv1alpha1.AccessReview{}
	if err := r.Get(ctx, req.NamespacedName, review); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !review.DeletionTimestamp.IsZero() {
		// Nothing to clean up: a cycle creates nothing in the cluster, and
		// the artefact it produced is in the store, deliberately outliving
		// it.
		return ctrl.Result{}, nil
	}

	if review.Status.Phase == kitchenv1alpha1.AccessReviewClosed {
		return r.finish(ctx, review)
	}

	now := r.now()
	if review.EffectivePhase(now) == kitchenv1alpha1.AccessReviewOverdue {
		if review.Status.Phase != kitchenv1alpha1.AccessReviewOverdue {
			return ctrl.Result{}, r.markOverdue(ctx, review)
		}
		return ctrl.Result{}, nil
	}

	if review.Status.Phase != kitchenv1alpha1.AccessReviewOpen {
		review.Status.Phase = kitchenv1alpha1.AccessReviewOpen
		if review.Status.OpenedAt == nil {
			review.Status.OpenedAt = &metav1.Time{Time: now}
		}
		r.setReady(review, metav1.ConditionTrue, "Open", fmt.Sprintf(
			"%d grant(s) to review by %s", len(review.Status.Entries),
			review.Spec.DueBy.UTC().Format(time.RFC3339)))
		if err := r.Status().Update(ctx, review); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Wake at the due moment: the Exception reconciler's RequeueAfter, with
	// its cushion, for its reason.
	return ctrl.Result{RequeueAfter: review.Spec.DueBy.Sub(now) + time.Second}, nil
}

// markOverdue stamps a cycle whose deadline has passed, audit record first.
//
// The record is privileged and classified `access`: "the platform's access
// went unreviewed past its own deadline" is a fact about the control
// environment, and it is exactly the fact an examiner asks for evidence of.
func (r *AccessReviewReconciler) markOverdue(
	ctx context.Context, review *kitchenv1alpha1.AccessReview,
) error {
	if err := r.Audit.Record(ctx, accessReviewOverdueTransition(review)); err != nil {
		return err
	}
	review.Status.Phase = kitchenv1alpha1.AccessReviewOverdue
	r.setReady(review, metav1.ConditionFalse, "Overdue", fmt.Sprintf(
		"due %s with %d grant(s) still undecided; the cycle stays open and nothing is refused "+
			"because of it — closing it is what moves this on",
		review.Spec.DueBy.UTC().Format(time.RFC3339), review.Status.Pending))
	if err := r.Status().Update(ctx, review); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("access review overdue", "review", review.Name,
		"scope", review.Spec.Scope, "pending", review.Status.Pending)
	return nil
}

// accessReviewOverdueTransition is the overdue stamp's audit record, built
// apart from the recording so a test can hold it up to the light without a
// store.
func accessReviewOverdueTransition(review *kitchenv1alpha1.AccessReview) audit.Transition {
	return audit.Transition{
		Object:     review,
		Kind:       audit.KindAccessReview,
		Controller: actorAccessReviewController,
		Privileged: audit.PrivilegeAccess,
		From:       string(kitchenv1alpha1.AccessReviewOpen),
		To:         string(kitchenv1alpha1.AccessReviewOverdue),
		Reason: fmt.Sprintf(
			"access review %s passed its due date of %s with %d grant(s) undecided",
			review.Name, review.Spec.DueBy.UTC().Format(time.RFC3339), review.Status.Pending),
		Details: map[string]any{
			"scope":     string(review.Spec.Scope),
			"dueBy":     review.Spec.DueBy.UTC().Format(time.RFC3339),
			"pending":   review.Status.Pending,
			"confirmed": review.Status.Confirmed,
			"revoked":   review.Status.Revoked,
			"reviewers": subjectsOfReviewers(review),
		},
	}
}

// finish carries out what a closed cycle decided: the revocations, then the
// artefact. Both are idempotent, because a reconcile of a closed cycle is
// ordinary — a status write, an operator restart, a resync.
//
// The ordering is deliberate. Revocations first, so that the artefact records
// what actually happened to each grant rather than what was intended; the
// artefact is the evidence and evidence that ran ahead of the act would be
// the wrong way round.
func (r *AccessReviewReconciler) finish(
	ctx context.Context, review *kitchenv1alpha1.AccessReview,
) (ctrl.Result, error) {
	changed, err := r.applyRevocations(ctx, review)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		if err := r.Status().Update(ctx, review); err != nil {
			return ctrl.Result{}, err
		}
	}
	if review.Status.Artifact == nil {
		r.recordArtifact(ctx, review)
		if err := r.Status().Update(ctx, review); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// applyRevocations takes off the grants the cycle decided to revoke. It is
// what makes a recertification a control rather than a survey: a `revoke`
// that left the role in place would be a form somebody filled in.
//
// Three things it will not do, each recorded on the entry rather than
// silently skipped:
//
//   - It never removes the **last** platform operator. A platform with no
//     operators refuses every operator-only route to everybody, `PATCH
//     /settings` — the route that names an operator — included, and there is
//     no way back that does not involve kubectl. That is a lockout, and a
//     compliance control that can lock an institution out of its own platform
//     is one that gets turned off.
//   - It does not revoke a grant that is already gone. Somebody may have
//     removed it during the cycle, which is the outcome the reviewer wanted.
//   - It does not touch a grant whose role has changed since the snapshot.
//     The reviewer decided about `admin on shop`; if that is `viewer` now,
//     the decision was about something else and the next cycle asks again.
func (r *AccessReviewReconciler) applyRevocations(
	ctx context.Context, review *kitchenv1alpha1.AccessReview,
) (bool, error) {
	changed := false
	for i := range review.Status.Entries {
		entry := &review.Status.Entries[i]
		if entry.Decision != kitchenv1alpha1.AccessRevoke || entry.Applied || entry.ApplyMessage != "" {
			continue
		}
		message, err := r.revoke(ctx, review, entry)
		if err != nil {
			return changed, err
		}
		if message == "" {
			entry.Applied = true
		} else {
			entry.ApplyMessage = message
		}
		changed = true
	}
	return changed, nil
}

// revoke removes one grant, audit record first. An empty message means it was
// carried out; anything else is why it was not, and goes on the entry.
func (r *AccessReviewReconciler) revoke(
	ctx context.Context,
	review *kitchenv1alpha1.AccessReview,
	entry *kitchenv1alpha1.AccessReviewEntry,
) (string, error) {
	if entry.Grant == access.PlatformGrant {
		return r.revokeOperator(ctx, review, entry)
	}
	return r.revokeProjectGrant(ctx, review, entry)
}

func (r *AccessReviewReconciler) revokeOperator(
	ctx context.Context,
	review *kitchenv1alpha1.AccessReview,
	entry *kitchenv1alpha1.AccessReviewEntry,
) (string, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, client.ObjectKey{Name: KitchenSingletonName}, kitchen); err != nil {
		return "", err
	}
	remaining := make([]kitchenv1alpha1.AccessSubject, 0, len(kitchen.Spec.Access.Operators))
	found := false
	for _, operator := range kitchen.Spec.Access.Operators {
		if operator.Subject == entry.Subject {
			found = true
			continue
		}
		remaining = append(remaining, operator)
	}
	if !found {
		return "the operator role was already gone by the time the cycle closed", nil
	}
	if len(remaining) == 0 {
		return "not applied: this is the platform's last operator, and a platform with none refuses " +
			"every operator-only route to everybody — including the one that names an operator. " +
			"Name a replacement operator first, then revoke this one", nil
	}

	if err := r.Audit.Record(ctx, accessRevocationTransition(review, entry, kitchen)); err != nil {
		return "", err
	}
	patch := client.MergeFrom(kitchen.DeepCopy())
	kitchen.Spec.Access.Operators = remaining
	if err := r.Patch(ctx, kitchen, patch); err != nil {
		return "", err
	}
	logf.FromContext(ctx).Info("access review revoked the operator role",
		"review", review.Name, "subject", entry.Subject)
	return "", nil
}

func (r *AccessReviewReconciler) revokeProjectGrant(
	ctx context.Context,
	review *kitchenv1alpha1.AccessReview,
	entry *kitchenv1alpha1.AccessReviewEntry,
) (string, error) {
	project := &kitchenv1alpha1.Project{}
	key := client.ObjectKey{Namespace: review.Namespace, Name: entry.Grant}
	if err := r.Get(ctx, key, project); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("project %s no longer exists, so the grant went with it", entry.Grant), nil
		}
		return "", err
	}
	remaining := make([]kitchenv1alpha1.AccessGrant, 0, len(project.Spec.Access))
	found := false
	for _, grant := range project.Spec.Access {
		if grant.Subject == entry.Subject && string(grant.Role) == entry.Role {
			found = true
			continue
		}
		if grant.Subject == entry.Subject {
			// Same account, different role: the decision was about a role
			// this account no longer holds, so it is not this cycle's to act
			// on. Kept, and said so below.
			remaining = append(remaining, grant)
			continue
		}
		remaining = append(remaining, grant)
	}
	if !found {
		return fmt.Sprintf(
			"the %s grant on %s was already gone, or had moved to another role, when the cycle closed",
			entry.Role, entry.Grant), nil
	}

	if err := r.Audit.Record(ctx, accessRevocationTransition(review, entry, project)); err != nil {
		return "", err
	}
	patch := client.MergeFrom(project.DeepCopy())
	project.Spec.Access = remaining
	if err := r.Patch(ctx, project, patch); err != nil {
		return "", err
	}
	logf.FromContext(ctx).Info("access review revoked a project grant",
		"review", review.Name, "project", entry.Grant, "subject", entry.Subject, "role", entry.Role)
	return "", nil
}

// accessRevocationTransition is one revocation's audit record. It names the
// cycle it came out of in the correlation, so the whole review reads as one
// cause — which is the question "why did Anna lose admin on shop in March"
// being answerable in one query.
func accessRevocationTransition(
	review *kitchenv1alpha1.AccessReview,
	entry *kitchenv1alpha1.AccessReviewEntry,
	object client.Object,
) audit.Transition {
	kind := audit.KindProject
	project := entry.Grant
	if entry.Grant == access.PlatformGrant {
		kind, project = audit.KindKitchen, ""
	}
	details := map[string]any{
		"review":     review.Name,
		"grant":      entry.Grant,
		"role":       entry.Role,
		"subject":    entry.Subject,
		"decidedBy":  entry.DecidedBy,
		"selfReview": entry.SelfReview,
	}
	if entry.Email != "" {
		details["email"] = entry.Email
	}
	if entry.Note != "" {
		details["note"] = entry.Note
	}
	return audit.Transition{
		Object:      object,
		Kind:        kind,
		Controller:  actorAccessReviewController,
		Privileged:  audit.PrivilegeAccess,
		Correlation: review.Name,
		From:        entry.Role,
		Project:     project,
		Reason: fmt.Sprintf(
			"access review %s revoked %s on %s from %s, decided by %s",
			review.Name, entry.Role, entry.Grant, describeSubject(entry), entry.DecidedBy),
		Details: details,
	}
}

// describeSubject words an entry's account for a record: the address where
// there is one, the subject otherwise. A log of opaque strings is a log
// nobody reads.
func describeSubject(entry *kitchenv1alpha1.AccessReviewEntry) string {
	if entry.Email != "" {
		return entry.Email
	}
	return entry.Subject
}

func subjectsOfReviewers(review *kitchenv1alpha1.AccessReview) []string {
	names := make([]string, 0, len(review.Spec.Reviewers))
	for _, reviewer := range review.Spec.Reviewers {
		names = append(names, reviewer.Subject)
	}
	return names
}

func (r *AccessReviewReconciler) setReady(
	review *kitchenv1alpha1.AccessReview, status metav1.ConditionStatus, reason, message string,
) {
	meta.SetStatusCondition(&review.Status.Conditions, metav1.Condition{
		Type: condReady, Status: status, Reason: reason, Message: message,
		ObservedGeneration: review.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *AccessReviewReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.AccessReview{}).
		Named("accessreview").
		Complete(r)
}

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
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/policy"
)

// The exception reconciler is deliberately lean: it is the expiry machinery
// and nothing more. It stamps a new Exception Active, requeues itself for the
// exact moment the grant runs out, and flips Active to Expired with an audit
// record when it does — no ticker, no sweep, one cheap RequeueAfter per
// object.
//
// What it deliberately does NOT do is re-evaluate policy. Expiry making the
// deployed release non-compliant is the rescan controller's job (#134): its
// sweep re-evaluates every (release, environment) pair through
// internal/policy, and an exception that has expired simply stops waiving —
// Exception.EffectivePhase and policy.ApplyExceptions both judge against the
// clock, so the sweep needs no signal from here. The seam #134 plugs into is
// ActiveExceptionsFor below: the one listing every evaluation's input is
// materialized through.
//
// The "further promotions block" consequence needs no machinery at all: an
// expired exception is excluded from every listing, the rules fire unwaived,
// and the verdict is Blocked. spec.autoRollback is carried on the object and
// deliberately not acted on here — rolling a workload back is the rescan
// controller's decision to make with the re-evaluation in hand, and it is off
// by default because the default consequence of expiry is that nothing new
// goes out, not that something running is yanked.

// ExceptionReconciler keeps every Exception's phase honest against the clock.
type ExceptionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Audit is waited on and fail-closed: an expiry the log refuses to
	// record is one this reconciler does not stamp. May be nil.
	Audit *audit.Recorder
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=exceptions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=exceptions/status,verbs=get;update;patch

// Reconcile drives one Exception: Active on first sight, Expired when the
// clock says so, and left alone once Resolved.
func (r *ExceptionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	exception := &kitchenv1alpha1.Exception{}
	if err := r.Get(ctx, req.NamespacedName, exception); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !exception.DeletionTimestamp.IsZero() {
		// Nothing to clean up: an exception creates nothing. Retention is the
		// default and project deletion is the garbage collector.
		return ctrl.Result{}, nil
	}

	switch exception.Status.Phase {
	case kitchenv1alpha1.ExceptionResolved, kitchenv1alpha1.ExceptionExpired:
		// Terminal. A resolved exception stays resolved past its expiry —
		// somebody already ended it — and an expired one has been recorded.
		return ctrl.Result{}, nil
	}

	now := time.Now()
	if exception.EffectivePhase(now) == kitchenv1alpha1.ExceptionExpired {
		return r.expire(ctx, exception)
	}

	if exception.Status.Phase != kitchenv1alpha1.ExceptionActive {
		exception.Status.Phase = kitchenv1alpha1.ExceptionActive
		r.setReady(exception, metav1.ConditionTrue, "Active",
			fmt.Sprintf("waives %v for %s until %s",
				exception.Spec.RuleIDs, exception.Spec.EnvironmentRef.Name,
				exception.Spec.ExpiresAt.UTC().Format(time.RFC3339)))
		if err := r.Status().Update(ctx, exception); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Wake at the expiry moment: the cheap alternative to a ticker. A small
	// cushion keeps a clock rounding from re-entering a hair early forever.
	return ctrl.Result{RequeueAfter: exception.Spec.ExpiresAt.Sub(now) + time.Second}, nil
}

// expire flips an exception to Expired, audit record first. The record is
// privileged: a waiver ending is a fact about the control environment, the
// same class as the waiver being granted.
func (r *ExceptionReconciler) expire(
	ctx context.Context, exception *kitchenv1alpha1.Exception,
) (ctrl.Result, error) {
	if err := r.Audit.Record(ctx, exceptionExpiryTransition(exception)); err != nil {
		return ctrl.Result{}, err
	}
	exception.Status.Phase = kitchenv1alpha1.ExceptionExpired
	r.setReady(exception, metav1.ConditionFalse, "Expired",
		fmt.Sprintf("expired %s; the rules it waived block again until it is resolved or replaced",
			exception.Spec.ExpiresAt.UTC().Format(time.RFC3339)))
	if err := r.Status().Update(ctx, exception); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("exception expired", "exception", exception.Name,
		"project", exception.Spec.ProjectRef.Name, "environment", exception.Spec.EnvironmentRef.Name,
		"rules", exception.Spec.RuleIDs, "autoRollback", exception.Spec.AutoRollback)
	return ctrl.Result{}, nil
}

// exceptionExpiryTransition is the expiry's audit record, built apart from
// the recording so a test can hold it up to the light without a store.
func exceptionExpiryTransition(exception *kitchenv1alpha1.Exception) audit.Transition {
	return audit.Transition{
		Object:     exception,
		Kind:       audit.KindException,
		Controller: actorExceptionController,
		From:       string(kitchenv1alpha1.ExceptionActive),
		To:         string(kitchenv1alpha1.ExceptionExpired),
		Project:    exception.Spec.ProjectRef.Name,
		Reason: fmt.Sprintf(
			"exception %s expired without being resolved: %v block again for %s",
			exception.Name, exception.Spec.RuleIDs, exception.Spec.EnvironmentRef.Name),
		Details: map[string]any{
			"privileged":   true,
			"environment":  exception.Spec.EnvironmentRef.Name,
			"ruleIDs":      exception.Spec.RuleIDs,
			"expiresAt":    exception.Spec.ExpiresAt.UTC().Format(time.RFC3339),
			"usedBy":       exception.Status.UsedBy,
			"autoRollback": exception.Spec.AutoRollback,
		},
	}
}

func (r *ExceptionReconciler) setReady(
	exception *kitchenv1alpha1.Exception, status metav1.ConditionStatus, reason, message string,
) {
	meta.SetStatusCondition(&exception.Status.Conditions, metav1.Condition{
		Type: condReady, Status: status, Reason: reason, Message: message,
		ObservedGeneration: exception.Generation,
	})
}

// ActiveExceptionsFor lists the break-glass grants in scope for one
// (project, environment, release) triple, as the policy engine consumes
// them. It is the one implementation of "which exceptions apply": the
// promotion reconciler's seam, the build reconciler's pull-request
// break-glass and the rescan sweep (#134) all judge scope and expiry here,
// so an exception cannot waive in one place and not another.
//
// Expiry and resolution are judged against `at` through EffectivePhase, so a
// stamped-late status row cannot keep a dead grant waiving.
func ActiveExceptionsFor(
	ctx context.Context, c client.Client, namespace string,
	project, environment, release string, at time.Time,
) ([]kitchenv1alpha1.Exception, error) {
	list := &kitchenv1alpha1.ExceptionList{}
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	active := []kitchenv1alpha1.Exception{}
	for i := range list.Items {
		exception := &list.Items[i]
		if !exception.Covers(project, environment, release) {
			continue
		}
		if exception.EffectivePhase(at) != kitchenv1alpha1.ExceptionActive {
			continue
		}
		active = append(active, *exception)
	}
	return active, nil
}

// PolicyExceptions materializes exceptions into the engine's input shape.
func PolicyExceptions(exceptions []kitchenv1alpha1.Exception) []policy.Exception {
	out := make([]policy.Exception, 0, len(exceptions))
	for i := range exceptions {
		exception := &exceptions[i]
		out = append(out, policy.Exception{
			Name:       exception.Name,
			RuleIDs:    exception.Spec.RuleIDs,
			ExpiresAt:  exception.Spec.ExpiresAt.Time,
			ApprovedBy: exception.Spec.ApprovedBy,
			Reason:     exception.Spec.Reason,
		})
	}
	return out
}

// appendUsedBy records that a promotion relied on an exception, idempotently.
// The status list is the register's answer to "what went out under this
// grant", so it is appended before the promotion applies and never rewritten.
func appendUsedBy(
	ctx context.Context, c client.Client, exception *kitchenv1alpha1.Exception, promotion string,
) error {
	for _, name := range exception.Status.UsedBy {
		if name == promotion {
			return nil
		}
	}
	exception.Status.UsedBy = append(exception.Status.UsedBy, promotion)
	return c.Status().Update(ctx, exception)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ExceptionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Exception{}).
		Named("exception").
		Complete(r)
}

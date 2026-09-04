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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The saved-query alert: the second trigger onto the notification path.
//
// A SavedQuery is a question about the logs with a name on it, and until this
// existed it was only ever asked by a person opening it. An alert makes it a
// standing question — counted over a window, on a schedule, compared to a
// threshold — and the crossing is announced.
//
// **It adds nothing to the delivery path.** Crossing the threshold records an
// `alert.firing` activity event, exactly the way the Build controller records
// a failure, and internal/notify does the rest. That is the whole argument for
// one delivery path rather than two: the two triggers differ in what fires
// them and in nothing else.
//
// # Edge, not level
//
// The notification is sent on the transition into firing. A threshold that
// stays crossed all afternoon is one message; the count that keeps it crossed
// is on the object, in status.lastCount, for whoever goes to look. An alert
// that repeated every interval would be an alert somebody silences, which is
// the failure mode this is arranged to avoid.

// alertEvaluationStep is how often a saved query carrying an alert is looked
// at. It is not the evaluation interval — that is the alert's, and this is
// only how often the reconciler asks whether one is due, so that an operator
// that has just restarted does not wait out a whole interval first.
const alertEvaluationStep = time.Minute

// SavedQueryReconciler evaluates the alerts on saved queries.
//
// A saved query with no alert is reconciled to nothing at all, which is the
// ordinary case: the type's whole effect is still that it exists.
type SavedQueryReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Activity is where a crossing is recorded, and so — through the
	// recorder's sink — what notifies anybody. May be nil, which evaluates
	// the alerts and tells nobody, and is what an installation with no
	// notifications configured effectively has.
	Activity *activity.Recorder

	// Counter counts the lines a selection matches. Nil resolves the
	// platform's telemetry store the way every other reader here does; tests
	// answer without a ClickHouse to ask.
	Counter LogCounter

	// Now is the clock, for tests.
	Now func() time.Time
}

// LogCounter is the slice of the telemetry store an alert needs: one count of
// one selection. It is an interface so that a test can make an alert fire
// without a store to put lines in.
type LogCounter interface {
	CountLogs(ctx context.Context, selection clickhouse.LogSelection) (uint64, error)
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=savedqueries,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=savedqueries/status,verbs=get;update;patch

func (r *SavedQueryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	saved := &kitchenv1alpha1.SavedQuery{}
	if err := r.Get(ctx, req.NamespacedName, saved); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	alert := saved.Spec.Alert
	if alert == nil || alert.Suspended {
		return ctrl.Result{}, nil
	}

	now := r.now()
	interval := time.Duration(alert.Interval()) * time.Minute
	if last := saved.Status.LastEvaluationTime; last != nil {
		if due := last.Time.Add(interval); due.After(now) {
			return ctrl.Result{RequeueAfter: due.Sub(now)}, nil
		}
	}

	counter, err := r.counter(ctx)
	if err != nil {
		// No store, or one that cannot be reached. The alert says so rather
		// than staying silently at "never fired", which is what an alert that
		// has been broken for a week looks like otherwise.
		return r.publish(ctx, saved, saved.Status.LastCount, saved.Status.Firing, now,
			"not evaluated: "+err.Error())
	}

	window := time.Duration(alert.WindowMinutes) * time.Minute
	count, err := counter.CountLogs(ctx, clickhouse.LogSelection{
		Query: saved.Spec.Query,
		Where: saved.Spec.Where,
		Since: now.Add(-window),
		Until: now,
	})
	if err != nil {
		return r.publish(ctx, saved, saved.Status.LastCount, saved.Status.Firing, now,
			"not evaluated: "+err.Error())
	}

	firing := alert.Fires(int64(count))
	message := fmt.Sprintf("%d line(s) in the last %d minute(s); the alert fires %s %d",
		count, alert.WindowMinutes, comparisonWord(alert.Comparison), alert.Threshold)

	if firing && !saved.Status.Firing {
		// The edge. Recorded rather than sent: the feed is the one account of
		// what the platform did, and the subscriptions read it.
		r.Activity.Record(ctx, clickhouse.Event{
			Type:    clickhouse.EventAlertFiring,
			Message: fmt.Sprintf("alert %q fired: %s", saved.Spec.Title, message),
			Value:   float64(count),
		})
		logf.FromContext(ctx).Info("saved-query alert fired",
			"query", saved.Name, "count", count, "threshold", alert.Threshold)
	}
	return r.publish(ctx, saved, int64(count), firing, now, message)
}

// publish writes the evaluation down and comes back when the next one is due.
func (r *SavedQueryReconciler) publish(
	ctx context.Context,
	saved *kitchenv1alpha1.SavedQuery,
	count int64,
	firing bool,
	now time.Time,
	message string,
) (ctrl.Result, error) {
	saved.Status.LastEvaluationTime = &metav1.Time{Time: now}
	saved.Status.LastCount = count
	saved.Status.Message = message
	switch {
	case firing && !saved.Status.Firing:
		saved.Status.FiringSince = &metav1.Time{Time: now}
	case !firing:
		saved.Status.FiringSince = nil
	}
	saved.Status.Firing = firing

	if err := r.Status().Update(ctx, saved); err != nil {
		return ctrl.Result{}, err
	}
	interval := time.Duration(saved.Spec.Alert.Interval()) * time.Minute
	if interval < alertEvaluationStep {
		interval = alertEvaluationStep
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// counter resolves the telemetry store, or says why there is none in words an
// operator can act on.
func (r *SavedQueryReconciler) counter(ctx context.Context) (LogCounter, error) {
	if r.Counter != nil {
		return r.Counter, nil
	}
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return nil, err
	}
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		return nil, fmt.Errorf("this installation has no telemetry store, so there are no logs to " +
			"count. Set spec.observability.clickhouse.secretRef")
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	cfg, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		return nil, err
	}
	return clickhouse.New(cfg), nil
}

func (r *SavedQueryReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *SavedQueryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.SavedQuery{}).
		Named("savedquery").
		Complete(r)
}

// comparisonWord renders the comparison the way the message reads it.
func comparisonWord(comparison string) string {
	if comparison == kitchenv1alpha1.AlertComparisonBelow {
		return kitchenv1alpha1.AlertComparisonBelow
	}
	return kitchenv1alpha1.AlertComparisonAbove
}

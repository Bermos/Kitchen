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
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/notify"
)

// Delivering a notification, which is the half of issue #77 that talks to
// somebody else's HTTP server.
//
// It is a reconciler over NotificationDelivery objects, and every property the
// issue asked for falls out of that being where it lives:
//
//   - **At-least-once.** The delivery is in etcd before the first attempt, so
//     a restart resumes the ladder rather than losing what was in flight. An
//     attempt whose response never arrives is retried, which is what
//     at-least-once means and why the payload carries an idempotency key.
//   - **Bounded.** spec.maxAttempts on the subscription, and the backoff in
//     notify.Backoff. The attempt that exhausts the ladder dead-letters.
//   - **A dead letter somebody can see.** It is the delivery object, in a
//     phase that says so, holding the exact payload and every attempt made.
//   - **Never affects what it reports on.** Nothing here runs on the path of
//     the build, release or environment the event is about. The only write
//     that path makes is the delivery object itself, off its own goroutine
//     (internal/notify), and this reconciler's failures are recorded on the
//     delivery and nowhere else.
//
// # Why the request is not made from a worker pool
//
// A pool would need its own durability, its own leader election and its own
// backoff, all of which the reconciler already has from controller-runtime.
// The requeue *is* the backoff: status.nextAttemptTime is written and the
// object is requeued for it, so the wait is in the queue rather than in a
// goroutine holding a timer.

const (
	// deliveryFinishedTTL is how long a delivered notification is kept. It is
	// short: the record of what happened is the activity feed, and a
	// successful delivery is only interesting for as long as somebody might
	// be watching it happen.
	deliveryFinishedTTL = time.Hour

	// deadLetterTTL is how long a dead letter is kept, which is much longer
	// for the obvious reason: it is the thing somebody has to find, and they
	// find it on Monday.
	deadLetterTTL = 7 * 24 * time.Hour

	// maxDeliveryAttemptRecords bounds status.attempted. The ladder is
	// bounded at ten, so this is the whole of it in practice; it exists so
	// that a hand-edited maxAttempts cannot grow a status without limit.
	maxDeliveryAttemptRecords = 10

	// maxReceiverErrorLength bounds what is kept from a failed attempt. The
	// receiver's *body* is never kept — it is somebody else's data and may be
	// anything at all — so this bounds a transport error message.
	maxReceiverErrorLength = 300

	condSubscriptionReady = "Ready"
)

// NotificationDeliveryReconciler delivers one notification to one receiver.
type NotificationDeliveryReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// HTTPClient makes the request. Nil is a client with no timeout of its
	// own — each attempt is bounded by the subscription's timeout, applied
	// through the request's context — and tests point it at a receiver they
	// control.
	//
	// It deliberately does not follow redirects: a subscription's URL is
	// where the operator agreed to send the project's activity, and a
	// receiver that 302s it somewhere else has changed that agreement
	// without anybody's say-so.
	HTTPClient *http.Client

	// Now is the clock. Nil is time.Now; tests move it to make a backoff
	// elapse without waiting for one.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=notificationdeliveries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=notificationdeliveries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=notificationsubscriptions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=notificationsubscriptions/status,verbs=get;update;patch

func (r *NotificationDeliveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	delivery := &kitchenv1alpha1.NotificationDelivery{}
	if err := r.Get(ctx, req.NamespacedName, delivery); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !delivery.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	now := r.now()
	if delivery.Done() {
		return r.prune(ctx, delivery, now)
	}

	// A backoff that has not elapsed. The requeue *is* the wait: nothing here
	// sleeps, so a restart in the middle of a ladder picks it up where it was.
	if next := delivery.Status.NextAttemptTime; next != nil && next.Time.After(now) {
		return ctrl.Result{RequeueAfter: next.Time.Sub(now)}, nil
	}

	subscription := &kitchenv1alpha1.NotificationSubscription{}
	key := types.NamespacedName{Namespace: delivery.Namespace, Name: delivery.Spec.SubscriptionRef.Name}
	if err := r.Get(ctx, key, subscription); err != nil {
		if apierrors.IsNotFound(err) {
			// The subscription is gone. Garbage collection is already coming
			// for this object (it is an owner reference), so there is nothing
			// to do and nothing to say.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if subscription.Spec.Suspended {
		// Suspended holds what is already queued rather than dropping it: the
		// switch is for a receiver being repaired, and the deploy somebody
		// silenced it during is still worth hearing about afterwards.
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	secret, err := r.signingKey(ctx, subscription)
	if err != nil {
		// A subscription whose key cannot be read is a configuration fault,
		// not a receiver fault, so it does not consume the ladder: the
		// delivery waits, and the subscription says why.
		r.markSubscription(ctx, subscription, false, "SecretUnreadable", err.Error())
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	attempt := delivery.Status.Attempts + 1
	result := r.post(ctx, subscription, delivery, secret, attempt, now)
	log.V(1).Info("notification attempted",
		"delivery", delivery.Name, "subscription", subscription.Name,
		"event", delivery.Spec.Event, "attempt", attempt,
		"status", result.StatusCode, "error", result.Error)

	return r.record(ctx, delivery, subscription, result, now)
}

// attemptResult is one attempt, before it is written down.
type attemptResult = kitchenv1alpha1.NotificationDeliveryAttempt

// post makes one request. It never returns an error: every outcome is an
// attempt, and an attempt is a status field rather than a reconcile failure.
func (r *NotificationDeliveryReconciler) post(
	ctx context.Context,
	subscription *kitchenv1alpha1.NotificationSubscription,
	delivery *kitchenv1alpha1.NotificationDelivery,
	secret []byte,
	attempt int32,
	now time.Time,
) attemptResult {
	made := attemptResult{Number: attempt, Time: metav1.NewTime(now)}
	body := []byte(delivery.Spec.Payload)

	timeout := time.Duration(subscription.Spec.Timeout()) * time.Second
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, subscription.Spec.URL, bytes.NewReader(body))
	if err != nil {
		made.Error = truncateError(err.Error())
		return made
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "Kitchen")
	request.Header.Set(notify.HeaderEvent, string(delivery.Spec.Event))
	request.Header.Set(notify.HeaderEventID, delivery.Spec.EventID)
	request.Header.Set(notify.HeaderDelivery, delivery.Name)
	request.Header.Set(notify.HeaderAttempt, strconv.Itoa(int(attempt)))
	request.Header.Set(notify.HeaderTimestamp, strconv.FormatInt(now.UTC().Unix(), 10))
	request.Header.Set(notify.HeaderSignature, notify.Sign(secret, now, body))

	started := time.Now()
	response, err := r.httpClient().Do(request)
	made.DurationMillis = time.Since(started).Milliseconds()
	if err != nil {
		made.Error = truncateError(err.Error())
		return made
	}
	defer func() {
		// Drained and closed so the connection can be reused, and bounded so
		// a receiver cannot answer with a gigabyte.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
	}()

	made.StatusCode = int32(response.StatusCode)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		// The status line, never the body: a receiver's body is somebody
		// else's data, and this string is shown in the dashboard.
		made.Error = "receiver answered " + response.Status
	}
	return made
}

// record writes the attempt down and decides what happens next.
func (r *NotificationDeliveryReconciler) record(
	ctx context.Context,
	delivery *kitchenv1alpha1.NotificationDelivery,
	subscription *kitchenv1alpha1.NotificationSubscription,
	made attemptResult,
	now time.Time,
) (ctrl.Result, error) {
	delivered := made.Error == ""

	delivery.Status.Attempts = made.Number
	delivery.Status.Attempted = append(delivery.Status.Attempted, made)
	if len(delivery.Status.Attempted) > maxDeliveryAttemptRecords {
		delivery.Status.Attempted = delivery.Status.Attempted[len(delivery.Status.Attempted)-maxDeliveryAttemptRecords:]
	}
	delivery.Status.LastError = made.Error
	delivery.Status.LastStatusCode = made.StatusCode

	var requeue ctrl.Result
	switch {
	case delivered:
		delivery.Status.Phase = kitchenv1alpha1.DeliveryDelivered
		delivery.Status.NextAttemptTime = nil
		delivery.Status.CompletedTime = &metav1.Time{Time: now}
		requeue = ctrl.Result{RequeueAfter: deliveryFinishedTTL}
	case made.Number >= subscription.Spec.Attempts():
		delivery.Status.Phase = kitchenv1alpha1.DeliveryDeadLettered
		delivery.Status.NextAttemptTime = nil
		delivery.Status.CompletedTime = &metav1.Time{Time: now}
		requeue = ctrl.Result{RequeueAfter: deadLetterTTL}
	default:
		wait := notify.Backoff(made.Number)
		delivery.Status.Phase = kitchenv1alpha1.DeliveryPending
		delivery.Status.NextAttemptTime = &metav1.Time{Time: now.Add(wait)}
		requeue = ctrl.Result{RequeueAfter: wait}
	}

	if err := r.Status().Update(ctx, delivery); err != nil {
		return ctrl.Result{}, err
	}
	r.countOnSubscription(ctx, subscription, delivery, made, now)
	return requeue, nil
}

// countOnSubscription keeps the subscription's counters and its last outcome.
//
// A conflict here is ignored on purpose: two deliveries finishing at once both
// write this, and the counter is an account of what happened rather than
// something anything depends on. Failing the reconcile — and so retrying a
// *delivery* — to keep a counter exact would be the tail wagging the dog.
func (r *NotificationDeliveryReconciler) countOnSubscription(
	ctx context.Context,
	subscription *kitchenv1alpha1.NotificationSubscription,
	delivery *kitchenv1alpha1.NotificationDelivery,
	made attemptResult,
	now time.Time,
) {
	status := &subscription.Status
	if made.Error == "" {
		status.Delivered++
		status.LastResult = "delivered"
	} else {
		status.Failed++
		status.LastResult = "failed"
	}
	if delivery.Status.Phase == kitchenv1alpha1.DeliveryDeadLettered {
		status.DeadLettered++
	}
	status.LastDeliveryTime = &metav1.Time{Time: now}
	status.LastStatusCode = made.StatusCode
	status.LastError = made.Error

	if err := r.Status().Update(ctx, subscription); err != nil {
		logf.FromContext(ctx).V(1).Info("notification counters not updated",
			"subscription", subscription.Name, "reason", err.Error())
	}
}

// prune deletes a finished delivery once its retention has passed.
func (r *NotificationDeliveryReconciler) prune(
	ctx context.Context,
	delivery *kitchenv1alpha1.NotificationDelivery,
	now time.Time,
) (ctrl.Result, error) {
	ttl := deliveryFinishedTTL
	if delivery.Status.Phase == kitchenv1alpha1.DeliveryDeadLettered {
		ttl = deadLetterTTL
	}
	completed := delivery.Status.CompletedTime
	if completed == nil {
		// A delivery finished by an older version, or by a hand edit. Age it
		// from its creation rather than keeping it for ever.
		completed = &metav1.Time{Time: delivery.CreationTimestamp.Time}
	}
	if due := completed.Time.Add(ttl); due.After(now) {
		return ctrl.Result{RequeueAfter: due.Sub(now)}, nil
	}
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, delivery))
}

// signingKey reads the subscription's secret. The value never leaves this
// process: it is used to compute a signature and is not written anywhere, not
// even into a log line about a failure.
func (r *NotificationDeliveryReconciler) signingKey(
	ctx context.Context,
	subscription *kitchenv1alpha1.NotificationSubscription,
) ([]byte, error) {
	name := subscription.Spec.SecretRef.Name
	if name == "" {
		return nil, fmt.Errorf("spec.secretRef.name is empty: nothing can sign this subscription's payloads")
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: subscription.Namespace, Name: name}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("the signing secret %q is not there; rotate it with "+
				"PATCH /api/v1/notifications/subscriptions/%s", name, subscription.Name)
		}
		return nil, err
	}
	value := secret.Data[notify.SecretKey]
	if len(value) == 0 {
		return nil, fmt.Errorf("the signing secret %q has no %q key", name, notify.SecretKey)
	}
	return value, nil
}

func (r *NotificationDeliveryReconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return defaultNotificationClient
}

// defaultNotificationClient is the operator's own. Redirects are refused
// rather than followed: see HTTPClient.
var defaultNotificationClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (r *NotificationDeliveryReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// markSubscription records why a subscription is not usable, on the
// subscription rather than on the delivery: the fault is the configuration's,
// and every delivery would otherwise carry the same sentence.
func (r *NotificationDeliveryReconciler) markSubscription(
	ctx context.Context,
	subscription *kitchenv1alpha1.NotificationSubscription,
	ready bool,
	reason, message string,
) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	changed := meta.SetStatusCondition(&subscription.Status.Conditions, metav1.Condition{
		Type:               condSubscriptionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: subscription.Generation,
	})
	if !changed {
		return
	}
	if err := r.Status().Update(ctx, subscription); err != nil {
		logf.FromContext(ctx).V(1).Info("subscription condition not written",
			"subscription", subscription.Name, "reason", err.Error())
	}
}

func (r *NotificationDeliveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.NotificationDelivery{}).
		Named("notificationdelivery").
		Complete(r)
}

// truncateError bounds a message that came from somewhere else.
func truncateError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= maxReceiverErrorLength {
		return message
	}
	return message[:maxReceiverErrorLength] + "…"
}

// NotificationSubscriptionReconciler is the other half, and it is small: it
// says whether a subscription is usable, and it bounds how much history one
// subscription may hold.
//
// It exists because the house rule is that a write surface waits for its
// reconciler, and because the two things it does are the two things a person
// cannot see for themselves — whether the signing key the API wrote is still
// there, and whether the deliveries are being cleared up.
type NotificationSubscriptionReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Now is the clock, for tests.
	Now func() time.Time
}

// maxDeliveriesPerSubscription bounds the history one subscription keeps.
// Past it the oldest finished deliveries are deleted early, whatever their
// retention: a receiver that has been down for a week must not be able to fill
// etcd with its own dead letters.
const maxDeliveriesPerSubscription = 200

func (r *NotificationSubscriptionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	subscription := &kitchenv1alpha1.NotificationSubscription{}
	if err := r.Get(ctx, req.NamespacedName, subscription); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !subscription.DeletionTimestamp.IsZero() {
		// Nothing to finalize: the signing secret and every delivery are
		// owned by this object, so Kubernetes collects them.
		return ctrl.Result{}, nil
	}

	reason, message := r.usable(ctx, subscription)
	ready := reason == ""
	if ready {
		reason, message = "Subscribed", fmt.Sprintf("%d event(s) are posted to %s",
			len(subscription.Spec.Events), subscription.Spec.URL)
	}
	if meta.SetStatusCondition(&subscription.Status.Conditions, metav1.Condition{
		Type:               condSubscriptionReady,
		Status:             conditionStatus(ready),
		Reason:             reason,
		Message:            message,
		ObservedGeneration: subscription.Generation,
	}) {
		if err := r.Status().Update(ctx, subscription); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.trim(ctx, subscription); err != nil {
		return ctrl.Result{}, err
	}
	// Slowly: nothing about a subscription changes on its own except the
	// secret being deleted out from under it, and the trim is a bound rather
	// than a schedule.
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

// usable reports why this subscription cannot deliver, empty when it can.
func (r *NotificationSubscriptionReconciler) usable(
	ctx context.Context,
	subscription *kitchenv1alpha1.NotificationSubscription,
) (string, string) {
	if subscription.Spec.Suspended {
		return "Suspended", "delivery is paused; nothing new is queued and what is queued waits"
	}
	parsed, err := url.Parse(subscription.Spec.URL)
	switch {
	case err != nil:
		return "URLUnreadable", fmt.Sprintf("spec.url is not a URL: %s", err.Error())
	case parsed.Scheme != "https":
		return "URLNotHTTPS", "spec.url must be https: a signed payload over plain HTTP is one " +
			"anybody on the path can read"
	case parsed.Host == "":
		return "URLNotAbsolute", "spec.url must be absolute, with a host"
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: subscription.Namespace, Name: subscription.Spec.SecretRef.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "SecretMissing", fmt.Sprintf("the signing secret %q is not there; rotate it with "+
				"PATCH /api/v1/notifications/subscriptions/%s", subscription.Spec.SecretRef.Name, subscription.Name)
		}
		return "SecretUnreadable", err.Error()
	}
	if len(secret.Data[notify.SecretKey]) == 0 {
		return "SecretEmpty", fmt.Sprintf("the signing secret %q has no %q key",
			subscription.Spec.SecretRef.Name, notify.SecretKey)
	}
	return "", ""
}

// trim keeps the delivery history bounded, oldest finished first. Anything
// still pending is left alone whatever the count: dropping a delivery that has
// not been attempted would turn at-least-once into most-of-the-time.
func (r *NotificationSubscriptionReconciler) trim(
	ctx context.Context,
	subscription *kitchenv1alpha1.NotificationSubscription,
) error {
	deliveries := &kitchenv1alpha1.NotificationDeliveryList{}
	if err := r.List(ctx, deliveries,
		client.InNamespace(subscription.Namespace),
		client.MatchingLabels{notify.SubscriptionLabel: subscription.Name}); err != nil {
		return err
	}
	if len(deliveries.Items) <= maxDeliveriesPerSubscription {
		return nil
	}

	finished := make([]*kitchenv1alpha1.NotificationDelivery, 0, len(deliveries.Items))
	for i := range deliveries.Items {
		if deliveries.Items[i].Done() {
			finished = append(finished, &deliveries.Items[i])
		}
	}
	sort.Slice(finished, func(i, j int) bool {
		return finished[i].CreationTimestamp.Time.Before(finished[j].CreationTimestamp.Time)
	})

	over := len(deliveries.Items) - maxDeliveriesPerSubscription
	for _, delivery := range finished {
		if over <= 0 {
			break
		}
		if err := r.Delete(ctx, delivery); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		over--
	}
	return nil
}

func (r *NotificationSubscriptionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.NotificationSubscription{}).
		Named("notificationsubscription").
		Complete(r)
}

func conditionStatus(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

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

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/notify"
)

// Outbound notifications: subscriptions, and what became of each delivery.
//
// # The signing key goes in and never comes back
//
// A receiver has to know the key to verify the signature, which makes this
// look like the one credential the platform ought to hand back. It is not, and
// the rule holds without an exception: **the caller supplies the key** on
// create, and rotating it is another write of a value that also never comes
// back. The platform generating one and echoing it once would put a live
// credential in a shell history, a browser's memory and whatever logged the
// response — for the sake of saving the caller a `head -c 32 /dev/urandom`.
//
// # Scope decides the requirement, and the handler decides scope
//
// A subscription naming a project is that project's, and writing one is its
// admin's: it sends the project's activity to an address of somebody's
// choosing. A subscription naming no project is the platform's — every
// project's events — and only an operator may write one. The enforcement table
// resolves the project out of the body and so admits a member who names none;
// this file refuses that, in the same shape the environment requirements write
// does (see policy.go).

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=notificationsubscriptions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=notificationdeliveries,verbs=get;list;watch;update;patch;delete

const (
	// notificationSecretPrefix names the Secret the API writes for a
	// subscription's signing key. It is derived rather than chosen so that
	// nothing has to store the name twice.
	notificationSecretPrefix = "kitchen-notify-"

	// maxSubscriptions bounds the collection, for the reason
	// maxQueuedPerEvent bounds one event's fan-out: it is a bound on a
	// mistake, not on use.
	maxSubscriptions = 50

	// minSigningKeyLength is the shortest key this API will store. A
	// signature is worth exactly what its key is worth, and a person asked
	// for one without a floor types the name of their cat.
	minSigningKeyLength = 16

	// maxDeliveryPage is how many deliveries one list request answers with.
	maxDeliveryPage = 200

	// The two scopes, as the view spells them: a subscription is one
	// project's or the whole platform's, and which it is decides who may
	// write it.
	subscriptionScopePlatform = "platform"
	subscriptionScopeProject  = "project"
)

// subscriptionView is what the API answers with. There is no field for the
// signing key, here or anywhere.
type subscriptionView struct {
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	Events      []string `json:"events"`
	Project     string   `json:"project,omitempty"`
	Scope       string   `json:"scope"`
	Description string   `json:"description,omitempty"`
	Suspended   bool     `json:"suspended"`
	MaxAttempts int32    `json:"maxAttempts"`
	Timeout     int32    `json:"timeoutSeconds"`
	CreatedBy   string   `json:"createdBy,omitempty"`
	CreatedAt   string   `json:"createdAt"`

	// Ready and Reason are the reconciler's verdict: whether this
	// subscription can deliver, and why not when it cannot.
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`

	Delivered      int64  `json:"delivered"`
	Failed         int64  `json:"failed"`
	DeadLettered   int64  `json:"deadLettered"`
	LastResult     string `json:"lastResult,omitempty"`
	LastDeliveryAt string `json:"lastDeliveryAt,omitempty"`
	LastStatusCode int32  `json:"lastStatusCode,omitempty"`
	LastError      string `json:"lastError,omitempty"`
}

func newSubscriptionView(subscription *kitchenv1alpha1.NotificationSubscription) subscriptionView {
	events := make([]string, 0, len(subscription.Spec.Events))
	for _, event := range subscription.Spec.Events {
		events = append(events, string(event))
	}
	scope := subscriptionScopePlatform
	if project := subscription.Spec.Project(); project != "" {
		scope = subscriptionScopeProject
	}
	view := subscriptionView{
		Name:           subscription.Name,
		URL:            subscription.Spec.URL,
		Events:         events,
		Project:        subscription.Spec.Project(),
		Scope:          scope,
		Description:    subscription.Spec.Description,
		Suspended:      subscription.Spec.Suspended,
		MaxAttempts:    subscription.Spec.Attempts(),
		Timeout:        subscription.Spec.Timeout(),
		CreatedBy:      subscription.Spec.CreatedBy,
		CreatedAt:      subscription.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
		Delivered:      subscription.Status.Delivered,
		Failed:         subscription.Status.Failed,
		DeadLettered:   subscription.Status.DeadLettered,
		LastResult:     subscription.Status.LastResult,
		LastStatusCode: subscription.Status.LastStatusCode,
		LastError:      subscription.Status.LastError,
	}
	if at := subscription.Status.LastDeliveryTime; at != nil {
		view.LastDeliveryAt = at.UTC().Format("2006-01-02T15:04:05Z")
	}
	if ready := meta.FindStatusCondition(subscription.Status.Conditions, "Ready"); ready != nil {
		view.Ready = ready.Status == metav1.ConditionTrue
		if !view.Ready {
			view.Reason = ready.Message
		}
	}
	return view
}

// deliveryView is one delivery, including a dead letter. It carries the
// payload because a dead letter that cannot be read is a dead letter nobody
// can act on — and the payload holds nothing secret: it is what the receiver
// would have been sent, and the signature is not part of it.
type deliveryView struct {
	Name         string `json:"name"`
	Subscription string `json:"subscription"`
	Event        string `json:"event"`
	EventID      string `json:"eventId"`
	Project      string `json:"project,omitempty"`
	Phase        string `json:"phase"`
	Attempts     int32  `json:"attempts"`
	QueuedAt     string `json:"queuedAt"`
	CompletedAt  string `json:"completedAt,omitempty"`
	NextAttempt  string `json:"nextAttemptAt,omitempty"`
	LastError    string `json:"lastError,omitempty"`
	LastStatus   int32  `json:"lastStatusCode,omitempty"`
	Payload      string `json:"payload,omitempty"`

	Attempted []deliveryAttemptView `json:"attempted,omitempty"`
}

type deliveryAttemptView struct {
	Number     int32  `json:"number"`
	At         string `json:"at"`
	StatusCode int32  `json:"statusCode,omitempty"`
	Error      string `json:"error,omitempty"`
	Millis     int64  `json:"durationMillis,omitempty"`
}

func newDeliveryView(delivery *kitchenv1alpha1.NotificationDelivery) deliveryView {
	view := deliveryView{
		Name:         delivery.Name,
		Subscription: delivery.Spec.SubscriptionRef.Name,
		Event:        string(delivery.Spec.Event),
		EventID:      delivery.Spec.EventID,
		Project:      delivery.Spec.Project,
		Phase:        string(delivery.Status.Phase),
		Attempts:     delivery.Status.Attempts,
		QueuedAt:     delivery.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
		LastError:    delivery.Status.LastError,
		LastStatus:   delivery.Status.LastStatusCode,
		Payload:      delivery.Spec.Payload,
	}
	if view.Phase == "" {
		view.Phase = string(kitchenv1alpha1.DeliveryPending)
	}
	if at := delivery.Status.CompletedTime; at != nil {
		view.CompletedAt = at.UTC().Format("2006-01-02T15:04:05Z")
	}
	if at := delivery.Status.NextAttemptTime; at != nil {
		view.NextAttempt = at.UTC().Format("2006-01-02T15:04:05Z")
	}
	for _, attempt := range delivery.Status.Attempted {
		view.Attempted = append(view.Attempted, deliveryAttemptView{
			Number:     attempt.Number,
			At:         attempt.Time.UTC().Format("2006-01-02T15:04:05Z"),
			StatusCode: attempt.StatusCode,
			Error:      attempt.Error,
			Millis:     attempt.DurationMillis,
		})
	}
	return view
}

func (s *Server) listSubscriptions(w http.ResponseWriter, req *http.Request) {
	list := &kitchenv1alpha1.NotificationSubscriptionList{}
	if err := s.Client.List(req.Context(), list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	scope := scopeFrom(req.Context())
	filter := projectFilter(req)
	if !s.visibleProject(w, req, filter) {
		return
	}

	views := make([]subscriptionView, 0, len(list.Items))
	for i := range list.Items {
		subscription := &list.Items[i]
		project := subscription.Spec.Project()
		// A platform subscription is the operator's, and a caller who is not
		// one is not told it exists: it names no project, so there is no
		// project of theirs it could be about.
		if project == "" && !scope.all {
			continue
		}
		if project != "" && !scope.allows(project) {
			continue
		}
		if filter != "" && project != filter {
			continue
		}
		views = append(views, newSubscriptionView(subscription))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	writeList(w, views)
}

func (s *Server) getSubscription(w http.ResponseWriter, req *http.Request) {
	subscription := &kitchenv1alpha1.NotificationSubscription{}
	if err := s.get(req.Context(), req.PathValue("name"), subscription); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newSubscriptionView(subscription))
}

// subscriptionRequest is create and patch both. Every field is optional on a
// patch and only what is present is changed, which is why the pointers: an
// absent `suspended` and a `false` one are different requests.
type subscriptionRequest struct {
	Name        string   `json:"name,omitempty"`
	URL         string   `json:"url,omitempty"`
	Events      []string `json:"events,omitempty"`
	Project     string   `json:"project,omitempty"`
	Description *string  `json:"description,omitempty"`
	Suspended   *bool    `json:"suspended,omitempty"`
	MaxAttempts int32    `json:"maxAttempts,omitempty"`
	Timeout     int32    `json:"timeoutSeconds,omitempty"`

	// Secret is the signing key. It goes in and is never answered with —
	// here, or on any other route.
	Secret string `json:"secret,omitempty"`
}

func (s *Server) createSubscription(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	body := subscriptionRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.URL = strings.TrimSpace(body.URL)
	body.Project = strings.TrimSpace(body.Project)
	body.Secret = strings.TrimSpace(body.Secret)

	if body.Project == "" && !platformRoleFrom(ctx).AtLeast(access.PlatformOperator) {
		// The enforcement table resolves this route's project out of the
		// body, so a body naming none reaches the handler whoever sent it.
		// A subscription with no project hears every project's events, which
		// is the platform scope and the operator's alone.
		forbidden(w, "a subscription with no project hears every project's events, "+
			"which needs the operator role; name a project to subscribe to that project's events")
		return
	}
	if err := s.validSubscriptionURL(body.URL); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	events, err := parseNotificationEvents(body.Events)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if len(body.Secret) < minSigningKeyLength {
		badRequest(w, "secret is required and must be at least %d characters: it is the key every "+
			"payload is signed with, and the platform never reads it back to you "+
			"(rotate it with PATCH). Generate one, for example with "+
			"`openssl rand -hex 32`", minSigningKeyLength)
		return
	}
	if body.MaxAttempts < 0 || body.MaxAttempts > 10 {
		badRequest(w, "maxAttempts must be between 1 and 10 (got %d)", body.MaxAttempts)
		return
	}
	if body.Timeout < 0 || body.Timeout > 30 {
		badRequest(w, "timeoutSeconds must be between 1 and 30 (got %d)", body.Timeout)
		return
	}

	if body.Name == "" {
		badRequest(w, "name is required: what this subscription is called")
		return
	}
	if errs := validation.IsDNS1123Label(body.Name); len(errs) > 0 {
		badRequest(w, "name must work as a DNS label — lowercase letters, digits and '-', "+
			"starting and ending alphanumeric (got %q)", body.Name)
		return
	}

	list := &kitchenv1alpha1.NotificationSubscriptionList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	if len(list.Items) >= maxSubscriptions {
		badRequest(w, "the platform already holds %d notification subscriptions; delete one before "+
			"adding another", maxSubscriptions)
		return
	}
	// Checked before the secret is written, so a name collision cannot
	// overwrite the signing key of the subscription already there.
	existing := &kitchenv1alpha1.NotificationSubscription{}
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: body.Name},
		existing); err == nil {
		writeJSON(w, http.StatusConflict, errorBody{
			Error: fmt.Sprintf("a notification subscription called %q already exists", body.Name)})
		return
	} else if !apierrors.IsNotFound(err) {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	subscription := &kitchenv1alpha1.NotificationSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: body.Name, Namespace: s.Namespace},
		Spec: kitchenv1alpha1.NotificationSubscriptionSpec{
			URL:            body.URL,
			Events:         events,
			SecretRef:      kitchenv1alpha1.LocalObjectReference{Name: notificationSecretPrefix + body.Name},
			MaxAttempts:    body.MaxAttempts,
			TimeoutSeconds: body.Timeout,
			CreatedBy:      callerName(caller),
		},
	}
	if body.Project != "" {
		subscription.Spec.ProjectRef = &kitchenv1alpha1.LocalObjectReference{Name: body.Project}
	}
	if body.Description != nil {
		subscription.Spec.Description = strings.TrimSpace(*body.Description)
	}
	if body.Suspended != nil {
		subscription.Spec.Suspended = *body.Suspended
	}

	// Recorded before the credential is written, for the reason a connection
	// is: the secret is the part of this request that matters, and a
	// credential on the cluster no record mentions is the failure to avoid.
	if !s.recorded(w, req, audit.Transition{
		Object:     subscription,
		Kind:       audit.KindNotificationSubscription,
		Operation:  clickhouse.AuditCreate,
		Privileged: audit.PrivilegeCredential,
		Project:    body.Project,
		To:         body.URL,
		Reason: fmt.Sprintf("notification subscription %s created for %s",
			body.Name, subscriptionScopeWords(body.Project)),
		Details: map[string]any{"url": body.URL, "events": body.Events, "scope": body.Project},
	}) {
		return
	}
	if err := s.Client.Create(ctx, subscription); err != nil {
		s.writeError(w, err)
		return
	}
	if err := s.writeSigningKey(ctx, subscription, body.Secret); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("notification subscription created through the api",
		"subscription", subscription.Name, "scope", body.Project, "caller", callerName(caller))
	writeJSON(w, http.StatusCreated, newSubscriptionView(subscription))
}

func (s *Server) patchSubscription(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	subscription := &kitchenv1alpha1.NotificationSubscription{}
	if err := s.get(ctx, req.PathValue("name"), subscription); err != nil {
		s.writeError(w, err)
		return
	}

	body := subscriptionRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	body.Secret = strings.TrimSpace(body.Secret)

	// The scope is not patchable. Moving a subscription between a project and
	// the platform changes who may write it, which would make this one route
	// two different requirements; delete it and make the one you meant.
	if strings.TrimSpace(body.Project) != "" && strings.TrimSpace(body.Project) != subscription.Spec.Project() {
		badRequest(w, "a subscription's scope cannot be changed: it decides who may write it. "+
			"Delete this one and create the subscription you meant")
		return
	}

	changed := []string{}
	if body.URL != "" {
		if err := s.validSubscriptionURL(body.URL); err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		subscription.Spec.URL = body.URL
		changed = append(changed, "url")
	}
	if len(body.Events) > 0 {
		events, err := parseNotificationEvents(body.Events)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		subscription.Spec.Events = events
		changed = append(changed, "events")
	}
	if body.Description != nil {
		subscription.Spec.Description = strings.TrimSpace(*body.Description)
		changed = append(changed, "description")
	}
	if body.Suspended != nil {
		subscription.Spec.Suspended = *body.Suspended
		changed = append(changed, "suspended")
	}
	if body.MaxAttempts != 0 {
		if body.MaxAttempts < 1 || body.MaxAttempts > 10 {
			badRequest(w, "maxAttempts must be between 1 and 10 (got %d)", body.MaxAttempts)
			return
		}
		subscription.Spec.MaxAttempts = body.MaxAttempts
		changed = append(changed, "maxAttempts")
	}
	if body.Timeout != 0 {
		if body.Timeout < 1 || body.Timeout > 30 {
			badRequest(w, "timeoutSeconds must be between 1 and 30 (got %d)", body.Timeout)
			return
		}
		subscription.Spec.TimeoutSeconds = body.Timeout
		changed = append(changed, "timeoutSeconds")
	}
	rotating := body.Secret != ""
	if rotating && len(body.Secret) < minSigningKeyLength {
		badRequest(w, "secret must be at least %d characters", minSigningKeyLength)
		return
	}
	if rotating {
		changed = append(changed, "secret")
	}
	if len(changed) == 0 {
		badRequest(w, "nothing to change: send url, events, description, suspended, maxAttempts, "+
			"timeoutSeconds or secret")
		return
	}

	var privilege audit.Privilege
	if rotating {
		privilege = audit.PrivilegeCredential
	}
	if !s.recorded(w, req, audit.Transition{
		Object:     subscription,
		Kind:       audit.KindNotificationSubscription,
		Operation:  clickhouse.AuditUpdate,
		Privileged: privilege,
		Project:    subscription.Spec.Project(),
		To:         subscription.Spec.URL,
		Reason: fmt.Sprintf("notification subscription %s changed: %s",
			subscription.Name, strings.Join(changed, ", ")),
		Details: map[string]any{"changed": changed, "url": subscription.Spec.URL},
	}) {
		return
	}
	if err := s.Client.Update(ctx, subscription); err != nil {
		s.writeError(w, err)
		return
	}
	if rotating {
		if err := s.writeSigningKey(ctx, subscription, body.Secret); err != nil {
			s.writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, newSubscriptionView(subscription))
}

func (s *Server) deleteSubscription(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	subscription := &kitchenv1alpha1.NotificationSubscription{}
	if err := s.get(ctx, req.PathValue("name"), subscription); err != nil {
		s.writeError(w, err)
		return
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    subscription,
		Kind:      audit.KindNotificationSubscription,
		Operation: clickhouse.AuditDelete,
		Project:   subscription.Spec.Project(),
		From:      subscription.Spec.URL,
		Reason: fmt.Sprintf("notification subscription %s, its signing key and its delivery history "+
			"were deleted", subscription.Name),
		Details: map[string]any{"url": subscription.Spec.URL},
	}) {
		return
	}
	if err := s.Client.Delete(ctx, subscription); err != nil {
		s.writeError(w, err)
		return
	}
	// The signing key and every delivery are owned by the subscription, so
	// the cluster collects them. The key is deleted here as well rather than
	// waited for: it is a credential, and a credential's removal is not
	// something to leave to a collector's schedule.
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: s.Namespace, Name: subscription.Spec.SecretRef.Name}
	if err := s.Client.Get(ctx, key, secret); err == nil &&
		secret.Labels[managedByLabelKey] == managedByLabelValue {
		if err := s.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			s.writeError(w, err)
			return
		}
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("notification subscription deleted through the api",
		"subscription", subscription.Name, "caller", callerName(caller))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listDeliveries(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	list := &kitchenv1alpha1.NotificationDeliveryList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	subscriptions, err := s.subscriptionsByName(ctx)
	if err != nil {
		s.writeError(w, err)
		return
	}

	scope := scopeFrom(ctx)
	wanted := strings.TrimSpace(req.URL.Query().Get("subscription"))
	phase := strings.TrimSpace(req.URL.Query().Get("phase"))
	switch phase {
	case "", string(kitchenv1alpha1.DeliveryPending),
		string(kitchenv1alpha1.DeliveryDelivered), string(kitchenv1alpha1.DeliveryDeadLettered):
	default:
		badRequest(w, "phase must be Pending, Delivered or DeadLettered (got %q)", phase)
		return
	}

	views := make([]deliveryView, 0, len(list.Items))
	for i := range list.Items {
		delivery := &list.Items[i]
		// A delivery is visible exactly when its subscription is: the
		// question "who may see this" was already answered once, and
		// answering it a second way here is how the two answers drift apart.
		subscription, ok := subscriptions[delivery.Spec.SubscriptionRef.Name]
		if !ok {
			continue
		}
		project := subscription.Spec.Project()
		if project == "" && !scope.all {
			continue
		}
		if project != "" && !scope.allows(project) {
			continue
		}
		if wanted != "" && delivery.Spec.SubscriptionRef.Name != wanted {
			continue
		}
		view := newDeliveryView(delivery)
		if phase != "" && view.Phase != phase {
			continue
		}
		views = append(views, view)
	}
	// Newest first: a dead letter is looked for by "what just failed".
	sort.Slice(views, func(i, j int) bool { return views[i].QueuedAt > views[j].QueuedAt })
	if len(views) > maxDeliveryPage {
		views = views[:maxDeliveryPage]
	}
	writeList(w, views)
}

// retryDelivery puts a dead letter back on the queue.
//
// It is the answer to the receiver that was down for an hour: the payload is
// still exactly the one that would have been sent, so re-sending it is not a
// re-derivation of anything — and the event id is unchanged, so a receiver
// that did get it de-duplicates.
func (s *Server) retryDelivery(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	delivery := &kitchenv1alpha1.NotificationDelivery{}
	if err := s.get(ctx, req.PathValue("name"), delivery); err != nil {
		s.writeError(w, err)
		return
	}
	if delivery.Status.Phase != kitchenv1alpha1.DeliveryDeadLettered {
		badRequest(w, "only a dead letter can be retried; this delivery is %s",
			strings.ToLower(string(delivery.Status.Phase)))
		return
	}

	// A fresh ladder, and a fresh account of it: the attempts that
	// dead-lettered it are the reason somebody is here, but leaving them
	// beside a new attempt numbered 1 would make the list read as two
	// ladders spliced together. What happened is in the activity feed and in
	// this route's log line.
	delivery.Status.Phase = kitchenv1alpha1.DeliveryPending
	delivery.Status.Attempts = 0
	delivery.Status.Attempted = nil
	delivery.Status.NextAttemptTime = nil
	delivery.Status.CompletedTime = nil
	delivery.Status.LastError = ""
	delivery.Status.LastStatusCode = 0
	if err := s.Client.Status().Update(ctx, delivery); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("notification delivery retried through the api",
		"delivery", delivery.Name, "caller", callerName(caller))
	// 202: the operator makes the attempt, and this route only says it will.
	writeJSON(w, http.StatusAccepted, newDeliveryView(delivery))
}

// subscriptionsByName reads every subscription once, for the delivery list.
func (s *Server) subscriptionsByName(
	ctx context.Context,
) (map[string]*kitchenv1alpha1.NotificationSubscription, error) {
	list := &kitchenv1alpha1.NotificationSubscriptionList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	byName := make(map[string]*kitchenv1alpha1.NotificationSubscription, len(list.Items))
	for i := range list.Items {
		byName[list.Items[i].Name] = &list.Items[i]
	}
	return byName, nil
}

// writeSigningKey stores the key the payloads are signed with. It carries the
// managed-by label, so deleting the subscription deletes it, and an owner
// reference, so a subscription deleted any other way takes it too.
func (s *Server) writeSigningKey(
	ctx context.Context,
	subscription *kitchenv1alpha1.NotificationSubscription,
	value string,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subscription.Spec.SecretRef.Name,
			Namespace: s.Namespace,
			Labels:    map[string]string{managedByLabelKey: managedByLabelValue},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kitchenv1alpha1.GroupVersion.String(),
				Kind:       "NotificationSubscription",
				Name:       subscription.Name,
				UID:        subscription.UID,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{notify.SecretKey: []byte(value)},
	}
	err := s.Client.Create(ctx, secret)
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing := &corev1.Secret{}
	key := client.ObjectKey{Namespace: s.Namespace, Name: secret.Name}
	if err := s.Client.Get(ctx, key, existing); err != nil {
		return err
	}
	patch := client.MergeFrom(existing.DeepCopy())
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	existing.Labels[managedByLabelKey] = managedByLabelValue
	existing.Data = secret.Data
	return s.Client.Patch(ctx, existing, patch)
}

// validSubscriptionURL refuses everything the reconciler would refuse later,
// at the moment somebody could still fix it.
func (s *Server) validSubscriptionURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url is required: where the payloads are posted")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url is not a URL: %s", err.Error())
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("url must be https: the payload says what was deployed where, and a " +
			"signature proves only that it was not changed on the way — not that nobody read it")
	}
	if parsed.Host == "" {
		return fmt.Errorf("url must be absolute, with a host")
	}
	return nil
}

// parseNotificationEvents turns the request's list into the enum, refusing an
// unknown one by name and saying what the vocabulary is.
func parseNotificationEvents(raw []string) ([]kitchenv1alpha1.NotificationEvent, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("events is required: %s. A subscription with no events is not "+
			"one that hears everything — it is one that would start hearing whatever the platform "+
			"learns next", notificationEventWords())
	}
	known := map[string]kitchenv1alpha1.NotificationEvent{}
	for _, event := range kitchenv1alpha1.AllNotificationEvents {
		known[string(event)] = event
	}
	seen := map[kitchenv1alpha1.NotificationEvent]bool{}
	events := make([]kitchenv1alpha1.NotificationEvent, 0, len(raw))
	for _, name := range raw {
		event, ok := known[strings.TrimSpace(name)]
		if !ok {
			return nil, fmt.Errorf("unknown event %q: %s", name, notificationEventWords())
		}
		if seen[event] {
			continue
		}
		seen[event] = true
		events = append(events, event)
	}
	return events, nil
}

func notificationEventWords() string {
	names := make([]string, 0, len(kitchenv1alpha1.AllNotificationEvents))
	for _, event := range kitchenv1alpha1.AllNotificationEvents {
		names = append(names, string(event))
	}
	return "one of " + strings.Join(names, ", ")
}

func subscriptionScopeWords(project string) string {
	if project == "" {
		return "every project on the platform"
	}
	return "project " + project
}

// ofSubscription resolves a subscription's project for the enforcement table.
// A platform subscription resolves to no project at all, which is what tells a
// member it does not exist.
var ofSubscription = projectResolver{
	Resource: "notificationsubscriptions",
	Resolve: func(s *Server, req *http.Request) (string, string, error) {
		name := req.PathValue("name")
		subscription := &kitchenv1alpha1.NotificationSubscription{}
		if err := s.get(req.Context(), name, subscription); err != nil {
			return "", name, err
		}
		keepResolved(req.Context(), subscription)
		return subscription.Spec.Project(), name, nil
	},
}

// ofDelivery resolves a delivery's project through its subscription, which is
// the one place that decides who may see a delivery.
var ofDelivery = projectResolver{
	Resource: "notificationdeliveries",
	Resolve: func(s *Server, req *http.Request) (string, string, error) {
		name := req.PathValue("name")
		delivery := &kitchenv1alpha1.NotificationDelivery{}
		if err := s.get(req.Context(), name, delivery); err != nil {
			return "", name, err
		}
		keepResolved(req.Context(), delivery)

		subscription := &kitchenv1alpha1.NotificationSubscription{}
		if err := s.get(req.Context(), delivery.Spec.SubscriptionRef.Name, subscription); err != nil {
			if apierrors.IsNotFound(err) {
				// A delivery whose subscription is gone belongs to nobody,
				// and only an operator can still see it.
				return "", name, nil
			}
			return "", name, err
		}
		keepResolved(req.Context(), subscription)
		return subscription.Spec.Project(), name, nil
	},
}

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

// Package notify turns what the platform did into what somebody is told.
//
// # Where it sits
//
// The platform already writes every build, release and environment transition
// into one place: internal/activity, whose Recorder the reconcilers and the
// API both hold. This package is a second reader of that same stream — an
// activity.Sink — rather than a set of calls added next to every existing
// Record. Three things follow from that, and all three are the point:
//
//   - **One seam.** A reconciler that records an event notifies about it for
//     free, and a reconciler that forgets to record one has a hole in the
//     activity feed as well, which somebody notices.
//   - **Nothing is notified that is not also in the feed.** "What was I told
//     about, and why" is answerable from the feed, for events nobody
//     subscribed to as much as for events somebody did.
//   - **It is already off the reconcile path.** Record hands its event to a
//     detached goroutine and returns; this runs there. A subscription's
//     receiver being down cannot slow a deploy down, because the deploy was
//     finished before anything here ran.
//
// # What it does, and what it does not
//
// It matches an event against the subscriptions and creates one
// NotificationDelivery per match. It does not make an HTTP request: that is
// NotificationDeliveryReconciler's, on the delivery object, with the retry
// ladder and the dead letter. The split is what makes at-least-once survive a
// restart — see the commentary on the types.
package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// PayloadVersion is the `version` field every payload carries, and the one
// promise this package makes to a relay author: fields may be added under this
// version, and nothing is removed or given a new meaning without it changing.
const PayloadVersion = "v1"

// maxQueuedPerEvent bounds how many deliveries one event may create. It is a
// bound on a configuration mistake rather than on normal use — a platform with
// forty subscriptions to `deploy.succeeded` has made a decision, but a runaway
// that creates thousands of objects per deploy is not a decision anybody made.
const maxQueuedPerEvent = 50

// notifiable maps the activity feed's vocabulary onto the notification
// vocabulary. An activity type absent from this map is not notifiable, which
// is most of them: `project.created` and `claim.bound` are things a person
// reads in the feed, not things a relay pages somebody about.
//
// The mapping is many-to-one on purpose. A promotion, an auto-deploy on a push
// and a rollback are three code paths and one fact — what is serving changed —
// and a receiver that had to know the difference would have to be rewritten
// every time the platform grew a fourth way to deploy.
var notifiable = map[string]kitchenv1alpha1.NotificationEvent{
	clickhouse.EventBuildFailed:          kitchenv1alpha1.NotifyBuildFailed,
	clickhouse.EventReleasePromoted:      kitchenv1alpha1.NotifyDeploySucceeded,
	clickhouse.EventReleaseRolledBack:    kitchenv1alpha1.NotifyDeploySucceeded,
	clickhouse.EventPreviewCreated:       kitchenv1alpha1.NotifyPreviewCreated,
	clickhouse.EventPreviewRemoved:       kitchenv1alpha1.NotifyPreviewDestroyed,
	clickhouse.EventEnvironmentUnhealthy: kitchenv1alpha1.NotifyEnvironmentUnhealthy,
	clickhouse.EventAlertFiring:          kitchenv1alpha1.NotifyAlertFiring,
}

// EventFor reports which notification event an activity event is, and whether
// it is one at all.
func EventFor(activityType string) (kitchenv1alpha1.NotificationEvent, bool) {
	event, ok := notifiable[activityType]
	return event, ok
}

// Payload is the JSON body a receiver gets. It is a flat document on purpose:
// the receivers this exists for are short relays, and a nested shape costs
// every one of them a few lines to walk.
//
// docs/api/notifications.md documents it field by field and is the contract; a
// field added here is added there.
type Payload struct {
	// Version is PayloadVersion.
	Version string `json:"version"`
	// ID is this event's id, and the receiver's idempotency key: every
	// attempt of this delivery carries it, and so does the same event sent
	// to a second subscription.
	ID string `json:"id"`
	// Type is the NotificationEvent.
	Type string `json:"type"`
	// OccurredAt is when the platform recorded the event, RFC 3339 in UTC.
	// It is not when the request was made — that is the timestamp header,
	// which is part of the signature.
	OccurredAt string `json:"occurredAt"`
	// Subscription is the name of the subscription this was sent to, so that
	// a relay fronting several of them can tell which one is talking.
	Subscription string `json:"subscription"`

	// What it was about. Empty fields are simply not involved: a platform
	// upgrade names no project, and a build failure names no release.
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Build       string `json:"build,omitempty"`
	Release     string `json:"release,omitempty"`

	// Message is the same sentence the activity feed shows.
	Message string `json:"message,omitempty"`
	// Actor is who caused it: an authenticated caller by name, or "operator"
	// for what the reconcilers decided on their own.
	Actor string `json:"actor,omitempty"`
	// Value is the one number some events carry — a finished build's
	// duration in seconds, an alert's count.
	Value float64 `json:"value,omitempty"`
}

// Notifier creates deliveries for the events it is handed. The zero value is
// not usable; a nil *Notifier is, and notifies nothing, so a caller wired
// without one does not have to care.
type Notifier struct {
	// Client reads the subscriptions and creates the deliveries.
	Client client.Client
	// Namespace is where both live.
	Namespace string

	// Now is the clock, for tests. Nil is time.Now.
	Now func() time.Time
}

// Deliver implements activity.Sink: it is handed every event the platform
// records, on the recorder's own goroutine, and never returns an error —
// there is nobody to return one to, and the caller is a reconcile that has
// already finished.
func (n *Notifier) Deliver(ctx context.Context, event clickhouse.Event) {
	if n == nil {
		return
	}
	if _, err := n.Queue(ctx, event); err != nil {
		logf.Log.WithName("notify").V(1).Info("notifications not queued",
			"type", event.Type, "reason", err.Error())
	}
}

// Queue is Deliver with its answer kept: how many deliveries were created. It
// is what the tests drive, because "it created two" is the assertion and
// Deliver deliberately has no return value.
func (n *Notifier) Queue(ctx context.Context, event clickhouse.Event) (int, error) {
	kind, ok := EventFor(event.Type)
	if !ok {
		return 0, nil
	}

	subscriptions := &kitchenv1alpha1.NotificationSubscriptionList{}
	if err := n.Client.List(ctx, subscriptions, client.InNamespace(n.Namespace)); err != nil {
		return 0, err
	}

	occurred := event.Timestamp
	if occurred.IsZero() {
		occurred = n.now()
	}
	id, err := eventID()
	if err != nil {
		return 0, err
	}

	queued := 0
	for i := range subscriptions.Items {
		subscription := &subscriptions.Items[i]
		if !Matches(subscription, kind, event.Project) {
			continue
		}
		if queued >= maxQueuedPerEvent {
			return queued, fmt.Errorf(
				"more than %d subscriptions match one event; the rest were not queued", maxQueuedPerEvent)
		}
		if err := n.create(ctx, subscription, kind, id, occurred, event); err != nil {
			// One receiver's delivery failing to be created must not cost the
			// others theirs, so this reports at the end rather than returning
			// here.
			logf.Log.WithName("notify").V(1).Info("notification not queued",
				"subscription", subscription.Name, "type", event.Type, "reason", err.Error())
			continue
		}
		queued++
	}
	return queued, nil
}

// Matches reports whether a subscription wants this event, about this project.
//
// Scope is the whole of it: a subscription naming a project hears that
// project's events, and one naming none hears every project's — which is the
// operator's subscription, and why the API refuses to write one for anybody
// else. A platform event that names no project (an upgrade, an alert over
// everything) reaches only the platform-scoped subscriptions, because there is
// no project whose subscribers it could be for.
func Matches(
	subscription *kitchenv1alpha1.NotificationSubscription,
	event kitchenv1alpha1.NotificationEvent,
	project string,
) bool {
	if subscription.Spec.Suspended || !subscription.Spec.Wants(event) {
		return false
	}
	scope := subscription.Spec.Project()
	return scope == "" || scope == project
}

func (n *Notifier) create(
	ctx context.Context,
	subscription *kitchenv1alpha1.NotificationSubscription,
	kind kitchenv1alpha1.NotificationEvent,
	id string,
	occurred time.Time,
	event clickhouse.Event,
) error {
	payload := Payload{
		Version:      PayloadVersion,
		ID:           id,
		Type:         string(kind),
		OccurredAt:   occurred.UTC().Format(time.RFC3339),
		Subscription: subscription.Name,
		Project:      event.Project,
		Environment:  event.Environment,
		Build:        event.Build,
		Release:      event.Release,
		Message:      event.Message,
		Actor:        event.Actor,
		Value:        event.Value,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	delivery := &kitchenv1alpha1.NotificationDelivery{
		ObjectMeta: metav1.ObjectMeta{
			// Generated rather than derived from the event id: two
			// subscriptions receive one event, and a name would collide.
			GenerateName: subscription.Name + "-",
			Namespace:    n.Namespace,
			Labels: map[string]string{
				SubscriptionLabel: subscription.Name,
				EventLabel:        string(kind),
			},
			// Owned by the subscription, so deleting one takes its whole
			// history with it and nothing has to walk the list to clean up.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kitchenv1alpha1.GroupVersion.String(),
				Kind:       "NotificationSubscription",
				Name:       subscription.Name,
				UID:        subscription.UID,
			}},
		},
		Spec: kitchenv1alpha1.NotificationDeliverySpec{
			SubscriptionRef: kitchenv1alpha1.LocalObjectReference{Name: subscription.Name},
			Event:           kind,
			EventID:         id,
			Payload:         string(body),
			Project:         event.Project,
		},
	}
	return n.Client.Create(ctx, delivery)
}

// The labels a delivery carries, so that "this subscription's deliveries" is a
// selector rather than a scan of every delivery on the platform.
const (
	SubscriptionLabel = "kitchen.bermos.dev/subscription"
	EventLabel        = "kitchen.bermos.dev/event"
)

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// eventID is 16 random bytes in hex. It is not derived from the event's
// contents: two identical deploys of the same release are two events, and a
// receiver de-duplicating on this id must not silently drop the second.
func eventID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

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

package notify

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

const (
	platformNamespace = "kitchen-system"
	// relayName is the subscription under test: the payload names it, the
	// label selects on it, and the owner reference points at it.
	relayName = "relay"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("adding the core scheme: %v", err)
	}
	if err := kitchenv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding the Kitchen scheme: %v", err)
	}
	return s
}

func subscription(name string, project string, events ...kitchenv1alpha1.NotificationEvent) *kitchenv1alpha1.NotificationSubscription {
	sub := &kitchenv1alpha1.NotificationSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: platformNamespace},
		Spec: kitchenv1alpha1.NotificationSubscriptionSpec{
			URL:       "https://relay.example.com/hook",
			Events:    events,
			SecretRef: kitchenv1alpha1.LocalObjectReference{Name: "kitchen-notify-" + name},
		},
	}
	if project != "" {
		sub.Spec.ProjectRef = &kitchenv1alpha1.LocalObjectReference{Name: project}
	}
	return sub
}

// TestSelectionIsPerSubscription is the event-selection rule, which is the one
// a subscriber notices immediately when it is wrong: "preview created" is
// noise for some teams and the whole point for others, and a platform that
// sends everything to everybody is a platform whose notifications are muted.
func TestSelectionIsPerSubscription(t *testing.T) {
	deploys := subscription("deploys", "shop", kitchenv1alpha1.NotifyDeploySucceeded)
	previews := subscription("previews", "shop", kitchenv1alpha1.NotifyPreviewCreated)
	otherProject := subscription("billing-deploys", "billing", kitchenv1alpha1.NotifyDeploySucceeded)
	platform := subscription("platform", "", kitchenv1alpha1.NotifyDeploySucceeded)
	suspended := subscription("paused", "shop", kitchenv1alpha1.NotifyDeploySucceeded)
	suspended.Spec.Suspended = true

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(deploys, previews, otherProject, platform, suspended).
		Build()
	notifier := &Notifier{Client: c, Namespace: platformNamespace}

	queued, err := notifier.Queue(context.Background(), clickhouse.Event{
		Type:        clickhouse.EventReleasePromoted,
		Project:     "shop",
		Environment: "shop-production",
		Release:     "shop-rel-000004",
		Message:     "release shop-rel-000004 went live on shop-production",
	})
	if err != nil {
		t.Fatalf("queueing: %v", err)
	}
	// shop's deploy subscription and the platform-wide one. Not the preview
	// subscription (wrong event), not billing's (wrong project), not the
	// suspended one.
	if queued != 2 {
		t.Fatalf("queued %d deliveries, want 2 (shop's deploys and the platform's)", queued)
	}

	deliveries := &kitchenv1alpha1.NotificationDeliveryList{}
	if err := c.List(context.Background(), deliveries, client.InNamespace(platformNamespace)); err != nil {
		t.Fatalf("listing deliveries: %v", err)
	}
	got := map[string]bool{}
	for i := range deliveries.Items {
		got[deliveries.Items[i].Spec.SubscriptionRef.Name] = true
		if deliveries.Items[i].Spec.Event != kitchenv1alpha1.NotifyDeploySucceeded {
			t.Errorf("delivery carries event %q, want deploy.succeeded", deliveries.Items[i].Spec.Event)
		}
	}
	for _, want := range []string{"deploys", "platform"} {
		if !got[want] {
			t.Errorf("no delivery for subscription %q", want)
		}
	}
	for _, unwanted := range []string{"previews", "billing-deploys", "paused"} {
		if got[unwanted] {
			t.Errorf("subscription %q was sent an event it did not ask for", unwanted)
		}
	}
}

// TestNotEveryActivityEventIsNotifiable pins the vocabulary. The feed carries
// far more than anybody should be paged about, and the map is what keeps
// `claim.bound` out of somebody's evening.
func TestNotEveryActivityEventIsNotifiable(t *testing.T) {
	for _, notifiableType := range []string{
		clickhouse.EventBuildFailed,
		clickhouse.EventReleasePromoted,
		clickhouse.EventReleaseRolledBack,
		clickhouse.EventPreviewCreated,
		clickhouse.EventPreviewRemoved,
		clickhouse.EventEnvironmentUnhealthy,
		clickhouse.EventAlertFiring,
	} {
		if _, ok := EventFor(notifiableType); !ok {
			t.Errorf("%s is not notifiable, and should be", notifiableType)
		}
	}
	for _, quiet := range []string{
		clickhouse.EventBuildSucceeded,
		clickhouse.EventClaimBound,
		clickhouse.EventProjectCreated,
		clickhouse.EventSecretRotated,
	} {
		if _, ok := EventFor(quiet); ok {
			t.Errorf("%s is notifiable, and should not be: the feed is not the notification", quiet)
		}
	}
}

// TestPlatformEventsReachOnlyPlatformSubscriptions: an event about no project
// — an alert over the whole install — has no project whose subscribers it
// could belong to.
func TestPlatformEventsReachOnlyPlatformSubscriptions(t *testing.T) {
	projectScoped := subscription("shop-alerts", "shop", kitchenv1alpha1.NotifyAlertFiring)
	platform := subscription("platform-alerts", "", kitchenv1alpha1.NotifyAlertFiring)

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(projectScoped, platform).
		Build()
	notifier := &Notifier{Client: c, Namespace: platformNamespace}

	queued, err := notifier.Queue(context.Background(), clickhouse.Event{
		Type:    clickhouse.EventAlertFiring,
		Message: `alert "Checkout 500s" fired`,
		Value:   42,
	})
	if err != nil {
		t.Fatalf("queueing: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued %d deliveries, want 1 (the platform's alone)", queued)
	}
}

// TestPayloadIsTheDocumentedShape checks the body a relay author builds
// against: the version, the idempotency key, the type, and the objects it was
// about. The delivery stores the bytes rather than the fields, so this is also
// what every retry will send.
func TestPayloadIsTheDocumentedShape(t *testing.T) {
	sub := subscription(relayName, "shop", kitchenv1alpha1.NotifyBuildFailed)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sub).Build()
	occurred := time.Date(2026, 9, 4, 2, 0, 0, 0, time.UTC)
	notifier := &Notifier{Client: c, Namespace: platformNamespace}

	if _, err := notifier.Queue(context.Background(), clickhouse.Event{
		Type:      clickhouse.EventBuildFailed,
		Timestamp: occurred,
		Project:   "shop",
		Build:     "shop-build-000012",
		Message:   "build failed: npm run build exited 1",
		Actor:     "operator",
	}); err != nil {
		t.Fatalf("queueing: %v", err)
	}

	deliveries := &kitchenv1alpha1.NotificationDeliveryList{}
	if err := c.List(context.Background(), deliveries, client.InNamespace(platformNamespace)); err != nil {
		t.Fatalf("listing deliveries: %v", err)
	}
	if len(deliveries.Items) != 1 {
		t.Fatalf("got %d deliveries, want 1", len(deliveries.Items))
	}
	delivery := deliveries.Items[0]

	payload := Payload{}
	if err := json.Unmarshal([]byte(delivery.Spec.Payload), &payload); err != nil {
		t.Fatalf("the stored payload is not JSON: %v", err)
	}
	if payload.Version != PayloadVersion {
		t.Errorf("payload version %q, want %q", payload.Version, PayloadVersion)
	}
	if payload.Type != string(kitchenv1alpha1.NotifyBuildFailed) {
		t.Errorf("payload type %q, want build.failed", payload.Type)
	}
	if payload.ID == "" || payload.ID != delivery.Spec.EventID {
		t.Errorf("payload id %q does not match the delivery's event id %q", payload.ID, delivery.Spec.EventID)
	}
	if payload.OccurredAt != occurred.Format(time.RFC3339) {
		t.Errorf("payload occurredAt %q, want %q", payload.OccurredAt, occurred.Format(time.RFC3339))
	}
	if payload.Project != "shop" || payload.Build != "shop-build-000012" {
		t.Errorf("payload names %q/%q, want shop/shop-build-000012", payload.Project, payload.Build)
	}
	if payload.Subscription != relayName {
		t.Errorf("payload subscription %q, want relay", payload.Subscription)
	}
	if delivery.Labels[SubscriptionLabel] != relayName {
		t.Errorf("delivery is not labelled with its subscription: %v", delivery.Labels)
	}
	if len(delivery.OwnerReferences) != 1 || delivery.OwnerReferences[0].Name != relayName {
		t.Errorf("a delivery is owned by its subscription, so that deleting one takes its history: %v",
			delivery.OwnerReferences)
	}
}

// TestNilNotifierNotifiesNothing: a caller wired without one must not have to
// care, which is the same contract the activity recorder makes.
func TestNilNotifierNotifiesNothing(t *testing.T) {
	var notifier *Notifier
	notifier.Deliver(context.Background(), clickhouse.Event{Type: clickhouse.EventBuildFailed})
}

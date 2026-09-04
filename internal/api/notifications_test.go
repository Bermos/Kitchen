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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/notify"
)

// The signing key is the thing these tests are mostly about. It goes in with
// the request and never comes back out — not on the create that wrote it, not
// on a read, and not on the rotation that replaces it.
const (
	testSigningKey  = "a-signing-key-nobody-else-has"
	testRotatedKey  = "the-second-signing-key-entirely"
	testRelayURL    = "https://relay.example.com/kitchen"
	testRelay       = "shop-relay"
	testPlatformSub = "platform-relay"
)

func createSubscription(t *testing.T, h *harness, body string) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(t, http.MethodPost, "/api/v1/notifications/subscriptions", body)
}

func TestSubscribingWritesTheKeyAndNeverAnswersWithIt(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := createSubscription(t, h, `{"name":"`+testRelay+`","project":"`+feedProject+`",
		"url":"`+testRelayURL+`","events":["deploy.succeeded","build.failed"],
		"description":"into the deploy channel","secret":"`+testSigningKey+`"}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /notifications/subscriptions = %d: %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), testSigningKey) {
		t.Fatalf("the response echoed the signing key: %s", res.Body.String())
	}
	view := decode[subscriptionView](t, res)
	if view.Scope != "project" || view.Project != feedProject {
		t.Errorf("want a project-scoped subscription, got %+v", view)
	}
	if view.MaxAttempts != kitchenv1alpha1.DefaultMaxAttempts ||
		view.Timeout != kitchenv1alpha1.DefaultTimeoutSeconds {
		t.Errorf("a subscription that says nothing gets the defaults, got %+v", view)
	}
	if view.CreatedBy != testCaller {
		t.Errorf("the byline is the caller's, got %q", view.CreatedBy)
	}

	// The key is on the cluster, under the name the subscription points at,
	// carrying the label that says the platform wrote it.
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: testNamespace, Name: notificationSecretPrefix + testRelay}
	if err := h.server.Client.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("the signing key was not written: %v", err)
	}
	if string(secret.Data[notify.SecretKey]) != testSigningKey {
		t.Errorf("the stored key is not the one that was sent")
	}
	if secret.Labels[managedByLabelKey] != managedByLabelValue {
		t.Errorf("a secret the API wrote carries the managed-by label, got %v", secret.Labels)
	}

	// And no read answers with it either.
	read := h.do(t, http.MethodGet, "/api/v1/notifications/subscriptions/"+testRelay, "")
	if read.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", read.Code, read.Body.String())
	}
	if strings.Contains(read.Body.String(), testSigningKey) {
		t.Errorf("a read answered with the signing key: %s", read.Body.String())
	}
}

func TestSubscribingRefusesWhatCannotWork(t *testing.T) {
	cases := map[string]struct{ body, wants string }{
		"no key at all": {
			`{"name":"r","project":"shop","url":"` + testRelayURL + `","events":["deploy.succeeded"]}`,
			"never reads it back",
		},
		"a key too short to be one": {
			`{"name":"r","project":"shop","url":"` + testRelayURL + `","events":["deploy.succeeded"],"secret":"hunter2"}`,
			"at least 16 characters",
		},
		"plain http": {
			`{"name":"r","project":"shop","url":"http://relay.example.com/k","events":["deploy.succeeded"],"secret":"` +
				testSigningKey + `"}`,
			"must be https",
		},
		"no events": {
			`{"name":"r","project":"shop","url":"` + testRelayURL + `","events":[],"secret":"` + testSigningKey + `"}`,
			"events is required",
		},
		"an event nobody has heard of": {
			`{"name":"r","project":"shop","url":"` + testRelayURL + `","events":["deploy.exploded"],"secret":"` +
				testSigningKey + `"}`,
			"unknown event",
		},
		"a name that is not a label": {
			`{"name":"Shop Relay","project":"shop","url":"` + testRelayURL + `","events":["deploy.succeeded"],"secret":"` +
				testSigningKey + `"}`,
			"DNS label",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil, fixtures()...)
			res := createSubscription(t, h, tc.body)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", res.Code, res.Body.String())
			}
			if !strings.Contains(res.Body.String(), tc.wants) {
				t.Errorf("the refusal should say %q, got %s", tc.wants, res.Body.String())
			}
		})
	}
}

// The platform scope hears every project's events, so it is the operator's
// alone — and the enforcement table cannot say so, because it resolves this
// route's project out of a body that names none.
func TestOnlyAnOperatorSubscribesToEveryProject(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)

	res := createSubscription(t, h, `{"name":"`+testPlatformSub+`","url":"`+testRelayURL+`",
		"events":["build.failed"],"secret":"`+testSigningKey+`"}`)
	if res.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "operator role") {
		t.Errorf("the refusal should name the role it wanted, got %s", res.Body.String())
	}

	// The same request from an operator is fine.
	operator := newHarness(t, nil, fixtures()...)
	if res := createSubscription(t, operator, `{"name":"`+testPlatformSub+`","url":"`+testRelayURL+`",
		"events":["build.failed"],"secret":"`+testSigningKey+`"}`); res.Code != http.StatusCreated {
		t.Fatalf("an operator may subscribe to the platform: %d %s", res.Code, res.Body.String())
	}
}

// A platform subscription names no project, so there is no project of a
// member's it could be about: they are not told it exists.
func TestAMemberIsNotShownThePlatformsSubscriptions(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin, platformSubscription(), projectSubscription())

	list := h.do(t, http.MethodGet, "/api/v1/notifications/subscriptions", "")
	items := decode[struct {
		Items []subscriptionView `json:"items"`
	}](t, list).Items
	if len(items) != 1 || items[0].Name != testRelay {
		t.Fatalf("a member sees their project's subscription and no other, got %+v", items)
	}

	res := h.do(t, http.MethodGet, "/api/v1/notifications/subscriptions/"+testPlatformSub, "")
	if res.Code != http.StatusNotFound {
		t.Errorf("want 404 — not 403, which would confirm it exists — got %d: %s", res.Code, res.Body.String())
	}
}

func TestRotatingTheKeyReplacesItAndAnswersWithNeither(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), projectSubscription(), signingSecret())...)

	res := h.do(t, http.MethodPatch, "/api/v1/notifications/subscriptions/"+testRelay,
		`{"secret":"`+testRotatedKey+`"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", res.Code, res.Body.String())
	}
	if body := res.Body.String(); strings.Contains(body, testRotatedKey) || strings.Contains(body, testSigningKey) {
		t.Fatalf("a rotation answered with a key: %s", body)
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: testNamespace, Name: notificationSecretPrefix + testRelay}
	if err := h.server.Client.Get(context.Background(), key, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data[notify.SecretKey]) != testRotatedKey {
		t.Errorf("the key was not rotated: %q", secret.Data[notify.SecretKey])
	}
}

// The scope decides who may write it, so changing the scope would make one
// route two requirements.
func TestASubscriptionsScopeCannotBeMoved(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), projectSubscription(), signingSecret())...)

	res := h.do(t, http.MethodPatch, "/api/v1/notifications/subscriptions/"+testRelay,
		`{"project":"`+otherProject+`"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "cannot be changed") {
		t.Errorf("the refusal should say so, got %s", res.Body.String())
	}
}

func TestDeletingASubscriptionTakesItsKeyWithIt(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), projectSubscription(), signingSecret())...)

	res := h.do(t, http.MethodDelete, "/api/v1/notifications/subscriptions/"+testRelay, "")
	if res.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", res.Code, res.Body.String())
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: testNamespace, Name: notificationSecretPrefix + testRelay}
	err := h.server.Client.Get(context.Background(), key, secret)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the signing key outlived its subscription: %v", err)
	}
}

func TestDeliveriesAreListedWithTheirDeadLetters(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(),
		projectSubscription(), signingSecret(), deadLetter(), deliveredDelivery())...)

	all := h.do(t, http.MethodGet, "/api/v1/notifications/deliveries", "")
	if all.Code != http.StatusOK {
		t.Fatalf("GET /notifications/deliveries = %d: %s", all.Code, all.Body.String())
	}
	if got := len(decode[struct {
		Items []deliveryView `json:"items"`
	}](t, all).Items); got != 2 {
		t.Fatalf("want both deliveries, got %d", got)
	}

	dead := h.do(t, http.MethodGet, "/api/v1/notifications/deliveries?phase=DeadLettered", "")
	items := decode[struct {
		Items []deliveryView `json:"items"`
	}](t, dead).Items
	if len(items) != 1 || items[0].Phase != string(kitchenv1alpha1.DeliveryDeadLettered) {
		t.Fatalf("want the dead letter alone, got %+v", items)
	}
	// The payload is what makes a dead letter something a person can act on.
	if !strings.Contains(items[0].Payload, `"type":"build.failed"`) {
		t.Errorf("a dead letter carries the payload it would have sent, got %q", items[0].Payload)
	}

	bad := h.do(t, http.MethodGet, "/api/v1/notifications/deliveries?phase=Exploded", "")
	if bad.Code != http.StatusBadRequest {
		t.Errorf("an unknown phase is a 400, got %d", bad.Code)
	}
}

func TestOnlyADeadLetterIsRetried(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(),
		projectSubscription(), signingSecret(), deadLetter(), deliveredDelivery())...)

	done := h.do(t, http.MethodPost, "/api/v1/notifications/deliveries/shop-relay-delivered/retry", "")
	if done.Code != http.StatusBadRequest {
		t.Fatalf("a delivered notification is not retried: %d %s", done.Code, done.Body.String())
	}

	res := h.do(t, http.MethodPost, "/api/v1/notifications/deliveries/shop-relay-dead/retry", "")
	if res.Code != http.StatusAccepted {
		t.Fatalf("want 202 — the operator makes the attempt — got %d: %s", res.Code, res.Body.String())
	}
	view := decode[deliveryView](t, res)
	if view.Phase != string(kitchenv1alpha1.DeliveryPending) || view.Attempts != 0 {
		t.Errorf("a retried delivery is pending again with a fresh ladder, got %+v", view)
	}
	// The event id is untouched: a receiver that did get it de-duplicates.
	if view.EventID != testEventID {
		t.Errorf("the event id must survive a retry, got %q", view.EventID)
	}
}

const testEventID = "0123456789abcdef0123456789abcdef"

func projectSubscription() runtime.Object {
	return &kitchenv1alpha1.NotificationSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: testRelay, Namespace: testNamespace},
		Spec: kitchenv1alpha1.NotificationSubscriptionSpec{
			URL:        testRelayURL,
			Events:     []kitchenv1alpha1.NotificationEvent{kitchenv1alpha1.NotifyBuildFailed},
			ProjectRef: &kitchenv1alpha1.LocalObjectReference{Name: feedProject},
			SecretRef:  kitchenv1alpha1.LocalObjectReference{Name: notificationSecretPrefix + testRelay},
		},
	}
}

func platformSubscription() runtime.Object {
	return &kitchenv1alpha1.NotificationSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: testPlatformSub, Namespace: testNamespace},
		Spec: kitchenv1alpha1.NotificationSubscriptionSpec{
			URL:       testRelayURL,
			Events:    []kitchenv1alpha1.NotificationEvent{kitchenv1alpha1.NotifyBuildFailed},
			SecretRef: kitchenv1alpha1.LocalObjectReference{Name: notificationSecretPrefix + testPlatformSub},
		},
	}
}

func signingSecret() runtime.Object {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notificationSecretPrefix + testRelay,
			Namespace: testNamespace,
			Labels:    map[string]string{managedByLabelKey: managedByLabelValue},
		},
		Data: map[string][]byte{notify.SecretKey: []byte(testSigningKey)},
	}
}

func deadLetter() runtime.Object {
	return &kitchenv1alpha1.NotificationDelivery{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shop-relay-dead",
			Namespace: testNamespace,
			Labels:    map[string]string{notify.SubscriptionLabel: testRelay},
		},
		Spec: kitchenv1alpha1.NotificationDeliverySpec{
			SubscriptionRef: kitchenv1alpha1.LocalObjectReference{Name: testRelay},
			Event:           kitchenv1alpha1.NotifyBuildFailed,
			EventID:         testEventID,
			Payload:         `{"version":"v1","id":"` + testEventID + `","type":"build.failed"}`,
			Project:         feedProject,
		},
		Status: kitchenv1alpha1.NotificationDeliveryStatus{
			Phase:          kitchenv1alpha1.DeliveryDeadLettered,
			Attempts:       5,
			LastStatusCode: 502,
			LastError:      "receiver answered 502 Bad Gateway",
		},
	}
}

func deliveredDelivery() runtime.Object {
	return &kitchenv1alpha1.NotificationDelivery{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shop-relay-delivered",
			Namespace: testNamespace,
			Labels:    map[string]string{notify.SubscriptionLabel: testRelay},
		},
		Spec: kitchenv1alpha1.NotificationDeliverySpec{
			SubscriptionRef: kitchenv1alpha1.LocalObjectReference{Name: testRelay},
			Event:           kitchenv1alpha1.NotifyBuildFailed,
			EventID:         "fedcba9876543210fedcba9876543210",
			Payload:         `{"version":"v1","type":"build.failed"}`,
			Project:         feedProject,
		},
		Status: kitchenv1alpha1.NotificationDeliveryStatus{
			Phase:    kitchenv1alpha1.DeliveryDelivered,
			Attempts: 1,
		},
	}
}

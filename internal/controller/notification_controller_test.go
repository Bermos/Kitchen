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
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/notify"
)

// receiver is somebody else's HTTP server, as far as the operator is
// concerned: it records what arrived and answers whatever the test told it to.
type receiver struct {
	mu       sync.Mutex
	requests []receivedRequest
	// status is what it answers with. 0 means "hang up" — the server closes
	// the connection without a response, which is the transport failure the
	// status ladder cannot express.
	status int
}

type receivedRequest struct {
	body      []byte
	signature string
	timestamp string
	event     string
	eventID   string
	attempt   string
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)

	r.mu.Lock()
	r.requests = append(r.requests, receivedRequest{
		body:      body,
		signature: req.Header.Get(notify.HeaderSignature),
		timestamp: req.Header.Get(notify.HeaderTimestamp),
		event:     req.Header.Get(notify.HeaderEvent),
		eventID:   req.Header.Get(notify.HeaderEventID),
		attempt:   req.Header.Get(notify.HeaderAttempt),
	})
	status := r.status
	r.mu.Unlock()

	w.WriteHeader(status)
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *receiver) last() receivedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[len(r.requests)-1]
}

func (r *receiver) answer(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

var _ = Describe("Notification delivery", func() {
	const (
		subscriptionName = "notify-relay"
		signingSecret    = "a-signing-key-nobody-else-has"
		buildName        = "notifyshop-build-000001"
		projectName      = "notifyshop"
	)

	ctx := context.Background()

	var (
		server       *httptest.Server
		fake         *receiver
		reconciler   *NotificationDeliveryReconciler
		subscription *kitchenv1alpha1.NotificationSubscription
		secret       *corev1.Secret
		now          time.Time
	)

	subscriptionKey := types.NamespacedName{Name: subscriptionName, Namespace: PlatformNamespace}

	// queue is what internal/notify does on the platform: one delivery
	// object, holding the exact bytes that will be sent.
	queue := func(payload string) *kitchenv1alpha1.NotificationDelivery {
		delivery := &kitchenv1alpha1.NotificationDelivery{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: subscriptionName + "-",
				Namespace:    PlatformNamespace,
				Labels:       map[string]string{notify.SubscriptionLabel: subscriptionName},
			},
			Spec: kitchenv1alpha1.NotificationDeliverySpec{
				SubscriptionRef: kitchenv1alpha1.LocalObjectReference{Name: subscriptionName},
				Event:           kitchenv1alpha1.NotifyDeploySucceeded,
				EventID:         "0123456789abcdef0123456789abcdef",
				Payload:         payload,
				Project:         projectName,
			},
		}
		Expect(k8sClient.Create(ctx, delivery)).To(Succeed())
		return delivery
	}

	reconcileDelivery := func(delivery *kitchenv1alpha1.NotificationDelivery) reconcile.Result {
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: delivery.Name, Namespace: delivery.Namespace},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: delivery.Name, Namespace: delivery.Namespace}, delivery)).To(Succeed())
		return result
	}

	BeforeEach(func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())

		fake = &receiver{status: http.StatusOK}
		server = httptest.NewServer(fake)
		now = time.Date(2026, 9, 4, 2, 0, 0, 0, time.UTC)

		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kitchen-notify-" + subscriptionName,
				Namespace: PlatformNamespace,
			},
			Data: map[string][]byte{notify.SecretKey: []byte(signingSecret)},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		subscription = &kitchenv1alpha1.NotificationSubscription{
			ObjectMeta: metav1.ObjectMeta{Name: subscriptionName, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.NotificationSubscriptionSpec{
				// The receiver is plain HTTP because httptest is; the
				// reconciler does not decide what may be posted to, the API
				// and the subscription reconciler do (both refuse anything
				// but https at the moment somebody could still fix it).
				URL:         server.URL,
				Events:      []kitchenv1alpha1.NotificationEvent{kitchenv1alpha1.NotifyDeploySucceeded},
				ProjectRef:  &kitchenv1alpha1.LocalObjectReference{Name: projectName},
				SecretRef:   kitchenv1alpha1.LocalObjectReference{Name: secret.Name},
				MaxAttempts: 3,
			},
		}
		Expect(k8sClient.Create(ctx, subscription)).To(Succeed())

		reconciler = &NotificationDeliveryReconciler{
			Client:     k8sClient,
			Scheme:     k8sClient.Scheme(),
			HTTPClient: server.Client(),
			Now:        func() time.Time { return now },
		}
	})

	AfterEach(func() {
		server.Close()
		deliveries := &kitchenv1alpha1.NotificationDeliveryList{}
		Expect(k8sClient.List(ctx, deliveries, client.InNamespace(PlatformNamespace))).To(Succeed())
		for i := range deliveries.Items {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &deliveries.Items[i]))).To(Succeed())
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, subscription))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed())
	})

	It("posts the payload signed, and marks it delivered", func() {
		body := `{"version":"v1","id":"0123456789abcdef0123456789abcdef","type":"deploy.succeeded"}`
		delivery := queue(body)

		reconcileDelivery(delivery)

		Expect(fake.count()).To(Equal(1))
		got := fake.last()
		Expect(string(got.body)).To(Equal(body), "the bytes stored are the bytes sent")
		Expect(got.event).To(Equal("deploy.succeeded"))
		Expect(got.eventID).To(Equal("0123456789abcdef0123456789abcdef"))
		Expect(got.attempt).To(Equal("1"))

		// The whole point of the header: a receiver with the key verifies it,
		// and it is bound to the timestamp it was sent with.
		Expect(notify.Verify([]byte(signingSecret), got.signature, now, got.body)).To(BeTrue())
		Expect(got.timestamp).To(Equal("1788487200"))

		Expect(delivery.Status.Phase).To(Equal(kitchenv1alpha1.DeliveryDelivered))
		Expect(delivery.Status.Attempts).To(Equal(int32(1)))
		Expect(delivery.Status.CompletedTime).NotTo(BeNil())

		Expect(k8sClient.Get(ctx, subscriptionKey, subscription)).To(Succeed())
		Expect(subscription.Status.Delivered).To(Equal(int64(1)))
		Expect(subscription.Status.LastResult).To(Equal("delivered"))
	})

	It("retries with a growing backoff and then dead-letters", func() {
		fake.answer(http.StatusInternalServerError)
		delivery := queue(`{"version":"v1","type":"deploy.succeeded"}`)

		// One: failed, and due again in ten seconds.
		result := reconcileDelivery(delivery)
		Expect(delivery.Status.Phase).To(Equal(kitchenv1alpha1.DeliveryPending))
		Expect(delivery.Status.Attempts).To(Equal(int32(1)))
		Expect(delivery.Status.LastStatusCode).To(Equal(int32(500)))
		Expect(result.RequeueAfter).To(Equal(10 * time.Second))
		Expect(delivery.Status.NextAttemptTime.Time.Unix()).To(Equal(now.Add(10 * time.Second).Unix()))

		// A reconcile before the backoff has elapsed makes no request: the
		// wait is in the queue, which is what survives a restart.
		result = reconcileDelivery(delivery)
		Expect(fake.count()).To(Equal(1), "the backoff was not waited out")
		Expect(result.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		// Two: the ladder doubles.
		now = now.Add(10 * time.Second)
		result = reconcileDelivery(delivery)
		Expect(fake.count()).To(Equal(2))
		Expect(result.RequeueAfter).To(Equal(20 * time.Second))

		// Three exhausts maxAttempts, so it dead-letters rather than waiting
		// again.
		now = now.Add(20 * time.Second)
		reconcileDelivery(delivery)
		Expect(fake.count()).To(Equal(3))
		Expect(delivery.Status.Phase).To(Equal(kitchenv1alpha1.DeliveryDeadLettered))
		Expect(delivery.Status.NextAttemptTime).To(BeNil())
		Expect(delivery.Status.Attempted).To(HaveLen(3))
		Expect(delivery.Status.LastError).To(ContainSubstring("500"))
		// The payload is kept exactly as it was, which is what makes a dead
		// letter something a person can retry rather than only read.
		Expect(delivery.Spec.Payload).To(Equal(`{"version":"v1","type":"deploy.succeeded"}`))

		// A dead letter is not attempted again on its own.
		now = now.Add(time.Hour)
		reconcileDelivery(delivery)
		Expect(fake.count()).To(Equal(3))

		Expect(k8sClient.Get(ctx, subscriptionKey, subscription)).To(Succeed())
		Expect(subscription.Status.Failed).To(Equal(int64(3)))
		Expect(subscription.Status.DeadLettered).To(Equal(int64(1)))
	})

	It("keeps the same event id across every attempt, so a receiver can de-duplicate", func() {
		fake.answer(http.StatusBadGateway)
		delivery := queue(`{"version":"v1","type":"deploy.succeeded"}`)

		reconcileDelivery(delivery)
		now = now.Add(time.Minute)
		reconcileDelivery(delivery)

		Expect(fake.count()).To(Equal(2))
		fake.mu.Lock()
		defer fake.mu.Unlock()
		Expect(fake.requests[0].eventID).To(Equal(fake.requests[1].eventID))
		Expect(fake.requests[0].attempt).To(Equal("1"))
		Expect(fake.requests[1].attempt).To(Equal("2"))
	})

	It("leaves the object it is reporting on untouched when delivery fails", func() {
		// The guarantee the issue asks for, and the reason delivery is its own
		// object: a notification that cannot be delivered must not reach back
		// into the build, release or environment it is about.
		build := &kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{Name: buildName, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.BuildSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Git:        kitchenv1alpha1.GitRevision{SHA: "0f1e2d3c4b5a69788796a5b4c3d2e1f001234567", Branch: "main"},
			},
		}
		Expect(k8sClient.Create(ctx, build)).To(Succeed())
		defer func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, build))).To(Succeed())
		}()

		before := build.DeepCopy()

		fake.answer(http.StatusInternalServerError)
		delivery := queue(`{"version":"v1","type":"deploy.succeeded","build":"` + buildName + `"}`)
		for i := 0; i < 3; i++ {
			reconcileDelivery(delivery)
			now = now.Add(5 * time.Minute)
		}
		Expect(delivery.Status.Phase).To(Equal(kitchenv1alpha1.DeliveryDeadLettered))

		after := &kitchenv1alpha1.Build{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: buildName, Namespace: PlatformNamespace}, after)).To(Succeed())
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion),
			"a failed notification wrote to the object it was reporting on")
		Expect(after.Spec).To(Equal(before.Spec))
		Expect(after.Status).To(Equal(before.Status))
	})

	It("holds a suspended subscription's deliveries rather than dropping them", func() {
		delivery := queue(`{"version":"v1","type":"deploy.succeeded"}`)
		subscription.Spec.Suspended = true
		Expect(k8sClient.Update(ctx, subscription)).To(Succeed())

		reconcileDelivery(delivery)
		Expect(fake.count()).To(BeZero())
		Expect(delivery.Status.Attempts).To(BeZero())

		subscription.Spec.Suspended = false
		Expect(k8sClient.Update(ctx, subscription)).To(Succeed())
		reconcileDelivery(delivery)
		Expect(fake.count()).To(Equal(1), "what was queued while suspended is still sent afterwards")
	})

	It("does not spend the ladder on a signing key that is not there", func() {
		Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		delivery := queue(`{"version":"v1","type":"deploy.succeeded"}`)

		reconcileDelivery(delivery)
		Expect(fake.count()).To(BeZero())
		Expect(delivery.Status.Attempts).To(BeZero(),
			"a configuration fault is not a receiver failure and must not consume an attempt")

		Expect(k8sClient.Get(ctx, subscriptionKey, subscription)).To(Succeed())
		ready := meta.FindStatusCondition(subscription.Status.Conditions, condSubscriptionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Message).To(ContainSubstring("signing secret"))

		// Put it back so AfterEach's delete is a no-op rather than an error.
		secret.ResourceVersion = ""
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	})

	It("prunes a delivered notification once its retention has passed", func() {
		delivery := queue(`{"version":"v1","type":"deploy.succeeded"}`)
		reconcileDelivery(delivery)
		Expect(delivery.Status.Phase).To(Equal(kitchenv1alpha1.DeliveryDelivered))

		now = now.Add(deliveryFinishedTTL + time.Minute)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: delivery.Name, Namespace: delivery.Namespace},
		})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, types.NamespacedName{
			Name: delivery.Name, Namespace: delivery.Namespace}, delivery)
		Expect(errors.IsNotFound(err)).To(BeTrue(), "a delivered notification is not kept for ever")
	})
})

var _ = Describe("Notification subscription", func() {
	ctx := context.Background()

	const subscriptionName = "notify-subject"

	var (
		reconciler   *NotificationSubscriptionReconciler
		subscription *kitchenv1alpha1.NotificationSubscription
		secret       *corev1.Secret
	)

	key := types.NamespacedName{Name: subscriptionName, Namespace: PlatformNamespace}

	reconcileSubscription := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, subscription)).To(Succeed())
	}

	BeforeEach(func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())

		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kitchen-notify-" + subscriptionName,
				Namespace: PlatformNamespace,
			},
			Data: map[string][]byte{notify.SecretKey: []byte("a-signing-key-nobody-else-has")},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		subscription = &kitchenv1alpha1.NotificationSubscription{
			ObjectMeta: metav1.ObjectMeta{Name: subscriptionName, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.NotificationSubscriptionSpec{
				URL:       "https://relay.example.com/kitchen",
				Events:    []kitchenv1alpha1.NotificationEvent{kitchenv1alpha1.NotifyBuildFailed},
				SecretRef: kitchenv1alpha1.LocalObjectReference{Name: secret.Name},
			},
		}
		Expect(k8sClient.Create(ctx, subscription)).To(Succeed())
		reconciler = &NotificationSubscriptionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, subscription))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed())
	})

	It("reports a subscription that can deliver", func() {
		reconcileSubscription()
		ready := meta.FindStatusCondition(subscription.Status.Conditions, condSubscriptionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal("Subscribed"))
	})

	It("refuses a URL that is not https, and says why", func() {
		subscription.Spec.URL = "http://relay.example.com/kitchen"
		Expect(k8sClient.Update(ctx, subscription)).To(Succeed())

		reconcileSubscription()
		ready := meta.FindStatusCondition(subscription.Status.Conditions, condSubscriptionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("URLNotHTTPS"))
	})

	It("says a suspended subscription is suspended rather than broken", func() {
		subscription.Spec.Suspended = true
		Expect(k8sClient.Update(ctx, subscription)).To(Succeed())

		reconcileSubscription()
		ready := meta.FindStatusCondition(subscription.Status.Conditions, condSubscriptionReady)
		Expect(ready.Reason).To(Equal("Suspended"))
	})
})

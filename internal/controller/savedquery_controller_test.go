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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// countingLogs is the telemetry store's one answer an alert needs, without a
// store: how many lines the selection matched.
type countingLogs struct {
	mu       sync.Mutex
	count    uint64
	err      error
	askedFor []clickhouse.LogSelection
}

func (c *countingLogs) CountLogs(_ context.Context, selection clickhouse.LogSelection) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.askedFor = append(c.askedFor, selection)
	return c.count, c.err
}

func (c *countingLogs) answer(count uint64, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count, c.err = count, err
}

func (c *countingLogs) asked() []clickhouse.LogSelection {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]clickhouse.LogSelection(nil), c.askedFor...)
}

// recordedEvents is an activity.Sink that keeps what it was handed. It stands
// in for internal/notify, which is the real one: the reconciler's whole
// announcement is one recorded event, and what becomes of it afterwards is
// the subscriptions' business rather than this controller's.
type recordedEvents struct {
	mu     sync.Mutex
	events []clickhouse.Event
}

func (r *recordedEvents) Deliver(_ context.Context, event clickhouse.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordedEvents) ofType(kind string) []clickhouse.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	found := []clickhouse.Event{}
	for _, event := range r.events {
		if event.Type == kind {
			found = append(found, event)
		}
	}
	return found
}

var _ = Describe("Saved-query alerts", func() {
	ctx := context.Background()

	const queryName = "checkout-500s"

	var (
		reconciler *SavedQueryReconciler
		saved      *kitchenv1alpha1.SavedQuery
		logs       *countingLogs
		sink       *recordedEvents
		now        time.Time
	)

	key := types.NamespacedName{Name: queryName, Namespace: PlatformNamespace}

	evaluate := func() reconcile.Result {
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, saved)).To(Succeed())
		return result
	}

	// The event reaches the sink on the recorder's own goroutine — the
	// property everything else here relies on, since nothing waits for it —
	// so every assertion about one is eventual.
	firings := func() []clickhouse.Event {
		return sink.ofType(clickhouse.EventAlertFiring)
	}

	BeforeEach(func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())

		now = time.Date(2026, 9, 4, 2, 0, 0, 0, time.UTC)
		logs = &countingLogs{}
		sink = &recordedEvents{}

		saved = &kitchenv1alpha1.SavedQuery{
			ObjectMeta: metav1.ObjectMeta{Name: queryName, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.SavedQuerySpec{
				Title: "Checkout 500s",
				Query: "level:error service:shop",
				// What whoever saved it could see. An alert is a question
				// nobody is watching being asked, so the scope it may count
				// over is written down rather than assumed.
				Scope:        &kitchenv1alpha1.SavedQueryScope{Projects: []string{"shop"}},
				RangeMinutes: 60,
				Alert: &kitchenv1alpha1.SavedQueryAlert{
					WindowMinutes:   10,
					Threshold:       25,
					IntervalMinutes: 5,
				},
			},
		}
		Expect(k8sClient.Create(ctx, saved)).To(Succeed())

		reconciler = &SavedQueryReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Activity: &activity.Recorder{Client: k8sClient, Namespace: PlatformNamespace, Sink: sink},
			Counter:  logs,
			Now:      func() time.Time { return now },
		}
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, saved))).To(Succeed())
	})

	It("counts the alert's own window, not the query's reading window", func() {
		logs.answer(3, nil)
		evaluate()

		asked := logs.asked()
		Expect(asked).To(HaveLen(1))
		Expect(asked[0].Query).To(Equal("level:error service:shop"))
		Expect(asked[0].Scope).To(Equal(clickhouse.LogScope{Projects: []string{"shop"}}),
			"an alert counts over the scope its author had, never over the whole store")
		Expect(asked[0].Until.Sub(asked[0].Since)).To(Equal(10*time.Minute),
			"an alert evaluates over spec.alert.windowMinutes; rangeMinutes is what a person reads")
	})

	It("says nothing while the count is under the threshold", func() {
		logs.answer(3, nil)
		result := evaluate()

		Expect(saved.Status.Firing).To(BeFalse())
		Expect(saved.Status.LastCount).To(Equal(int64(3)))
		Expect(saved.Status.LastEvaluationTime).NotTo(BeNil())
		Expect(saved.Status.Message).To(ContainSubstring("fires above 25"))
		Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
		Consistently(firings).WithTimeout(500 * time.Millisecond).
			WithPolling(50 * time.Millisecond).Should(BeEmpty())
	})

	It("records the crossing once, on the edge, and not again while it stays crossed", func() {
		logs.answer(63, nil)
		evaluate()

		Expect(saved.Status.Firing).To(BeTrue())
		Expect(saved.Status.FiringSince).NotTo(BeNil())
		Expect(saved.Status.LastCount).To(Equal(int64(63)))

		Eventually(func() int { return len(firings()) }).
			WithTimeout(5 * time.Second).WithPolling(20 * time.Millisecond).Should(Equal(1))
		fired := firings()[0]
		Expect(fired.Message).To(ContainSubstring("Checkout 500s"))
		Expect(fired.Value).To(Equal(float64(63)))

		// Still crossed, five minutes later: one alert is one message.
		now = now.Add(5 * time.Minute)
		logs.answer(71, nil)
		evaluate()
		Expect(saved.Status.LastCount).To(Equal(int64(71)))
		Consistently(func() int { return len(firings()) }).
			WithTimeout(500 * time.Millisecond).WithPolling(50 * time.Millisecond).Should(Equal(1))

		// It falls back under, and crosses again: that is a second edge.
		now = now.Add(5 * time.Minute)
		logs.answer(2, nil)
		evaluate()
		Expect(saved.Status.Firing).To(BeFalse())
		Expect(saved.Status.FiringSince).To(BeNil())

		now = now.Add(5 * time.Minute)
		logs.answer(40, nil)
		evaluate()
		Eventually(func() int { return len(firings()) }).
			WithTimeout(5 * time.Second).WithPolling(20 * time.Millisecond).Should(Equal(2))
	})

	It("fires below the threshold when that is the question — the heartbeat", func() {
		saved.Spec.Alert.Comparison = kitchenv1alpha1.AlertComparisonBelow
		saved.Spec.Alert.Threshold = 1
		Expect(k8sClient.Update(ctx, saved)).To(Succeed())

		logs.answer(0, nil)
		evaluate()
		Expect(saved.Status.Firing).To(BeTrue(), "nothing logged at all is what a heartbeat watches for")
	})

	It("waits out the interval rather than counting on every reconcile", func() {
		logs.answer(3, nil)
		evaluate()

		now = now.Add(time.Minute)
		result := evaluate()
		Expect(logs.asked()).To(HaveLen(1), "an evaluation that is not due is not made")
		Expect(result.RequeueAfter).To(Equal(4 * time.Minute))

		now = now.Add(4 * time.Minute)
		evaluate()
		Expect(logs.asked()).To(HaveLen(2))
	})

	It("says why an evaluation could not be made rather than staying silently quiet", func() {
		logs.answer(0, fmt.Errorf("the telemetry store refused the query"))
		evaluate()

		Expect(saved.Status.Firing).To(BeFalse())
		Expect(saved.Status.Message).To(ContainSubstring("not evaluated"))
		Expect(saved.Status.Message).To(ContainSubstring("refused the query"))
		Consistently(firings).WithTimeout(500 * time.Millisecond).
			WithPolling(50 * time.Millisecond).Should(BeEmpty())
	})

	// Issue #421: the alert used to be evaluated with no scope whatsoever and
	// with whatever ClickHouse expression the saved query carried, and it
	// published the count on the object for anyone the query is listed to. The
	// three ways a query can no longer be asked all fail the same way: nothing
	// is counted, nothing stays firing, and the object says why.
	DescribeTable("refuses to evaluate a question it cannot ask safely",
		func(prepare func(), reason string) {
			logs.answer(9000, nil)
			// It was firing on the old rules, so the refusal has to take that
			// back rather than leave the last number standing.
			saved.Status.Firing = true
			saved.Status.LastCount = 9000
			Expect(k8sClient.Status().Update(ctx, saved)).To(Succeed())

			prepare()
			Expect(k8sClient.Update(ctx, saved)).To(Succeed())
			evaluate()

			Expect(logs.asked()).To(BeEmpty(), "the store must not be asked at all")
			Expect(saved.Status.Firing).To(BeFalse())
			Expect(saved.Status.LastCount).To(BeZero(),
				"a count taken under the old rules must not stay published")
			Expect(saved.Status.Message).To(ContainSubstring("not evaluated"))
			Expect(saved.Status.Message).To(ContainSubstring(reason))
			Consistently(firings).WithTimeout(500 * time.Millisecond).
				WithPolling(50 * time.Millisecond).Should(BeEmpty())
		},
		Entry("one saved before a scope was recorded", func() {
			saved.Spec.Scope = nil
		}, "no project scope was recorded"),
		Entry("one whose author could see nothing", func() {
			saved.Spec.Scope = &kitchenv1alpha1.SavedQueryScope{}
		}, "no project scope was recorded"),
		Entry("one still carrying the raw ClickHouse filter", func() {
			saved.Spec.Where = "project = 'billing'"
		}, "no longer evaluated"),
	)

	// A query the language has moved under is the caller's to fix and nothing
	// that happens later fixes it, so it is a permanent refusal rather than a
	// store that will be back in a minute.
	It("fails an alert whose query no longer compiles, rather than counting", func() {
		logs.answer(0, &clickhouse.LogQueryError{Message: "a bracket is never closed"})
		saved.Status.LastCount = 42
		Expect(k8sClient.Status().Update(ctx, saved)).To(Succeed())
		evaluate()

		Expect(saved.Status.LastCount).To(BeZero())
		Expect(saved.Status.Firing).To(BeFalse())
		Expect(saved.Status.Message).To(ContainSubstring("a bracket is never closed"))
	})

	// A store that cannot be reached is the other kind: the question is still
	// a good one, so the last count it produced stays where a person can see
	// it and the message says why there is no newer one.
	It("keeps the last count when it is the store that could not answer", func() {
		logs.answer(3, nil)
		evaluate()
		Expect(saved.Status.LastCount).To(Equal(int64(3)))

		now = now.Add(10 * time.Minute)
		logs.answer(0, fmt.Errorf("dial tcp: connection refused"))
		evaluate()
		Expect(saved.Status.LastCount).To(Equal(int64(3)))
		Expect(saved.Status.Message).To(ContainSubstring("connection refused"))
	})

	It("does nothing at all for a query with no alert, or a suspended one", func() {
		saved.Spec.Alert = nil
		Expect(k8sClient.Update(ctx, saved)).To(Succeed())
		result := evaluate()
		Expect(result.RequeueAfter).To(BeZero())
		Expect(logs.asked()).To(BeEmpty())
		Expect(saved.Status.LastEvaluationTime).To(BeNil())

		saved.Spec.Alert = &kitchenv1alpha1.SavedQueryAlert{
			WindowMinutes: 10, Threshold: 0, Suspended: true,
		}
		Expect(k8sClient.Update(ctx, saved)).To(Succeed())
		evaluate()
		Expect(logs.asked()).To(BeEmpty())
	})
})

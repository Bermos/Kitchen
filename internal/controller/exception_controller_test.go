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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
)

var _ = Describe("Exception Controller", func() {
	const namespace = PlatformNamespace

	ctx := context.Background()

	var reconciler *ExceptionReconciler

	key := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: namespace}
	}

	newException := func(name string, expiresIn time.Duration) *kitchenv1alpha1.Exception {
		return &kitchenv1alpha1.Exception{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ExceptionSpec{
				ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: "shop"},
				EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: "shop-production"},
				RuleIDs:        []string{"max-severity"},
				Reason:         "hotfix for the checkout outage",
				RequestedBy:    "grace@example.com",
				ApprovedBy:     "heidi@example.com",
				ExpiresAt:      metav1.NewTime(time.Now().Add(expiresIn).Truncate(time.Second)),
			},
		}
	}

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())
		reconciler = &ExceptionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	AfterEach(func() {
		for _, name := range []string{"exc-live", "exc-lapsed", "exc-scoped", "exc-done"} {
			obj := &kitchenv1alpha1.Exception{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("stamps a fresh exception Active and wakes itself for the expiry moment", func() {
		Expect(k8sClient.Create(ctx, newException("exc-live", time.Hour))).To(Succeed())

		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key("exc-live")})
		Expect(err).NotTo(HaveOccurred())
		// The requeue is the expiry machinery: no ticker, one wake-up at the
		// moment the grant runs out.
		Expect(result.RequeueAfter).To(BeNumerically(">", 50*time.Minute))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Hour+time.Minute))

		exception := &kitchenv1alpha1.Exception{}
		Expect(k8sClient.Get(ctx, key("exc-live"), exception)).To(Succeed())
		Expect(exception.Status.Phase).To(Equal(kitchenv1alpha1.ExceptionActive))
	})

	It("flips a lapsed exception to Expired and stops", func() {
		lapsed := newException("exc-lapsed", -time.Minute)
		Expect(k8sClient.Create(ctx, lapsed)).To(Succeed())

		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key("exc-lapsed")})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero(), "expired is terminal; nothing to wake for")

		exception := &kitchenv1alpha1.Exception{}
		Expect(k8sClient.Get(ctx, key("exc-lapsed"), exception)).To(Succeed())
		Expect(exception.Status.Phase).To(Equal(kitchenv1alpha1.ExceptionExpired))

		// A second pass changes nothing: the flip was recorded once.
		version := exception.ResourceVersion
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key("exc-lapsed")})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key("exc-lapsed"), exception)).To(Succeed())
		Expect(exception.ResourceVersion).To(Equal(version))
	})

	It("leaves a resolved exception resolved, past its expiry included", func() {
		done := newException("exc-done", -time.Minute)
		Expect(k8sClient.Create(ctx, done)).To(Succeed())
		Expect(k8sClient.Get(ctx, key("exc-done"), done)).To(Succeed())
		done.Status.Phase = kitchenv1alpha1.ExceptionResolved
		done.Status.ResolvedBy = "heidi@example.com"
		Expect(k8sClient.Status().Update(ctx, done)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key("exc-done")})
		Expect(err).NotTo(HaveOccurred())

		exception := &kitchenv1alpha1.Exception{}
		Expect(k8sClient.Get(ctx, key("exc-done"), exception)).To(Succeed())
		Expect(exception.Status.Phase).To(Equal(kitchenv1alpha1.ExceptionResolved),
			"somebody ended it; the clock does not reopen it as Expired")
	})

	It("refuses a one-person exception and an edited one, at admission", func() {
		selfApproved := newException("exc-live", time.Hour)
		selfApproved.Spec.ApprovedBy = selfApproved.Spec.RequestedBy
		err := k8sClient.Create(ctx, selfApproved)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("two people"))

		Expect(k8sClient.Create(ctx, newException("exc-live", time.Hour))).To(Succeed())
		stored := &kitchenv1alpha1.Exception{}
		Expect(k8sClient.Get(ctx, key("exc-live"), stored)).To(Succeed())
		stored.Spec.Reason = "edited after the fact"
		err = k8sClient.Update(ctx, stored)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Exception spec is immutable"))
	})

	It("lists exactly the grants in scope, through the one shared implementation", func() {
		Expect(k8sClient.Create(ctx, newException("exc-live", time.Hour))).To(Succeed())
		Expect(k8sClient.Create(ctx, newException("exc-lapsed", -time.Minute))).To(Succeed())
		scoped := newException("exc-scoped", time.Hour)
		scoped.Spec.ReleaseRef = &kitchenv1alpha1.LocalObjectReference{Name: "shop-rel-9"}
		Expect(k8sClient.Create(ctx, scoped)).To(Succeed())

		// The whole environment: the release-scoped grant covers shop-rel-9
		// alone, the lapsed one covers nothing.
		active, err := ActiveExceptionsFor(ctx, k8sClient, namespace,
			"shop", "shop-production", "shop-rel-1", time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(active).To(HaveLen(1))
		Expect(active[0].Name).To(Equal("exc-live"))

		// The named release picks the scoped grant up too.
		active, err = ActiveExceptionsFor(ctx, k8sClient, namespace,
			"shop", "shop-production", "shop-rel-9", time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(active).To(HaveLen(2))

		// Another environment: nothing carries over.
		active, err = ActiveExceptionsFor(ctx, k8sClient, namespace,
			"shop", "shop-staging", "shop-rel-1", time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(active).To(BeEmpty())
	})

	It("words the expiry record so it can stand on its own", func() {
		exception := newException("exc-lapsed", -time.Minute)
		exception.Status.UsedBy = []string{"shop-promo-7"}
		transition := exceptionExpiryTransition(exception)
		Expect(transition.Kind).To(Equal("Exception"))
		Expect(transition.From).To(Equal("Active"))
		Expect(transition.To).To(Equal("Expired"))
		Expect(transition.Privileged).To(Equal(audit.PrivilegeBreakGlass))
		Expect(transition.Details["ruleIDs"]).To(Equal([]string{"max-severity"}))
		Expect(transition.Details["usedBy"]).To(Equal([]string{"shop-promo-7"}))
	})
})

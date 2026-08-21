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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The promotion surface: the API creates the request and the reconciler
// decides — so what these tests pin is the asking. The one place the API
// itself routes through a promotion is the environment PATCH against a
// target that declares requirements, and that answer must be honest: 202,
// the promotion, and an environment that has not moved.

// storedPromotion is a Promotion fixture with an explicit creation time, so
// list ordering is decided by the test rather than by the fake client.
func storedPromotion(name, project, environment, release string, phase kitchenv1alpha1.PromotionPhase, age time.Duration) *kitchenv1alpha1.Promotion {
	return &kitchenv1alpha1.Promotion{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Spec: kitchenv1alpha1.PromotionSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: project},
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: environment},
			ReleaseRef:     kitchenv1alpha1.LocalObjectReference{Name: release},
			RequestedBy:    "system:controller/build",
			Trigger:        kitchenv1alpha1.PromotionAutomatic,
		},
		Status: kitchenv1alpha1.PromotionStatus{Phase: phase},
	}
}

func TestPromoteCreatesAPendingPromotion(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/promotions",
		`{"environment":"`+testEnvironment+`","release":"`+testPreviousRelease+`","reason":"put 1.3 back"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[promotionView](t, recorder)
	if view.Phase != string(kitchenv1alpha1.PromotionPending) {
		t.Fatalf("a fresh promotion is Pending, got %q", view.Phase)
	}
	if view.Trigger != string(kitchenv1alpha1.PromotionManual) || view.RequestedBy != testCaller {
		t.Fatalf("the asking is attributed to the caller: %+v", view)
	}
	if view.Environment != testEnvironment || view.Release != testPreviousRelease || view.Reason != "put 1.3 back" {
		t.Fatalf("the view must echo the ask: %+v", view)
	}

	stored := &kitchenv1alpha1.Promotion{}
	if err := h.server.Client.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: view.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Annotations[requestedByAnnotation]; got != testCaller {
		t.Fatalf("the object carries who asked, got %q", got)
	}
	if stored.Labels["kitchen.bermos.dev/project"] != feedProject {
		t.Fatalf("the object is labelled with its project, got %v", stored.Labels)
	}
}

func TestPromoteRefusesWhatDoesNotBelongToTheProject(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), blogFixtures()...)...)

	// A release of another project: the body names something that is not the
	// project's, which is a 400 — not a promotion that fails later.
	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/promotions",
		`{"environment":"`+testEnvironment+`","release":"blog-rel-0"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "belongs to project") {
		t.Fatalf("the refusal must say whose it is, got %q", got)
	}

	// An environment that does not exist.
	recorder = h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/promotions",
		`{"environment":"shop-qa","release":"`+testRelease+`"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}

	promotions := &kitchenv1alpha1.PromotionList{}
	if err := h.server.Client.List(context.Background(), promotions); err != nil {
		t.Fatal(err)
	}
	if len(promotions.Items) != 0 {
		t.Fatalf("a refused ask must create nothing, got %d", len(promotions.Items))
	}
}

func TestPromotionsListNewestFirstWithFilters(t *testing.T) {
	extra := []runtime.Object{
		storedPromotion("shop-promo-old", feedProject, testEnvironment, testPreviousRelease,
			kitchenv1alpha1.PromotionApplied, 2*time.Hour),
		storedPromotion("shop-promo-new", feedProject, testEnvironment, testRelease,
			kitchenv1alpha1.PromotionBlocked, time.Hour),
		storedPromotion("blog-promo", otherProject, "blog-production", "blog-rel-0",
			kitchenv1alpha1.PromotionApplied, time.Hour),
	}
	h := newHarness(t, nil, append(fixtures(), extra...)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/projects/"+feedProject+"/promotions", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	answer := decode[listBody[promotionView]](t, recorder)
	if len(answer.Items) != 2 {
		t.Fatalf("only the project's promotions, got %+v", answer.Items)
	}
	if answer.Items[0].Name != "shop-promo-new" || answer.Items[1].Name != "shop-promo-old" {
		t.Fatalf("newest first, got %s then %s", answer.Items[0].Name, answer.Items[1].Name)
	}

	recorder = h.do(t, http.MethodGet, "/api/v1/projects/"+feedProject+"/promotions?phase=Blocked", "")
	answer = decode[listBody[promotionView]](t, recorder)
	if len(answer.Items) != 1 || answer.Items[0].Name != "shop-promo-new" {
		t.Fatalf("the phase filter must narrow, got %+v", answer.Items)
	}

	recorder = h.do(t, http.MethodGet, "/api/v1/projects/"+feedProject+"/promotions?release="+testPreviousRelease, "")
	answer = decode[listBody[promotionView]](t, recorder)
	if len(answer.Items) != 1 || answer.Items[0].Name != "shop-promo-old" {
		t.Fatalf("the release filter must narrow, got %+v", answer.Items)
	}
}

func TestAPromotionOfAnInvisibleProjectIsNotFound(t *testing.T) {
	// A member with a role on shop and none on blog: blog's promotion answers
	// the same not-found a missing one gets, never a 403 that confirms the
	// name exists.
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, append(blogFixtures(),
		storedPromotion("blog-promo", otherProject, "blog-production", "blog-rel-0",
			kitchenv1alpha1.PromotionApplied, time.Hour))...)

	recorder := h.do(t, http.MethodGet, "/api/v1/promotions/blog-promo", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = h.do(t, http.MethodGet, "/api/v1/promotions/no-such-promotion", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("a genuinely missing one answers alike, got %d", recorder.Code)
	}
}

func TestAViewerMayReadPromotionsAndNotAsk(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)

	recorder := h.do(t, http.MethodGet, "/api/v1/projects/"+feedProject+"/promotions", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("reading is a viewer's, got %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/promotions",
		`{"environment":"`+testEnvironment+`","release":"`+testPreviousRelease+`"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("asking needs developer, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestMovingAGatedEnvironmentBecomesAPromotion(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.updateEnvironment(t, func(env *kitchenv1alpha1.Environment) {
		env.Spec.Requirements = &kitchenv1alpha1.EnvironmentRequirements{
			BundleDigest: testBundleDigest,
		}
	})

	recorder := h.do(t, http.MethodPatch, "/api/v1/environments/"+testEnvironment,
		`{"release":"`+testPreviousRelease+`","reason":"back out 1.4"}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[promotionView](t, recorder)
	if view.Phase != string(kitchenv1alpha1.PromotionPending) || view.Release != testPreviousRelease {
		t.Fatalf("the answer is the pending promotion, got %+v", view)
	}
	if view.Trigger != string(kitchenv1alpha1.PromotionManual) || view.Reason != "back out 1.4" {
		t.Fatalf("the ask travels onto the promotion, got %+v", view)
	}

	// Nothing moved: the environment still runs what it ran, and the record
	// of the ask exists as a Promotion instead.
	if env := h.environment(t); env.Spec.ReleaseRef.Name != testRelease {
		t.Fatalf("a gated environment must not be moved by the API, got %q", env.Spec.ReleaseRef.Name)
	}
	stored := &kitchenv1alpha1.Promotion{}
	if err := h.server.Client.Get(context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: view.Name}, stored); err != nil {
		t.Fatal(err)
	}
}

func TestAnUngatedEnvironmentStillMovesDirectly(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/environments/"+testEnvironment,
		`{"release":"`+testPreviousRelease+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if env := h.environment(t); env.Spec.ReleaseRef.Name != testPreviousRelease {
		t.Fatalf("today's fast path must be untouched, got %q", env.Spec.ReleaseRef.Name)
	}
}

func TestAPromotionTheLogCannotRecordIsNotCreated(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withUnreachableAuditLog(t)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/promotions",
		`{"environment":"`+testEnvironment+`","release":"`+testPreviousRelease+`"}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
	promotions := &kitchenv1alpha1.PromotionList{}
	if err := h.server.Client.List(context.Background(), promotions); err != nil {
		t.Fatal(err)
	}
	if len(promotions.Items) != 0 {
		t.Fatalf("an unrecorded ask must not be made, got %d", len(promotions.Items))
	}
}

// The audit record of the asking, held up to the light without a store.
func TestThePromotionTransitionCarriesTheAsk(t *testing.T) {
	promotion := &kitchenv1alpha1.Promotion{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-promo-x", Namespace: testNamespace},
		Spec: kitchenv1alpha1.PromotionSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: feedProject},
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: testEnvironment},
			ReleaseRef:     kitchenv1alpha1.LocalObjectReference{Name: testRelease},
			RequestedBy:    testCaller,
			Trigger:        kitchenv1alpha1.PromotionManual,
			Reason:         "ship 1.4",
		},
	}
	transition := promotionTransition(promotion)
	if transition.Kind != "Promotion" || transition.Project != feedProject || transition.To != testRelease {
		t.Fatalf("unexpected transition: %+v", transition)
	}
	if transition.Details["environment"] != testEnvironment || transition.Details["trigger"] != "manual" {
		t.Fatalf("the details must carry the ask: %+v", transition.Details)
	}
	if transition.Details["reason"] != "ship 1.4" {
		t.Fatalf("the reason is part of the record: %+v", transition.Details)
	}
}

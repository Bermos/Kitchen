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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// Growing one of the platform's own volumes, against what #533 asks of it: the
// operation exists in the API rather than as four kubectl invocations, it is
// refused where it cannot work rather than accepted and quietly abandoned, and
// the screen can tell one from the other before anybody presses anything.

const (
	storeClaimName   = "data-kitchen-clickhouse-0"
	storeSetName     = "kitchen-clickhouse"
	expandingClass   = "fast-expands"
	fixedClass       = "fast-fixed"
	resizeRoute      = "/api/v1/platform/storage/claims/" + storeClaimName + "/resize"
	twentyGigabytes  = "20Gi"
	requestForEighty = `{"size": "80Gi"}`
)

// storageFixtures is one platform StatefulSet, the claim its template made,
// and two storage classes: one that expands and one that does not.
func storageFixtures(className string) []runtime.Object {
	return storageFixturesInPhase(className, corev1.ClaimBound)
}

// storageFixturesInPhase is the same, with the claim in a phase of the
// caller's choosing — an unbound claim cannot be expanded at all.
func storageFixturesInPhase(className string, phase corev1.PersistentVolumeClaimPhase) []runtime.Object {
	return []runtime.Object{
		&storagev1.StorageClass{
			ObjectMeta:           metav1.ObjectMeta{Name: expandingClass},
			Provisioner:          "kitchen.test/none",
			AllowVolumeExpansion: ptr.To(true),
		},
		&storagev1.StorageClass{
			ObjectMeta:           metav1.ObjectMeta{Name: fixedClass},
			Provisioner:          "kitchen.test/none",
			AllowVolumeExpansion: ptr.To(false),
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   controller.PlatformNamespace,
				Name:        storeSetName,
				Annotations: map[string]string{controller.StorageSizeAnnotation: twentyGigabytes},
			},
			Spec: appsv1.StatefulSetSpec{
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
				}},
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: controller.PlatformNamespace,
				Name:      storeClaimName,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: ptr.To(className),
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(twentyGigabytes)},
				},
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase:    phase,
				Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(twentyGigabytes)},
			},
		},
	}
}

func TestResizingAPlatformVolumeRecordsTheSizeAndAnswersAccepted(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), storageFixtures(expandingClass)...)...)

	res := h.do(t, http.MethodPost, resizeRoute, requestForEighty)
	if res.Code != http.StatusAccepted {
		t.Fatalf("POST resize = %d: %s", res.Code, res.Body.String())
	}
	accepted := decode[resizeAcceptedView](t, res)
	if accepted.StatefulSet != storeSetName || accepted.Desired != "80Gi" || accepted.Current != twentyGigabytes {
		t.Fatalf("the answer says what was accepted against what is running: %+v", accepted)
	}

	// The route writes a size and nothing else — everything that has to happen
	// in order is the operator's, which is why this answers 202.
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	asked, ok := kitchen.Spec.Storage.Volumes[storeSetName]
	if !ok || asked.Cmp(resource.MustParse("80Gi")) != 0 {
		t.Fatalf("the singleton carries the size the operator acts on: %+v", kitchen.Spec.Storage)
	}

	claim := &corev1.PersistentVolumeClaim{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: controller.PlatformNamespace, Name: storeClaimName}, claim); err != nil {
		t.Fatal(err)
	}
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != twentyGigabytes {
		t.Errorf("the API expands nothing itself; the reconciler does: %s", got.String())
	}
}

func TestResizingAPlatformVolumeRefusesWhatCannotWork(t *testing.T) {
	for _, refusal := range []struct {
		name      string
		className string
		route     string
		body      string
		unbound   bool
		says      string
	}{
		{
			name:      "a size at or below what it already asks for",
			className: expandingClass,
			route:     resizeRoute,
			body:      `{"size": "10Gi"}`,
			says:      "only ever grown",
		},
		{
			name:      "a size that is not a size",
			className: expandingClass,
			route:     resizeRoute,
			body:      `{"size": "lots"}`,
			says:      "positive Kubernetes quantity",
		},
		{
			name:      "a storage class that does not expand",
			className: fixedClass,
			route:     resizeRoute,
			body:      requestForEighty,
			says:      fixedClass + " does not allow volume expansion",
		},
		{
			name:      "a volume no platform workload created",
			className: expandingClass,
			route:     "/api/v1/platform/storage/claims/somebody-elses/resize",
			body:      requestForEighty,
			says:      "not one of the platform's own volumes",
		},
		{
			// Growing cannot be undone, so a step big enough to be a typed
			// unit rather than a decision is refused with the ceiling.
			name:      "a size more than eight times the current one",
			className: expandingClass,
			route:     resizeRoute,
			body:      `{"size": "500Gi"}`,
			says:      "times that",
		},
		{
			name:      "a claim that is not bound",
			className: expandingClass,
			route:     resizeRoute,
			body:      requestForEighty,
			unbound:   true,
			says:      "can only be grown once it is bound",
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			phase := corev1.ClaimBound
			if refusal.unbound {
				phase = corev1.ClaimPending
			}
			objects := append(fixtures(), storageFixturesInPhase(refusal.className, phase)...)
			if strings.Contains(refusal.route, "somebody-elses") {
				objects = append(objects, &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: controller.PlatformNamespace, Name: "somebody-elses",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						StorageClassName: ptr.To(expandingClass),
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(twentyGigabytes),
							},
						},
					},
					Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
				})
			}
			h := newHarness(t, nil, objects...)

			res := h.do(t, http.MethodPost, refusal.route, refusal.body)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("POST resize = %d, want 400: %s", res.Code, res.Body.String())
			}
			if !strings.Contains(res.Body.String(), refusal.says) {
				t.Errorf("the refusal has to say why: %s", res.Body.String())
			}

			// Nothing was written on the way to a refusal.
			kitchen := &kitchenv1alpha1.Kitchen{}
			if err := h.server.Client.Get(context.Background(),
				types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
				t.Fatal(err)
			}
			if len(kitchen.Spec.Storage.Volumes) != 0 {
				t.Errorf("a refused resize records nothing: %+v", kitchen.Spec.Storage)
			}
		})
	}
}

// A project's claim is not in the platform's namespace, so the route does not
// find it at all — 404, like everything else in this API a caller may not act
// on, rather than a 400 explaining what it would have refused.
func TestResizingAVolumeOutsideThePlatformsNamespaceIsNotFound(t *testing.T) {
	project := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kitchen-shop", Name: "shop-data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To(expandingClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(twentyGigabytes)},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	h := newHarness(t, nil, append(append(fixtures(), storageFixtures(expandingClass)...), project)...)

	res := h.do(t, http.MethodPost, "/api/v1/platform/storage/claims/shop-data/resize", requestForEighty)
	if res.Code != http.StatusNotFound {
		t.Fatalf("POST resize on a project's claim = %d, want 404: %s", res.Code, res.Body.String())
	}
}

func TestPlatformStorageSaysWhetherAVolumeCanBeGrownAndWhatIsHappeningToIt(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), storageFixtures(fixedClass)...)...)

	kitchen := &kitchenv1alpha1.Kitchen{}
	ctx := context.Background()
	if err := h.server.Client.Get(ctx,
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Status.Storage = &kitchenv1alpha1.PlatformStorageStatus{
		Volumes: []kitchenv1alpha1.PlatformVolumeStatus{{
			StatefulSet: storeSetName,
			Claims:      []string{storeClaimName},
			Current:     twentyGigabytes,
			Desired:     "80Gi",
			Phase:       "Blocked",
			Message:     "growing this volume from 20Gi to 80Gi needs a storage class that allows volume expansion",
		}},
	}
	if err := h.server.Client.Update(ctx, kitchen); err != nil {
		t.Fatal(err)
	}

	res := h.do(t, http.MethodGet, "/api/v1/platform/storage", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /platform/storage = %d: %s", res.Code, res.Body.String())
	}
	body := decode[platformStorageBody](t, res)

	var store volumeView
	for _, item := range body.Items {
		if item.Name == storeClaimName {
			store = item
		}
	}
	if store.Expandable == nil || *store.Expandable {
		t.Fatalf("a class that does not expand is answered as a no, not as a silence: %+v", store.Expandable)
	}
	if store.Resize == nil || store.Resize.Phase != "Blocked" || store.Resize.Desired != "80Gi" {
		t.Fatalf("the row carries what the platform is doing about its size: %+v", store.Resize)
	}
	if !strings.Contains(store.Resize.Message, "allows volume expansion") {
		t.Errorf("and why it is not doing it: %+v", store.Resize)
	}
}

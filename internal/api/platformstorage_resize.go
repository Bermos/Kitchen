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
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// Growing one of the platform's own volumes.
//
// This is the operation an operator reaches for the moment `pvc.filling` fires
// on a platform volume, and until #533 the only route to it was four kubectl
// invocations against the cluster this platform exists to abstract away — a
// StatefulSet's claim template is immutable, so `helm upgrade --set
// clickhouse.persistence.size=40Gi` fails on it and the volume has to be
// expanded, the StatefulSet orphan-deleted, and the release upgraded again.
//
// So the route writes a size and nothing else. Everything that has to happen
// in order is `KitchenReconciler`'s — it is under none of Helm's constraints
// and, unlike a Helm release, it can wait — which is why the answer is `202`
// and the outcome is read back off the same screen as a condition, rather than
// being reported here as though it were finished.
//
// Two refusals are made here rather than left to the reconciler, because a
// screen whose button is always a condition later is a screen that lies: a
// size smaller than the volume already is, and a storage class that does not
// admit expansion at all.

// maxGrowthFactor bounds one resize. Growing a volume is irreversible — a
// smaller one is a new volume and a restore — and the only way back from a
// mistyped unit is editing the Kitchen singleton by hand, which is exactly the
// thing this route exists to remove. Eight times the current size is more than
// any deliberate step and less than any slip of the keyboard: 50Gi to 500Gi is
// refused, 50Gi to 400Gi is not.
const maxGrowthFactor = 8

// resizeVolumeRequest is the whole of the body: how big it should be.
type resizeVolumeRequest struct {
	Size string `json:"size"`
}

// resizeAcceptedView is what a `202` carries: what was accepted, against what
// is running, and where the outcome will appear.
type resizeAcceptedView struct {
	Claim       string `json:"claim"`
	StatefulSet string `json:"statefulSet"`
	Current     string `json:"current"`
	Desired     string `json:"desired"`
	Message     string `json:"message"`
}

// resizePlatformVolume records how big one of the platform's own volumes
// should be.
func (s *Server) resizePlatformVolume(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	name := req.PathValue("name")

	body := resizeVolumeRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	desired, err := resource.ParseQuantity(strings.TrimSpace(body.Size))
	if err != nil || desired.Sign() <= 0 {
		badRequest(w, "size must be a positive Kubernetes quantity such as \"40Gi\" (got %q)", body.Size)
		return
	}

	// Only the platform's own namespace is looked in: a project's claim is
	// not one this platform grows, and answering 404 for one is the same
	// answer this API gives for anything a caller may not act on.
	claim := &corev1.PersistentVolumeClaim{}
	if err := s.reader().Get(ctx,
		client.ObjectKey{Namespace: controller.PlatformNamespace, Name: name}, claim); err != nil {
		s.writeError(w, err)
		return
	}

	set, err := s.statefulSetBehind(ctx, claim)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if set == nil {
		badRequest(w, "%s is not one of the platform's own volumes: only a volume a platform "+
			"StatefulSet created can be grown from here, because growing one means rewriting "+
			"that StatefulSet's claim template", name)
		return
	}

	if claim.Status.Phase != corev1.ClaimBound {
		badRequest(w, "%s is %s, and a volume can only be grown once it is bound: "+
			"the expansion happens underneath a volume that exists",
			name, strings.ToLower(string(claim.Status.Phase)))
		return
	}

	current, hasCurrent := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if hasCurrent && desired.Cmp(current) <= 0 {
		badRequest(w, "%s already requests %s: a volume is only ever grown, because shrinking one "+
			"means replacing it and restoring what was on it", name, current.String())
		return
	}

	if hasCurrent {
		ceiling := current.DeepCopy()
		ceiling.Set(current.Value() * maxGrowthFactor)
		if desired.Cmp(ceiling) > 0 {
			badRequest(w, "%s asks for %s, and %s is more than %d times that: growing a volume cannot "+
				"be undone, so a step this large is refused in case it is a typed unit rather than a "+
				"decision. Grow it to %s or less, and again from there",
				name, current.String(), desired.String(), maxGrowthFactor, ceiling.String())
			return
		}
	}

	expandable, className, err := s.storageClassExpands(ctx, claim)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if !expandable {
		badRequest(w, "%s cannot be grown: %s, and expansion is what growing a bound volume is. "+
			"The volume can only be replaced, which is a restore rather than a resize",
			name, expansionRefusal(className))
		return
	}

	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	base := kitchen.DeepCopy()
	if kitchen.Spec.Storage.Volumes == nil {
		kitchen.Spec.Storage.Volumes = map[string]resource.Quantity{}
	}
	if already, ok := kitchen.Spec.Storage.Volumes[set.Name]; ok && already.Cmp(desired) >= 0 {
		writeJSON(w, http.StatusAccepted, resizeAcceptedView{
			Claim:       name,
			StatefulSet: set.Name,
			Current:     quantityString(claim.Spec.Resources.Requests, corev1.ResourceStorage),
			Desired:     already.String(),
			Message: fmt.Sprintf("%s is already asked to be %s, and the platform is still working on it",
				set.Name, already.String()),
		})
		return
	}
	kitchen.Spec.Storage.Volumes[set.Name] = desired

	if !s.recorded(w, req, audit.Transition{
		Object:    kitchen,
		Kind:      audit.KindKitchen,
		Operation: clickhouse.AuditUpdate,
		Reason:    fmt.Sprintf("%s is to be grown to %s", name, desired.String()),
		Details: map[string]any{
			"change":      "volumeSize",
			"claim":       name,
			"statefulSet": set.Name,
			"size":        desired.String(),
		},
	}) {
		return
	}
	if err := s.Client.Patch(ctx, kitchen, client.MergeFrom(base)); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("a platform volume was asked to grow through the api",
		"claim", name, "statefulSet", set.Name, "size", desired.String(), "caller", callerName(caller))
	writeJSON(w, http.StatusAccepted, resizeAcceptedView{
		Claim:       name,
		StatefulSet: set.Name,
		Current:     quantityString(claim.Spec.Resources.Requests, corev1.ResourceStorage),
		Desired:     desired.String(),
		Message: fmt.Sprintf("the volume is being grown to %s; %s is replaced with a matching claim "+
			"template once the expansion is under way, and this screen reports the outcome",
			desired.String(), set.Name),
	})
}

// statefulSetBehind is the platform StatefulSet whose claim template produced
// this claim, or nil for a volume nothing here created.
//
// The claims a StatefulSet makes are named `<template>-<set>-<ordinal>` and
// nothing owner-references them, so the name is the whole of the relationship
// — the same reading the reconciler takes.
func (s *Server) statefulSetBehind(
	ctx context.Context,
	claim *corev1.PersistentVolumeClaim,
) (*appsv1.StatefulSet, error) {
	sets := &appsv1.StatefulSetList{}
	if err := s.reader().List(ctx, sets, client.InNamespace(claim.Namespace)); err != nil {
		return nil, err
	}
	for i := range sets.Items {
		set := &sets.Items[i]
		for j := range set.Spec.VolumeClaimTemplates {
			prefix := set.Spec.VolumeClaimTemplates[j].Name + "-" + set.Name + "-"
			if strings.HasPrefix(claim.Name, prefix) {
				return set, nil
			}
		}
	}
	return nil, nil
}

// storageClassExpands is whether the class behind one claim admits expansion,
// which is the one refusal neither this API nor the operator can work around.
func (s *Server) storageClassExpands(
	ctx context.Context,
	claim *corev1.PersistentVolumeClaim,
) (bool, string, error) {
	classes, err := s.expandableClasses(ctx)
	if err != nil {
		return false, "", err
	}
	name := storageClassOf(claim)
	if name == "" {
		name = classes.defaultClass
	}
	if name == "" {
		return false, "", nil
	}
	expands, known := classes.expands[name]
	return known && expands, name, nil
}

// expansionRefusal says why a volume cannot be grown, in a sentence.
func expansionRefusal(className string) string {
	if className == "" {
		return "the storage class it was bound by cannot be identified"
	}
	return className + " does not allow volume expansion"
}

// storageClasses is what this screen needs to know about the cluster's
// classes: which admit expansion, and which is the default a claim naming none
// was bound by.
type storageClasses struct {
	expands      map[string]bool
	defaultClass string
}

// expandableClasses reads the cluster's storage classes once.
func (s *Server) expandableClasses(ctx context.Context) (storageClasses, error) {
	list := &storagev1.StorageClassList{}
	if err := s.reader().List(ctx, list); err != nil {
		return storageClasses{}, err
	}
	classes := storageClasses{expands: make(map[string]bool, len(list.Items))}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		class := &list.Items[i]
		classes.expands[class.Name] = class.AllowVolumeExpansion != nil && *class.AllowVolumeExpansion
		if class.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			names = append(names, class.Name)
		}
	}
	// A cluster with two defaults is a misconfiguration the API server
	// resolves by the newest, and this is not the screen to argue about it;
	// naming one deterministically is enough to answer "can this be grown".
	sort.Strings(names)
	if len(names) > 0 {
		classes.defaultClass = names[0]
	}
	return classes, nil
}

// volumeResizes is what the operator is doing about each platform volume's
// size, keyed by claim name.
//
// The singleton is read again here rather than threaded through
// `storeHealth`: this is a different question about the same object, and one
// cached read is cheaper than a signature every future reader of this screen
// has to widen.
func (s *Server) volumeResizes(ctx context.Context) map[string]*volumeResizeView {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, client.ObjectKey{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		return nil
	}
	if kitchen.Status.Storage == nil {
		return nil
	}
	resizes := map[string]*volumeResizeView{}
	for _, volume := range kitchen.Status.Storage.Volumes {
		for _, claim := range volume.Claims {
			resizes[controller.PlatformNamespace+"/"+claim] = &volumeResizeView{
				StatefulSet: volume.StatefulSet,
				Desired:     volume.Desired,
				Phase:       volume.Phase,
				Message:     volume.Message,
			}
		}
	}
	return resizes
}

// volumeResizeView is what the platform is doing about one volume's size.
//
// It is present only for the platform's own volumes: a project's claim is not
// something this platform grows, and drawing a size somebody could ask for
// next to one nothing would act on is the screen lying about what it can do.
type volumeResizeView struct {
	// StatefulSet whose claim template this volume came from, which is what
	// has to be rewritten for the new size to outlive the pod.
	StatefulSet string `json:"statefulSet"`
	// Desired is the largest size anybody has asked for — the chart's, or
	// this API's. Equal to the row's `requested` once the platform has
	// caught up.
	Desired string `json:"desired,omitempty"`
	// Phase is `Settled`, `Growing` (the platform's half), `Resizing` (the
	// storage driver's half), `Blocked`, or `Unknown` where a read failed.
	Phase string `json:"phase,omitempty"`
	// Message explains a Blocked volume, and states the harmless case where
	// what is running is larger than what the chart asks for.
	Message string `json:"message,omitempty"`
}

// expandsFor is whether one named class admits expansion, or nil where the
// question could not be answered — the classes were unreadable, the claim
// names none and there is no default, or it names one that no longer exists.
func (c storageClasses) expandsFor(name string) *bool {
	if c.expands == nil {
		return nil
	}
	if name == "" {
		name = c.defaultClass
	}
	expands, known := c.expands[name]
	if !known {
		return nil
	}
	return &expands
}

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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Growing the platform's own volumes.
//
// A StatefulSet's `volumeClaimTemplates` are immutable, so `helm upgrade --set
// clickhouse.persistence.size=40Gi` used to fail on a rendered template that
// was otherwise correct, and the only route to a bigger telemetry volume was
// four kubectl invocations against the cluster this platform exists to
// abstract away (#533). The operator is under none of Helm's constraints —
// the same argument as `spec.scaleToZero.install` — so it does the work here:
// it expands the bound claims, then replaces the StatefulSet with one whose
// template matches, and waits.
//
// Three things about the shape are worth knowing before reading it.
//
//   - **The chart declares its size on the StatefulSet, not in the claim
//     template.** `kitchen.persistenceSize` renders the template's size from
//     the *live* object where there is one, so Helm never submits a change to
//     the immutable field and the upgrade succeeds; the value the operator
//     asked for arrives as an annotation, which is mutable. Established
//     against a real API server, Helm 4.2.2 and Helm 3.16 alike.
//   - **A volume is only ever grown.** The chart's annotation and
//     `spec.storage.volumes` both say a size and the larger wins, so a `helm
//     upgrade` still carrying the old value cannot undo a resize somebody did
//     from the dashboard, and no typo can shrink the volume the platform's
//     telemetry lives on. Shrinking is not refused with an error; it is simply
//     not a thing this does, and the status says which volume is bigger than
//     the chart asks for.
//   - **The replacement is applied as the field manager that owned the
//     original.** Helm 4 applies server-side, so a StatefulSet recreated under
//     any other manager makes every later `helm upgrade` fail with a field
//     conflict on `.spec.volumeClaimTemplates`. Re-applying under the previous
//     owner hands the object back exactly as Helm left it. Also established
//     against a real API server, because nothing about it is guessable.
const (
	// StorageSizeAnnotation is the size the chart asks a platform
	// StatefulSet's volume to be. It carries the value the claim template
	// cannot, because the template is immutable and an annotation is not.
	StorageSizeAnnotation = "kitchen.bermos.dev/storage-size"

	// volumeResizeStashName is the ConfigMap the replacement StatefulSet is
	// written to before the original is deleted. Orphan-deleting the
	// platform's telemetry StatefulSet is a moment where a crashed operator
	// would otherwise leave nothing behind to recreate it from, and the
	// installation would need a `helm upgrade` to get its store back.
	volumeResizeStashName = "kitchen-volume-resize"

	// volumePhaseUnknown is a read that failed rather than a fact about the
	// volume. It is separate from Blocked because Blocked is deliberately
	// not retried — nothing about a class that cannot expand changes on its
	// own — and a transient API error is exactly the thing that does.
	volumePhaseUnknown = "Unknown"

	// The phases status.storage.volumes reports.
	//
	// `Resizing` is separate from `Growing` because the two are different
	// waits with different owners: `Growing` is the platform still doing its
	// half — patching claims, replacing the StatefulSet — and `Resizing` is
	// the CSI driver doing its, which is the half that can get stuck. A
	// volume judged done the moment its *request* moved would report Settled
	// over a `ControllerResizeFailed` and say nothing at all.
	volumePhaseSettled  = "Settled"
	volumePhaseGrowing  = "Growing"
	volumePhaseResizing = "Resizing"
	volumePhaseBlocked  = "Blocked"
)

// The condition and the reasons this file writes. They are exported because
// the dashboard classifies a condition by its reason and defaults an unknown
// one to a fault (internal/api/conditions.go) — a volume on a class that
// cannot expand is a fact about the cluster, and a volume whose CSI driver is
// still working is nothing at all, and neither should paint the platform red.
// `TestEveryExportedReasonIsClassified` is what makes that a step somebody has
// to take rather than one they are trusted to remember.
const (
	ConditionVolumesResized = "VolumesResized"

	// ReasonNoPlatformVolumes: nothing here declares a size, so there is
	// nothing to keep.
	ReasonNoPlatformVolumes = "NoPlatformVolumes"
	// ReasonVolumesAtRequestedSize: every platform volume is at least the
	// size it was asked to be.
	ReasonVolumesAtRequestedSize = "VolumesAtRequestedSize"
	// ReasonVolumesGrowing: the platform is part-way through growing one,
	// or the driver underneath it is.
	ReasonVolumesGrowing = "VolumesGrowing"
	// ReasonVolumeExpansionBlocked: something asked for a size this
	// installation cannot reach — usually a storage class that does not
	// admit expansion. Nothing more will happen until somebody changes
	// something, and nothing is broken.
	ReasonVolumeExpansionBlocked = "VolumeExpansionBlocked"
	// ReasonVolumeSurveyFailed: the workloads or their claims could not be
	// read, so nothing here was assessed.
	ReasonVolumeSurveyFailed = "VolumeSurveyFailed"
	// ReasonVolumeRestoreFailed: a StatefulSet the platform deleted could
	// not be put back, which is the one state in this file that is urgent.
	ReasonVolumeRestoreFailed = "VolumeRestoreFailed"
)

// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

// reconcilePlatformVolumes grows every platform volume that has been asked to
// grow, and reports what it could not do.
//
// It returns whether the platform's volumes are the size they were asked to
// be, which is what keeps the reconcile requeueing while an expansion is in
// flight.
func (r *KitchenReconciler) reconcilePlatformVolumes(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	// A replacement that was stashed and never created is restored first:
	// nothing else in this file is safe to run while a StatefulSet the
	// platform deleted is missing.
	if err := r.restoreStashedStatefulSet(ctx); err != nil {
		setCond(ConditionVolumesResized, metav1.ConditionFalse, ReasonVolumeRestoreFailed, err.Error())
		return false
	}

	sets := &appsv1.StatefulSetList{}
	if err := r.List(ctx, sets,
		client.InNamespace(PlatformNamespace),
		client.MatchingLabels{labelPartOfKey: labelPartOfValue},
	); err != nil {
		setCond(ConditionVolumesResized, metav1.ConditionFalse, ReasonVolumeSurveyFailed, err.Error())
		return false
	}

	statuses := make([]kitchenv1alpha1.PlatformVolumeStatus, 0, len(sets.Items))
	for i := range sets.Items {
		if status := r.reconcileOneVolume(ctx, kitchen, &sets.Items[i]); status != nil {
			statuses = append(statuses, *status)
		}
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].StatefulSet < statuses[j].StatefulSet })

	if len(statuses) == 0 {
		kitchen.Status.Storage = nil
		setCond(ConditionVolumesResized, metav1.ConditionTrue, ReasonNoPlatformVolumes,
			"no platform StatefulSet declares a volume size")
		return true
	}
	kitchen.Status.Storage = &kitchenv1alpha1.PlatformStorageStatus{Volumes: statuses}

	var moving, blocked, unreadable []string
	for _, status := range statuses {
		switch status.Phase {
		case volumePhaseGrowing, volumePhaseResizing:
			moving = append(moving, status.StatefulSet+": "+status.Message)
		case volumePhaseBlocked:
			blocked = append(blocked, status.StatefulSet+": "+status.Message)
		case volumePhaseUnknown:
			unreadable = append(unreadable, status.StatefulSet+": "+status.Message)
		}
	}
	switch {
	case len(unreadable) > 0:
		// A read that failed is a fault and is worth coming back for; it is
		// deliberately reported ahead of everything else, because a volume
		// nobody could assess makes every other answer here partial.
		setCond(ConditionVolumesResized, metav1.ConditionFalse, ReasonVolumeSurveyFailed,
			strings.Join(append(append(unreadable, moving...), blocked...), "; "))
	case len(moving) == 0 && len(blocked) == 0:
		setCond(ConditionVolumesResized, metav1.ConditionTrue, ReasonVolumesAtRequestedSize,
			fmt.Sprintf("%d platform %s at the requested size", len(statuses), plural("volume", len(statuses))))
	case len(moving) > 0:
		setCond(ConditionVolumesResized, metav1.ConditionFalse, ReasonVolumesGrowing,
			strings.Join(append(moving, blocked...), "; "))
	default:
		setCond(ConditionVolumesResized, metav1.ConditionFalse, ReasonVolumeExpansionBlocked,
			strings.Join(blocked, "; "))
	}
	// A resize in flight and a read that failed are both worth coming back
	// for. A volume nothing can grow is not: that is a fact about the
	// cluster, and requeueing every thirty seconds for as long as it stays
	// true would make an installation on a storage class that cannot expand
	// reconcile forever over a number in a values file.
	return len(moving) == 0 && len(unreadable) == 0
}

// plural is "volume"/"volumes", which is the whole of what this file needs of
// English.
func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// reconcileOneVolume grows one StatefulSet's volume. A StatefulSet that
// declares no size at all is not this reconciler's business and answers nil.
func (r *KitchenReconciler) reconcileOneVolume(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	set *appsv1.StatefulSet,
) *kitchenv1alpha1.PlatformVolumeStatus {
	template := dataClaimTemplate(set)
	if template == nil {
		return nil
	}
	declared := template.Spec.Resources.Requests[corev1.ResourceStorage]
	desired := largestRequest(set, kitchen)

	status := &kitchenv1alpha1.PlatformVolumeStatus{
		StatefulSet: set.Name,
		Component:   set.Labels[labelComponentKind],
		Phase:       volumePhaseSettled,
	}

	claims, err := r.claimsOf(ctx, set, template.Name)
	if err != nil {
		status.Phase = volumePhaseUnknown
		status.Message = "the claims this volume is made of could not be read: " + err.Error()
		return status
	}
	for _, claim := range claims {
		status.Claims = append(status.Claims, claim.Name)
	}
	// Three numbers, and the difference between them is the whole of this
	// function. `request` is what the claims ask for, `capacity` is what the
	// driver has actually given them, and `desired` is what somebody asked
	// for. A volume is done when the *capacity* is there: judged on the
	// request alone, a claim stuck in `ControllerResizeFailed` reads as
	// finished and says nothing at all.
	request := smallestRequest(claims, declared)
	capacity := smallestCapacity(claims)
	status.Current = request.String()
	status.Desired = desired.String()
	if capacity != nil {
		status.Capacity = capacity.String()
	}
	status.StorageClass = claimsStorageClass(claims, template)

	// Whether the volume can be grown at all is a fact about it, true of a
	// volume nobody has asked to grow as much as of one somebody has — and it
	// is the fact the Storage screen greys its button out on, so it is
	// recorded whatever the sizes say.
	expandable, className, classErr := r.storageClassExpands(ctx, claims, template)
	if className != "" {
		status.StorageClass = className
	}
	if classErr == nil {
		status.Expandable = &expandable
	}

	if desired.Cmp(request) <= 0 {
		if desired.Cmp(request) < 0 {
			// Not a fault and not an instruction: a volume that is already
			// bigger than the chart asks for is what a resize done from the
			// dashboard leaves behind, and it stays that way until somebody
			// carries the number back into the values.
			status.Message = fmt.Sprintf(
				"this volume is %s, larger than the %s asked for; volumes are grown and never shrunk, "+
					"so set the chart's persistence size to %s to make the two agree",
				request.String(), desired.String(), request.String())
		}
		// An expansion already asked for — by this reconciler, by the chart,
		// or by anybody — is not finished until the driver says so.
		if waiting := resizingClaim(claims, request); waiting != "" {
			status.Phase = volumePhaseResizing
			status.Message = waiting
		}
		return status
	}

	// The API server refuses a size change to an unbound claim outright —
	// "spec is immutable after creation except resources.requests … for bound
	// claims" — so an unbound one is a wait rather than a failure, and saying
	// which claim and what phase it is in is more use than the refusal.
	if unbound := unboundClaim(claims); unbound != "" {
		status.Phase = volumePhaseBlocked
		status.Message = fmt.Sprintf(
			"growing this volume from %s to %s waits for %s to bind, because a claim can only be "+
				"expanded once it is bound", request.String(), desired.String(), unbound)
		return status
	}

	if classErr != nil {
		status.Phase = volumePhaseUnknown
		status.Message = "the storage class behind this volume could not be read: " + classErr.Error()
		return status
	}
	if !expandable {
		status.Phase = volumePhaseBlocked
		status.Message = fmt.Sprintf(
			"growing this volume from %s to %s needs a storage class that allows volume expansion, and %s does not; "+
				"the volume can only be replaced, which is a restore rather than a resize",
			request.String(), desired.String(), storageClassLabel(status.StorageClass))
		return status
	}

	status.Phase = volumePhaseGrowing
	status.Message = fmt.Sprintf("growing from %s to %s", request.String(), desired.String())
	if err := r.growClaims(ctx, claims, desired); err != nil {
		status.Phase = volumePhaseUnknown
		status.Message = fmt.Sprintf("growing this volume from %s to %s failed: %s",
			request.String(), desired.String(), err.Error())
		return status
	}
	if declared.Cmp(desired) < 0 {
		if err := r.replaceStatefulSet(ctx, set, desired); err != nil {
			status.Phase = volumePhaseUnknown
			status.Message = fmt.Sprintf("the claim template could not be rewritten to %s: %s",
				desired.String(), err.Error())
			return status
		}
	}
	return status
}

// smallestCapacity is the least a claim has actually been given, or nil where
// none of them has reported one — an unbound claim has no capacity, and "not
// yet reported" is not "zero".
func smallestCapacity(claims []corev1.PersistentVolumeClaim) *resource.Quantity {
	var smallest *resource.Quantity
	for i := range claims {
		given, ok := claims[i].Status.Capacity[corev1.ResourceStorage]
		if !ok {
			continue
		}
		if smallest == nil || given.Cmp(*smallest) < 0 {
			value := given.DeepCopy()
			smallest = &value
		}
	}
	return smallest
}

// resizingClaim is the sentence for a claim whose request has moved and whose
// capacity has not caught up, or empty where every claim is where it asked to
// be.
//
// The driver's own account of it is preferred over anything this could
// compose: `ControllerResizeFailed` and `FileSystemResizePending` are
// different problems with different answers, and only the claim knows which
// one it is in.
func resizingClaim(claims []corev1.PersistentVolumeClaim, request resource.Quantity) string {
	for i := range claims {
		claim := &claims[i]
		given, ok := claim.Status.Capacity[corev1.ResourceStorage]
		if !ok || given.Cmp(request) >= 0 {
			continue
		}
		asked := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		sentence := fmt.Sprintf("%s asks for %s and has %s; the driver has not finished expanding it",
			claim.Name, asked.String(), given.String())
		for _, condition := range claim.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}
			detail := strings.TrimSpace(condition.Message)
			if detail == "" {
				detail = string(condition.Type)
			}
			return sentence + " (" + detail + ")"
		}
		return sentence
	}
	return ""
}

// dataClaimTemplate is the volume a platform StatefulSet keeps its data on.
//
// Every StatefulSet the chart ships has exactly one claim template, and a
// workload with more than one is not something this can grow without being
// told which — so it is left alone rather than guessed at.
func dataClaimTemplate(set *appsv1.StatefulSet) *corev1.PersistentVolumeClaim {
	if len(set.Spec.VolumeClaimTemplates) != 1 {
		return nil
	}
	template := &set.Spec.VolumeClaimTemplates[0]
	if _, ok := template.Spec.Resources.Requests[corev1.ResourceStorage]; !ok {
		return nil
	}
	if _, ok := set.Annotations[StorageSizeAnnotation]; !ok {
		return nil
	}
	return template
}

// largestRequest is the size to grow to: the chart's annotation or the
// singleton's own entry, whichever is bigger. The two are not ranked — a
// volume is grown to the largest size anybody has asked for, which is what
// makes an upgrade still carrying the old value harmless.
func largestRequest(set *appsv1.StatefulSet, kitchen *kitchenv1alpha1.Kitchen) resource.Quantity {
	largest := resource.Quantity{}
	if declared, err := resource.ParseQuantity(set.Annotations[StorageSizeAnnotation]); err == nil {
		largest = declared
	}
	if asked, ok := kitchen.Spec.Storage.Volumes[set.Name]; ok && asked.Cmp(largest) > 0 {
		largest = asked
	}
	return largest
}

// unboundClaim is the first claim that is not bound, which is the one thing
// that makes expansion impossible for a reason that will pass on its own.
func unboundClaim(claims []corev1.PersistentVolumeClaim) string {
	for i := range claims {
		if claims[i].Status.Phase != corev1.ClaimBound {
			return claims[i].Name + " (" + string(claims[i].Status.Phase) + ")"
		}
	}
	return ""
}

// smallestRequest is the size the platform still has to grow: the smallest a
// claim asks for, falling back to the template for a StatefulSet whose pods
// have never run.
func smallestRequest(claims []corev1.PersistentVolumeClaim, declared resource.Quantity) resource.Quantity {
	smallest := declared
	for i := range claims {
		request, ok := claims[i].Spec.Resources.Requests[corev1.ResourceStorage]
		if !ok {
			continue
		}
		if request.Cmp(smallest) < 0 {
			smallest = request
		}
	}
	return smallest
}

// claimsOf is the claims one StatefulSet's template produced. They are named
// `<template>-<set>-<ordinal>` and nothing owner-references them, so the name
// is what identifies them.
func (r *KitchenReconciler) claimsOf(
	ctx context.Context,
	set *appsv1.StatefulSet,
	templateName string,
) ([]corev1.PersistentVolumeClaim, error) {
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(set.Namespace)); err != nil {
		return nil, err
	}
	prefix := templateName + "-" + set.Name + "-"
	var mine []corev1.PersistentVolumeClaim
	for i := range claims.Items {
		if strings.HasPrefix(claims.Items[i].Name, prefix) {
			mine = append(mine, claims.Items[i])
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Name < mine[j].Name })
	return mine, nil
}

// claimsStorageClass is the class the claims are on, or the template's where
// there are none yet.
func claimsStorageClass(claims []corev1.PersistentVolumeClaim, template *corev1.PersistentVolumeClaim) string {
	for i := range claims {
		if name := claims[i].Spec.StorageClassName; name != nil && *name != "" {
			return *name
		}
	}
	if name := template.Spec.StorageClassName; name != nil {
		return *name
	}
	return ""
}

// storageClassExpands is whether the class behind these claims admits online
// expansion, which is the one refusal the platform cannot work around.
//
// A claim that names no class was bound by the cluster's default, and the
// default is looked up rather than assumed — a claim's own
// `spec.storageClassName` is set by admission on creation, so this is only
// ever the fallback for one written before that.
func (r *KitchenReconciler) storageClassExpands(
	ctx context.Context,
	claims []corev1.PersistentVolumeClaim,
	template *corev1.PersistentVolumeClaim,
) (bool, string, error) {
	name := claimsStorageClass(claims, template)
	if name == "" {
		classes := &storagev1.StorageClassList{}
		if err := r.List(ctx, classes); err != nil {
			return false, "", err
		}
		for i := range classes.Items {
			if classes.Items[i].Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
				name = classes.Items[i].Name
				break
			}
		}
		if name == "" {
			return false, "", nil
		}
	}
	class := &storagev1.StorageClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, class); err != nil {
		if apierrors.IsNotFound(err) {
			return false, name, nil
		}
		return false, name, err
	}
	return class.AllowVolumeExpansion != nil && *class.AllowVolumeExpansion, name, nil
}

// storageClassLabel names a class in a sentence, including the case where
// there is none to name.
func storageClassLabel(name string) string {
	if name == "" {
		return "the class it was bound by cannot be identified"
	}
	return name
}

// growClaims raises each claim's storage request. A claim already at or past
// the size is left alone: a request is only ever raised, and the CSI driver
// rejects a lowered one anyway.
func (r *KitchenReconciler) growClaims(
	ctx context.Context,
	claims []corev1.PersistentVolumeClaim,
	desired resource.Quantity,
) error {
	for i := range claims {
		claim := claims[i]
		if request, ok := claim.Spec.Resources.Requests[corev1.ResourceStorage]; ok && request.Cmp(desired) >= 0 {
			continue
		}
		patch := client.MergeFrom(claim.DeepCopy())
		if claim.Spec.Resources.Requests == nil {
			claim.Spec.Resources.Requests = corev1.ResourceList{}
		}
		claim.Spec.Resources.Requests[corev1.ResourceStorage] = desired
		if err := r.Patch(ctx, &claim, patch); err != nil {
			return fmt.Errorf("expanding %s: %w", claim.Name, err)
		}
	}
	return nil
}

// replaceStatefulSet rewrites the immutable claim template, which can only be
// done by deleting the StatefulSet and creating it again.
//
// The pods are orphaned rather than deleted — the volume is expanded online
// and the process on top of it has no reason to stop — and the replacement is
// stashed before the original goes, so that an operator that dies between the
// two has left behind everything needed to put it back.
func (r *KitchenReconciler) replaceStatefulSet(
	ctx context.Context,
	set *appsv1.StatefulSet,
	desired resource.Quantity,
) error {
	log := logf.FromContext(ctx)
	replacement := replacementFor(set, desired)

	if err := r.stashStatefulSet(ctx, replacement); err != nil {
		return fmt.Errorf("stashing the replacement: %w", err)
	}
	// The UID is a precondition, not decoration: `set` came off a cached
	// list, and a stale informer holding the object this reconciler replaced
	// a moment ago would otherwise delete its own replacement. With it, the
	// API server refuses the delete instead.
	orphan := metav1.DeletePropagationOrphan
	if err := r.Delete(ctx, set, &client.DeleteOptions{
		PropagationPolicy: &orphan,
		Preconditions:     &metav1.Preconditions{UID: &set.UID},
	}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting %s: %w", set.Name, err)
	}
	log.Info("replacing a platform StatefulSet to grow its volume",
		"statefulSet", set.Name, "size", desired.String())

	// An orphaning delete finishes when the garbage collector has taken the
	// `orphan` finalizer off, which is somebody else's clock. Waiting a few
	// seconds covers the ordinary case; where it takes longer the stash is
	// what puts the object back, on the next reconcile or the one after.
	gone, err := r.statefulSetGone(ctx, set.Name)
	if err != nil {
		return err
	}
	if !gone {
		log.Info("the replaced StatefulSet is still going away; the stash will finish this",
			"statefulSet", set.Name)
		return nil
	}
	if err := r.createStashedStatefulSet(ctx, replacement); err != nil {
		return err
	}
	return r.clearStash(ctx, set.Name)
}

// statefulSetGone waits a bounded few seconds for a deleted StatefulSet to
// actually disappear.
func (r *KitchenReconciler) statefulSetGone(ctx context.Context, name string) (bool, error) {
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			existing := &appsv1.StatefulSet{}
			// Uncached: a stale informer answering "still there" would only
			// slow this down, but one answering "gone" for an object that is
			// not would make the create fail and the stash be cleared over a
			// StatefulSet that is still on its way out.
			getErr := r.APIReader.Get(ctx,
				types.NamespacedName{Name: name, Namespace: PlatformNamespace}, existing)
			if apierrors.IsNotFound(getErr) {
				return true, nil
			}
			lastErr = getErr
			return false, nil
		})
	if err == nil {
		return true, nil
	}
	if wait.Interrupted(err) {
		return false, nil
	}
	if lastErr != nil {
		return false, lastErr
	}
	return false, err
}

// replacementFor is the same StatefulSet with a bigger claim template and
// nothing the API server assigned.
func replacementFor(set *appsv1.StatefulSet, desired resource.Quantity) *appsv1.StatefulSet {
	replacement := set.DeepCopy()
	replacement.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = desired
	replacement.Annotations = withoutLastApplied(replacement.Annotations)
	replacement.ObjectMeta.ResourceVersion = ""
	replacement.ObjectMeta.UID = ""
	replacement.ObjectMeta.Generation = 0
	replacement.ObjectMeta.CreationTimestamp = metav1.Time{}
	replacement.ObjectMeta.DeletionTimestamp = nil
	// `ManagedFields` is deliberately kept: `applyOwnerOf` reads the field
	// manager that owned the claim template through a server-side apply out
	// of it, and that owner is what the replacement has to go back under or
	// every later `helm upgrade` conflicts on this exact field. It is
	// stripped from the copy that is actually sent, in
	// createStashedStatefulSet, and it travels in the stash so that a restore
	// after a crash puts the object back under the same owner.
	replacement.Status = appsv1.StatefulSetStatus{}
	replacement.TypeMeta = metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "StatefulSet"}
	return replacement
}

// withoutLastApplied drops the client-side-apply record, which describes the
// object as it was before the template was rewritten and would otherwise make
// a Helm 3 upgrade compute its patch against a size that no longer exists.
func withoutLastApplied(annotations map[string]string) map[string]string {
	if annotations == nil {
		return nil
	}
	kept := make(map[string]string, len(annotations))
	for key, value := range annotations {
		if key == corev1.LastAppliedConfigAnnotation {
			continue
		}
		kept[key] = value
	}
	return kept
}

// createStashedStatefulSet puts the replacement back, under the field manager
// that owned the original.
//
// Helm 4 applies server-side, so ownership of `.spec.volumeClaimTemplates`
// matters: a StatefulSet recreated under any other manager makes every later
// `helm upgrade` fail with a field conflict, even when the value it is
// applying is the one already there. Re-applying under the previous owner
// hands the object back exactly as Helm left it. Where nothing owned it
// through an apply — a Helm 3 release, or an object somebody created by hand —
// there is no ownership to preserve and a plain create is right.
func (r *KitchenReconciler) createStashedStatefulSet(ctx context.Context, replacement *appsv1.StatefulSet) error {
	if owner := applyOwnerOf(replacement); owner != "" {
		applied := replacement.DeepCopy()
		applied.ObjectMeta.ManagedFields = nil
		if err := r.Patch(ctx, applied, client.Apply,
			client.FieldOwner(owner), client.ForceOwnership); err != nil {
			return fmt.Errorf("re-applying %s as %q: %w", replacement.Name, owner, err)
		}
		return nil
	}
	create := replacement.DeepCopy()
	create.ObjectMeta.ManagedFields = nil
	if err := r.Create(ctx, create); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("recreating %s: %w", replacement.Name, err)
	}
	return nil
}

// applyOwnerOf is the field manager that owns the claim template through a
// server-side apply, if any.
func applyOwnerOf(set *appsv1.StatefulSet) string {
	for _, entry := range set.ManagedFields {
		if entry.Operation != metav1.ManagedFieldsOperationApply || entry.FieldsV1 == nil {
			continue
		}
		if strings.Contains(string(entry.FieldsV1.Raw), "volumeClaimTemplates") {
			return entry.Manager
		}
	}
	return ""
}

// stashStatefulSet writes the replacement down before the original is deleted.
func (r *KitchenReconciler) stashStatefulSet(ctx context.Context, replacement *appsv1.StatefulSet) error {
	encoded, err := json.Marshal(replacement)
	if err != nil {
		return err
	}
	stash := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      volumeResizeStashName,
			Namespace: PlatformNamespace,
			Labels:    platformLabels(volumeResizeStashName, "storage"),
		},
		Data: map[string]string{replacement.Name: string(encoded)},
	}
	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: volumeResizeStashName, Namespace: PlatformNamespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.Create(ctx, stash)
	case err != nil:
		return err
	}
	if existing.Data == nil {
		existing.Data = map[string]string{}
	}
	existing.Data[replacement.Name] = string(encoded)
	return r.Update(ctx, existing)
}

// clearStash forgets a replacement that is back in place.
func (r *KitchenReconciler) clearStash(ctx context.Context, name string) error {
	stash := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: volumeResizeStashName, Namespace: PlatformNamespace}, stash)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, ok := stash.Data[name]; !ok {
		return nil
	}
	delete(stash.Data, name)
	if len(stash.Data) == 0 {
		return client.IgnoreNotFound(r.Delete(ctx, stash))
	}
	return r.Update(ctx, stash)
}

// restoreStashedStatefulSet puts back anything the platform deleted and did
// not manage to recreate.
//
// This is the crash window the stash exists for, and it is checked on every
// reconcile rather than only after a resize: a StatefulSet that is missing
// while its replacement is written down is the one state an installation
// cannot get itself out of, since the operator holds no chart to render it
// from again.
func (r *KitchenReconciler) restoreStashedStatefulSet(ctx context.Context) error {
	stash := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: volumeResizeStashName, Namespace: PlatformNamespace}, stash)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for name, encoded := range stash.Data {
		existing := &appsv1.StatefulSet{}
		// Uncached, for the reason in statefulSetGone: a stale cache here
		// would clear the stash for a StatefulSet that is actually gone,
		// which is the one thing this whole mechanism exists to prevent.
		err := r.APIReader.Get(ctx,
			types.NamespacedName{Name: name, Namespace: PlatformNamespace}, existing)
		if err == nil && existing.DeletionTimestamp == nil {
			if clearErr := r.clearStash(ctx, name); clearErr != nil {
				return clearErr
			}
			continue
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil {
			// Still going away. Nothing to do for this one this round — and
			// `continue` rather than `return`, because one StatefulSet still
			// terminating must not hold up putting another one back.
			continue
		}
		replacement := &appsv1.StatefulSet{}
		if unmarshalErr := json.Unmarshal([]byte(encoded), replacement); unmarshalErr != nil {
			// A stash nothing can read is worse than no stash: it would
			// block every reconcile forever.
			logf.FromContext(ctx).Error(unmarshalErr, "discarding an unreadable volume-resize stash",
				"statefulSet", name)
			if clearErr := r.clearStash(ctx, name); clearErr != nil {
				return clearErr
			}
			continue
		}
		if createErr := r.createStashedStatefulSet(ctx, replacement); createErr != nil {
			return createErr
		}
		if clearErr := r.clearStash(ctx, name); clearErr != nil {
			return clearErr
		}
	}
	return nil
}

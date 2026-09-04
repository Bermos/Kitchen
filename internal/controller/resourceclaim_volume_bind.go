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
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/database"
	"github.com/Bermos/Kitchen/internal/provider/volume"
)

// The other half of the volume contract: a volume that already exists.
//
// Everything downstream of the claim is unchanged — one named process mounts
// it, the access mode decides the replica cap and the recreate, the
// environment reconciler waits for it — and everything upstream is inverted.
// The platform provisions nothing, so there is no size and no StorageClass
// to ask for; it owns nothing, so there is no deletionPolicy that may
// destroy anything; and the data was there before the cluster was, which is
// the whole reason the mode exists.
//
// Three decisions are made here rather than left to the cluster to discover:
//
//  1. **A volume that is not there is a refusal, not a wait.** A claim
//     naming a PersistentVolume the cluster does not have is Failed with the
//     name it could not find, because no amount of waiting will conjure an
//     NFS export. A PersistentVolumeClaim that exists but has not bound yet
//     is a wait — that one really does resolve on its own.
//
//  2. **Two projects may read one volume; one may write it.** Sharing a
//     filesystem read-only costs nothing and is most of what an existing
//     export is for. Two writers is one project's deploy breaking another's,
//     which is the opposite of Kitchen owning what it deploys — so the
//     second writer is refused, naming the first claim and its project. The
//     comparison is on what the volumes *point at* (volume.VolumeIdentity),
//     not their names: two PersistentVolumes serving one export are two
//     objects and one twelve-terabyte filesystem, and a name comparison
//     would call them unrelated.
//
//  3. **Teardown unmounts and never deletes.** deletionPolicy Delete is
//     refused outright — the API refuses it at the door and this refuses it
//     again for a claim written another way — and finalize removes only the
//     PersistentVolumeClaim the platform created to reach the volume.

// reconcileBoundVolume drives a claim that mounts a volume the platform did
// not create.
func (r *ResourceClaimReconciler) reconcileBoundVolume(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	project *kitchenv1alpha1.Project,
	req volume.Requirements,
) (ctrl.Result, error) {
	if claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete {
		return r.failed(ctx, claim, "DeletionPolicyRefused", fmt.Errorf(
			"deletionPolicy %s is refused on a bound volume: %s is not the platform's to destroy — it existed "+
				"before this claim and the platform neither provisioned it nor owns it. Deleting the claim "+
				"unmounts the volume and leaves every byte of it where it is, which is the only policy this "+
				"source has", kitchenv1alpha1.ClaimDelete, boundVolumeNamed(req)))
	}

	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}

	pv, pvcName, result, err := r.resolveBoundVolume(ctx, claim, appNS, req)
	if pv == nil {
		return result, err
	}

	mode := req.Bind.AccessMode
	if !volumeOffers(pv, mode) {
		return r.failed(ctx, claim, "AccessModeUnavailable", fmt.Errorf(
			"PersistentVolume %s cannot be mounted %s: it offers %s. Ask for one of those, or have the "+
				"operator write a volume that offers this one — a mode the volume does not have is a pod "+
				"that never schedules rather than a mount that quietly works", pv.Name, mode,
			joinAccessModes(pv.Spec.AccessModes)))
	}

	identity := volume.VolumeIdentity(pv)
	shared, err := r.otherClaimsOn(ctx, claim, identity)
	if err != nil {
		return ctrl.Result{}, err
	}
	if writer, refused := writerBefore(claim, shared, volume.Writes(mode)); refused {
		return r.failed(ctx, claim, "VolumeWrittenElsewhere", fmt.Errorf(
			"the volume %s is already written by the claim %s in project %s, and two projects writing one "+
				"filesystem is one project's deploy breaking another's — which is what Kitchen owning what "+
				"it deploys rules out. Mount it %s to read alongside that claim, or have the writing claim "+
				"give it up", identity, writer.Name, writer.Spec.ProjectRef.Name, corev1.ReadOnlyMany))
	}

	if pvcName == "" {
		pvcName = volumeClaimName(claim.Name)
		if err := r.ensureBoundVolumeClaim(ctx, claim, appNS, pvcName, pv, mode); err != nil {
			var refusal *boundVolumeRefusal
			if errors.As(err, &refusal) {
				return r.failed(ctx, claim, refusal.reason, refusal)
			}
			return ctrl.Result{}, err
		}
	}

	claimType, _ := claim.Type()
	previewMode := declare(claim, claimType, volume.BoundProviderName)
	// The declaration is written before any claim exists and so cannot see
	// this claim's access mode; this reconcile can. A volume that attaches
	// to one pod at a time is production's while production is running, so
	// a preview gets nothing and is told why rather than left pending.
	claim.Status.ForcesRecreate = volume.AttachesOnce(mode)
	if previewMode != contract.PreviewNone && volume.AttachesOnce(mode) {
		claim.Status.PreviewMode = string(contract.PreviewNone)
		claim.Status.PreviewReason = fmt.Sprintf(
			"the volume is mounted %s, so it attaches to one pod at a time and production has it: a preview "+
				"mounting it would take it from production, so previews get nothing. A volume mounted %s or "+
				"%s is mounted read-only by every preview instead", mode, corev1.ReadOnlyMany,
			corev1.ReadWriteMany)
	}
	// The data on a bound volume is production's, whoever put it there —
	// a preview reading it is reading production, and the policy engine
	// judges it on exactly this value.
	claim.Status.DataProvenance = string(database.ProvenanceProduction)

	// A bound claim cuts no volume per preview, so the preview-volume
	// condition has nothing to say and is taken off rather than left
	// reporting a count from a mode this claim is no longer in.
	meta.RemoveStatusCondition(&claim.Status.Conditions, condVolumesReady)

	claim.Status.Volume = &kitchenv1alpha1.ClaimVolumeStatus{
		Process:      req.Process,
		MountPath:    req.MountPath,
		Source:       kitchenv1alpha1.VolumeBind,
		StorageClass: pv.Spec.StorageClassName,
		AccessMode:   string(mode),
		AccessModeReason: fmt.Sprintf("the claim mounts %s %s, and the volume offers %s", pv.Name, mode,
			joinAccessModes(pv.Spec.AccessModes)),
		ClaimName: pvcName,
		Bound: &kitchenv1alpha1.ClaimBoundVolume{
			PersistentVolume: pv.Name,
			Capacity:         volumeCapacity(pv),
			Named:            boundVolumeNamed(req),
			Identity:         identity,
			Writable:         volume.Writes(mode),
			SharedWith:       claimNames(shared),
		},
	}
	setClaimCondition(claim, condProvisioned, metav1.ConditionTrue, "Bound",
		fmt.Sprintf("volume %s (%s, %s) mounted at %s by process %s through %s/%s", pv.Name, volumeCapacity(pv),
			mode, req.MountPath, req.Process, appNS, pvcName))

	if err := r.bind(ctx, claim, volume.BoundProviderName,
		fmt.Sprintf("claim %s bound: the existing volume %s mounted %s at %s by process %s", claim.Name, pv.Name,
			mode, req.MountPath, req.Process),
		map[string]any{
			"type":             claim.Spec.Type,
			"source":           string(kitchenv1alpha1.VolumeBind),
			"process":          req.Process,
			"mountPath":        req.MountPath,
			"accessMode":       string(mode),
			"persistentVolume": pv.Name,
			"identity":         identity,
			"claimName":        pvcName,
			"sharedWith":       strings.Join(claimNames(shared), ", "),
			"dataProvenance":   claim.Status.DataProvenance,
			"previewMode":      claim.Status.PreviewMode,
		}); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("reconciled bound volume claim", "claim", claim.Name, "persistentVolume", pv.Name,
		"accessMode", mode, "writable", volume.Writes(mode), "sharedWith", claimNames(shared))
	return ctrl.Result{}, nil
}

// resolveBoundVolume finds the PersistentVolume the claim names, and — where
// the claim named a PersistentVolumeClaim already in the namespace — the
// name of that claim, which the platform then mounts as it found it.
//
// A nil volume means the caller returns the result and error given: a
// refusal for a volume that is not there, a wait for one that has not bound
// yet.
func (r *ResourceClaimReconciler) resolveBoundVolume(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	appNS string,
	req volume.Requirements,
) (*corev1.PersistentVolume, string, ctrl.Result, error) {
	if named := req.Bind.PersistentVolumeClaim; named != "" {
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: named}, pvc)
		switch {
		case apierrors.IsNotFound(err):
			result, updateErr := r.failed(ctx, claim, "VolumeNotFound", fmt.Errorf(
				"there is no PersistentVolumeClaim %q in this project's namespace (%s), so there is nothing "+
					"to mount. Name one that is there, or name the PersistentVolume itself with "+
					"bind.persistentVolume — a claim in another namespace cannot be mounted by this "+
					"project's pods", named, appNS))
			return nil, "", result, updateErr
		case err != nil:
			return nil, "", ctrl.Result{}, err
		}
		if pvc.Spec.VolumeName == "" {
			result, updateErr := r.pending(ctx, claim, "VolumeNotBound", fmt.Errorf(
				"PersistentVolumeClaim %s/%s has not bound to a volume yet, so what it is cannot be read. A "+
					"class that binds on first consumer binds when a pod is scheduled; this claim waits for "+
					"it", appNS, named))
			return nil, "", result, updateErr
		}
		pv := &corev1.PersistentVolume{}
		if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
			if apierrors.IsNotFound(err) {
				result, updateErr := r.pending(ctx, claim, "VolumeNotBound", fmt.Errorf(
					"PersistentVolumeClaim %s/%s names the volume %q, which is not in this cluster yet",
					appNS, named, pvc.Spec.VolumeName))
				return nil, "", result, updateErr
			}
			return nil, "", ctrl.Result{}, err
		}
		return pv, named, ctrl.Result{}, nil
	}

	pv := &corev1.PersistentVolume{}
	err := r.Get(ctx, types.NamespacedName{Name: req.Bind.PersistentVolume}, pv)
	switch {
	case apierrors.IsNotFound(err):
		result, updateErr := r.failed(ctx, claim, "VolumeNotFound", fmt.Errorf(
			"there is no PersistentVolume %q in this cluster, so there is nothing to mount. A bound volume is "+
				"one somebody already made — an NFS export, an SMB share, a CSI volume — and the platform "+
				"will not create it: ask the operator for the name of the PersistentVolume that holds the "+
				"data", req.Bind.PersistentVolume))
		return nil, "", result, updateErr
	case err != nil:
		return nil, "", ctrl.Result{}, err
	}
	return pv, "", ctrl.Result{}, nil
}

// ensureBoundVolumeClaim makes the PersistentVolumeClaim the project's pods
// mount, pre-bound to the named volume.
//
// It is pre-bound by claimRef rather than left to match on size and class,
// for the reason a retained volume is: a Pending claim of the right shape
// binds to whichever volume the controller reaches first, and "whichever"
// is not a thing to say about somebody's twelve terabytes.
func (r *ResourceClaimReconciler) ensureBoundVolumeClaim(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	appNS, name string,
	pv *corev1.PersistentVolume,
	mode corev1.PersistentVolumeAccessMode,
) error {
	if ref := pv.Spec.ClaimRef; ref != nil && (ref.Namespace != appNS || ref.Name != name) {
		return &boundVolumeRefusal{
			reason: "VolumeAlreadyClaimed",
			message: fmt.Sprintf("PersistentVolume %s is already bound to %s/%s, and a PersistentVolume binds "+
				"to one claim at a time. Two projects reach one export through two PersistentVolumes — ask "+
				"the operator for a second one over the same storage, and the platform will see that they "+
				"are the same data", pv.Name, ref.Namespace, ref.Name),
		}
	}

	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: name}, pvc)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	default:
		// Already there. The volume it names is what matters, and a claim
		// bound to a different one is a claim that has been repointed:
		// nothing may quietly move a mount out from under a running
		// process, so it is a refusal with the two names in it.
		if pvc.Spec.VolumeName != "" && pvc.Spec.VolumeName != pv.Name {
			return &boundVolumeRefusal{
				reason: "VolumeChanged",
				message: fmt.Sprintf("this claim already mounts the volume %s, and now names %s. A mount "+
					"cannot be moved under a running process without losing what it was reading: delete "+
					"the claim and ask for the other volume", pvc.Spec.VolumeName, pv.Name),
			}
		}
		return nil
	}

	if err := r.prebindVolume(ctx, pv, appNS, name); err != nil {
		return err
	}
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS, Labels: volumeLabels(claim)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{mode},
			VolumeName:  pv.Name,
			// An empty class, not the cluster's default: a statically
			// written volume usually has none, and a claim that named a
			// class the volume does not carry would never bind to it.
			StorageClassName: ptr.To(pv.Spec.StorageClassName),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: pv.Spec.Capacity[corev1.ResourceStorage]},
			},
		},
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// deleteBoundVolumeClaim takes back the one object this contract created for
// a bound volume: the PersistentVolumeClaim in the application namespace,
// and only when the platform is the one that made it. A claim that named a
// PersistentVolumeClaim already in the namespace leaves it exactly as it
// found it.
func (r *ResourceClaimReconciler) deleteBoundVolumeClaim(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	appNS string,
) error {
	if req := volumeRequirements(claim); req.Bind.PersistentVolumeClaim != "" {
		return nil
	}
	return r.deleteVolumeClaim(ctx, appNS, volumeClaimName(claim.Name))
}

// otherClaimsOn is every other volume claim mounting the same storage,
// oldest first — the claims a new one shares a filesystem with.
func (r *ResourceClaimReconciler) otherClaimsOn(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	identity string,
) ([]kitchenv1alpha1.ResourceClaim, error) {
	if identity == "" {
		return nil, nil
	}
	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(claim.Namespace)); err != nil {
		return nil, err
	}
	others := make([]kitchenv1alpha1.ResourceClaim, 0, 1)
	for i := range claims.Items {
		other := claims.Items[i]
		if other.Name == claim.Name || other.Spec.Type != kitchenv1alpha1.ClaimTypeVolume {
			continue
		}
		if !other.DeletionTimestamp.IsZero() {
			continue
		}
		bound := other.Status.Volume
		if bound == nil || bound.Bound == nil || bound.Bound.Identity != identity {
			continue
		}
		others = append(others, other)
	}
	sort.Slice(others, func(a, b int) bool { return claimOlder(&others[a], &others[b]) })
	return others, nil
}

// writerBefore answers whether this claim must be refused, and which claim
// refuses it: the one writer of a volume is the oldest claim that asks to
// write it, so a claim that has been writing since yesterday is never
// displaced by one created a moment ago, and two created at once resolve the
// same way on every reconcile rather than refusing each other forever.
func writerBefore(
	claim *kitchenv1alpha1.ResourceClaim,
	others []kitchenv1alpha1.ResourceClaim,
	writes bool,
) (*kitchenv1alpha1.ResourceClaim, bool) {
	if !writes {
		return nil, false
	}
	for i := range others {
		other := &others[i]
		if other.Status.Volume == nil || other.Status.Volume.Bound == nil || !other.Status.Volume.Bound.Writable {
			continue
		}
		if claimOlder(other, claim) {
			return other, true
		}
	}
	return nil, false
}

// claimOlder orders two claims by when they were created, and by name where
// a cluster gave them the same second.
func claimOlder(a, b *kitchenv1alpha1.ResourceClaim) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name < b.Name
	}
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}

// claimNames is "project/claim" for each, which is how a shared volume names
// the other side of the sharing.
func claimNames(claims []kitchenv1alpha1.ResourceClaim) []string {
	if len(claims) == 0 {
		return nil
	}
	names := make([]string, 0, len(claims))
	for i := range claims {
		names = append(names, claims[i].Spec.ProjectRef.Name+"/"+claims[i].Name)
	}
	return names
}

// boundVolumeNamed is what the claim named, for a message about it.
func boundVolumeNamed(req volume.Requirements) string {
	if req.Bind.PersistentVolumeClaim != "" {
		return req.Bind.PersistentVolumeClaim
	}
	return req.Bind.PersistentVolume
}

// volumeOffers reports whether a PersistentVolume can be mounted the way the
// claim asks. A volume that lists nothing is taken at its word rather than
// guessed at — the platform refuses instead of hoping.
func volumeOffers(pv *corev1.PersistentVolume, mode corev1.PersistentVolumeAccessMode) bool {
	for _, offered := range pv.Spec.AccessModes {
		if offered == mode {
			return true
		}
		// A volume that may be written by many may be read by many; the
		// reverse is not true, and neither implies the single-writer mode
		// on its own.
		if offered == corev1.ReadWriteMany && mode == corev1.ReadOnlyMany {
			return true
		}
	}
	return false
}

func joinAccessModes(modes []corev1.PersistentVolumeAccessMode) string {
	if len(modes) == 0 {
		return "none at all"
	}
	names := make([]string, 0, len(modes))
	for _, mode := range modes {
		names = append(names, string(mode))
	}
	return strings.Join(names, ", ")
}

func volumeCapacity(pv *corev1.PersistentVolume) string {
	if quantity, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
		return quantity.String()
	}
	return ""
}

// boundVolumeRefusal is a refusal raised where the reconciler's own return
// shape cannot carry one: it is turned into a Failed claim by the caller,
// with the reason it names.
type boundVolumeRefusal struct {
	reason  string
	message string
}

func (e *boundVolumeRefusal) Error() string { return e.message }

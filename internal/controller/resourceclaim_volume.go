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
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/database"
	"github.com/Bermos/Kitchen/internal/provider/volume"
)

// The volume half of the ResourceClaim reconciler: a PersistentVolumeClaim
// in the application namespace, mounted into exactly one of the project's
// processes by the Environment reconciler, and one more per preview
// Environment under preview mode fresh.
//
// It is the odd contract. Every other one produces credentials and writes
// them into a Secret; this one produces a mount, so status.secretName stays
// empty and status.volume is where the Environment reconciler reads what to
// mount. What it shares with the others is the lifecycle: the Pending/Bound/
// Failed transitions, deletionPolicy, preview teardown, dataClass.
//
// Two things are decided here and acted on elsewhere. The claim names the
// process that mounts the volume, and a claim naming none or naming a
// process the project does not have is refused rather than left to fail
// three layers in. And the StorageClass is looked at, not assumed: a class
// detected to support ReadWriteMany lifts the one-replica cap and the
// recreate the provider's declaration conservatively promises, and the
// claim's status carries which it is and why.
//
// Retention is the other thing worth the machinery. A PVC in the
// application namespace dies with the namespace, and the namespace dies with
// the project — so under deletionPolicy Retain the reconciler, the moment the
// production claim binds, patches its PersistentVolume's reclaim policy to
// Retain and labels the volume with the claim. The namespace can then go and
// the volume stays, Released; a later claim of the same name in the same
// project finds it by its labels and pre-binds a new PVC to it. That is the
// same promise a retained database makes, kept with the cluster's own
// primitives rather than a namespace of the platform's own.

const (
	// condVolumesReady says whether every live preview has its own volume.
	// Separate from Ready for the reason PreviewBranchesReady is: the
	// production mount works either way, and the condition carries what is
	// missing.
	condVolumesReady = "PreviewVolumesReady"

	// labelClaimNamespace is written on the PersistentVolumeClaims and
	// PersistentVolumes a claim creates, beside the claim's name, so that a
	// change to one of them enqueues the claim it belongs to — a PVC binding
	// is what records the PersistentVolume on the status.
	labelClaimNamespace = "kitchen.bermos.dev/claim-namespace"
)

// The cluster's own storage primitives: the claim in the application
// namespace, the volume behind it whose reclaim policy is what retention
// is made of, and the classes the access mode is read off.
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

// volumeContract is the claimContract for type volume. The StorageClass is
// its provider, so conn is always nil and never read.
type volumeContract struct{}

func (volumeContract) reconcile(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
	project *kitchenv1alpha1.Project,
	_ *kitchenv1alpha1.Connection,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	req := volumeRequirements(claim)
	if err := req.Validate(); err != nil {
		return r.failed(ctx, claim, "VolumeInvalid", err)
	}
	if err := volumeProcessOf(project, req.Process); err != nil {
		return r.failed(ctx, claim, "ProcessUnknown", err)
	}
	class, err := r.storageClassFor(ctx, req.StorageClass)
	if err != nil {
		var refusal *storageClassRefusal
		if errors.As(err, &refusal) {
			return r.failed(ctx, claim, "StorageClassMissing", err)
		}
		return ctrl.Result{}, err
	}
	mode, why := volume.AccessMode(class)

	// The PVC lives in the application namespace, where the pods are; a
	// claim can be bound before the project's first build creates it.
	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}

	pvcName := volumeClaimName(claim.Name)
	boundTo, err := r.ensureProductionVolume(ctx, claim, appNS, pvcName, class.Name, req, mode)
	if err != nil {
		return ctrl.Result{}, err
	}

	// What previews get, and what the binding does to the workload. The
	// declaration promises a recreate because it cannot see the class; this
	// reconcile has seen it, and the class's own answer replaces the
	// promise on the status.
	claimType, _ := claim.Type()
	previewMode := declare(claim, claimType, volume.ProviderName)
	claim.Status.ForcesRecreate = mode != corev1.ReadWriteMany
	// The production volume is production's data, however empty it starts
	// — the same declaration the self-hosted database makes for the claim's
	// own database.
	claim.Status.DataProvenance = string(database.ProvenanceProduction)

	previous := map[string]kitchenv1alpha1.ClaimPreviewVolume{}
	if claim.Status.Volume != nil {
		for _, preview := range claim.Status.Volume.Previews {
			previous[preview.Environment] = preview
		}
	}
	status := &kitchenv1alpha1.ClaimVolumeStatus{
		Process:          req.Process,
		MountPath:        req.MountPath,
		StorageClass:     class.Name,
		AccessMode:       string(mode),
		AccessModeReason: why,
		ClaimName:        pvcName,
		PersistentVolume: boundTo,
	}
	previews, previewErr := r.reconcilePreviewVolumes(ctx, claim, project.Name, appNS, class.Name, req, mode,
		previewMode.Isolated(), previous)
	status.Previews = previews
	claim.Status.Volume = status
	setClaimCondition(claim, condProvisioned, metav1.ConditionTrue, "Provisioned",
		fmt.Sprintf("volume %s/%s (%s, %s from StorageClass %s) mounted at %s by process %s",
			appNS, pvcName, req.Size, mode, class.Name, req.MountPath, req.Process))

	if err := r.bind(ctx, claim, volume.ProviderName,
		fmt.Sprintf("claim %s bound: %s %s mounted at %s by process %s", claim.Name, req.Size, claim.Spec.Type,
			req.MountPath, req.Process),
		map[string]any{
			"type":           claim.Spec.Type,
			"process":        req.Process,
			"mountPath":      req.MountPath,
			"size":           req.Size,
			"storageClass":   class.Name,
			"accessMode":     string(mode),
			"claimName":      pvcName,
			"dataProvenance": claim.Status.DataProvenance,
			"previewMode":    claim.Status.PreviewMode,
		}); err != nil {
		return ctrl.Result{}, err
	}
	if previewErr != nil {
		// The production mount works either way; the condition carries the
		// complaint and the requeue retries it.
		return ctrl.Result{RequeueAfter: claimRequeueDelay}, nil
	}
	log.Info("reconciled volume claim", "claim", claim.Name, "pvc", pvcName, "accessMode", mode,
		"persistentVolume", boundTo)
	return ctrl.Result{}, nil
}

// finalize takes the preview volumes away always — they are the platform's
// bookkeeping, like branches — and the production volume only under
// deletionPolicy Delete. Under Retain the PVC is left in the application
// namespace, where a claim of the same name finds it again; and should the
// namespace go with the project, the PersistentVolume behind it has already
// been made to survive that (see retainVolume).
func (volumeContract) finalize(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
) error {
	appNS := appNamespace(claim.Spec.ProjectRef.Name)
	if claim.Status.Volume != nil {
		for _, preview := range claim.Status.Volume.Previews {
			if err := r.deleteVolumeClaim(ctx, appNS, preview.ClaimName); err != nil {
				return err
			}
		}
	}
	if claim.Spec.DeletionPolicy != kitchenv1alpha1.ClaimDelete {
		return nil
	}
	// Delete means the data goes. The volume's reclaim policy was set to
	// Retain the moment it bound, whatever the policy said then — so it is
	// put back to Delete first, and the PVC's deletion takes the volume with
	// it the way the class always would have.
	if claim.Status.Volume != nil && claim.Status.Volume.PersistentVolume != "" {
		err := r.setReclaimPolicy(ctx, claim.Status.Volume.PersistentVolume, corev1.PersistentVolumeReclaimDelete)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return r.deleteVolumeClaim(ctx, appNS, volumeClaimName(claim.Name))
}

// volumeRequirements reads the claim's spec.config into what the contract
// takes — the two vocabularies kept apart for the reason claimRequirements
// keeps the database's.
func volumeRequirements(claim *kitchenv1alpha1.ResourceClaim) volume.Requirements {
	cfg := claim.Volume()
	return volume.Requirements{
		Process:      cfg.Process,
		Size:         cfg.Size,
		StorageClass: cfg.StorageClass,
		MountPath:    cfg.MountPath,
	}
}

// volumeProcessOf refuses a process the project does not have. The web
// process is implicit — it is spec.runtime, not a member of spec.processes
// — and is named by the one name a process cannot take.
func volumeProcessOf(project *kitchenv1alpha1.Project, name string) error {
	names := project.ProcessNames()
	if slices.Contains(names, name) {
		return nil
	}
	return fmt.Errorf("process %q is not one of project %s's: a volume claim names the one process that mounts "+
		"it, and this project's processes are %s", name, project.Name, strings.Join(names, ", "))
}

// storageClassRefusal is a StorageClass the cluster cannot supply — one the
// claim named that does not exist, or no default where the claim named
// none. Both are refusals with the list of what does exist, rather than a
// PVC left Pending with nothing anywhere saying the words "storage class".
type storageClassRefusal struct{ message string }

func (e *storageClassRefusal) Error() string { return e.message }

// storageClassFor resolves the class a claim's volume is cut from: the one
// it names, or the cluster's default. Read uncached, like the snapshot
// class check in the API: it is one list per reconcile, and a list that
// misses a class an operator created a moment ago would refuse a claim the
// cluster can serve.
func (r *ResourceClaimReconciler) storageClassFor(ctx context.Context, name string) (*storagev1.StorageClass, error) {
	classes := &storagev1.StorageClassList{}
	if err := r.reader().List(ctx, classes); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(classes.Items))
	for i := range classes.Items {
		names = append(names, classes.Items[i].Name)
	}
	sort.Strings(names)
	if name != "" {
		for i := range classes.Items {
			if classes.Items[i].Name == name {
				return &classes.Items[i], nil
			}
		}
		return nil, &storageClassRefusal{fmt.Sprintf("StorageClass %q does not exist in this cluster; the classes "+
			"there are [%s]. Name one of them, or none to take the default", name, strings.Join(names, ", "))}
	}
	class := volume.DefaultClass(classes.Items)
	if class == nil {
		return nil, &storageClassRefusal{fmt.Sprintf("this cluster has no default StorageClass and the claim names "+
			"none. A default StorageClass is a prerequisite of every Kitchen cluster: mark one with the "+
			"%s=true annotation, or name one of [%s] on the claim", volume.DefaultClassAnnotation,
			strings.Join(names, ", "))}
	}
	return class, nil
}

// ensureProductionVolume makes sure the claim's own PVC exists, and answers
// the PersistentVolume it is bound to once it is — recording, under Retain,
// that the volume is to outlive the namespace. The answer is empty while the
// PVC is unbound, which is not a reason to wait: a class that binds on first
// consumer binds when the pod is scheduled, and a claim that waited for the
// binding would be waiting for the pod that waits for the claim.
//
// A PVC that does not exist is first looked for among retained volumes: a
// PersistentVolume this claim's earlier life left behind, labelled with the
// project and the claim, is pre-bound to the new PVC rather than a fresh one
// being cut. That is how a retained volume comes back.
func (r *ResourceClaimReconciler) ensureProductionVolume(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	appNS, name, className string,
	req volume.Requirements,
	mode corev1.PersistentVolumeAccessMode,
) (string, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: name}, pvc)
	switch {
	case apierrors.IsNotFound(err):
		retained, err := r.retainedVolume(ctx, claim)
		if err != nil {
			return "", err
		}
		pvc = newVolumeClaim(claim, appNS, name, "", className, req.Size, mode)
		if retained != nil {
			// The retained volume is the data, and its class and modes are
			// where and how the data is: the new PVC takes them from the
			// volume rather than from a claim that may have been written
			// against a different cluster.
			if err := r.prebindVolume(ctx, retained, appNS, name); err != nil {
				return "", err
			}
			pvc.Spec.VolumeName = retained.Name
			if retained.Spec.StorageClassName != "" {
				pvc.Spec.StorageClassName = ptr.To(retained.Spec.StorageClassName)
			}
			if len(retained.Spec.AccessModes) > 0 {
				pvc.Spec.AccessModes = retained.Spec.AccessModes
			}
		}
		if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", err
		}
		return "", nil
	case err != nil:
		return "", err
	}
	if pvc.Spec.VolumeName == "" || pvc.Status.Phase != corev1.ClaimBound {
		return "", nil
	}
	if claim.Spec.DeletionPolicy != kitchenv1alpha1.ClaimDelete {
		if err := r.retainVolume(ctx, claim, pvc.Spec.VolumeName); err != nil {
			return "", err
		}
	}
	return pvc.Spec.VolumeName, nil
}

// reconcilePreviewVolumes keeps one fresh, empty PVC per live preview
// Environment while the claim's preview mode gives previews a volume of
// their own, none otherwise, and takes a departed preview's volume away.
// The teardown needs no finalizer on the Environment: a PVC is not a
// provider-side object that would be orphaned, and the Environment watch
// brings the claim round again as the preview goes.
func (r *ResourceClaimReconciler) reconcilePreviewVolumes(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	projectName, appNS, className string,
	req volume.Requirements,
	mode corev1.PersistentVolumeAccessMode,
	fresh bool,
	previous map[string]kitchenv1alpha1.ClaimPreviewVolume,
) ([]kitchenv1alpha1.ClaimPreviewVolume, error) {
	kept := make([]kitchenv1alpha1.ClaimPreviewVolume, 0, len(previous)+1)
	keepRest := func() {
		for _, preview := range previous {
			kept = append(kept, preview)
		}
	}

	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := r.List(ctx, environments, client.InNamespace(claim.Namespace)); err != nil {
		keepRest()
		return kept, r.volumesNotReady(claim, "ListFailed", err)
	}

	for i := range environments.Items {
		env := &environments.Items[i]
		if env.Spec.ProjectRef.Name != projectName || env.Spec.Type != kitchenv1alpha1.EnvironmentPreview {
			continue
		}
		if !env.DeletionTimestamp.IsZero() || !fresh {
			if preview, ok := previous[env.Name]; ok {
				if err := r.deleteVolumeClaim(ctx, appNS, preview.ClaimName); err != nil {
					keepRest()
					return kept, r.volumesNotReady(claim, "VolumeTeardownFailed", err)
				}
				delete(previous, env.Name)
			}
			continue
		}
		preview, existed := previous[env.Name]
		if !existed {
			preview = kitchenv1alpha1.ClaimPreviewVolume{
				Environment: env.Name,
				ClaimName:   previewVolumeClaimName(claim.Name, env.Name),
				Provenance:  string(database.ProvenanceSynthetic),
			}
		}
		pvc := newVolumeClaim(claim, appNS, preview.ClaimName, env.Name, className, req.Size, mode)
		if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
			keepRest()
			return kept, r.volumesNotReady(claim, "VolumeFailed", err)
		}
		kept = append(kept, preview)
		delete(previous, env.Name)
		if !existed {
			// A volume this claim did not have before: sign and keep the
			// declaration for it, naming the preview, exactly as a new
			// database branch is recorded.
			r.recordDataClassDeclaration(ctx, claim, env.Name, preview.Provenance, volume.ProviderName)
		}
	}

	// Whatever is left over belongs to Environments that no longer exist.
	for name, preview := range previous {
		if err := r.deleteVolumeClaim(ctx, appNS, preview.ClaimName); err != nil {
			keepRest()
			return kept, r.volumesNotReady(claim, "VolumeTeardownFailed", err)
		}
		delete(previous, name)
	}
	sort.Slice(kept, func(a, b int) bool { return kept[a].Environment < kept[b].Environment })

	if !fresh {
		meta.RemoveStatusCondition(&claim.Status.Conditions, condVolumesReady)
		return kept, nil
	}
	setClaimCondition(claim, condVolumesReady, metav1.ConditionTrue, "VolumesReady",
		fmt.Sprintf("%d preview volume(s) in place", len(kept)))
	return kept, nil
}

func (r *ResourceClaimReconciler) volumesNotReady(claim *kitchenv1alpha1.ResourceClaim, reason string, err error) error {
	setClaimCondition(claim, condVolumesReady, metav1.ConditionFalse, reason, err.Error())
	return err
}

// newVolumeClaim is the PVC as this contract writes it: labelled with the
// project, the claim and — for a preview's — the environment, so that each
// can be found by whose it is.
func newVolumeClaim(
	claim *kitchenv1alpha1.ResourceClaim,
	appNS, name, environment, className, size string,
	mode corev1.PersistentVolumeAccessMode,
) *corev1.PersistentVolumeClaim {
	labels := volumeLabels(claim)
	if environment != "" {
		labels[labelEnvironment] = environment
	}
	quantity, err := resource.ParseQuantity(size)
	if err != nil {
		// Validated before anything reaches here; a PVC asking for nothing
		// is refused by the API server, which is the honest failure.
		quantity = resource.Quantity{}
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{mode},
			StorageClassName: ptr.To(className),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: quantity},
			},
		},
	}
}

func volumeLabels(claim *kitchenv1alpha1.ResourceClaim) map[string]string {
	return map[string]string{
		labelProject:        claim.Spec.ProjectRef.Name,
		labelClaim:          claim.Name,
		labelClaimNamespace: claim.Namespace,
		labelManagedByKey:   labelManagedByValue,
	}
}

func (r *ResourceClaimReconciler) deleteVolumeClaim(ctx context.Context, appNS, name string) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}}
	if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// retainVolume makes the PersistentVolume behind a bound claim survive the
// deletion of the PVC — and so of the application namespace, and so of the
// project: reclaim policy Retain, plus the labels a later claim finds it by.
// It is done at binding time rather than at deletion time because at
// deletion time the namespace may already be going.
func (r *ResourceClaimReconciler) retainVolume(ctx context.Context, claim *kitchenv1alpha1.ResourceClaim, name string) error {
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, pv); err != nil {
		return client.IgnoreNotFound(err)
	}
	patch := client.MergeFrom(pv.DeepCopy())
	changed := pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
	if pv.Labels == nil {
		pv.Labels = map[string]string{}
	}
	for key, value := range volumeLabels(claim) {
		if pv.Labels[key] != value {
			pv.Labels[key] = value
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.Patch(ctx, pv, patch)
}

// setReclaimPolicy is the other direction: a retained volume whose claim is
// deleted under Delete follows its PVC out.
func (r *ResourceClaimReconciler) setReclaimPolicy(
	ctx context.Context,
	name string,
	policy corev1.PersistentVolumeReclaimPolicy,
) error {
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, pv); err != nil {
		return err
	}
	if pv.Spec.PersistentVolumeReclaimPolicy == policy {
		return nil
	}
	patch := client.MergeFrom(pv.DeepCopy())
	pv.Spec.PersistentVolumeReclaimPolicy = policy
	return r.Patch(ctx, pv, patch)
}

// retainedVolume finds the PersistentVolume an earlier life of this claim
// left behind: labelled with the project and the claim, and no longer bound
// to anything — Released once its PVC went with the namespace, or Available
// if something already cleared its claimRef. Nil when there is none, which
// is every first claim.
func (r *ResourceClaimReconciler) retainedVolume(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
) (*corev1.PersistentVolume, error) {
	volumes := &corev1.PersistentVolumeList{}
	if err := r.List(ctx, volumes, client.MatchingLabels(volumeLabels(claim))); err != nil {
		return nil, err
	}
	candidates := make([]*corev1.PersistentVolume, 0, 1)
	for i := range volumes.Items {
		pv := &volumes.Items[i]
		if !pv.DeletionTimestamp.IsZero() {
			continue
		}
		switch pv.Status.Phase {
		case corev1.VolumeReleased, corev1.VolumeAvailable:
			candidates = append(candidates, pv)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	// Deterministic, so two reconciles racing pick the same one.
	sort.Slice(candidates, func(a, b int) bool { return candidates[a].Name < candidates[b].Name })
	return candidates[0], nil
}

// prebindVolume points a retained volume at the PVC about to be created —
// name and namespace, no UID — which is what makes the volume controller
// bind exactly that PVC to it rather than whichever claim of a matching
// size happens to be Pending. A Released volume's stale claimRef, still
// naming the PVC that went with the old namespace, is replaced by it.
func (r *ResourceClaimReconciler) prebindVolume(ctx context.Context, pv *corev1.PersistentVolume, appNS, name string) error {
	patch := client.MergeFrom(pv.DeepCopy())
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		APIVersion: "v1",
		Kind:       "PersistentVolumeClaim",
		Namespace:  appNS,
		Name:       name,
	}
	return r.Patch(ctx, pv, patch)
}

// mapVolumeToClaim enqueues the claim a PVC or PV belongs to, read off the
// labels the contract wrote. A PVC binding is the event that records the
// PersistentVolume on the status and, under Retain, makes it survive.
func (r *ResourceClaimReconciler) mapVolumeToClaim(_ context.Context, obj client.Object) []ctrl.Request {
	labels := obj.GetLabels()
	name, namespace := labels[labelClaim], labels[labelClaimNamespace]
	if name == "" || namespace == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}

// volumeClaimName is the production PVC's name, and previewVolumeClaimName
// a preview's — both deterministic, so a lost status is recovered by name
// rather than by cutting a second volume.
func volumeClaimName(claim string) string {
	return claim + "-volume"
}

func previewVolumeClaimName(claim, environment string) string {
	return claim + "-volume-" + environment
}

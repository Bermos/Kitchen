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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The names this suite reuses, spelled once — goconst counts a literal across
// every test file in the package.
const (
	testVolumeSet      = "resize-store"
	testExpandingClass = "resize-expands"
	testFixedClass     = "resize-fixed"
)

var _ = Describe("Growing the platform's volumes", func() {
	ctx := context.Background()

	var reconciler *KitchenReconciler
	var kitchen *kitchenv1alpha1.Kitchen
	var created []client.Object

	setCond := func(condType string, status metav1.ConditionStatus, reason, message string) {
		meta := metav1.Condition{
			Type: condType, Status: status, Reason: reason, Message: message,
			LastTransitionTime: metav1.Now(),
		}
		for i := range kitchen.Status.Conditions {
			if kitchen.Status.Conditions[i].Type == condType {
				kitchen.Status.Conditions[i] = meta
				return
			}
		}
		kitchen.Status.Conditions = append(kitchen.Status.Conditions, meta)
	}

	condition := func() *metav1.Condition {
		for i := range kitchen.Status.Conditions {
			if kitchen.Status.Conditions[i].Type == ConditionVolumesResized {
				return &kitchen.Status.Conditions[i]
			}
		}
		return nil
	}

	volume := func(name string) *kitchenv1alpha1.PlatformVolumeStatus {
		if kitchen.Status.Storage == nil {
			return nil
		}
		for i := range kitchen.Status.Storage.Volumes {
			if kitchen.Status.Storage.Volumes[i].StatefulSet == name {
				return &kitchen.Status.Storage.Volumes[i]
			}
		}
		return nil
	}

	track := func(obj client.Object) {
		ExpectWithOffset(1, k8sClient.Create(ctx, obj)).To(Succeed())
		created = append(created, obj)
	}

	// bound creates a claim and marks it Bound, because the API server
	// refuses a size change to a claim that is not — "spec is immutable after
	// creation except resources.requests … for bound claims" — and a platform
	// volume with anything on it is bound.
	bound := func(pvc *corev1.PersistentVolumeClaim) {
		track(pvc)
		pvc.Status.Phase = corev1.ClaimBound
		// Capacity as well as phase: a volume is Settled on what the driver
		// has actually given it, so a fixture that reports none would read as
		// forever mid-expansion.
		pvc.Status.Capacity = corev1.ResourceList{
			corev1.ResourceStorage: pvc.Spec.Resources.Requests[corev1.ResourceStorage],
		}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pvc)).To(Succeed())
	}

	// statefulSet is one platform StatefulSet with a single claim template and
	// the annotation that says how big its volume should be.
	statefulSet := func(declared, asked, className string) *appsv1.StatefulSet {
		name := testVolumeSet
		labels := map[string]string{labelPartOfKey: labelPartOfValue, "sel": name}
		set := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   PlatformNamespace,
				Labels:      labels,
				Annotations: map[string]string{StorageSizeAnnotation: asked},
			},
			Spec: appsv1.StatefulSetSpec{
				Replicas:    ptr.To(int32(1)),
				ServiceName: name,
				Selector:    &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "c", Image: "busybox",
					}}},
				},
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(declared)},
						},
						StorageClassName: ptr.To(className),
					},
				}},
			},
		}
		return set
	}

	// claim is the claim that template would have made for pod 0.
	claim := func(request, className string) *corev1.PersistentVolumeClaim {
		setName := testVolumeSet
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "data-" + setName + "-0",
				Namespace: PlatformNamespace,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(request)},
				},
				StorageClassName: ptr.To(className),
			},
		}
	}

	liveSet := func(name string) *appsv1.StatefulSet {
		set := &appsv1.StatefulSet{}
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: PlatformNamespace}, set)).To(Succeed())
		return set
	}

	liveClaim := func(name string) *corev1.PersistentVolumeClaim {
		pvc := &corev1.PersistentVolumeClaim{}
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: PlatformNamespace}, pvc)).To(Succeed())
		return pvc
	}

	// grants stands in for the storage driver finishing an expansion, which
	// nothing in envtest does.
	grants := func(name, capacity string) {
		pvc := liveClaim(name)
		pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pvc)).To(Succeed())
	}

	// appliedAs creates a StatefulSet the way Helm 4 does — a server-side
	// apply under a named field manager — which is the ownership the
	// replacement has to be handed back under.
	appliedAs := func(set *appsv1.StatefulSet, manager string) {
		// An apply patch is sent as the object itself, so it has to say what
		// kind it is; a typed object built in Go carries no TypeMeta.
		set.TypeMeta = metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "StatefulSet"}
		ExpectWithOffset(1, k8sClient.Patch(ctx, set, client.Apply,
			client.FieldOwner(manager), client.ForceOwnership)).To(Succeed())
		created = append(created, set)
	}

	BeforeEach(func() {
		reconciler = &KitchenReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
		}
		kitchen = &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		}
		created = nil
		Expect(reconciler.ensurePlatformNamespace(ctx)).To(Succeed())

		for _, class := range []struct {
			name    string
			expands bool
		}{{testExpandingClass, true}, {testFixedClass, false}} {
			existing := &storagev1.StorageClass{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: class.name}, existing)
			if err == nil {
				continue
			}
			Expect(k8sClient.Create(ctx, &storagev1.StorageClass{
				ObjectMeta:           metav1.ObjectMeta{Name: class.name},
				Provisioner:          "kitchen.test/none",
				AllowVolumeExpansion: ptr.To(class.expands),
			})).To(Succeed())
		}
	})

	AfterEach(func() {
		// envtest runs neither a garbage collector nor the storage-protection
		// controller, so an orphaning delete leaves the `orphan` finalizer and
		// every claim keeps its `pvc-protection` one. Standing in for both is
		// what keeps one spec's half-deleted object out of the next one's way.
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		for _, obj := range created {
			key := client.ObjectKeyFromObject(obj)
			Eventually(func() bool {
				fresh, _ := obj.DeepCopyObject().(client.Object)
				if err := k8sClient.Get(ctx, key, fresh); err != nil {
					return true
				}
				if len(fresh.GetFinalizers()) > 0 {
					fresh.SetFinalizers(nil)
					Expect(client.IgnoreNotFound(k8sClient.Update(ctx, fresh))).To(Succeed())
				}
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, fresh))).To(Succeed())
				return false
			}).Should(BeTrue())
		}
		stash := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: volumeResizeStashName, Namespace: PlatformNamespace,
		}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, stash))).To(Succeed())
	})

	It("has nothing to say about a platform with no volumes that declare a size", func() {
		Expect(reconciler.reconcilePlatformVolumes(ctx, kitchen, setCond)).To(BeTrue())
		Expect(kitchen.Status.Storage).To(BeNil())
		Expect(condition().Reason).To(Equal(ReasonNoPlatformVolumes))
	})

	It("leaves a volume already at the size it was asked to be alone", func() {
		track(statefulSet("20Gi", "20Gi", testExpandingClass))
		bound(claim("20Gi", testExpandingClass))

		Expect(reconciler.reconcilePlatformVolumes(ctx, kitchen, setCond)).To(BeTrue())
		Expect(condition().Status).To(Equal(metav1.ConditionTrue))
		Expect(condition().Reason).To(Equal(ReasonVolumesAtRequestedSize))

		entry := volume(testVolumeSet)
		Expect(entry).NotTo(BeNil())
		Expect(entry.Phase).To(Equal(volumePhaseSettled))
		Expect(entry.Current).To(Equal("20Gi"))
		Expect(entry.Desired).To(Equal("20Gi"))
		Expect(entry.Claims).To(ConsistOf("data-" + testVolumeSet + "-0"))
		Expect(entry.Message).To(BeEmpty())
		// A fact about the volume, recorded whether or not anybody has asked
		// it to grow: it is what the screen greys its button out on.
		Expect(entry.Expandable).To(HaveValue(BeTrue()))
		Expect(entry.Capacity).To(Equal("20Gi"))
		Expect(liveSet(testVolumeSet).UID).NotTo(BeEmpty())
	})

	It("expands the claim and replaces the workload when the chart asks for more", func() {
		// Applied rather than created, because Helm 4 applies: the field
		// manager that owns the claim template is what the replacement has to
		// go back under, and a plain create would not have one to preserve.
		appliedAs(statefulSet("20Gi", "40Gi", testExpandingClass), "helm")
		bound(claim("20Gi", testExpandingClass))
		before := liveSet(testVolumeSet).UID
		Expect(applyOwnerOf(liveSet(testVolumeSet))).To(Equal("helm"))

		Expect(reconciler.reconcilePlatformVolumes(ctx, kitchen, setCond)).To(BeFalse())

		// The claim is expanded first: it is the thing that actually has to
		// grow, and it is the step that is safe on its own.
		Expect(liveClaim("data" + "-" + testVolumeSet + "-0").
			Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("40Gi")))

		entry := volume(testVolumeSet)
		Expect(entry.Phase).To(Equal(volumePhaseGrowing))
		Expect(entry.Message).To(ContainSubstring("growing from 20Gi to 40Gi"))

		// envtest runs no garbage collector, so an orphaning delete leaves the
		// object with its `orphan` finalizer and a deletion timestamp rather
		// than removing it — which is exactly the state the stash exists for.
		deleting := liveSet(testVolumeSet)
		Expect(deleting.DeletionTimestamp).NotTo(BeNil())
		Expect(deleting.UID).To(Equal(before))

		stash := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: volumeResizeStashName, Namespace: PlatformNamespace}, stash)).To(Succeed())
		Expect(stash.Data).To(HaveKey(testVolumeSet))
		Expect(stash.Data[testVolumeSet]).To(ContainSubstring("40Gi"))

		// Standing in for the garbage collector, which is what finishes an
		// orphaning delete on a real cluster.
		deleting.Finalizers = nil
		Expect(k8sClient.Update(ctx, deleting)).To(Succeed())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: testVolumeSet, Namespace: PlatformNamespace}, &appsv1.StatefulSet{})
			return err != nil
		}).Should(BeTrue())

		// The next reconcile puts it back from the stash, at the new size.
		Expect(reconciler.reconcilePlatformVolumes(ctx, kitchen, setCond)).To(BeFalse())
		restored := liveSet(testVolumeSet)
		created = append(created, restored)
		Expect(restored.UID).NotTo(Equal(before))
		Expect(restored.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]).
			To(Equal(resource.MustParse("40Gi")))

		// And under the field manager that owned it. Helm 4 applies
		// server-side, so an object put back under any other manager makes
		// every later `helm upgrade` fail with a conflict on this exact
		// field — which is the failure this whole mechanism exists to avoid,
		// and it is invisible from anywhere but here.
		Expect(applyOwnerOf(restored)).To(Equal("helm"))

		// The claim has the request and not yet the space, so the volume is
		// the driver's to finish and says so rather than reading as done.
		Expect(volume(testVolumeSet).Phase).To(Equal(volumePhaseResizing))
		Expect(volume(testVolumeSet).Capacity).To(Equal("20Gi"))
		Expect(volume(testVolumeSet).Message).To(ContainSubstring("has not finished expanding"))
		Expect(condition().Reason).To(Equal(ReasonVolumesGrowing))

		// And once it has, the volume settles.
		grants("data-"+testVolumeSet+"-0", "40Gi")
		Expect(reconciler.reconcilePlatformVolumes(ctx, kitchen, setCond)).To(BeTrue())
		Expect(volume(testVolumeSet).Phase).To(Equal(volumePhaseSettled))

		// And the stash is forgotten, so nothing recreates it a second time.
		stashAfter := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: volumeResizeStashName, Namespace: PlatformNamespace}, stashAfter)).
			To(MatchError(ContainSubstring("not found")))
	})

	It("takes the largest size anybody asked for, so an old chart value cannot shrink one", func() {
		track(statefulSet("20Gi", "20Gi", testExpandingClass))
		bound(claim("20Gi", testExpandingClass))
		kitchen.Spec.Storage.Volumes = map[string]resource.Quantity{
			testVolumeSet: resource.MustParse("60Gi"),
		}

		Expect(reconciler.reconcilePlatformVolumes(ctx, kitchen, setCond)).To(BeFalse())
		Expect(volume(testVolumeSet).Desired).To(Equal("60Gi"))
		Expect(liveClaim("data-" + testVolumeSet + "-0").
			Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("60Gi")))
	})

	It("never shrinks, and says which volume is bigger than the chart asks for", func() {
		track(statefulSet("80Gi", "20Gi", testExpandingClass))
		bound(claim("80Gi", testExpandingClass))

		Expect(reconciler.reconcilePlatformVolumes(ctx, kitchen, setCond)).To(BeTrue())
		entry := volume(testVolumeSet)
		Expect(entry.Phase).To(Equal(volumePhaseSettled))
		Expect(entry.Message).To(ContainSubstring("volumes are grown and never shrunk"))
		Expect(liveClaim("data-" + testVolumeSet + "-0").
			Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("80Gi")))
		Expect(liveSet(testVolumeSet).DeletionTimestamp).To(BeNil())
	})

	It("refuses to grow a volume whose storage class does not expand, and names it", func() {
		track(statefulSet("20Gi", "40Gi", testFixedClass))
		bound(claim("20Gi", testFixedClass))

		// True, not false: nothing is in flight, so there is nothing to come
		// back for. A volume on a class that cannot expand is a fact about
		// the cluster, and requeueing over it for ever would make an
		// installation reconcile itself into the ground over a values file.
		Expect(reconciler.reconcilePlatformVolumes(ctx, kitchen, setCond)).To(BeTrue())
		entry := volume(testVolumeSet)
		Expect(entry.Phase).To(Equal(volumePhaseBlocked))
		Expect(entry.Expandable).To(HaveValue(BeFalse()))
		Expect(entry.Message).To(ContainSubstring(testFixedClass))
		Expect(entry.Message).To(ContainSubstring("does not"))
		Expect(condition().Status).To(Equal(metav1.ConditionFalse))
		Expect(condition().Reason).To(Equal(ReasonVolumeExpansionBlocked))
		Expect(condition().Message).To(ContainSubstring(testVolumeSet))

		// And it touched nothing: the refusal is the whole of what it did.
		Expect(liveClaim("data-" + testVolumeSet + "-0").
			Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("20Gi")))
		Expect(liveSet(testVolumeSet).DeletionTimestamp).To(BeNil())
	})

	It("will not delete a StatefulSet the object in hand is a stale copy of", func() {
		appliedAs(statefulSet("20Gi", "40Gi", testExpandingClass), "helm")
		bound(claim("20Gi", testExpandingClass))
		live := liveSet(testVolumeSet)

		// What a stale informer hands a reconciler: the object as it was
		// before something replaced it. Without the UID precondition on the
		// delete, this reconcile would orphan-delete the replacement it made
		// a moment ago — the one failure in this file that loses a workload
		// rather than merely failing to change one.
		stale := live.DeepCopy()
		stale.UID = "00000000-0000-0000-0000-000000000000"

		status := reconciler.reconcileOneVolume(ctx, kitchen, stale)
		Expect(status).NotTo(BeNil())
		// A read or a write that failed is `Unknown`, not `Blocked`: Blocked
		// is never retried and this has to be.
		Expect(status.Phase).To(Equal(volumePhaseUnknown))
		Expect(status.Message).To(ContainSubstring("claim template could not be rewritten"))

		// And the workload is where it was, with the identity it had.
		after := liveSet(testVolumeSet)
		Expect(after.UID).To(Equal(live.UID))
		Expect(after.DeletionTimestamp).To(BeNil())

		// The whole round reports it as a fault and asks to come back, which
		// is what makes it recoverable rather than a volume nobody looks at
		// again.
		Expect(reconciler.reconcilePlatformVolumes(ctx, kitchen, setCond)).To(BeFalse())
	})

	It("ignores a workload that declares no size, however many volumes it has", func() {
		set := statefulSet("20Gi", "40Gi", testExpandingClass)
		delete(set.Annotations, StorageSizeAnnotation)
		track(set)
		bound(claim("20Gi", testExpandingClass))

		Expect(reconciler.reconcilePlatformVolumes(ctx, kitchen, setCond)).To(BeTrue())
		Expect(kitchen.Status.Storage).To(BeNil())
		Expect(liveSet(testVolumeSet).DeletionTimestamp).To(BeNil())
	})
})

var _ = Describe("The storage status the singleton carries", func() {
	// Everything above builds this status; nothing above persists it. The
	// whole of `status` goes to the API server in one update, so a field the
	// CRD would refuse takes every other reconciler's status down with it —
	// which is a failure that shows up as some unrelated feature quietly not
	// reporting, and never as a message about storage.
	It("is one the API server accepts, with every field populated", func() {
		ctx := context.Background()
		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: "storage-status-probe"},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeNone},
			},
		}
		Expect(k8sClient.Create(ctx, kitchen)).To(Succeed())
		defer func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, kitchen))).To(Succeed()) }()

		kitchen.Status.Storage = &kitchenv1alpha1.PlatformStorageStatus{
			Volumes: []kitchenv1alpha1.PlatformVolumeStatus{{
				StatefulSet:  "kitchen-clickhouse",
				Component:    "clickhouse",
				Claims:       []string{"data-kitchen-clickhouse-0"},
				Current:      "80Gi",
				Capacity:     "20Gi",
				Desired:      "80Gi",
				StorageClass: "standard",
				Expandable:   ptr.To(true),
				Phase:        volumePhaseResizing,
				Message:      "the driver has not finished expanding it",
			}},
		}
		Expect(k8sClient.Status().Update(ctx, kitchen)).To(Succeed())

		fresh := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "storage-status-probe"}, fresh)).To(Succeed())
		Expect(fresh.Status.Storage).NotTo(BeNil())
		Expect(fresh.Status.Storage.Volumes).To(HaveLen(1))
		volume := fresh.Status.Storage.Volumes[0]
		// Read back one by one: a field the schema silently pruned is the
		// failure this is for, and a struct comparison would hide which.
		Expect(volume.Capacity).To(Equal("20Gi"))
		Expect(volume.Current).To(Equal("80Gi"))
		Expect(volume.Desired).To(Equal("80Gi"))
		Expect(volume.Expandable).To(HaveValue(BeTrue()))
		Expect(volume.Phase).To(Equal(volumePhaseResizing))
		Expect(volume.Claims).To(ConsistOf("data-kitchen-clickhouse-0"))
	})
})

var _ = Describe("The field manager a replaced StatefulSet is put back under", func() {
	It("is the one that owned the claim template through an apply", func() {
		set := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			ManagedFields: []metav1.ManagedFieldsEntry{
				{
					Manager:   "kubectl-edit",
					Operation: metav1.ManagedFieldsOperationUpdate,
					FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:volumeClaimTemplates":{}}}`)},
				},
				{
					Manager:   "helm",
					Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:volumeClaimTemplates":{}}}`)},
				},
			},
		}}
		// Helm 4 applies server-side, so an object put back under any other
		// manager makes every later `helm upgrade` fail with a conflict on
		// this exact field.
		Expect(applyOwnerOf(set)).To(Equal("helm"))
	})

	It("is nobody where nothing applied it, which is a plain create", func() {
		set := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager:   "helm",
				Operation: metav1.ManagedFieldsOperationUpdate,
				FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:volumeClaimTemplates":{}}}`)},
			}},
		}}
		Expect(applyOwnerOf(set)).To(BeEmpty())
	})
})

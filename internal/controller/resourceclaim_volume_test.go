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
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/volume"
)

var _ = Describe("ResourceClaim of type volume", func() {
	const (
		projectName = "volshop"
		namespace   = "default"
		previewEnv  = "volshop-pr-4"
	)

	ctx := context.Background()
	appNS := "kitchen-" + projectName

	var reconciler *ResourceClaimReconciler

	claimKey := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: namespace}
	}
	pvcKey := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: appNS}
	}

	reconcileOnce := func(name string) {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	getClaim := func(name string) *kitchenv1alpha1.ResourceClaim {
		claim := &kitchenv1alpha1.ResourceClaim{}
		ExpectWithOffset(1, k8sClient.Get(ctx, claimKey(name), claim)).To(Succeed())
		return claim
	}

	// createClaim writes the claim through the API server, so its spec.config
	// travels exactly as the REST API would have written it.
	createClaim := func(name string, policy kitchenv1alpha1.ClaimDeletionPolicy, config string) {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:           kitchenv1alpha1.ClaimTypeVolume,
				DeletionPolicy: policy,
				Config:         &runtime.RawExtension{Raw: []byte(config)},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	}

	// deleteClaim drives the finalizer until the claim is gone.
	deleteClaim := func(name string) {
		claim := &kitchenv1alpha1.ResourceClaim{}
		if err := k8sClient.Get(ctx, claimKey(name), claim); apierrors.IsNotFound(err) {
			return
		}
		ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
		EventuallyWithOffset(1, func() bool {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey(name)})
			Expect(err).NotTo(HaveOccurred())
			return apierrors.IsNotFound(k8sClient.Get(ctx, claimKey(name), &kitchenv1alpha1.ResourceClaim{}))
		}).Should(BeTrue())
	}

	// terminating says a PVC is gone or on its way: envtest runs no
	// controller to lift the storage-protection finalizer the API server
	// puts on every claim, so a deleted PVC stays Terminating here.
	terminating := func(key types.NamespacedName) bool {
		pvc := &corev1.PersistentVolumeClaim{}
		err := k8sClient.Get(ctx, key, pvc)
		if apierrors.IsNotFound(err) {
			return true
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return !pvc.DeletionTimestamp.IsZero()
	}

	// bindTo fakes what the volume controller would do: a PersistentVolume,
	// the PVC pointed at it, and the PVC's phase Bound.
	bindTo := func(pvcName, pvName string) {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: pvName},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
				StorageClassName:              "standard",
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/" + pvName},
				},
				ClaimRef: &corev1.ObjectReference{Namespace: appNS, Name: pvcName},
			},
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, pv))).To(Succeed())

		pvc := &corev1.PersistentVolumeClaim{}
		ExpectWithOffset(1, k8sClient.Get(ctx, pvcKey(pvcName), pvc)).To(Succeed())
		pvc.Spec.VolumeName = pvName
		ExpectWithOffset(1, k8sClient.Update(ctx, pvc)).To(Succeed())
		ExpectWithOffset(1, k8sClient.Get(ctx, pvcKey(pvcName), pvc)).To(Succeed())
		pvc.Status.Phase = corev1.ClaimBound
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pvc)).To(Succeed())
	}

	// removePVC takes a PVC away the way a namespace's teardown does:
	// finalizers and all, until it is not there.
	removePVC := func(name string) {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := k8sClient.Get(ctx, pvcKey(name), pvc); apierrors.IsNotFound(err) {
			return
		}
		pvc.Finalizers = nil
		ExpectWithOffset(1, k8sClient.Update(ctx, pvc)).To(Succeed())
		ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, pvc))).To(Succeed())
		EventuallyWithOffset(1, func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, pvcKey(name), &corev1.PersistentVolumeClaim{}))
		}).Should(BeTrue())
	}

	// releasePV is what the volume controller does to a Retain volume whose
	// claim went: phase Released, claimRef still naming the old claim.
	releasePV := func(name string) {
		pv := &corev1.PersistentVolume{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name}, pv)).To(Succeed())
		pv.Status.Phase = corev1.VolumeReleased
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pv)).To(Succeed())
	}

	BeforeEach(func() {
		reconciler = &ResourceClaimReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		// Two classes: the cluster's default, a block driver nothing knows
		// to be shared, and a shared filesystem.
		standard := &storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "standard",
				Annotations: map[string]string{volume.DefaultClassAnnotation: "true"},
			},
			Provisioner: "rancher.io/local-path",
		}
		shared := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "shared"},
			Provisioner: "nfs.csi.k8s.io",
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, standard))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, shared))).To(Succeed())

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/volshop",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
				Processes: []kitchenv1alpha1.ProcessSpec{{
					Name: "worker",
					Type: kitchenv1alpha1.ProcessWorker,
				}},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())
	})

	AfterEach(func() {
		env := &kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: previewEnv, Namespace: namespace}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())

		// Every claim of the project, through its finalizer; then the PVCs
		// and PVs the tests left, finalizers and all.
		claims := &kitchenv1alpha1.ResourceClaimList{}
		Expect(k8sClient.List(ctx, claims, client.InNamespace(namespace))).To(Succeed())
		for i := range claims.Items {
			if claims.Items[i].Spec.ProjectRef.Name == projectName {
				deleteClaim(claims.Items[i].Name)
			}
		}
		pvcs := &corev1.PersistentVolumeClaimList{}
		Expect(k8sClient.List(ctx, pvcs, client.InNamespace(appNS))).To(Succeed())
		for i := range pvcs.Items {
			removePVC(pvcs.Items[i].Name)
		}
		pvs := &corev1.PersistentVolumeList{}
		Expect(k8sClient.List(ctx, pvs, client.MatchingLabels{labelProject: projectName})).To(Succeed())
		for i := range pvs.Items {
			pv := &pvs.Items[i]
			pv.Finalizers = nil
			Expect(k8sClient.Update(ctx, pv)).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pv))).To(Succeed())
		}
	})

	It("refuses a claim naming a process the project does not have, and binds once it names one", func() {
		const name = "volshop-a"
		createClaim(name, "", `{"volume": {"process": "cache", "size": "1Gi", "mountPath": "/data"}}`)
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		ready := meta.FindStatusCondition(claim.Status.Conditions, condReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("ProcessUnknown"))
		Expect(ready.Message).To(ContainSubstring("web, worker"))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, pvcKey(name+"-volume"), &corev1.PersistentVolumeClaim{}))).
			To(BeTrue(), "a refused claim cuts no volume")

		claim.Spec.Config = &runtime.RawExtension{
			Raw: []byte(`{"volume": {"process": "worker", "size": "1Gi", "mountPath": "/data"}}`),
		}
		Expect(k8sClient.Update(ctx, claim)).To(Succeed())
		reconcileOnce(name)

		claim = getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.SecretName).To(BeEmpty(), "a volume binds to a mount, not to a secret")
		Expect(claim.Status.Volume).NotTo(BeNil())
		Expect(claim.Status.Volume.Process).To(Equal("worker"))
		Expect(claim.Status.Volume.MountPath).To(Equal("/data"))
		Expect(claim.Status.Volume.ClaimName).To(Equal(name + "-volume"))
		Expect(claim.Status.Volume.StorageClass).To(Equal("standard"))
		Expect(claim.Status.Volume.AccessMode).To(Equal(string(corev1.ReadWriteOnce)))
		Expect(claim.Status.ForcesRecreate).To(BeTrue())
		Expect(claim.Status.PreviewMode).To(Equal("fresh"))
		Expect(claim.Status.DataProvenance).To(Equal("production"))

		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, pvcKey(name+"-volume"), pvc)).To(Succeed())
		Expect(pvc.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}))
		Expect(*pvc.Spec.StorageClassName).To(Equal("standard"))
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("1Gi"))
		Expect(pvc.Labels).To(HaveKeyWithValue(labelClaim, name))
		Expect(pvc.Labels).To(HaveKeyWithValue(labelManagedByKey, labelManagedByValue))
	})

	It("refuses a StorageClass the cluster does not have, naming the ones it has", func() {
		const name = "volshop-b"
		createClaim(name, "", `{"volume": {"process": "web", "size": "1Gi", "mountPath": "/data", "storageClass": "nvme"}}`)
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		ready := meta.FindStatusCondition(claim.Status.Conditions, condReady)
		Expect(ready.Reason).To(Equal("StorageClassMissing"))
		Expect(ready.Message).To(ContainSubstring("standard"))
	})

	It("detects ReadWriteMany and lifts the recreate", func() {
		const name = "volshop-c"
		createClaim(name, "", `{"volume": {"process": "web", "size": "1Gi", "mountPath": "/data", "storageClass": "shared"}}`)
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Volume.AccessMode).To(Equal(string(corev1.ReadWriteMany)))
		Expect(claim.Status.Volume.AccessModeReason).To(ContainSubstring("shared filesystem"))
		Expect(claim.Status.ForcesRecreate).To(BeFalse(), "a shared filesystem attaches to more than one pod")

		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, pvcKey(name+"-volume"), pvc)).To(Succeed())
		Expect(pvc.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}))
	})

	It("cuts a fresh, empty volume per preview and removes it with the preview", func() {
		const name = "volshop-d"
		createClaim(name, "", `{"volume": {"process": "web", "size": "1Gi", "mountPath": "/data"}}`)
		reconcileOnce(name)

		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: previewEnv, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentPreview,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: "volshop-rel-1"},
				Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 4, Branch: "feature"},
			},
		}
		Expect(k8sClient.Create(ctx, env)).To(Succeed())
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Volume.Previews).To(HaveLen(1))
		preview := claim.Status.Volume.Previews[0]
		Expect(preview.Environment).To(Equal(previewEnv))
		Expect(preview.ClaimName).To(Equal(name + "-volume-" + previewEnv))
		Expect(preview.Provenance).To(Equal("synthetic"))
		Expect(meta.IsStatusConditionTrue(claim.Status.Conditions, condVolumesReady)).To(BeTrue())

		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, pvcKey(preview.ClaimName), pvc)).To(Succeed())
		Expect(pvc.Labels).To(HaveKeyWithValue(labelEnvironment, previewEnv))
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("1Gi"))

		Expect(k8sClient.Delete(ctx, env)).To(Succeed())
		reconcileOnce(name)

		Expect(terminating(pvcKey(preview.ClaimName))).To(BeTrue(), "the preview's volume goes with the preview")
		Expect(getClaim(name).Status.Volume.Previews).To(BeEmpty())
		Expect(terminating(pvcKey(name+"-volume"))).To(BeFalse(), "production's volume stays")
	})

	It("retains the volume behind a bound claim, and re-binds a later claim of the same name to it", func() {
		const (
			name = "volshop-e"
			pv   = "pv-volshop-e"
		)
		createClaim(name, kitchenv1alpha1.ClaimRetain, `{"volume": {"process": "web", "size": "1Gi", "mountPath": "/data"}}`)
		reconcileOnce(name)
		Expect(getClaim(name).Status.Volume.PersistentVolume).To(BeEmpty(), "unbound until the cluster binds it")

		bindTo(name+"-volume", pv)
		reconcileOnce(name)

		Expect(getClaim(name).Status.Volume.PersistentVolume).To(Equal(pv))
		retained := &corev1.PersistentVolume{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pv}, retained)).To(Succeed())
		Expect(retained.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain),
			"the volume is made to outlive its namespace the moment it binds")
		Expect(retained.Labels).To(HaveKeyWithValue(labelClaim, name))
		Expect(retained.Labels).To(HaveKeyWithValue(labelProject, projectName))

		// The project goes: its namespace takes the PVC, the volume
		// controller marks the volume Released, and the claim is finalized
		// under Retain — which leaves the volume exactly where it is.
		removePVC(name + "-volume")
		releasePV(pv)
		deleteClaim(name)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pv}, retained)).To(Succeed())
		Expect(retained.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))

		// A claim of the same name finds it and pre-binds a new PVC to it
		// rather than cutting a fresh one.
		createClaim(name, kitchenv1alpha1.ClaimRetain, `{"volume": {"process": "web", "size": "1Gi", "mountPath": "/data"}}`)
		reconcileOnce(name)

		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, pvcKey(name+"-volume"), pvc)).To(Succeed())
		Expect(pvc.Spec.VolumeName).To(Equal(pv))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pv}, retained)).To(Succeed())
		Expect(retained.Spec.ClaimRef).NotTo(BeNil())
		Expect(retained.Spec.ClaimRef.Namespace).To(Equal(appNS))
		Expect(retained.Spec.ClaimRef.Name).To(Equal(name + "-volume"))
		Expect(getClaim(name).Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
	})

	It("deletes the volume with the claim under Delete", func() {
		const (
			name = "volshop-f"
			pv   = "pv-volshop-f"
		)
		createClaim(name, kitchenv1alpha1.ClaimDelete, `{"volume": {"process": "web", "size": "1Gi", "mountPath": "/data"}}`)
		reconcileOnce(name)
		bindTo(name+"-volume", pv)
		reconcileOnce(name)

		retained := &corev1.PersistentVolume{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pv}, retained)).To(Succeed())
		Expect(retained.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimDelete),
			"a claim under Delete does not retain its volume")

		deleteClaim(name)
		Expect(terminating(pvcKey(name+"-volume"))).To(BeTrue(), "the PVC goes with the claim")
	})
})

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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
)

// The other source: a volume that was already there. What is under test is
// the half of the contract that inverts — nothing is cut, nothing may be
// destroyed, and two projects holding one filesystem is a question the
// platform answers rather than discovers.
var _ = Describe("ResourceClaim of type volume, bound to one that exists", func() {
	const (
		projectName = "bindshop"
		otherName   = "bindshop-two"
		namespace   = "default"
		export      = "nfs://nas.lan/export/media"
	)

	ctx := context.Background()
	appNS := "kitchen-" + projectName

	var reconciler *ResourceClaimReconciler
	var made []string

	claimKey := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: namespace}
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

	readyOf := func(name string) *metav1.Condition {
		condition := meta.FindStatusCondition(getClaim(name).Status.Conditions, condReady)
		ExpectWithOffset(1, condition).NotTo(BeNil())
		return condition
	}

	// An NFS-backed volume somebody wrote before the platform was installed.
	// Two of them over one export is how two projects reach one filesystem,
	// which is why the platform compares what they point at.
	makeVolume := func(name, path string, modes ...corev1.PersistentVolumeAccessMode) {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("12Ti")},
				AccessModes: modes,
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					NFS: &corev1.NFSVolumeSource{Server: "nas.lan", Path: path},
				},
			},
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, pv))).To(Succeed())
		made = append(made, name)
	}

	createClaim := func(name, project string, policy kitchenv1alpha1.ClaimDeletionPolicy, config string) {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: project},
				Type:           kitchenv1alpha1.ClaimTypeVolume,
				DeletionPolicy: policy,
				Config:         &runtime.RawExtension{Raw: []byte(config)},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	}

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

	project := func(name string) *kitchenv1alpha1.Project {
		return &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/" + name,
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
			},
		}
	}

	BeforeEach(func() {
		reconciler = &ResourceClaimReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		made = nil
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project(projectName)))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project(otherName)))).To(Succeed())
	})

	AfterEach(func() {
		claims := &kitchenv1alpha1.ResourceClaimList{}
		Expect(k8sClient.List(ctx, claims, client.InNamespace(namespace))).To(Succeed())
		for i := range claims.Items {
			if name := claims.Items[i].Spec.ProjectRef.Name; name == projectName || name == otherName {
				deleteClaim(claims.Items[i].Name)
			}
		}
		// The volumes this file made are the operator's in real life and
		// nothing in the platform may remove them — which is the point — so
		// the test takes back exactly what the test wrote.
		for _, name := range made {
			pv := &corev1.PersistentVolume{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, pv); apierrors.IsNotFound(err) {
				continue
			}
			pv.Finalizers = nil
			Expect(k8sClient.Update(ctx, pv)).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pv))).To(Succeed())
		}
	})

	It("fails legibly, naming the volume, when the cluster does not have it", func() {
		const name = "bind-missing"
		createClaim(name, projectName, "", `{"volume": {"source": "bind", "process": "web", "mountPath": "/media",
			"bind": {"persistentVolume": "nas-that-is-not-there", "accessMode": "ReadOnlyMany"}}}`)
		reconcileOnce(name)

		Expect(getClaim(name).Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		ready := readyOf(name)
		Expect(ready.Reason).To(Equal("VolumeNotFound"))
		Expect(ready.Message).To(ContainSubstring("nas-that-is-not-there"))
	})

	It("mounts an existing volume read-only, provisioning nothing, and shares it with previews", func() {
		const name = "bind-media"
		makeVolume("nas-media", "/export/media", corev1.ReadWriteMany)
		createClaim(name, projectName, "", `{"volume": {"source": "bind", "process": "web", "mountPath": "/media",
			"bind": {"persistentVolume": "nas-media", "accessMode": "ReadOnlyMany"}}}`)
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Volume).NotTo(BeNil())
		Expect(claim.Status.Volume.Source).To(Equal(kitchenv1alpha1.VolumeBind))
		Expect(claim.Status.Volume.AccessMode).To(Equal(string(corev1.ReadOnlyMany)))
		Expect(claim.Status.Volume.Bound).NotTo(BeNil())
		Expect(claim.Status.Volume.Bound.PersistentVolume).To(Equal("nas-media"))
		Expect(claim.Status.Volume.Bound.Identity).To(Equal(export))
		Expect(claim.Status.Volume.Bound.Capacity).To(Equal("12Ti"))
		Expect(claim.Status.Volume.Bound.Writable).To(BeFalse())
		// Read by many at once, so neither the replica cap nor the recreate
		// applies — exactly as a class detected ReadWriteMany lifts both.
		Expect(claim.Status.ForcesRecreate).To(BeFalse())
		// A preview of an application whose data is the point of it reads
		// what production reads, and can change none of it.
		Expect(claim.Status.PreviewMode).To(Equal(string(contract.PreviewShared)))
		Expect(claim.Status.DataProvenance).To(Equal("production"))

		// The one object the platform made, pre-bound to the volume by name
		// rather than left to match whichever the controller reached first.
		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: name + "-volume"}, pvc)).To(Succeed())
		Expect(pvc.Spec.VolumeName).To(Equal("nas-media"))
		Expect(pvc.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}))

		pv := &corev1.PersistentVolume{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "nas-media"}, pv)).To(Succeed())
		Expect(pv.Spec.ClaimRef).NotTo(BeNil())
		Expect(pv.Spec.ClaimRef.Name).To(Equal(name + "-volume"))

		// Teardown unmounts and never deletes: the volume outlives the claim
		// that was reading it, because it was never the platform's.
		deleteClaim(name)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "nas-media"}, pv)).To(Succeed())
	})

	It("refuses a deletionPolicy that would destroy a volume the platform did not create", func() {
		const name = "bind-delete"
		makeVolume("nas-delete", "/export/delete", corev1.ReadWriteMany)
		createClaim(name, projectName, kitchenv1alpha1.ClaimDelete,
			`{"volume": {"source": "bind", "process": "web", "mountPath": "/media",
			  "bind": {"persistentVolume": "nas-delete", "accessMode": "ReadOnlyMany"}}}`)
		reconcileOnce(name)

		Expect(getClaim(name).Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		ready := readyOf(name)
		Expect(ready.Reason).To(Equal("DeletionPolicyRefused"))
		Expect(ready.Message).To(ContainSubstring("not the platform's to destroy"))
	})

	It("refuses an access mode the volume does not offer, naming the ones it does", func() {
		const name = "bind-mode"
		makeVolume("nas-readonly", "/export/readonly", corev1.ReadOnlyMany)
		createClaim(name, projectName, "", `{"volume": {"source": "bind", "process": "web", "mountPath": "/media",
			"bind": {"persistentVolume": "nas-readonly", "accessMode": "ReadWriteMany"}}}`)
		reconcileOnce(name)

		ready := readyOf(name)
		Expect(ready.Reason).To(Equal("AccessModeUnavailable"))
		Expect(ready.Message).To(ContainSubstring("ReadOnlyMany"))
	})

	It("gives previews nothing where the volume attaches to one pod at a time", func() {
		const name = "bind-once"
		makeVolume("nas-once", "/export/once", corev1.ReadWriteOnce)
		createClaim(name, projectName, "", `{"volume": {"source": "bind", "process": "web", "mountPath": "/data",
			"bind": {"persistentVolume": "nas-once", "accessMode": "ReadWriteOnce"}}}`)
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.ForcesRecreate).To(BeTrue())
		Expect(claim.Status.PreviewMode).To(Equal(string(contract.PreviewNone)))
		Expect(claim.Status.PreviewReason).To(ContainSubstring("production has it"))
		Expect(claim.Status.Volume.Bound.Writable).To(BeTrue())
	})

	// The decision the issue asked to be made in the open, made where the
	// claim is validated: any number of readers, one writer, and the second
	// writer is told which claim holds it.
	It("lets two projects read one volume and refuses the second one that would write it", func() {
		// The names are chosen so that the two claims resolve the same way
		// on every reconcile: a cluster stamps creation to the second, so
		// two claims made in one second are ordered by name, and the writer
		// has to be the earlier of the two for the refusal to be the
		// second one's rather than a pair refusing each other forever.
		const (
			first  = "bind-a-writer"
			reader = "bind-b-reader"
			second = "bind-c-writer"
		)
		makeVolume("nas-shared-a", "/export/shared", corev1.ReadWriteMany)
		makeVolume("nas-shared-b", "/export/shared", corev1.ReadWriteMany)

		createClaim(first, projectName, "", `{"volume": {"source": "bind", "process": "web", "mountPath": "/media",
			"bind": {"persistentVolume": "nas-shared-a", "accessMode": "ReadWriteMany"}}}`)
		reconcileOnce(first)
		Expect(getClaim(first).Status.Volume.Bound.Writable).To(BeTrue())

		// A reader of the same storage through a second volume: allowed,
		// and each of them told who else is holding it.
		createClaim(reader, otherName, "", `{"volume": {"source": "bind", "process": "web", "mountPath": "/media",
			"bind": {"persistentVolume": "nas-shared-b", "accessMode": "ReadOnlyMany"}}}`)
		reconcileOnce(reader)
		claim := getClaim(reader)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Volume.Bound.SharedWith).To(ContainElement(projectName + "/" + first))

		// A second writer of the same storage: refused, naming the claim and
		// the project that already writes it.
		claim = getClaim(reader)
		claim.Spec.Config = &runtime.RawExtension{
			Raw: []byte(`{"volume": {"source": "bind", "process": "web", "mountPath": "/media",
			  "bind": {"persistentVolume": "nas-shared-b", "accessMode": "ReadWriteMany"}}}`),
		}
		Expect(k8sClient.Update(ctx, claim)).To(Succeed())
		reconcileOnce(reader)

		ready := readyOf(reader)
		Expect(ready.Reason).To(Equal("VolumeWrittenElsewhere"))
		Expect(ready.Message).To(ContainSubstring(first))
		Expect(ready.Message).To(ContainSubstring(projectName))
		// The refusal names the storage, not the object in front of it: two
		// volumes, one export, one filesystem.
		Expect(ready.Message).To(ContainSubstring("nfs://nas.lan/export/shared"))

		// And a claim created after the writer is refused the same way, for
		// the same reason: the oldest writer keeps it.
		createClaim(second, otherName, "", `{"volume": {"source": "bind", "process": "web", "mountPath": "/media",
			"bind": {"persistentVolume": "nas-shared-b", "accessMode": "ReadWriteMany"}}}`)
		reconcileOnce(second)
		Expect(readyOf(second).Reason).To(Equal("VolumeWrittenElsewhere"))
	})
})

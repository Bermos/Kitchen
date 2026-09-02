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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

var _ = Describe("An environment with a volume claim", func() {
	const (
		projectName = "mountshop"
		envName     = "mountshop-production"
		releaseName = "mountshop-rel-000001"
		claimName   = "mountshop-data"
		namespace   = "default"
		image       = "registry.example.com/kitchen/mountshop@sha256:0123456789abcdef"
	)

	ctx := context.Background()
	appNS := "kitchen-" + projectName
	envKey := types.NamespacedName{Name: envName, Namespace: namespace}
	claimKey := types.NamespacedName{Name: claimName, Namespace: namespace}
	webKey := types.NamespacedName{Name: envName, Namespace: appNS}
	workerKey := types.NamespacedName{Name: envName + "-worker", Namespace: appNS}

	var reconciler *EnvironmentReconciler

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	// setVolume writes on the claim what its own reconciler would have: a
	// bound volume for the named process, with the access mode the class
	// was detected to support.
	setVolume := func(process string, mode corev1.PersistentVolumeAccessMode) {
		claim := &kitchenv1alpha1.ResourceClaim{}
		ExpectWithOffset(1, k8sClient.Get(ctx, claimKey, claim)).To(Succeed())
		claim.Status.Phase = kitchenv1alpha1.ClaimBound
		claim.Status.PreviewMode = "fresh"
		claim.Status.ForcesRecreate = mode == corev1.ReadWriteOnce
		claim.Status.Volume = &kitchenv1alpha1.ClaimVolumeStatus{
			Process:      process,
			MountPath:    "/data",
			StorageClass: "standard",
			AccessMode:   string(mode),
			ClaimName:    claimName + "-volume",
		}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, claim)).To(Succeed())
	}

	deployment := func(key types.NamespacedName) *appsv1.Deployment {
		deploy := &appsv1.Deployment{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, deploy)).To(Succeed())
		return deploy
	}

	mountOf := func(deploy *appsv1.Deployment) (string, string) {
		spec := deploy.Spec.Template.Spec
		if len(spec.Volumes) == 0 || len(spec.Containers[0].VolumeMounts) == 0 {
			return "", ""
		}
		return spec.Volumes[0].PersistentVolumeClaim.ClaimName, spec.Containers[0].VolumeMounts[0].MountPath
	}

	BeforeEach(func() {
		reconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())

		worker := kitchenv1alpha1.ProcessSpec{
			Name:     "worker",
			Type:     kitchenv1alpha1.ProcessWorker,
			Command:  []string{"node", "worker.js"},
			Replicas: ptr.To(int32(2)),
		}
		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/mountshop",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
				Processes: []kitchenv1alpha1.ProcessSpec{worker},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		release := &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "mountshop-bld-1"},
				Image:      image,
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime:   kitchenv1alpha1.RuntimeSpec{Port: 8080, Replicas: ptr.To(int32(3))},
					Processes: []kitchenv1alpha1.ProcessSpec{worker},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())

		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentProduction,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())

		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.ClaimTypeVolume,
				Config: &runtime.RawExtension{
					Raw: []byte(`{"volume": {"process": "web", "size": "1Gi", "mountPath": "/data"}}`),
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, claim))).To(Succeed())
	})

	AfterEach(func() {
		env := &kitchenv1alpha1.Environment{}
		if err := k8sClient.Get(ctx, envKey, env); err == nil {
			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			reconcileOnce()
		}
		claim := &kitchenv1alpha1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, claimKey, &kitchenv1alpha1.ResourceClaim{}))
		}).Should(BeTrue())
	})

	It("waits while the claim has no volume for it yet", func() {
		reconcileOnce()

		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		ready := meta.FindStatusCondition(env.Status.Conditions, condReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("VolumeNotBound"))
		Expect(ready.Message).To(ContainSubstring(claimName))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, webKey, &appsv1.Deployment{}))).To(BeTrue(),
			"nothing is deployed without its volume")
	})

	It("mounts the volume into the web process, caps it at one replica and recreates it", func() {
		setVolume(kitchenv1alpha1.WebProcessName, corev1.ReadWriteOnce)
		reconcileOnce()

		web := deployment(webKey)
		Expect(*web.Spec.Replicas).To(Equal(int32(1)), "a volume that attaches once caps the process at one")
		Expect(web.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
		pvc, path := mountOf(web)
		Expect(pvc).To(Equal(claimName + "-volume"))
		Expect(path).To(Equal("/data"))

		worker := deployment(workerKey)
		Expect(*worker.Spec.Replicas).To(Equal(int32(2)), "a process the claim does not name is untouched")
		Expect(worker.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
		Expect(worker.Spec.Template.Spec.Volumes).To(BeEmpty())
	})

	It("mounts the volume into the named worker instead, and leaves the web process alone", func() {
		setVolume("worker", corev1.ReadWriteOnce)
		reconcileOnce()

		web := deployment(webKey)
		Expect(*web.Spec.Replicas).To(Equal(int32(3)))
		Expect(web.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
		Expect(web.Spec.Template.Spec.Volumes).To(BeEmpty())

		worker := deployment(workerKey)
		Expect(*worker.Spec.Replicas).To(Equal(int32(1)))
		Expect(worker.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
		pvc, path := mountOf(worker)
		Expect(pvc).To(Equal(claimName + "-volume"))
		Expect(path).To(Equal("/data"))
	})

	It("caps the autoscaler's ceiling too, where KEDA owns the replica count", func() {
		kitchenKey := types.NamespacedName{Name: KitchenSingletonName}
		setPlatform := func(enabled bool) {
			kitchen := &kitchenv1alpha1.Kitchen{}
			ExpectWithOffset(1, k8sClient.Get(ctx, kitchenKey, kitchen)).To(Succeed())
			kitchen.Spec.ScaleToZero.Enabled = enabled
			// The interceptor's namespace has to exist for the grant that
			// lets the route reach it; the platform's own does.
			kitchen.Spec.ScaleToZero.Interceptor = kitchenv1alpha1.InterceptorSpec{
				Service:   "keda-add-ons-http-interceptor-proxy",
				Namespace: PlatformNamespace,
				Port:      8080,
			}
			ExpectWithOffset(1, k8sClient.Update(ctx, kitchen)).To(Succeed())
		}
		setPlatform(true)
		DeferCleanup(func() { setPlatform(false) })
		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, project)).To(Succeed())
		project.Spec.ScaleToZero = kitchenv1alpha1.ScaleToZeroPolicy{
			Mode:        kitchenv1alpha1.ScaleToZeroAlways,
			MaxReplicas: ptr.To(int32(5)),
		}
		Expect(k8sClient.Update(ctx, project)).To(Succeed())

		setVolume(kitchenv1alpha1.WebProcessName, corev1.ReadWriteOnce)
		reconcileOnce()

		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		Expect(meta.FindStatusCondition(env.Status.Conditions, condScaleToZero).Reason).To(Equal("IdlesToZero"),
			"a volume does not stop an environment idling: 0 → 1 → 0 never has two attachers")

		scaled := &unstructured.Unstructured{}
		scaled.SetGroupVersionKind(HTTPScaledObjectGVK())
		Expect(k8sClient.Get(ctx, webKey, scaled)).To(Succeed())
		maxReplicas, _, err := unstructured.NestedInt64(scaled.Object, "spec", "replicas", "max")
		Expect(err).NotTo(HaveOccurred())
		Expect(maxReplicas).To(Equal(int64(1)), "the ceiling is capped where the count is KEDA's to write")

		// And the count itself stays KEDA's: a parked workload reconciled
		// again is still parked. Asserting the field is unset would assert
		// the wrong thing — the API server defaults it to one on creation,
		// so an untouched Deployment and a Deployment written with one look
		// the same. What must hold is that the reconciler does not write it.
		By("letting KEDA park the workload")
		parked := deployment(webKey)
		parked.Spec.Replicas = ptr.To(int32(0))
		Expect(k8sClient.Update(ctx, parked)).To(Succeed())

		reconcileOnce()

		Expect(deployment(webKey).Spec.Replicas).To(HaveValue(Equal(int32(0))),
			"an idling Deployment's count is not the reconciler's to write back")
	})

	It("lifts the cap and the recreate for a class detected to support ReadWriteMany", func() {
		setVolume(kitchenv1alpha1.WebProcessName, corev1.ReadWriteOnce)
		reconcileOnce()
		Expect(deployment(webKey).Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))

		setVolume(kitchenv1alpha1.WebProcessName, corev1.ReadWriteMany)
		reconcileOnce()

		web := deployment(webKey)
		Expect(*web.Spec.Replicas).To(Equal(int32(3)))
		Expect(web.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
		pvc, _ := mountOf(web)
		Expect(pvc).To(Equal(claimName+"-volume"), "still mounted, just no longer alone")
	})
})

func TestMountsAreThePodSpecsHalf(t *testing.T) {
	mounts := []mountedVolume{
		{claim: "data", process: "web", claimName: "data-volume", mountPath: "/data", attachOnce: true},
		{claim: "cache", process: "worker", claimName: "cache-volume", mountPath: "/cache"},
	}
	if got := mountsFor(mounts, "web"); len(got) != 1 || got[0].claim != "data" {
		t.Errorf("a process gets the mounts naming it and no other's: %+v", got)
	}
	if mountsFor(mounts, "cron") != nil {
		t.Error("a process nothing names gets nothing")
	}
	if !attachesOnce(mountsFor(mounts, "web")) || attachesOnce(mountsFor(mounts, "worker")) {
		t.Error("attachesOnce reads the claim's detected access mode")
	}
	if capReplicas(3, mountsFor(mounts, "web")) != 1 || capReplicas(3, mountsFor(mounts, "worker")) != 3 {
		t.Error("only a volume that attaches once caps the replica count")
	}
	if capReplicas(0, mountsFor(mounts, "web")) != 0 {
		t.Error("a cap lowers a count; it never raises one")
	}
	volumes, volumeMounts := podVolumes(mountsFor(mounts, "worker"))
	if len(volumes) != 1 || volumes[0].PersistentVolumeClaim.ClaimName != "cache-volume" ||
		len(volumeMounts) != 1 || volumeMounts[0].MountPath != "/cache" || volumeMounts[0].Name != volumes[0].Name {
		t.Errorf("the volume and its mount name each other: %+v %+v", volumes, volumeMounts)
	}
	if volumes, volumeMounts := podVolumes(nil); volumes != nil || volumeMounts != nil {
		t.Error("no mounts is nil, so a pod spec without volumes stays without them")
	}
}

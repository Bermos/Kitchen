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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/volumeinit"
)

// A volume a workload cannot start on, through a whole environment (#348).
//
// The two claims worth an envtest are the ones the unit tests cannot make: a
// declared init reaches the running workload's pod with the release's own
// seed content behind it, and a step that fails leaves the environment saying
// which step and why — on the condition path an unavailable environment
// already has, rather than on a second one nobody reads.

var _ = Describe("A workload that prepares its volume before it starts", func() {
	const (
		projectName = "hass"
		namespace   = "default"
		envName     = "hass-production"
		image       = "ghcr.io/home-assistant/home-assistant@sha256:2222222222222222"
		claimName   = "config"
	)
	appNS := "kitchen-" + projectName
	ctx := context.Background()

	var reconciler *EnvironmentReconciler
	var releases int

	envKey := types.NamespacedName{Name: envName, Namespace: namespace}

	// boundVolume is a volume claim the claim reconciler has already
	// finished with: a PersistentVolumeClaim in the application namespace,
	// mounted by the web process at /config.
	boundVolume := func() {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.ClaimTypeVolume,
				Config: &runtime.RawExtension{Raw: []byte(
					`{"volume": {"process": "web", "size": "1Gi", "mountPath": "/config"}}`)},
			},
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, claim))).To(Succeed())
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Name: claimName, Namespace: namespace}, claim)).To(Succeed())
		claim.Status.Phase = kitchenv1alpha1.ClaimBound
		claim.Status.Volume = &kitchenv1alpha1.ClaimVolumeStatus{
			Process:    kitchenv1alpha1.WebProcessName,
			MountPath:  "/config",
			Source:     kitchenv1alpha1.VolumeProvision,
			AccessMode: string(corev1.ReadWriteOnce),
			ClaimName:  projectName + "-" + claimName,
		}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, claim)).To(Succeed())
	}

	release := func(init []kitchenv1alpha1.VolumeInit, files []kitchenv1alpha1.ConfigFile) string {
		releases++
		name := fmt.Sprintf("hass-rel-00000%d", releases)
		ExpectWithOffset(1, k8sClient.Create(ctx, &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "hass-bld-1"},
				Image:      image,
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8123, Init: init},
					Files:   files,
				},
			},
		})).To(Succeed())
		return name
	}

	deploy := func(releaseName string) *kitchenv1alpha1.Environment {
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentProduction,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
			},
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
		for range 2 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}
		ExpectWithOffset(1, k8sClient.Get(ctx, envKey, env)).To(Succeed())
		return env
	}

	conditionOf := func(env *kitchenv1alpha1.Environment, condType string) *metav1.Condition {
		for i := range env.Status.Conditions {
			if env.Status.Conditions[i].Type == condType {
				return &env.Status.Conditions[i]
			}
		}
		return nil
	}

	configuration := kitchenv1alpha1.ConfigFile{
		// A file with no path: it is placed in no container, because a
		// mounted config file is read-only and would shadow the copy Home
		// Assistant then rewrites.
		Name:    "configuration",
		Content: "default_config:\n",
	}
	prepares := []kitchenv1alpha1.VolumeInit{{
		Volume:      claimName,
		Directories: []kitchenv1alpha1.VolumeInitDirectory{{Path: "custom_components", Mode: "0750"}},
		Seed:        []kitchenv1alpha1.VolumeInitSeed{{File: "configuration", Path: "configuration.yaml"}},
	}}

	BeforeEach(func() {
		reconciler = &EnvironmentReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), OperatorImage: testOperatorImage,
		}
		releases = 0
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
		})
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Image: &kitchenv1alpha1.ImageSourceSpec{
					Repository: "ghcr.io/home-assistant/home-assistant",
					Tag:        "2026.9.0",
				}},
				Previews: kitchenv1alpha1.PreviewsSpec{Enabled: ptr.To(false), Protected: ptr.To(false)},
			},
		}))).To(Succeed())
		boundVolume()
	})

	AfterEach(func() {
		env := &kitchenv1alpha1.Environment{}
		if err := k8sClient.Get(ctx, envKey, env); err == nil {
			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			for range 2 {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
				Expect(err).NotTo(HaveOccurred())
			}
		}
		pods := &corev1.PodList{}
		Expect(k8sClient.List(ctx, pods, client.InNamespace(appNS))).To(Succeed())
		for i := range pods.Items {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &pods.Items[i]))).To(Succeed())
		}
		claim := &kitchenv1alpha1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{
			Name: claimName, Namespace: namespace,
		}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
		releaseList := &kitchenv1alpha1.ReleaseList{}
		Expect(k8sClient.List(ctx, releaseList, client.InNamespace(namespace))).To(Succeed())
		for i := range releaseList.Items {
			if releaseList.Items[i].Spec.ProjectRef.Name == projectName {
				Expect(k8sClient.Delete(ctx, &releaseList.Items[i])).To(Succeed())
			}
		}
	})

	It("prepares the volume in the workload's own pod, from the release's own seed content", func() {
		deploy(release(prepares, []kitchenv1alpha1.ConfigFile{configuration}))

		web := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, web)).To(Succeed())
		spec := web.Spec.Template.Spec

		Expect(spec.InitContainers).To(HaveLen(1),
			"a workload that prepares a volume runs one init container and no more")
		init := spec.InitContainers[0]
		Expect(init.Name).To(Equal(VolumeInitContainerName))
		Expect(init.Command).To(Equal([]string{"/volume-init"}),
			"nothing a project declares reaches the argv")

		By("carrying the steps as data the platform's own program reads")
		var plan volumeinit.Plan
		var carried bool
		for _, variable := range init.Env {
			if variable.Name == volumeinit.PlanVariable {
				carried = true
				Expect(json.Unmarshal([]byte(variable.Value), &plan)).To(Succeed())
			}
		}
		Expect(carried).To(BeTrue())
		Expect(plan.Volumes).To(HaveLen(1))
		Expect(plan.Volumes[0].MountPath).To(Equal("/config"))
		Expect(plan.Volumes[0].Directories[0].Mode).To(Equal("0750"))

		By("mounting the volume it prepares, writable, at the application's own path")
		var mountsVolume bool
		for _, mount := range init.VolumeMounts {
			if mount.MountPath == "/config" {
				mountsVolume = true
				Expect(mount.ReadOnly).To(BeFalse())
				Expect(mount.Name).To(Equal(claimVolumeName(claimName)))
			}
		}
		Expect(mountsVolume).To(BeTrue())

		By("holding the seed content as the release froze it, so a rollback seeds what that release seeded")
		files := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: configFilesName(envName), Namespace: appNS}, files)).To(Succeed())
		Expect(files.Data).To(HaveKeyWithValue("configuration", "default_config:\n"))

		By("placing a file with no path in no container at all")
		for _, container := range spec.Containers {
			for _, mount := range container.VolumeMounts {
				Expect(mount.MountPath).NotTo(Equal("/config/configuration.yaml"),
					"a mounted config file is read-only and would shadow the copy the application owns")
			}
		}
	})

	It("reports the step that failed on the environment, in the step's own words", func() {
		env := deploy(release(prepares, []kitchenv1alpha1.ConfigFile{configuration}))
		Expect(env.Status.Phase).To(Equal(kitchenv1alpha1.EnvironmentDeploying))

		By("standing in for the kubelet: the step exited, and left its account in the termination log")
		said := `directory "custom_components" on volume "config": mkdir /config/custom_components: permission denied`
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      envName + "-abcdef",
				Namespace: appNS,
				Labels:    webLabels(map[string]string{labelEnvironment: envName}),
			},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: VolumeInitContainerName, Image: testOperatorImage}},
				Containers:     []corev1.Container{{Name: AppContainerName, Image: image}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: VolumeInitContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Message: said, Reason: "Error",
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())

		ready := conditionOf(env, condReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(reasonVolumeInitFailed),
			"a workload that never started because a step failed is not a workload that is merely pending")
		Expect(ready.Message).To(ContainSubstring(said),
			"the step's own words are what a person needs; the pod is not where they should have to find them")
		Expect(ready.Message).To(ContainSubstring(kitchenv1alpha1.WebProcessName),
			"a unit has four workloads, so the sentence says which of them did not start")
		Expect(conditionOf(env, condWorkloadAvailable).Reason).To(Equal(reasonVolumeInitFailed))
	})

	It("refuses a declaration it cannot honour, before any pod exists", func() {
		env := deploy(release([]kitchenv1alpha1.VolumeInit{{
			Volume:      "media",
			Directories: []kitchenv1alpha1.VolumeInitDirectory{{Path: "movies"}},
		}}, nil))

		ready := conditionOf(env, condReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("VolumeInitInvalid"))
		Expect(ready.Message).To(ContainSubstring("media"))

		By("writing no Deployment at all: the release that was serving carries on serving")
		web := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: envName, Namespace: appNS}, web)).NotTo(Succeed())
	})
})

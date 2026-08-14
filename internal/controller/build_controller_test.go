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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

var _ = Describe("Build Controller", func() {
	Context("When reconciling a build", func() {
		const (
			projectName = "bldshop"
			buildName   = "bldshop-bld-8f3a2c1d0abc"
			sha         = "8f3a2c1d0abc456789ab"
			namespace   = "default"
			registryURL = "harbor.example.com/kitchen"
		)

		ctx := context.Background()

		buildKey := types.NamespacedName{Name: buildName, Namespace: namespace}
		appNS := "kitchen-" + projectName
		jobKey := types.NamespacedName{Name: buildName, Namespace: appNS}
		wantTag := registryURL + "/" + projectName + ":" + sha[:12]

		var reconciler *BuildReconciler

		reconcileOnce := func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: buildKey})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		completeJob := func() {
			job := &batchv1.Job{}
			ExpectWithOffset(1, k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.CompletionTime = &now
			job.Status.Succeeded = 1
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}
			ExpectWithOffset(1, k8sClient.Status().Update(ctx, job)).To(Succeed())
		}

		createBuildPod := func(terminationMessage string) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      buildName + "-pod",
					Namespace: appNS,
					Labels:    map[string]string{"job-name": buildName},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "buildkit", Image: BuildkitImage}},
				},
			}
			ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, pod))).To(Succeed())
			pod.Status = corev1.PodStatus{
				Phase: corev1.PodSucceeded,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "buildkit",
					Image: BuildkitImage,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 0,
						Message:  terminationMessage,
					}},
				}},
			}
			ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
		}

		BeforeEach(func() {
			reconciler = &BuildReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			kitchen := &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
				Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com"},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())

			creds := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: namespace},
				Type:       corev1.SecretTypeDockerConfigJson,
				Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, creds))).To(Succeed())

			registry := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "dockerRegistry",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "registry-creds"},
					Config:               &runtime.RawExtension{Raw: []byte(`{"url":"` + registryURL + `"}`)},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, registry))).To(Succeed())

			project := &kitchenv1alpha1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
				Spec: kitchenv1alpha1.ProjectSpec{
					Source: kitchenv1alpha1.GitSourceSpec{
						ConnectionRef:    kitchenv1alpha1.LocalObjectReference{Name: "gh"},
						Repo:             "acme/shop",
						ProductionBranch: "main",
					},
					Registry: kitchenv1alpha1.RegistrySpec{
						ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
					},
					Env: []kitchenv1alpha1.EnvVar{
						{Name: "PUBLIC_API", Value: "https://api.example.com"},
					},
					Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8080, Replicas: ptr.To(int32(2))},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

			build := &kitchenv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: buildName, Namespace: namespace},
				Spec: kitchenv1alpha1.BuildSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					Git:        kitchenv1alpha1.GitRevision{SHA: sha, Branch: "main"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, build))).To(Succeed())
		})

		AfterEach(func() {
			// envtest runs no garbage collector: Jobs must be deleted with
			// background propagation (orphan, the default, leaves a finalizer
			// nobody clears) and Pods with no grace period (no kubelet will
			// ever confirm termination).
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: buildName + "-pod", Namespace: appNS}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: buildName, Namespace: appNS}}
			Expect(client.IgnoreNotFound(
				k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)))).To(Succeed())
			for _, obj := range []client.Object{
				&kitchenv1alpha1.Build{ObjectMeta: metav1.ObjectMeta{Name: buildName, Namespace: namespace}},
				&kitchenv1alpha1.Build{ObjectMeta: metav1.ObjectMeta{Name: "other-build", Namespace: namespace}},
				&kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: releaseName(projectName, sha), Namespace: namespace}},
				&kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: projectName + "-production", Namespace: namespace}},
				&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
				&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: namespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: namespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kitchen-registry-registry", Namespace: appNS}},
				&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
			} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		It("creates a buildkit job and marks the build running", func() {
			reconcileOnce()

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Image).To(Equal(BuildkitImage))
			joined := strings.Join(container.Args, " ")
			Expect(joined).To(ContainSubstring("context=https://github.com/acme/shop.git#" + sha))
			Expect(joined).To(ContainSubstring("name=" + wantTag + ",push=true"))
			// Plain progress keeps the build log readable once the collector
			// has shipped it into ClickHouse.
			Expect(joined).To(ContainSubstring("--progress plain"))
			Expect(*job.Spec.BackoffLimit).To(Equal(int32(0)))
			// The finished pod has to outlive the last log flush.
			Expect(job.Spec.TTLSecondsAfterFinished).NotTo(BeNil())
			Expect(*job.Spec.TTLSecondsAfterFinished).To(Equal(int32(buildJobTTLSeconds)))
			Expect(job.Labels).To(HaveKeyWithValue(labelBuild, buildName))
			Expect(job.Spec.Template.Labels).To(HaveKeyWithValue(labelProject, projectName))

			synced := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "kitchen-registry-registry", Namespace: appNS}, synced)).To(Succeed())
			Expect(synced.Type).To(Equal(corev1.SecretTypeDockerConfigJson))

			build := &kitchenv1alpha1.Build{}
			Expect(k8sClient.Get(ctx, buildKey, build)).To(Succeed())
			Expect(build.Status.Phase).To(Equal(kitchenv1alpha1.BuildRunning))
			Expect(build.Status.StartedAt).NotTo(BeNil())
		})

		It("records the digest, creates a release and promotes production on success", func() {
			reconcileOnce()
			completeJob()
			createBuildPod(`{"containerimage.digest":"sha256:feedface"}`)
			reconcileOnce()

			build := &kitchenv1alpha1.Build{}
			Expect(k8sClient.Get(ctx, buildKey, build)).To(Succeed())
			Expect(build.Status.Phase).To(Equal(kitchenv1alpha1.BuildSucceeded))
			Expect(build.Status.Image).To(Equal(wantTag + "@sha256:feedface"))
			Expect(build.Status.CompletedAt).NotTo(BeNil())

			release := &kitchenv1alpha1.Release{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: releaseName(projectName, sha), Namespace: namespace}, release)).To(Succeed())
			Expect(release.Spec.Image).To(Equal(wantTag + "@sha256:feedface"))
			Expect(release.Spec.BuildRef.Name).To(Equal(buildName))
			Expect(release.Spec.ConfigSnapshot.Env).To(HaveLen(1), "snapshot should freeze the project env")
			Expect(release.Spec.ConfigSnapshot.Runtime.Port).To(Equal(int32(8080)))

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName + "-production", Namespace: namespace}, env)).To(Succeed())
			Expect(env.Spec.Type).To(Equal(kitchenv1alpha1.EnvironmentProduction))
			Expect(env.Spec.ReleaseRef.Name).To(Equal(release.Name))
		})

		It("records how the replaced release stopped being current", func() {
			// Production already runs an older release when this build lands.
			env := &kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: projectName + "-production", Namespace: namespace},
				Spec: kitchenv1alpha1.EnvironmentSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					Type:       kitchenv1alpha1.EnvironmentProduction,
					ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: projectName + "-rel-previous"},
				},
			}
			Expect(k8sClient.Create(ctx, env)).To(Succeed())

			reconcileOnce()
			completeJob()
			createBuildPod(`{"containerimage.digest":"sha256:feedface"}`)
			reconcileOnce()

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName + "-production", Namespace: namespace}, env)).To(Succeed())
			Expect(env.Spec.ReleaseRef.Name).To(Equal(releaseName(projectName, sha)))
			Expect(env.Status.History).To(HaveLen(1))
			entry := env.Status.History[0]
			Expect(entry.Release).To(Equal(projectName + "-rel-previous"))
			Expect(entry.Reason).To(Equal(kitchenv1alpha1.ReleaseMovePromoted))
			Expect(entry.By).To(Equal(buildName), "the promoting build is the mover")
			Expect(entry.To.Time).NotTo(BeZero())
		})

		It("creates a preview environment for pull request builds", func() {
			build := &kitchenv1alpha1.Build{}
			Expect(k8sClient.Get(ctx, buildKey, build)).To(Succeed())
			Expect(k8sClient.Delete(ctx, build)).To(Succeed())
			build = &kitchenv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: buildName, Namespace: namespace},
				Spec: kitchenv1alpha1.BuildSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					Git: kitchenv1alpha1.GitRevision{
						SHA:         sha,
						Branch:      "feat/checkout",
						PullRequest: ptr.To(int32(42)),
					},
				},
			}
			Expect(k8sClient.Create(ctx, build)).To(Succeed())
			DeferCleanup(func() {
				env := &kitchenv1alpha1.Environment{
					ObjectMeta: metav1.ObjectMeta{Name: projectName + "-pr-42", Namespace: namespace},
				}
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
			})

			reconcileOnce()
			completeJob()
			createBuildPod(`{"containerimage.digest":"sha256:feedface"}`)
			reconcileOnce()

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName + "-pr-42", Namespace: namespace}, env)).To(Succeed())
			Expect(env.Spec.Type).To(Equal(kitchenv1alpha1.EnvironmentPreview))
			Expect(env.Spec.Preview).NotTo(BeNil())
			Expect(env.Spec.Preview.PullRequest).To(Equal(int32(42)))
			Expect(env.Spec.Preview.Branch).To(Equal("feat/checkout"))
			Expect(env.Spec.ReleaseRef.Name).To(Equal(releaseName(projectName, sha)))

			err := k8sClient.Get(ctx, types.NamespacedName{Name: projectName + "-production", Namespace: namespace}, &kitchenv1alpha1.Environment{})
			Expect(err).To(HaveOccurred(), "a PR build must not touch production")
		})

		It("marks the build failed when the job fails", func() {
			reconcileOnce()

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, jobKey, job)).To(Succeed())
			now := metav1.Now()
			job.Status.StartTime = &now
			job.Status.Failed = 1
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "buildkit exited with code 1"},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			reconcileOnce()

			build := &kitchenv1alpha1.Build{}
			Expect(k8sClient.Get(ctx, buildKey, build)).To(Succeed())
			Expect(build.Status.Phase).To(Equal(kitchenv1alpha1.BuildFailed))
			cond := build.Status.Conditions[0]
			Expect(cond.Reason).To(Equal("BuildFailed"))
			Expect(cond.Message).To(ContainSubstring("exited with code 1"))

			err := k8sClient.Get(ctx, types.NamespacedName{Name: releaseName(projectName, sha), Namespace: namespace}, &kitchenv1alpha1.Release{})
			Expect(err).To(HaveOccurred(), "no release should exist for a failed build")
		})

		It("queues the build while the concurrency limit is reached", func() {
			// The default limit is 2; put two other builds into Running.
			for _, name := range []string{"other-build", "other-build-2"} {
				other := &kitchenv1alpha1.Build{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
					Spec: kitchenv1alpha1.BuildSpec{
						ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
						Git:        kitchenv1alpha1.GitRevision{SHA: "aaaabbbbcccc", Branch: "feat/x"},
					},
				}
				Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, other))).To(Succeed())
				other.Status.Phase = kitchenv1alpha1.BuildRunning
				Expect(k8sClient.Status().Update(ctx, other)).To(Succeed())
				DeferCleanup(func(name string) func() {
					return func() {
						obj := &kitchenv1alpha1.Build{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
						Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
					}
				}(name))
			}

			reconcileOnce()

			build := &kitchenv1alpha1.Build{}
			Expect(k8sClient.Get(ctx, buildKey, build)).To(Succeed())
			Expect(build.Status.Phase).To(Equal(kitchenv1alpha1.BuildQueued))

			err := k8sClient.Get(ctx, jobKey, &batchv1.Job{})
			Expect(err).To(HaveOccurred(), "no job should be created while queued")
		})
	})
})

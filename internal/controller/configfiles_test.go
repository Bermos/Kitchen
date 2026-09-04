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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Software the platform did not build is configured by a file (#311).
//
// What these cases hold up is the four claims the issue is written against: a
// declared file reaches the workloads it names as a read-only file at its
// path, its content is the *release's* so a rollback restores it, a change to
// it rolls the workloads that read it rather than waiting for a pod to happen
// to restart, and a secret file's content never appears in a Release.

var _ = Describe("A project's configuration files", func() {
	const (
		projectName = "confshop"
		namespace   = "default"
		image       = "registry.example.com/kitchen/confshop@sha256:1111111111111111"
		prodName    = "confshop-production"

		firstYAML  = "logger: info\n"
		secondYAML = "logger: debug\n"
	)
	appNS := "kitchen-" + projectName

	ctx := context.Background()

	var reconciler *EnvironmentReconciler
	var releases int

	worker := kitchenv1alpha1.ProcessSpec{
		Name: "worker", Type: kitchenv1alpha1.ProcessWorker, Command: []string{"./worker"},
	}
	schedule := kitchenv1alpha1.ProcessSpec{
		Name: "nightly", Type: kitchenv1alpha1.ProcessCron, Schedule: "0 2 * * *", Command: []string{"./nightly"},
	}

	// release writes a new Release carrying one set of files and answers its
	// name. A Release spec is immutable, so a case that wants other content
	// makes another release — which is also what a rollback target is.
	release := func(files []kitchenv1alpha1.ConfigFile, processes ...kitchenv1alpha1.ProcessSpec) string {
		releases++
		name := fmt.Sprintf("confshop-rel-00000%d", releases)
		ExpectWithOffset(1, k8sClient.Create(ctx, &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "confshop-bld-1"},
				Image:      image,
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime:   kitchenv1alpha1.RuntimeSpec{Port: 3000},
					Processes: processes,
					Files:     files,
				},
			},
		})).To(Succeed())
		return name
	}

	environment := func(releaseName string) *kitchenv1alpha1.Environment {
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: prodName, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentProduction,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
			},
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
		key := types.NamespacedName{Name: prodName, Namespace: namespace}
		for range 2 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, env)).To(Succeed())
		return env
	}

	// moveTo is a rollback: the environment is pointed at another release and
	// reconciled again.
	moveTo := func(releaseName string) {
		key := types.NamespacedName{Name: prodName, Namespace: namespace}
		env := &kitchenv1alpha1.Environment{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, env)).To(Succeed())
		env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: releaseName}
		ExpectWithOffset(1, k8sClient.Update(ctx, env)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	get := func(name string, into client.Object) bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appNS}, into)
		if errors.IsNotFound(err) {
			return false
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return true
	}

	// mountOf is where one container puts a named file, and how.
	mountOf := func(spec corev1.PodSpec, path string) *corev1.VolumeMount {
		for i := range spec.Containers {
			if spec.Containers[i].Name != AppContainerName {
				continue
			}
			for j := range spec.Containers[i].VolumeMounts {
				if spec.Containers[i].VolumeMounts[j].MountPath == path {
					return &spec.Containers[i].VolumeMounts[j]
				}
			}
		}
		return nil
	}

	configuration := func(content string) kitchenv1alpha1.ConfigFile {
		return kitchenv1alpha1.ConfigFile{
			Name: "configuration", Path: "/config/configuration.yaml", Content: content,
		}
	}

	BeforeEach(func() {
		reconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
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
					Repository: "ghcr.io/vendor/appliance",
					Tag:        "2026.9.0",
				}},
				Previews: kitchenv1alpha1.PreviewsSpec{Enabled: ptr.To(false), Protected: ptr.To(false)},
			},
		}))).To(Succeed())
	})

	AfterEach(func() {
		env := &kitchenv1alpha1.Environment{}
		key := types.NamespacedName{Name: prodName, Namespace: namespace}
		if err := k8sClient.Get(ctx, key, env); err == nil {
			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			for range 2 {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}
		}
		list := &kitchenv1alpha1.ReleaseList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
		for i := range list.Items {
			if list.Items[i].Spec.ProjectRef.Name == projectName {
				Expect(k8sClient.Delete(ctx, &list.Items[i])).To(Succeed())
			}
		}
		source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: ProjectFilesName, Namespace: appNS,
		}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, source))).To(Succeed())
	})

	It("places a plain file as a read-only file at its path, in every workload that reads it", func() {
		environment(release([]kitchenv1alpha1.ConfigFile{configuration(firstYAML)}, worker, schedule))

		By("holding the content once, for the environment")
		files := &corev1.ConfigMap{}
		Expect(get(configFilesName(prodName), files)).To(BeTrue())
		Expect(files.Data).To(Equal(map[string]string{"configuration": firstYAML}))

		By("mounting it into the web process at its path and nowhere else")
		web := &appsv1.Deployment{}
		Expect(get(prodName, web)).To(BeTrue())
		mount := mountOf(web.Spec.Template.Spec, "/config/configuration.yaml")
		Expect(mount).NotTo(BeNil(), "a file the release declares has to reach the pod")
		Expect(mount.ReadOnly).To(BeTrue(), "the platform places the file; the application reads it")
		Expect(mount.SubPath).To(Equal("configuration"),
			"without a subPath the mount replaces the whole directory, and the image's own files with it")

		By("mounting it into a worker and a scheduled run too: a unit is one application")
		workerDeploy := &appsv1.Deployment{}
		Expect(get(ProcessWorkloadName(prodName, "worker"), workerDeploy)).To(BeTrue())
		Expect(mountOf(workerDeploy.Spec.Template.Spec, "/config/configuration.yaml")).NotTo(BeNil())
		cron := &batchv1.CronJob{}
		Expect(get(ProcessWorkloadName(prodName, "nightly"), cron)).To(BeTrue())
		Expect(mountOf(cron.Spec.JobTemplate.Spec.Template.Spec, "/config/configuration.yaml")).NotTo(BeNil())
	})

	It("gives a file that names its workloads to those and to no others", func() {
		environment(release([]kitchenv1alpha1.ConfigFile{{
			Name: "worker-conf", Path: "/etc/worker.toml", Content: "queue = \"jobs\"\n",
			Workloads: []string{"worker"},
		}}, worker, schedule))

		workerDeploy := &appsv1.Deployment{}
		Expect(get(ProcessWorkloadName(prodName, "worker"), workerDeploy)).To(BeTrue())
		Expect(mountOf(workerDeploy.Spec.Template.Spec, "/etc/worker.toml")).NotTo(BeNil())

		web := &appsv1.Deployment{}
		Expect(get(prodName, web)).To(BeTrue())
		Expect(mountOf(web.Spec.Template.Spec, "/etc/worker.toml")).To(BeNil(),
			"a file that named one workload reaching the rest is configuration nobody asked for")
		Expect(web.Spec.Template.Annotations).NotTo(HaveKey(ConfigFilesRevisionAnnotation),
			"a workload that reads no file has nothing to be rolled by")
	})

	It("rolls the workloads that read a file when the file changes, and back again", func() {
		environment(release([]kitchenv1alpha1.ConfigFile{configuration(firstYAML)}, worker))

		web, workerDeploy := &appsv1.Deployment{}, &appsv1.Deployment{}
		Expect(get(prodName, web)).To(BeTrue())
		Expect(get(ProcessWorkloadName(prodName, "worker"), workerDeploy)).To(BeTrue())
		first := web.Spec.Template.Annotations[ConfigFilesRevisionAnnotation]
		firstWorker := workerDeploy.Spec.Template.Annotations[ConfigFilesRevisionAnnotation]
		Expect(first).NotTo(BeEmpty())
		Expect(firstWorker).To(Equal(first), "one file read by two workloads is one digest")

		By("moving to a release whose file says something else")
		second := release([]kitchenv1alpha1.ConfigFile{configuration(secondYAML)}, worker)
		moveTo(second)

		files := &corev1.ConfigMap{}
		Expect(get(configFilesName(prodName), files)).To(BeTrue())
		Expect(files.Data["configuration"]).To(Equal(secondYAML))
		Expect(get(prodName, web)).To(BeTrue())
		Expect(web.Spec.Template.Annotations[ConfigFilesRevisionAnnotation]).NotTo(Equal(first),
			"the ConfigMap changed under a pod template that did not, so nothing would have restarted")
		Expect(get(ProcessWorkloadName(prodName, "worker"), workerDeploy)).To(BeTrue())
		Expect(workerDeploy.Spec.Template.Annotations[ConfigFilesRevisionAnnotation]).NotTo(Equal(firstWorker))

		By("rolling back: the file the release ran with comes back, and so does the digest")
		moveTo("confshop-rel-000001")
		Expect(get(configFilesName(prodName), files)).To(BeTrue())
		Expect(files.Data["configuration"]).To(Equal(firstYAML),
			"a rollback that restored the image and not the file would restore the wrong thing")
		Expect(get(prodName, web)).To(BeTrue())
		Expect(web.Spec.Template.Annotations[ConfigFilesRevisionAnnotation]).To(Equal(first))
	})

	It("keeps a secret file's content out of the release and rolls on it through the secret digest", func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: ProjectFilesName, Namespace: appNS},
			Data:       map[string][]byte{"app-ini": []byte("[server]\nSECRET_KEY = one\n")},
		}))).To(Succeed())

		environment(release([]kitchenv1alpha1.ConfigFile{{
			Name: "app-ini", Path: "/data/conf/app.ini", Secret: true,
		}}))

		By("mounting it from the project's own secret rather than from the release")
		web := &appsv1.Deployment{}
		Expect(get(prodName, web)).To(BeTrue())
		mount := mountOf(web.Spec.Template.Spec, "/data/conf/app.ini")
		Expect(mount).NotTo(BeNil())
		Expect(mount.ReadOnly).To(BeTrue())
		var mounted string
		for _, volume := range web.Spec.Template.Spec.Volumes {
			if volume.Name == mount.Name && volume.Secret != nil {
				mounted = volume.Secret.SecretName
			}
		}
		Expect(mounted).To(Equal(ProjectFilesName))

		By("writing no ConfigMap at all: there is no plain file to place")
		files := &corev1.ConfigMap{}
		Expect(get(configFilesName(prodName), files)).To(BeFalse())
		Expect(web.Spec.Template.Annotations).NotTo(HaveKey(ConfigFilesRevisionAnnotation),
			"a secret file is digested with the other secrets, not twice")

		By("digesting it with the rest of what the pod reads, so replacing it restarts the pod")
		before := web.Spec.Template.Annotations[SecretsRevisionAnnotation]
		Expect(before).NotTo(BeEmpty())

		secret := &corev1.Secret{}
		Expect(get(ProjectFilesName, secret)).To(BeTrue())
		secret.Data["app-ini"] = []byte("[server]\nSECRET_KEY = two\n")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		key := types.NamespacedName{Name: prodName, Namespace: namespace}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(get(prodName, web)).To(BeTrue())
		Expect(web.Spec.Template.Annotations[SecretsRevisionAnnotation]).NotTo(Equal(before),
			"a replaced credential that never reaches the running pod is the defect #288 exists for")
	})

	It("takes the file away when the release it moves to declares none", func() {
		environment(release([]kitchenv1alpha1.ConfigFile{configuration(firstYAML)}))
		files := &corev1.ConfigMap{}
		Expect(get(configFilesName(prodName), files)).To(BeTrue())

		moveTo(release(nil))

		Expect(get(configFilesName(prodName), files)).To(BeFalse(),
			"an object holding a file nothing mounts is residue the next reader has to reason about")
		web := &appsv1.Deployment{}
		Expect(get(prodName, web)).To(BeTrue())
		Expect(mountOf(web.Spec.Template.Spec, "/config/configuration.yaml")).To(BeNil())
		Expect(web.Spec.Template.Annotations).NotTo(HaveKey(ConfigFilesRevisionAnnotation))
	})
})

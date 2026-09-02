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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The project's own secrets, from the operator's side.
//
// The API writes one Secret in the platform namespace; a container can only
// read one in its own. So the whole of what these specs are about is that the
// copy exists where the application is, follows the source, and does not
// outlive the project.
var _ = Describe("Project secrets", func() {
	const namespace = "default"

	ctx := context.Background()

	// Every spec gets a project of its own, and so an application namespace
	// of its own. Deleting a project deletes that namespace, and envtest runs
	// no namespace controller — so a namespace one spec tore down would stay
	// Terminating and refuse every write the next spec made into it.
	specs := 0

	var (
		reconciler  *ProjectReconciler
		projectName string
		projectKey  types.NamespacedName
		sourceKey   types.NamespacedName
		copiedKey   types.NamespacedName
		appNS       string
	)

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: projectKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	// write is what the REST API does: the source Secret, in the platform
	// namespace, owned by its Project.
	write := func(data map[string][]byte) {
		project := &kitchenv1alpha1.Project{}
		ExpectWithOffset(1, k8sClient.Get(ctx, projectKey, project)).To(Succeed())

		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      sourceKey.Name,
			Namespace: sourceKey.Namespace,
			Labels:    map[string]string{labelProject: projectName, labelManagedByKey: labelManagedByValue},
		}}
		existing := &corev1.Secret{}
		if err := k8sClient.Get(ctx, sourceKey, existing); err == nil {
			existing.Data = data
			ExpectWithOffset(1, k8sClient.Update(ctx, existing)).To(Succeed())
			return
		}
		secret.Data = data
		ExpectWithOffset(1, k8sClient.Create(ctx, secret)).To(Succeed())
	}

	BeforeEach(func() {
		specs++
		projectName = fmt.Sprintf("pantry-%d", specs)
		projectKey = types.NamespacedName{Name: projectName, Namespace: namespace}
		sourceKey = types.NamespacedName{Name: ProjectSecretsSourceName(projectName), Namespace: namespace}
		appNS = appNamespace(projectName)
		copiedKey = types.NamespacedName{Name: ProjectSecretsName, Namespace: appNS}

		reconciler = &ProjectReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
		}
		ensureSingleton(ctx, kitchen)

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/" + projectName,
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())
	})

	AfterEach(func() {
		project := &kitchenv1alpha1.Project{}
		if err := k8sClient.Get(ctx, projectKey, project); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, project))).To(Succeed())
			Eventually(func() bool {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: projectKey})
				Expect(err).NotTo(HaveOccurred())
				return errors.IsNotFound(k8sClient.Get(ctx, projectKey, &kitchenv1alpha1.Project{}))
			}).Should(BeTrue())
		}
		for _, obj := range []client.Object{
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: sourceKey.Name, Namespace: sourceKey.Namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "kitchen-webhook-" + projectName, Namespace: namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("puts the project's secrets in the application namespace", func() {
		write(map[string][]byte{"SMTP_PASSWORD": []byte("first")})

		reconcileOnce()

		copied := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, copiedKey, copied)).To(Succeed())
		Expect(string(copied.Data["SMTP_PASSWORD"])).To(Equal("first"))
		// Labelled with the project, which is what the Environment reconciler
		// reads to know whose environments to roll when a value changes.
		Expect(copied.Labels).To(HaveKeyWithValue(labelProject, projectName))
		Expect(copied.Labels).To(HaveKeyWithValue(labelManagedByKey, labelManagedByValue))
	})

	It("carries a rotation across on the next reconcile", func() {
		write(map[string][]byte{"SMTP_PASSWORD": []byte("first")})
		reconcileOnce()

		write(map[string][]byte{"SMTP_PASSWORD": []byte("rotated"), "API_KEY": []byte("new")})
		reconcileOnce()

		copied := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, copiedKey, copied)).To(Succeed())
		Expect(string(copied.Data["SMTP_PASSWORD"])).To(Equal("rotated"))
		Expect(string(copied.Data["API_KEY"])).To(Equal("new"))
	})

	// The case CLAUDE.md keeps warning about: a namespace is created once and
	// found by every reconcile afterwards, so anything written only at
	// creation never reaches an installation that already has projects. The
	// mirror runs every time, which is also what recovers a namespace
	// somebody emptied.
	It("puts them back when the copy is deleted", func() {
		write(map[string][]byte{"SMTP_PASSWORD": []byte("first")})
		reconcileOnce()

		copied := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, copiedKey, copied)).To(Succeed())
		Expect(k8sClient.Delete(ctx, copied)).To(Succeed())

		reconcileOnce()

		recovered := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, copiedKey, recovered)).To(Succeed())
		Expect(string(recovered.Data["SMTP_PASSWORD"])).To(Equal("first"))
	})

	// Deleting the last secret removes the source, and the copy goes with it
	// — an empty Secret left behind would let a variable referencing a
	// deleted secret read as an empty value rather than as a container that
	// cannot start.
	It("removes the copy when the last secret is deleted", func() {
		write(map[string][]byte{"SMTP_PASSWORD": []byte("first")})
		reconcileOnce()
		Expect(k8sClient.Get(ctx, copiedKey, &corev1.Secret{})).To(Succeed())

		source := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, sourceKey, source)).To(Succeed())
		Expect(k8sClient.Delete(ctx, source)).To(Succeed())

		reconcileOnce()

		Expect(errors.IsNotFound(k8sClient.Get(ctx, copiedKey, &corev1.Secret{}))).To(BeTrue())
	})

	It("deletes the project's secrets with the project", func() {
		write(map[string][]byte{"SMTP_PASSWORD": []byte("first")})
		reconcileOnce()

		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
		Expect(k8sClient.Delete(ctx, project)).To(Succeed())
		Eventually(func() bool {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: projectKey})
			Expect(err).NotTo(HaveOccurred())
			return errors.IsNotFound(k8sClient.Get(ctx, projectKey, &kitchenv1alpha1.Project{}))
		}).Should(BeTrue(), "the project should be gone after finalization")

		// Nothing owner-references across namespaces, and the finalizer is
		// the garbage collector: the source is deleted by name rather than
		// left to a collector that may never run.
		Expect(errors.IsNotFound(k8sClient.Get(ctx, sourceKey, &corev1.Secret{}))).To(BeTrue())
	})
})

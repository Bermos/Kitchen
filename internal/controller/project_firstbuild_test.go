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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// resolvingGitProvider is a provider that can also answer what a branch points
// at, which is the capability the first build is built on.
type resolvingGitProvider struct {
	fakeGitProvider
	revision gitprovider.Revision
	err      error
	asked    int
}

func (f *resolvingGitProvider) HeadRevision(_ context.Context, _, ref string) (gitprovider.Revision, error) {
	f.asked++
	if f.err != nil {
		return gitprovider.Revision{}, f.err
	}
	revision := f.revision
	revision.Branch = ref
	return revision, nil
}

var _ = Describe("Project first build", func() {
	const (
		projectName = "seeded"
		namespace   = "default"
		headSHA     = "0123456789abcdef0123456789abcdef01234567"
	)

	ctx := context.Background()
	projectKey := types.NamespacedName{Name: projectName, Namespace: namespace}

	var (
		reconciler *ProjectReconciler
		provider   *resolvingGitProvider
	)

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: projectKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	buildsOfProject := func() []kitchenv1alpha1.Build {
		list := &kitchenv1alpha1.BuildList{}
		ExpectWithOffset(1, k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
		var mine []kitchenv1alpha1.Build
		for _, build := range list.Items {
			if build.Spec.ProjectRef.Name == projectName {
				mine = append(mine, build)
			}
		}
		return mine
	}

	BeforeEach(func() {
		provider = &resolvingGitProvider{revision: gitprovider.Revision{
			SHA:     headSHA,
			Message: "add the checkout",
			Author:  "ada",
		}}
		reconciler = &ProjectReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			GitProviders: func(_ *kitchenv1alpha1.Connection, _ string) (gitprovider.Provider, error) {
				return provider, nil
			},
		}

		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())

		creds := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "seed-creds", Namespace: namespace},
			StringData: map[string]string{"token": "gh-token"},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, creds))).To(Succeed())

		for _, conn := range []*kitchenv1alpha1.Connection{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "seed-gh", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "github",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "seed-creds"},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "seed-registry", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "dockerRegistry",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "seed-creds"},
				},
			},
		} {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, conn))).To(Succeed())
		}

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef:    kitchenv1alpha1.LocalObjectReference{Name: "seed-gh"},
					Repo:             "acme/shop",
					ProductionBranch: "trunk",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "seed-registry"},
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
				return apierrors.IsNotFound(k8sClient.Get(ctx, projectKey, &kitchenv1alpha1.Project{}))
			}).Should(BeTrue())
		}
		for _, obj := range []client.Object{
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kitchen-webhook-" + projectName, Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "seed-creds", Namespace: namespace}},
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "seed-gh", Namespace: namespace}},
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "seed-registry", Namespace: namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("builds the production branch as it already stands", func() {
		reconcileOnce()

		builds := buildsOfProject()
		Expect(builds).To(HaveLen(1))
		Expect(builds[0].Name).To(Equal(kitchenv1alpha1.BuildNameFor(projectName, headSHA)))
		Expect(builds[0].Spec.Git.SHA).To(Equal(headSHA))
		Expect(builds[0].Spec.Git.Branch).To(Equal("trunk"), "the project's production branch, not the provider's default")
		Expect(builds[0].Spec.Git.Author).To(Equal("ada"))
		Expect(builds[0].Annotations).To(HaveKey(initialBuildAnnotation))

		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
		Expect(project.Status.InitialBuildRef).NotTo(BeNil())
		Expect(project.Status.InitialBuildRef.Name).To(Equal(builds[0].Name))
		Expect(meta.IsStatusConditionTrue(project.Status.Conditions, condInitialBuild)).To(BeTrue())
	})

	It("seeds once, however often the project is reconciled", func() {
		reconcileOnce()
		reconcileOnce()
		reconcileOnce()

		Expect(buildsOfProject()).To(HaveLen(1))
		Expect(provider.asked).To(Equal(1), "a project that has been seeded asks the provider nothing")
	})

	It("leaves a project whose branch has no commit alone", func() {
		provider.err = fmt.Errorf("%w: trunk at acme/shop", gitprovider.ErrFileNotFound)

		reconcileOnce()

		Expect(buildsOfProject()).To(BeEmpty())
		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
		Expect(project.Status.InitialBuildRef).To(BeNil())
		cond := meta.FindStatusCondition(project.Status.Conditions, condInitialBuild)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("NoCommit"))
		Expect(meta.IsStatusConditionTrue(project.Status.Conditions, condReady)).To(BeTrue(),
			"a project with nothing to build yet is still a ready project")
	})

	It("leaves a project that has already built something alone", func() {
		existing := &kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{Name: projectName + "-bld-pushed000000", Namespace: namespace},
			Spec: kitchenv1alpha1.BuildSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Git:        kitchenv1alpha1.GitRevision{SHA: "pushed000000abc", Branch: "trunk"},
			},
		}
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())

		reconcileOnce()

		Expect(buildsOfProject()).To(HaveLen(1))
		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
		Expect(project.Status.InitialBuildRef).To(BeNil())
	})
})

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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// fakeGitProvider records webhook calls instead of talking to a real API.
type fakeGitProvider struct {
	repo      string
	spec      gitprovider.WebhookSpec
	deletedID string
}

func (f *fakeGitProvider) EnsureWebhook(_ context.Context, repo string, spec gitprovider.WebhookSpec) (string, error) {
	f.repo = repo
	f.spec = spec
	return "42", nil
}

func (f *fakeGitProvider) DeleteWebhook(_ context.Context, _ string, id string) error {
	f.deletedID = id
	return nil
}

var _ = Describe("Project Controller", func() {
	Context("When reconciling a project", func() {
		const (
			projectName = "shop"
			namespace   = "default"
		)

		ctx := context.Background()

		projectKey := types.NamespacedName{Name: projectName, Namespace: namespace}

		var (
			reconciler *ProjectReconciler
			fake       *fakeGitProvider
			fakeToken  string
		)

		reconcileOnce := func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: projectKey})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		BeforeEach(func() {
			fake = &fakeGitProvider{}
			fakeToken = ""
			reconciler = &ProjectReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				GitProviders: func(_ *kitchenv1alpha1.Connection, token string) (gitprovider.Provider, error) {
					fakeToken = token
					return fake, nil
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

			ghCreds := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "gh-creds", Namespace: namespace},
				StringData: map[string]string{"token": "gh-token"},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ghCreds))).To(Succeed())

			gh := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "github",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "gh-creds"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, gh))).To(Succeed())

			registry := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "dockerRegistry",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "registry-creds"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, registry))).To(Succeed())

			project := &kitchenv1alpha1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
				Spec: kitchenv1alpha1.ProjectSpec{
					Source: kitchenv1alpha1.GitSourceSpec{
						ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
						Repo:          "acme/shop",
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
				// Run the finalizer so the object actually goes away.
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: projectKey})
				Expect(err).NotTo(HaveOccurred())
			}
			for _, obj := range []client.Object{
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kitchen-webhook-" + projectName, Namespace: namespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gh-creds", Namespace: namespace}},
				&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: namespace}},
				&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: namespace}},
				&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
			} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		It("registers the webhook and reports the project ready", func() {
			reconcileOnce()

			By("checking the webhook registration")
			Expect(fake.repo).To(Equal("acme/shop"))
			Expect(fake.spec.URL).To(Equal("https://kitchen.apps.example.com/webhooks/git/gh"))
			Expect(fake.spec.Events).To(ConsistOf("push", "pull_request"))
			Expect(fakeToken).To(Equal("gh-token"))

			By("checking the webhook signing secret")
			secret := &corev1.Secret{}
			secretKey := types.NamespacedName{Name: "kitchen-webhook-" + projectName, Namespace: namespace}
			Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
			Expect(string(secret.Data["secret"])).To(Equal(fake.spec.Secret))
			Expect(fake.spec.Secret).NotTo(BeEmpty())

			By("checking status")
			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			Expect(project.Status.WebhookID).To(Equal("42"))
			Expect(meta.IsStatusConditionTrue(project.Status.Conditions, condReady)).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(project.Status.Conditions, condWebhookRegistered)).To(BeTrue())

			By("checking the app namespace")
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "kitchen-" + projectName}, ns)).To(Succeed())
		})

		It("reports not ready when a connection is missing", func() {
			registry := &kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: namespace}}
			Expect(k8sClient.Delete(ctx, registry)).To(Succeed())

			reconcileOnce()

			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(project.Status.Conditions, condReady)).To(BeFalse())
			Expect(meta.IsStatusConditionFalse(project.Status.Conditions, condRegistryConnected)).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(project.Status.Conditions, condSourceConnected)).To(BeTrue())
		})

		It("rejects a source connection without the gitSource capability", func() {
			conn := &kitchenv1alpha1.Connection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "gh", Namespace: namespace}, conn)).To(Succeed())
			conn.Status.Capabilities = []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityImageStore}
			Expect(k8sClient.Status().Update(ctx, conn)).To(Succeed())

			reconcileOnce()

			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			cond := meta.FindStatusCondition(project.Status.Conditions, condSourceConnected)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("CapabilityMissing"))
		})

		It("deregisters the webhook and removes the finalizer on delete", func() {
			reconcileOnce()

			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			Expect(k8sClient.Delete(ctx, project)).To(Succeed())

			reconcileOnce()

			Expect(fake.deletedID).To(Equal("42"))
			err := k8sClient.Get(ctx, projectKey, &kitchenv1alpha1.Project{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "project should be gone after finalization")
		})
	})
})

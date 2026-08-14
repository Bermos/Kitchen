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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

var _ = Describe("Project secret sync", func() {
	Context("When a project opts into synced secrets", func() {
		const (
			projectName = "secretshop"
			namespace   = "default"
		)

		ctx := context.Background()

		projectKey := types.NamespacedName{Name: projectName, Namespace: namespace}
		appNS := "kitchen-" + projectName

		var reconciler *ProjectReconciler

		reconcileOnce := func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: projectKey})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		getSync := func(name string) (*unstructured.Unstructured, error) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(infisicalSecretGVK())
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appNS}, obj)
			return obj, err
		}

		BeforeEach(func() {
			reconciler = &ProjectReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				GitProviders: func(_ *kitchenv1alpha1.Connection, _ string) (gitprovider.Provider, error) {
					return &fakeGitProvider{}, nil
				},
			}

			kitchen := &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
				Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com"},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())

			for _, secret := range []string{"secretshop-gh-creds", "infisical-machine-identity"} {
				obj := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: secret, Namespace: namespace},
					StringData: map[string]string{"token": "t", "clientId": "id", "clientSecret": "hush"},
				}
				Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, obj))).To(Succeed())
			}

			gh := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "secretshop-gh", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "github",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "secretshop-gh-creds"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, gh))).To(Succeed())

			registry := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "secretshop-registry", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "dockerRegistry",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "registry-creds"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, registry))).To(Succeed())

			store := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "secretshop-infisical", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "infisical",
					CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "infisical-machine-identity"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, store))).To(Succeed())

			project := &kitchenv1alpha1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
				Spec: kitchenv1alpha1.ProjectSpec{
					Source: kitchenv1alpha1.GitSourceSpec{
						ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "secretshop-gh"},
						Repo:          "acme/secretshop",
					},
					Registry: kitchenv1alpha1.RegistrySpec{
						ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "secretshop-registry"},
					},
					Secrets: &kitchenv1alpha1.ProjectSecretsSpec{
						ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "secretshop-infisical"},
						ProjectSlug:   "secretshop",
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
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "secretshop-gh-creds", Namespace: namespace}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "infisical-machine-identity", Namespace: namespace}},
				&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "secretshop-gh", Namespace: namespace}},
				&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "secretshop-registry", Namespace: namespace}},
				&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "secretshop-infisical", Namespace: namespace}},
				&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
			} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		It("materializes one sync CR per environment type", func() {
			reconcileOnce()

			By("checking the production sync")
			prod, err := getSync("kitchen-secrets-production")
			Expect(err).NotTo(HaveOccurred())
			env, _, err := unstructured.NestedString(prod.Object,
				"spec", "authentication", "universalAuth", "secretsScope", "envSlug")
			Expect(err).NotTo(HaveOccurred())
			Expect(env).To(Equal("prod"))
			slug, _, err := unstructured.NestedString(prod.Object,
				"spec", "authentication", "universalAuth", "secretsScope", "projectSlug")
			Expect(err).NotTo(HaveOccurred())
			Expect(slug).To(Equal("secretshop"))
			host, _, err := unstructured.NestedString(prod.Object, "spec", "hostAPI")
			Expect(err).NotTo(HaveOccurred())
			Expect(host).To(Equal("https://app.infisical.com/api"), "no config means Infisical Cloud")

			By("checking the machine identity is referenced where it lives")
			credsName, _, err := unstructured.NestedString(prod.Object,
				"spec", "authentication", "universalAuth", "credentialsRef", "secretName")
			Expect(err).NotTo(HaveOccurred())
			Expect(credsName).To(Equal("infisical-machine-identity"))
			credsNS, _, err := unstructured.NestedString(prod.Object,
				"spec", "authentication", "universalAuth", "credentialsRef", "secretNamespace")
			Expect(err).NotTo(HaveOccurred())
			Expect(credsNS).To(Equal(namespace))

			By("checking the managed secret lands in the app namespace")
			managed, _, err := unstructured.NestedSlice(prod.Object, "spec", "managedKubeSecretReferences")
			Expect(err).NotTo(HaveOccurred())
			Expect(managed).To(HaveLen(1))
			ref, ok := managed[0].(map[string]any)
			Expect(ok).To(BeTrue())
			Expect(ref["secretName"]).To(Equal("kitchen-secrets-production"))
			Expect(ref["secretNamespace"]).To(Equal(appNS))

			By("checking the preview sync scopes to the preview environment")
			preview, err := getSync("kitchen-secrets-preview")
			Expect(err).NotTo(HaveOccurred())
			env, _, err = unstructured.NestedString(preview.Object,
				"spec", "authentication", "universalAuth", "secretsScope", "envSlug")
			Expect(err).NotTo(HaveOccurred())
			Expect(env).To(Equal("staging"))

			By("checking the conditions")
			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(project.Status.Conditions, condSecretStoreConnected)).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(project.Status.Conditions, condSecretsSynced)).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(project.Status.Conditions, condReady)).To(BeTrue())
		})

		It("honours the connection's host and the project's scope overrides", func() {
			store := &kitchenv1alpha1.Connection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "secretshop-infisical", Namespace: namespace}, store)).To(Succeed())
			store.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"host":"https://infisical.example.com/"}`)}
			Expect(k8sClient.Update(ctx, store)).To(Succeed())

			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			project.Spec.Secrets.SecretsPath = "/apps/shop"
			project.Spec.Secrets.ProductionEnv = "production"
			project.Spec.Secrets.PreviewEnv = "dev"
			Expect(k8sClient.Update(ctx, project)).To(Succeed())

			reconcileOnce()

			prod, err := getSync("kitchen-secrets-production")
			Expect(err).NotTo(HaveOccurred())
			host, _, err := unstructured.NestedString(prod.Object, "spec", "hostAPI")
			Expect(err).NotTo(HaveOccurred())
			Expect(host).To(Equal("https://infisical.example.com/api"))
			env, _, err := unstructured.NestedString(prod.Object,
				"spec", "authentication", "universalAuth", "secretsScope", "envSlug")
			Expect(err).NotTo(HaveOccurred())
			Expect(env).To(Equal("production"))
			path, _, err := unstructured.NestedString(prod.Object,
				"spec", "authentication", "universalAuth", "secretsScope", "secretsPath")
			Expect(err).NotTo(HaveOccurred())
			Expect(path).To(Equal("/apps/shop"))

			preview, err := getSync("kitchen-secrets-preview")
			Expect(err).NotTo(HaveOccurred())
			env, _, err = unstructured.NestedString(preview.Object,
				"spec", "authentication", "universalAuth", "secretsScope", "envSlug")
			Expect(err).NotTo(HaveOccurred())
			Expect(env).To(Equal("dev"))
		})

		It("drops the preview sync when previews are disabled", func() {
			reconcileOnce()
			_, err := getSync("kitchen-secrets-preview")
			Expect(err).NotTo(HaveOccurred())

			// The typed client cannot express enabled=false — `omitempty` drops
			// the zero value and the API server re-defaults it to true — so
			// send it explicitly, the way kubectl would.
			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"previews":{"enabled":false}}}`))
			Expect(k8sClient.Patch(ctx, project, patch)).To(Succeed())

			reconcileOnce()

			_, err = getSync("kitchen-secrets-preview")
			Expect(errors.IsNotFound(err)).To(BeTrue(), "the preview sync should be gone")
			_, err = getSync("kitchen-secrets-production")
			Expect(err).NotTo(HaveOccurred(), "the production sync stays")
		})

		It("tears the sync down when the project opts out again", func() {
			reconcileOnce()

			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			project.Spec.Secrets = nil
			Expect(k8sClient.Update(ctx, project)).To(Succeed())

			reconcileOnce()

			for _, name := range []string{"kitchen-secrets-production", "kitchen-secrets-preview"} {
				_, err := getSync(name)
				Expect(errors.IsNotFound(err)).To(BeTrue(), name+" should be gone")
			}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			Expect(meta.FindStatusCondition(project.Status.Conditions, condSecretStoreConnected)).To(BeNil())
			Expect(meta.FindStatusCondition(project.Status.Conditions, condSecretsSynced)).To(BeNil())
		})

		It("refuses a connection that does not offer secretStore", func() {
			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			project.Spec.Secrets.ConnectionRef.Name = "secretshop-gh"
			Expect(k8sClient.Update(ctx, project)).To(Succeed())

			gh := &kitchenv1alpha1.Connection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "secretshop-gh", Namespace: namespace}, gh)).To(Succeed())
			gh.Status.Capabilities = []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityGitSource}
			Expect(k8sClient.Status().Update(ctx, gh)).To(Succeed())

			reconcileOnce()

			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			cond := meta.FindStatusCondition(project.Status.Conditions, condSecretStoreConnected)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("CapabilityMissing"))
			Expect(meta.IsStatusConditionTrue(project.Status.Conditions, condReady)).To(BeFalse())
		})
	})
})

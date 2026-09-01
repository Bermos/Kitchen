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
					CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "gh-creds"},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, gh))).To(Succeed())

			registry := &kitchenv1alpha1.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: namespace},
				Spec: kitchenv1alpha1.ConnectionSpec{
					Provider:             "dockerRegistry",
					CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "registry-creds"},
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
			// Set rather than inherited: the cluster default decides whether
			// the Dockerfile builder is admitted, and this singleton asks for
			// nothing, so it gets the CRD's default.
			Expect(ns.Labels).To(HaveKeyWithValue("pod-security.kubernetes.io/enforce", "privileged"))
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

		It("garbage-collects everything derived from the project on delete", func() {
			reconcileOnce()

			// The records the platform derived from the project: they
			// reference it by name, so no owner reference cleans them up.
			build := &kitchenv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: projectName + "-bld-abc123def456", Namespace: namespace},
				Spec: kitchenv1alpha1.BuildSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					Git:        kitchenv1alpha1.GitRevision{SHA: "abc123def456789", Branch: "main"},
				},
			}
			release := &kitchenv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: projectName + "-rel-abc123def456", Namespace: namespace},
				Spec: kitchenv1alpha1.ReleaseSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: build.Name},
					Image:      "registry.example.com/shop@sha256:1111",
				},
			}
			environment := &kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: projectName + "-production", Namespace: namespace},
				Spec: kitchenv1alpha1.EnvironmentSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					Type:       kitchenv1alpha1.EnvironmentProduction,
					ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: release.Name},
				},
			}
			domain := &kitchenv1alpha1.Domain{
				ObjectMeta: metav1.ObjectMeta{Name: projectName + "-com", Namespace: namespace},
				Spec: kitchenv1alpha1.DomainSpec{
					Hostname:       "shop.example.com",
					EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: environment.Name},
				},
			}
			// One claim of every registered type: a project's teardown must
			// take every kind of dependency with it, not only the first one
			// the platform learned to provision.
			claims := make([]client.Object, 0, len(kitchenv1alpha1.ClaimTypes))
			for _, claimType := range kitchenv1alpha1.ClaimTypes {
				claim := &kitchenv1alpha1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: projectName + "-" + strings.ToLower(claimType.Name), Namespace: namespace},
					Spec: kitchenv1alpha1.ResourceClaimSpec{
						ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
						Type:       claimType.Name,
					},
				}
				if claimType.TakesConnection() {
					claim.Spec.ConnectionRef = &kitchenv1alpha1.LocalObjectReference{Name: "neon"}
				}
				claims = append(claims, claim)
			}
			// A stranger's build must survive the neighbor's teardown.
			bystander := &kitchenv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: "blog-bld-abc123def456", Namespace: namespace},
				Spec: kitchenv1alpha1.BuildSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "blog"},
					Git:        kitchenv1alpha1.GitRevision{SHA: "abc123def456789", Branch: "main"},
				},
			}
			for _, obj := range append([]client.Object{build, release, environment, domain, bystander}, claims...) {
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			}
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, bystander))).To(Succeed())
			})

			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			Expect(k8sClient.Delete(ctx, project)).To(Succeed())

			// The finalizer requeues while dependents drain, so it can take
			// more than one pass.
			Eventually(func() bool {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: projectKey})
				Expect(err).NotTo(HaveOccurred())
				return errors.IsNotFound(k8sClient.Get(ctx, projectKey, &kitchenv1alpha1.Project{}))
			}).Should(BeTrue(), "project should be gone after finalization")

			for _, gone := range append([]client.Object{build, release, environment, domain}, claims...) {
				key := types.NamespacedName{Name: gone.GetName(), Namespace: namespace}
				err := k8sClient.Get(ctx, key, gone)
				Expect(errors.IsNotFound(err)).To(BeTrue(), gone.GetName()+" should be garbage-collected")
			}

			survivor := &kitchenv1alpha1.Build{}
			key := types.NamespacedName{Name: bystander.Name, Namespace: namespace}
			Expect(k8sClient.Get(ctx, key, survivor)).To(Succeed(), "another project's build must survive")
		})

		It("derives the refs from builds and environments nothing wrote to the project for", func() {
			reconcileOnce()

			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			Expect(project.Status.LatestBuildRef).To(BeNil())
			Expect(project.Status.ProductionEnvironmentRef).To(BeNil())

			// What a webhook leaves behind: a Build and, once it lands, an
			// Environment. Neither is owned by the Project and neither
			// writes to it, so the reconcile below is the only thing that
			// can put them in its status.
			build := &kitchenv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: projectName + "-bld-aaaaaaaaaaaa", Namespace: namespace},
				Spec: kitchenv1alpha1.BuildSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					Git:        kitchenv1alpha1.GitRevision{SHA: "aaaaaaaaaaaa0000", Branch: "main"},
				},
			}
			environment := &kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: projectName + "-production", Namespace: namespace},
				Spec: kitchenv1alpha1.EnvironmentSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					Type:       kitchenv1alpha1.EnvironmentProduction,
					ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: projectName + "-rel-aaaaaaaaaaaa"},
				},
			}
			for _, obj := range []client.Object{build, environment} {
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			}

			reconcileOnce()

			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			Expect(project.Status.LatestBuildRef).NotTo(BeNil())
			Expect(project.Status.LatestBuildRef.Name).To(Equal(build.Name))
			Expect(project.Status.ProductionEnvironmentRef).NotTo(BeNil())
			Expect(project.Status.ProductionEnvironmentRef.Name).To(Equal(environment.Name))

			By("clearing a ref whose object is gone")
			for _, obj := range []client.Object{build, environment} {
				Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
			}

			reconcileOnce()

			Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
			Expect(project.Status.LatestBuildRef).To(BeNil())
			Expect(project.Status.ProductionEnvironmentRef).To(BeNil())
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

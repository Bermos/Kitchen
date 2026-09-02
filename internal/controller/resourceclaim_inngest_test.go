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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/inngest"
	"github.com/Bermos/Kitchen/internal/provider/inngest/inngesttest"
)

var _ = Describe("An inngest claim", func() {
	const (
		projectName     = "cljobs"
		claimName       = "cljobs-inngest"
		connectionName  = "clinngest"
		credentialsName = "clinngest-credentials"
		namespace       = "default"
		previewEnvName  = "cljobs-pr-5"
	)

	ctx := context.Background()
	claimKey := types.NamespacedName{Name: claimName, Namespace: namespace}
	envKey := types.NamespacedName{Name: previewEnvName, Namespace: namespace}
	appNS := "kitchen-" + projectName

	var (
		fake       *inngesttest.CloudServer
		reconciler *ResourceClaimReconciler
	)

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	getClaim := func() *kitchenv1alpha1.ResourceClaim {
		claim := &kitchenv1alpha1.ResourceClaim{}
		ExpectWithOffset(1, k8sClient.Get(ctx, claimKey, claim)).To(Succeed())
		return claim
	}

	createClaim := func(config string) {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: projectName},
				ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: connectionName},
				Type:          kitchenv1alpha1.ClaimTypeInngest,
			},
		}
		if config != "" {
			claim.Spec.Config = &runtime.RawExtension{Raw: []byte(config)}
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	}

	createPreview := func() {
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: previewEnvName, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentPreview,
				Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 5, Branch: "feature/jobs"},
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: projectName + "-rel-1"},
			},
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
	}

	BeforeEach(func() {
		fake = inngesttest.NewCloudServer()
		reconciler = &ResourceClaimReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			// The real Cloud client; only its API URL points at the fake.
			Inngest: func(opts inngest.Options) (inngest.Provisioner, error) {
				return &inngest.Cloud{APIURL: fake.URL(), Token: opts.Token}, nil
			},
		}

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/cljobs",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
				Previews: kitchenv1alpha1.PreviewsSpec{Enabled: ptr.To(true), Protected: ptr.To(false)},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		credentials := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: credentialsName, Namespace: namespace},
			StringData: map[string]string{"token": "sk-inn-api-test"},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, credentials))).To(Succeed())

		connection := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             inngest.ProviderCloud,
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: credentialsName},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, connection))).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName, Namespace: namespace}, connection)).To(Succeed())
		connection.Status.Capabilities = []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityBackgroundJobs}
		Expect(k8sClient.Status().Update(ctx, connection)).To(Succeed())
	})

	AfterEach(func() {
		claim := &kitchenv1alpha1.ResourceClaim{}
		if err := k8sClient.Get(ctx, claimKey, claim); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey})
			Expect(err).NotTo(HaveOccurred())
		}
		fake.Close()

		environments := &kitchenv1alpha1.EnvironmentList{}
		Expect(k8sClient.List(ctx, environments, client.InNamespace(namespace))).To(Succeed())
		for i := range environments.Items {
			env := &environments.Items[i]
			if env.Spec.ProjectRef.Name != projectName {
				continue
			}
			if controllerutil.RemoveFinalizer(env, claimBranchFinalizer) ||
				controllerutil.RemoveFinalizer(env, environmentFinalizer) {
				Expect(k8sClient.Update(ctx, env)).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
		}
		for _, obj := range []client.Object{
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: previewEnvName, Namespace: appNS}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: previewEnvName, Namespace: appNS}},
			&kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: projectName + "-rel-1", Namespace: namespace}},
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: credentialsName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	// Provisioning creates nothing at Inngest: the binding is production's
	// keys, spelled as the SDK's variables, and the status carries what the
	// provider declares — a branch per preview, and no scale to zero.
	It("binds production's keys and declares what the worker costs", func() {
		createClaim(`{"inngest":{"app":"shop-worker"}}`)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.InstanceID).To(Equal("shop-worker"))
		Expect(claim.Status.DataProvenance).To(Equal("production"),
			"the production binding selects the production event stream")
		Expect(claim.Status.PreviewMode).To(Equal("branch"))
		Expect(claim.Status.KeepsPodsRunning).To(BeTrue(), "a connect worker holds the pods up")
		Expect(claim.Status.ForcesRecreate).To(BeFalse())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: claim.Status.SecretName}, secret)).To(Succeed())
		Expect(string(secret.Data[inngest.KeyEventKey])).To(Equal(inngesttest.ProductionEventKey))
		Expect(string(secret.Data[inngest.KeySigningKey])).To(Equal(inngesttest.ProductionSigningKey))
		Expect(secret.Data).To(HaveKeyWithValue(inngest.KeyEnv, []byte("")), "production selects itself")
		Expect(secret.Data).To(HaveKeyWithValue(inngest.KeyBaseURL, []byte("")), "Cloud sets no base URL")
		Expect(fake.EnvNamed("cljobs-pr-5")).To(BeNil(), "no preview, no branch environment")

		// Reported, not waited for: the app is the application's to bring.
		connected := meta.FindStatusCondition(claim.Status.Conditions, condAppConnected)
		Expect(connected).NotTo(BeNil())
		Expect(connected.Status).To(Equal(metav1.ConditionFalse))
		Expect(connected.Reason).To(Equal("NotConnected"))
		Expect(connected.Message).To(ContainSubstring("shop-worker"))
		workers := meta.FindStatusCondition(claim.Status.Conditions, condConnectWorkers)
		Expect(workers).NotTo(BeNil())
		Expect(workers.Message).To(ContainSubstring("1 environment(s)"))
		Expect(workers.Message).To(ContainSubstring("3 on Inngest's free plan"))

		// A worker connects; the next reconcile says so.
		fake.RegisterApp("production", "shop-worker", "CONNECT", 4)
		reconcileOnce()
		connected = meta.FindStatusCondition(getClaim().Status.Conditions, condAppConnected)
		Expect(connected.Status).To(Equal(metav1.ConditionTrue))
		Expect(connected.Message).To(ContainSubstring("4 function(s)"))
	})

	// The refusal is the feature: an environment with no event key cannot be
	// bound, the API cannot mint one, and the claim says where to.
	It("fails the claim, saying where to create the key, when the environment has none", func() {
		fake.RemoveEventKeys("production")
		createClaim("")
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		ready := meta.FindStatusCondition(claim.Status.Conditions, condReady)
		Expect(ready.Reason).To(Equal("RequirementsUnsatisfiable"))
		Expect(ready.Message).To(ContainSubstring("dashboard"))
		Expect(ready.Message).NotTo(ContainSubstring("sk-inn-api-test"), "the credential never reaches a status")
	})

	It("gives each preview a branch environment of its own, and archives it with the preview", func() {
		createPreview()
		createClaim("")
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Branches).To(HaveLen(1))
		entry := claim.Status.Branches[0]
		Expect(entry.Environment).To(Equal(previewEnvName))
		Expect(entry.Provenance).To(Equal("synthetic"), "an empty event stream of the preview's own")
		branchEnv := fake.EnvNamed(previewEnvName)
		Expect(branchEnv).NotTo(BeNil(), "no branch environment was created at Inngest")
		Expect(entry.ID).To(Equal(branchEnv.ID))

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: entry.SecretName}, secret)).To(Succeed())
		Expect(string(secret.Data[inngest.KeyEventKey])).To(Equal(inngesttest.BranchEventKey))
		Expect(string(secret.Data[inngest.KeySigningKey])).To(Equal(inngesttest.BranchSigningKey))
		Expect(string(secret.Data[inngest.KeyEnv])).To(Equal(previewEnvName),
			"the shared branch keys select nothing; INNGEST_ENV is what routes the preview")
		Expect(meta.FindStatusCondition(claim.Status.Conditions, condConnectWorkers).Message).
			To(ContainSubstring("2 environment(s)"))

		// The preview closes: the environment is archived, never deleted.
		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		Expect(k8sClient.Delete(ctx, env)).To(Succeed())
		reconcileOnce()
		Expect(getClaim().Status.Branches).To(BeEmpty())
		branchEnv = fake.EnvNamed(previewEnvName)
		Expect(branchEnv).NotTo(BeNil(), "archiving deletes nothing at Inngest")
		Expect(branchEnv.Archived).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: entry.SecretName}, secret)).NotTo(Succeed())
	})

	// The whole reason connect mode costs what it does: a bound inngest claim
	// vetoes idling on the environment that reads it, naming the claim.
	It("keeps an environment reading it on its pods, and names itself as the reason", func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				ScaleToZero: kitchenv1alpha1.ScaleToZeroSpec{
					Enabled: true,
					Interceptor: kitchenv1alpha1.InterceptorSpec{
						Service: "keda-add-ons-http-interceptor-proxy", Namespace: PlatformNamespace, Port: 8080,
					},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())
		release := &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: projectName + "-rel-1", Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: projectName + "-bld-1"},
				Image:      "registry.example.com/cljobs@sha256:1234",
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Env: []kitchenv1alpha1.EnvVar{{
						Name:              inngest.KeyEventKey,
						FromResourceClaim: &kitchenv1alpha1.ResourceClaimKeySelector{Name: claimName, Key: inngest.KeyEventKey},
					}},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())
		createPreview()
		createClaim("")
		reconcileOnce()

		environmentReconciler := &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := environmentReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
		Expect(err).NotTo(HaveOccurred())

		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		idling := meta.FindStatusCondition(env.Status.Conditions, condScaleToZero)
		Expect(idling).NotTo(BeNil(), "the environment says why it keeps its pods")
		Expect(idling.Status).To(Equal(metav1.ConditionFalse))
		Expect(idling.Reason).To(Equal("ClaimKeepsPodsRunning"))
		Expect(idling.Message).To(ContainSubstring(claimName))

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: previewEnvName}, deploy)).To(Succeed())
		podEnv := deploy.Spec.Template.Spec.Containers[0].Env
		Expect(podEnv[len(podEnv)-1].ValueFrom.SecretKeyRef.Name).To(Equal(claimName+"-binding-"+previewEnvName),
			"the preview reads its branch environment's binding")
	})
})

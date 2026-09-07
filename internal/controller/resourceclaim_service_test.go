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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/service"
)

// The edge between two projects (#493), end to end through the reconciler:
// one project offering something, another claiming it, and the four refusals
// in between.
var _ = Describe("ResourceClaim of type service", func() {
	const (
		namespace = "default"
		provider  = "pricing"
		consumer  = "checkout"
		prodEnv   = provider + "-production"
	)

	ctx := context.Background()
	consumerNS := "kitchen-" + consumer

	var reconciler *ResourceClaimReconciler

	claimKey := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: namespace}
	}

	reconcileOnce := func(name string) {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	getClaim := func(name string) *kitchenv1alpha1.ResourceClaim {
		claim := &kitchenv1alpha1.ResourceClaim{}
		ExpectWithOffset(1, k8sClient.Get(ctx, claimKey(name), claim)).To(Succeed())
		return claim
	}

	readyCondition := func(name string) *metav1.Condition {
		return meta.FindStatusCondition(getClaim(name).Status.Conditions, condReady)
	}

	createClaim := func(name, config string) {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: consumer},
				Type:       kitchenv1alpha1.ClaimTypeService,
				Config:     &runtime.RawExtension{Raw: []byte(config)},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	}

	deleteClaim := func(name string) {
		claim := &kitchenv1alpha1.ResourceClaim{}
		if err := k8sClient.Get(ctx, claimKey(name), claim); apierrors.IsNotFound(err) {
			return
		}
		ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
		EventuallyWithOffset(1, func() bool {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey(name)})
			Expect(err).NotTo(HaveOccurred())
			return apierrors.IsNotFound(k8sClient.Get(ctx, claimKey(name), &kitchenv1alpha1.ResourceClaim{}))
		}).Should(BeTrue())
	}

	// offer replaces the providing project's offerings.
	offer := func(offers ...kitchenv1alpha1.ServiceOffering) {
		project := &kitchenv1alpha1.Project{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: provider, Namespace: namespace},
			project)).To(Succeed())
		project.Spec.Offers = offers
		ExpectWithOffset(1, k8sClient.Update(ctx, project)).To(Succeed())
	}

	newProject := func(name string, processes ...kitchenv1alpha1.ProcessSpec) *kitchenv1alpha1.Project {
		return &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/" + name,
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
				Processes: processes,
			},
		}
	}

	// serveEnvironment creates or updates an environment of the providing
	// project declaring who may bind to it. Absent or empty serves nobody,
	// which is why every one of these says so out loud.
	serveEnvironment := func(
		name string,
		class kitchenv1alpha1.EnvironmentType,
		consumers ...kitchenv1alpha1.EnvironmentType,
	) {
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: provider},
				Type:       class,
				Serves:     &kitchenv1alpha1.EnvironmentServes{Consumers: consumers},
			},
		}
		err := k8sClient.Create(ctx, env)
		if apierrors.IsAlreadyExists(err) {
			current := &kitchenv1alpha1.Environment{}
			ExpectWithOffset(1, k8sClient.Get(ctx,
				types.NamespacedName{Name: name, Namespace: namespace}, current)).To(Succeed())
			current.Spec.Type = class
			current.Spec.Serves = &kitchenv1alpha1.EnvironmentServes{Consumers: consumers}
			ExpectWithOffset(1, k8sClient.Update(ctx, current)).To(Succeed())
			return
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	// bindingFor is what one class of the consumer's environments reaches.
	bindingFor := func(
		name string, consumer kitchenv1alpha1.EnvironmentType,
	) kitchenv1alpha1.ClaimServiceBinding {
		claim := getClaim(name)
		ExpectWithOffset(1, claim.Status.Service).NotTo(BeNil())
		for _, binding := range claim.Status.Service.Bindings {
			if binding.Consumer == consumer {
				return binding
			}
		}
		Fail("no binding recorded for a " + string(consumer) + " consumer")
		return kitchenv1alpha1.ClaimServiceBinding{}
	}

	BeforeEach(func() {
		reconciler = &ResourceClaimReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, newProject(provider,
			kitchenv1alpha1.ProcessSpec{
				Name: "api",
				Type: kitchenv1alpha1.ProcessService,
				Port: 8080,
			},
			kitchenv1alpha1.ProcessSpec{
				Name: "mailer",
				Type: kitchenv1alpha1.ProcessWorker,
			},
		)))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, newProject(consumer)))).To(Succeed())

		// The provider's production environment, open to every class of
		// consumer. Every assertion below that is about *addressing* rather
		// than about admission starts from that, so the admission tests are
		// the ones that narrow it (#494).
		serveEnvironment(prodEnv, kitchenv1alpha1.EnvironmentProduction,
			kitchenv1alpha1.EnvironmentTypes()...)
		offer(kitchenv1alpha1.ServiceOffering{
			Name:      "pricing-api",
			VisibleTo: kitchenv1alpha1.OfferingOpen,
		})
	})

	AfterEach(func() {
		claims := &kitchenv1alpha1.ResourceClaimList{}
		Expect(k8sClient.List(ctx, claims, client.InNamespace(namespace))).To(Succeed())
		for i := range claims.Items {
			if claims.Items[i].Spec.Type == kitchenv1alpha1.ClaimTypeService {
				deleteClaim(claims.Items[i].Name)
			}
		}
		// Any environment of the provider a test stood up beside its
		// production one: which environments exist is now what decides where
		// a binding lands, so one left behind would be read by the next test.
		envs := &kitchenv1alpha1.EnvironmentList{}
		Expect(k8sClient.List(ctx, envs, client.InNamespace(namespace))).To(Succeed())
		for i := range envs.Items {
			if envs.Items[i].Spec.ProjectRef.Name == provider && envs.Items[i].Name != prodEnv {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &envs.Items[i]))).To(Succeed())
			}
		}
	})

	It("binds to the offering's environment and writes the address into the consumer's namespace", func() {
		const name = "pricing"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.InstanceID).To(Equal("pricing/pricing-api"))
		// Shared, because a preview of the consumer calls the same
		// environment production calls — and it costs production nothing,
		// since a binding provisions no data of its own.
		Expect(claim.Status.PreviewMode).To(Equal("shared"))
		Expect(claim.Status.DataProvenance).To(Equal("production"))

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: claim.Status.SecretName, Namespace: consumerNS}, secret)).To(Succeed())
		Expect(string(secret.Data[service.BindingKeyHost])).
			To(Equal(prodEnv + ".kitchen-pricing.svc.cluster.local"))
		Expect(string(secret.Data[service.BindingKeyPort])).To(Equal("80"))
		Expect(string(secret.Data[service.BindingKeyURL])).
			To(Equal("http://" + prodEnv + ".kitchen-pricing.svc.cluster.local:80"))
		Expect(string(secret.Data[service.BindingKeyEnvironment])).To(Equal(prodEnv))
		Expect(string(secret.Data[service.BindingKeyOffering])).To(Equal("pricing-api"))

		// And the consumer's workloads read it under the same prefix a
		// sibling process is handed.
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: consumer + "-production", Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: consumer},
				Type:       kitchenv1alpha1.EnvironmentProduction,
			},
		}
		vars, unbound, err := serviceBindingEnv(ctx, k8sClient, env, consumer, consumerNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(unbound).To(BeEmpty())
		names := make([]string, 0, len(vars))
		for _, v := range vars {
			names = append(names, v.Name)
			Expect(v.ValueFrom).NotTo(BeNil())
			Expect(v.ValueFrom.SecretKeyRef.Name).To(Equal(claim.Status.SecretName))
		}
		Expect(names).To(Equal([]string{"KITCHEN_SERVICE_PRICING", "KITCHEN_SERVICE_PRICING_HOST",
			"KITCHEN_SERVICE_PRICING_PORT"}))
	})

	It("addresses a service workload of the provider, and refuses a workload nothing addresses", func() {
		offer(kitchenv1alpha1.ServiceOffering{
			Name:      "pricing-api",
			Process:   "api",
			VisibleTo: kitchenv1alpha1.OfferingOpen,
		})
		const name = "pricing-api"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: claim.Status.SecretName, Namespace: consumerNS}, secret)).To(Succeed())
		Expect(string(secret.Data[service.BindingKeyHost])).
			To(Equal(prodEnv + "-api.kitchen-pricing.svc.cluster.local"))
		Expect(string(secret.Data[service.BindingKeyPort])).To(Equal("8080"))

		// A worker is not addressed by anything, so there is no address to
		// hand over and the claim says so rather than binding one.
		offer(kitchenv1alpha1.ServiceOffering{
			Name:      "pricing-api",
			Process:   "mailer",
			VisibleTo: kitchenv1alpha1.OfferingOpen,
		})
		reconcileOnce(name)
		Expect(getClaim(name).Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		Expect(readyCondition(name).Reason).To(Equal("OfferingNotAddressed"))
		Expect(readyCondition(name).Message).To(ContainSubstring("mailer"))
	})

	It("resolves a workload the release declares and the project does not", func() {
		// A project whose workloads come from its repository declares none of
		// them in spec.processes: kitchen.json replaces the list at every
		// build, so the release is the only place the workload exists.
		bare := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: provider, Namespace: namespace},
			bare)).To(Succeed())
		bare.Spec.Processes = nil
		Expect(k8sClient.Update(ctx, bare)).To(Succeed())
		defer func() {
			current := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: provider, Namespace: namespace},
				current)).To(Succeed())
			current.Spec.Processes = []kitchenv1alpha1.ProcessSpec{
				{Name: "api", Type: kitchenv1alpha1.ProcessService, Port: 8080},
				{Name: "mailer", Type: kitchenv1alpha1.ProcessWorker},
			}
			Expect(k8sClient.Update(ctx, current)).To(Succeed())
		}()

		release := &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: provider + "-rel-1", Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: provider},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: provider + "-bld-1"},
				Image:      "registry.example.com/pricing@sha256:" + strings.Repeat("a", 64),
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Processes: []kitchenv1alpha1.ProcessSpec{
						{Name: "api", Type: kitchenv1alpha1.ProcessService, Port: 9000},
					},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())
		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prodEnv, Namespace: namespace}, env)).To(Succeed())
		env.Spec.ReleaseRef = kitchenv1alpha1.ReleaseReference{Name: release.Name}
		Expect(k8sClient.Update(ctx, env)).To(Succeed())
		defer func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, release))).To(Succeed())
			current := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prodEnv, Namespace: namespace},
				current)).To(Succeed())
			current.Spec.ReleaseRef = kitchenv1alpha1.ReleaseReference{}
			Expect(k8sClient.Update(ctx, current)).To(Succeed())
		}()

		offer(kitchenv1alpha1.ServiceOffering{
			Name:      "pricing-api",
			Process:   "api",
			VisibleTo: kitchenv1alpha1.OfferingOpen,
		})
		const name = "repo-declared"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: claim.Status.SecretName, Namespace: consumerNS}, secret)).To(Succeed())
		Expect(string(secret.Data[service.BindingKeyPort])).To(Equal("9000"),
			"the port is the release's, which is what the environment is running")
	})

	It("hands a tcp offering a host and a port and no URL", func() {
		offer(kitchenv1alpha1.ServiceOffering{
			Name:      "pricing-api",
			Process:   "api",
			Speaks:    kitchenv1alpha1.OfferingTCP,
			VisibleTo: kitchenv1alpha1.OfferingOpen,
		})
		const name = "prices"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: claim.Status.SecretName, Namespace: consumerNS}, secret)).To(Succeed())
		Expect(secret.Data).NotTo(HaveKey(service.BindingKeyURL))
		Expect(string(secret.Data[service.BindingKeyPort])).To(Equal("8080"))

		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: consumer + "-production", Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: consumer},
				Type:       kitchenv1alpha1.EnvironmentProduction,
			},
		}
		vars, unbound, err := serviceBindingEnv(ctx, k8sClient, env, consumer, consumerNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(unbound).To(BeEmpty())
		names := make([]string, 0, len(vars))
		for _, v := range vars {
			names = append(names, v.Name)
		}
		Expect(names).To(Equal([]string{"KITCHEN_SERVICE_PRICES_HOST", "KITCHEN_SERVICE_PRICES_PORT"}),
			"a wire protocol gets no URL invented for it")
	})

	It("refuses a project that does not exist, and an offering that project does not make", func() {
		const missing = "nowhere"
		createClaim(missing, `{"service": {"project": "billing", "offering": "rates"}}`)
		reconcileOnce(missing)
		Expect(getClaim(missing).Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		Expect(readyCondition(missing).Reason).To(Equal("ProjectUnknown"))
		Expect(readyCondition(missing).Message).To(ContainSubstring("billing"))

		const wrong = "wrong-offering"
		createClaim(wrong, `{"service": {"project": "pricing", "offering": "rates"}}`)
		reconcileOnce(wrong)
		Expect(getClaim(wrong).Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		Expect(readyCondition(wrong).Reason).To(Equal("OfferingUnknown"))
		Expect(readyCondition(wrong).Message).To(ContainSubstring("rates"))
		Expect(readyCondition(wrong).Message).To(ContainSubstring("pricing-api"),
			"the refusal says what the project does offer")
	})

	It("refuses an offering that admits consumers by request, and binds once it is opened", func() {
		offer(kitchenv1alpha1.ServiceOffering{Name: "pricing-api"})
		const name = "by-request"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)

		Expect(getClaim(name).Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		Expect(readyCondition(name).Reason).To(Equal("NotGranted"))
		Expect(readyCondition(name).Message).To(ContainSubstring("checkout"))

		offer(kitchenv1alpha1.ServiceOffering{
			Name:      "pricing-api",
			VisibleTo: kitchenv1alpha1.OfferingOpen,
		})
		reconcileOnce(name)
		Expect(getClaim(name).Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
	})

	It("waits for an environment the provider has not deployed into yet", func() {
		offer(kitchenv1alpha1.ServiceOffering{
			Name:        "pricing-api",
			Environment: provider + "-staging",
			VisibleTo:   kitchenv1alpha1.OfferingOpen,
		})
		const name = "staging-prices"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)

		// Pending, not Failed: an environment appears when something is
		// deployed there, so this one can come right on its own.
		Expect(getClaim(name).Status.Phase).To(Equal(kitchenv1alpha1.ClaimPending))
		Expect(readyCondition(name).Reason).To(Equal("EnvironmentMissing"))
		Expect(readyCondition(name).Message).To(ContainSubstring(provider + "-staging"))
	})
	// #494: which consumers may bind here, and what a preview reaches.
	//
	// The declaration belongs to the *provider* environment's owners, for the
	// same reason its requirements and its data class do — what an
	// environment is worth, and who it will answer, is not the deploying
	// team's to say — so every assertion here is about what that declaration
	// does to a consumer that has changed nothing.

	It("gives a preview nothing when the environment the offering names admits only production", func() {
		serveEnvironment(prodEnv, kitchenv1alpha1.EnvironmentProduction, kitchenv1alpha1.EnvironmentProduction)
		const name = "prices"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound),
			"a class nobody admits does not fail the whole binding")
		Expect(bindingFor(name, kitchenv1alpha1.EnvironmentProduction).Environment).To(Equal(prodEnv))

		preview := bindingFor(name, kitchenv1alpha1.EnvironmentPreview)
		Expect(preview.Environment).To(BeEmpty())
		Expect(preview.SecretName).To(BeEmpty())
		Expect(preview.Reason).To(ContainSubstring("serves.consumers"),
			"the refusal names what would permit it")

		// And the consumer's own preview reads no address, while its
		// production reads one — which is the whole of the feature.
		previewEnv := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: consumer + "-pr-1", Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: consumer},
				Type:       kitchenv1alpha1.EnvironmentPreview,
			},
		}
		vars, unbound, err := serviceBindingEnv(ctx, k8sClient, previewEnv, consumer, consumerNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(vars).To(BeEmpty())
		Expect(unbound).To(HaveLen(1))
		Expect(unbound[0]).To(ContainSubstring(name))

		prod := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: consumer + "-production", Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: consumer},
				Type:       kitchenv1alpha1.EnvironmentProduction,
			},
		}
		vars, unbound, err = serviceBindingEnv(ctx, k8sClient, prod, consumer, consumerNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(unbound).To(BeEmpty())
		Expect(vars).NotTo(BeEmpty())
	})

	It("sends a preview to the stage its owners opened, while production keeps production", func() {
		serveEnvironment(prodEnv, kitchenv1alpha1.EnvironmentProduction, kitchenv1alpha1.EnvironmentProduction)
		stage := provider + "-staging"
		serveEnvironment(stage, kitchenv1alpha1.EnvironmentStage, kitchenv1alpha1.EnvironmentPreview)

		const name = "prices"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)

		Expect(bindingFor(name, kitchenv1alpha1.EnvironmentProduction).Environment).To(Equal(prodEnv))
		preview := bindingFor(name, kitchenv1alpha1.EnvironmentPreview)
		Expect(preview.Environment).To(Equal(stage))
		Expect(preview.Host).To(Equal(stage + ".kitchen-pricing.svc.cluster.local"))
		Expect(preview.SecretName).NotTo(Equal(bindingFor(name, kitchenv1alpha1.EnvironmentProduction).SecretName),
			"two addresses cannot live in one Secret under one key")

		// The preview's own Secret carries the environment it reached, so
		// the address can be traced back to what admitted it.
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: preview.SecretName, Namespace: consumerNS}, secret)).To(Succeed())
		Expect(string(secret.Data[service.BindingKeyEnvironment])).To(Equal(stage))

		previewEnv := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: consumer + "-pr-2", Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: consumer},
				Type:       kitchenv1alpha1.EnvironmentPreview,
			},
		}
		vars, unbound, err := serviceBindingEnv(ctx, k8sClient, previewEnv, consumer, consumerNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(unbound).To(BeEmpty())
		Expect(vars).NotTo(BeEmpty())
		for _, v := range vars {
			Expect(v.ValueFrom.SecretKeyRef.Name).To(Equal(preview.SecretName))
		}
	})

	It("sends a consumer's production to a stage when only the stage admits it", func() {
		// The decision #494 asks for, made the way the offering's owners
		// declared it: production reaches what admits production, and where
		// that is a stage rather than the environment the offering names, it
		// is the stage. Refusing instead would be the platform overruling a
		// grant somebody deliberately made.
		serveEnvironment(prodEnv, kitchenv1alpha1.EnvironmentProduction)
		stage := provider + "-staging"
		serveEnvironment(stage, kitchenv1alpha1.EnvironmentStage, kitchenv1alpha1.EnvironmentProduction)

		const name = "prices"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)

		Expect(bindingFor(name, kitchenv1alpha1.EnvironmentProduction).Environment).To(Equal(stage))
		Expect(bindingFor(name, kitchenv1alpha1.EnvironmentPreview).Environment).To(BeEmpty())
	})

	It("fails a claim no environment of the provider admits at all", func() {
		serveEnvironment(prodEnv, kitchenv1alpha1.EnvironmentProduction)
		const name = "prices"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)

		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed),
			"a binding nothing admits is refused rather than quietly sent to production")
		Expect(readyCondition(name).Reason).To(Equal("NotAdmitted"))
		Expect(readyCondition(name).Message).To(ContainSubstring("serves nobody"))
		Expect(claim.Status.SecretName).To(BeEmpty())
		secret := &corev1.Secret{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
			Name: claimSecretName(name), Namespace: consumerNS}, secret))).To(BeTrue(),
			"and no address is left behind for anything to read")
	})

	It("takes the address back when the offering's environment stops admitting a class", func() {
		serveEnvironment(prodEnv, kitchenv1alpha1.EnvironmentProduction, kitchenv1alpha1.EnvironmentPreview)
		const name = "prices"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)
		previewSecret := bindingFor(name, kitchenv1alpha1.EnvironmentPreview).SecretName
		Expect(previewSecret).NotTo(BeEmpty())

		serveEnvironment(prodEnv, kitchenv1alpha1.EnvironmentProduction, kitchenv1alpha1.EnvironmentProduction)
		reconcileOnce(name)
		Expect(bindingFor(name, kitchenv1alpha1.EnvironmentPreview).SecretName).To(BeEmpty())
		secret := &corev1.Secret{}
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
			Name: previewSecret, Namespace: consumerNS}, secret))).To(BeTrue())
	})

	It("refuses a consumer environment rated above the environment it would reach", func() {
		// The data class composes: the comparison is DataClass.Exceeds, the
		// one every other refusal on the platform makes, and it says data
		// does not flow somewhere rated below it.
		serveEnvironment(prodEnv, kitchenv1alpha1.EnvironmentProduction, kitchenv1alpha1.EnvironmentTypes()...)
		current := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prodEnv, Namespace: namespace},
			current)).To(Succeed())
		current.Spec.DataClass = kitchenv1alpha1.DataClassInternal
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		const name = "prices"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)
		Expect(bindingFor(name, kitchenv1alpha1.EnvironmentProduction).DataClass).
			To(Equal(kitchenv1alpha1.DataClassInternal))

		classified := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: consumer + "-production", Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: consumer},
				Type:       kitchenv1alpha1.EnvironmentProduction,
				DataClass:  kitchenv1alpha1.DataClassConfidential,
			},
		}
		vars, unbound, err := serviceBindingEnv(ctx, k8sClient, classified, consumer, consumerNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(vars).To(BeEmpty())
		Expect(unbound).To(HaveLen(1))
		Expect(unbound[0]).To(ContainSubstring("confidential"))

		// An environment at or below the rating reads it, unchanged.
		classified.Spec.DataClass = kitchenv1alpha1.DataClassPublic
		vars, unbound, err = serviceBindingEnv(ctx, k8sClient, classified, consumer, consumerNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(unbound).To(BeEmpty())
		Expect(vars).NotTo(BeEmpty())
	})
	// The other half of #494, and the one that is wrong in the most
	// expensive way if it is wrong at all: a project that names its binding
	// in `spec.env` reads it through `fromResourceClaim`, which resolves a
	// claim's Secret by a different path from the platform's own
	// KITCHEN_SERVICE_ variables. A preview reading production's address
	// there would be exactly the thing this issue exists to stop.
	It("does not hand a preview the address through a fromResourceClaim variable either", func() {
		serveEnvironment(prodEnv, kitchenv1alpha1.EnvironmentProduction, kitchenv1alpha1.EnvironmentProduction)
		const name = "prices"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)
		production := bindingFor(name, kitchenv1alpha1.EnvironmentProduction)
		Expect(production.SecretName).NotTo(BeEmpty())

		release := &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: consumer + "-rel-494", Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: consumer},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: consumer + "-bld-494"},
				Image:      "registry.example.com/checkout@sha256:" + strings.Repeat("b", 64),
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Env: []kitchenv1alpha1.EnvVar{{
						Name: "PRICES_HOST",
						FromResourceClaim: &kitchenv1alpha1.ResourceClaimKeySelector{
							Name: name, Key: service.BindingKeyHost,
						},
					}},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())
		defer func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, release))).To(Succeed()) }()

		environments := &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		consumerEnv := func(name string, class kitchenv1alpha1.EnvironmentType) *kitchenv1alpha1.Environment {
			return &kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: kitchenv1alpha1.EnvironmentSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: consumer},
					Type:       class,
					ReleaseRef: kitchenv1alpha1.ReleaseReference{Name: release.Name},
				},
			}
		}

		// Production reads it, from its own class's binding.
		vars, _, requeue, err := environments.resolveEnv(ctx,
			consumerEnv(consumer+"-production", kitchenv1alpha1.EnvironmentProduction), release, consumerNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeFalse())
		named := map[string]string{}
		for _, v := range vars {
			if v.ValueFrom != nil && v.ValueFrom.SecretKeyRef != nil {
				named[v.Name] = v.ValueFrom.SecretKeyRef.Name
			}
		}
		Expect(named).To(HaveKeyWithValue("PRICES_HOST", production.SecretName))

		// The preview reads nothing at all, and is not held back waiting
		// for something another team's owners have not declared.
		vars, effects, requeue, err := environments.resolveEnv(ctx,
			consumerEnv(consumer+"-pr-9", kitchenv1alpha1.EnvironmentPreview), release, consumerNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeFalse(), "a grant another team has not made is not a state to wait through")
		for _, v := range vars {
			Expect(v.Name).NotTo(Equal("PRICES_HOST"))
		}
		Expect(effects.unboundHere).To(HaveLen(1))
		Expect(effects.unboundHere[0]).To(ContainSubstring(name))
	})

	It("does not send a consumer's production to the class a preview was admitted to", func() {
		// status.secretName is the first class that resolved, so a claim
		// only previews may bind would otherwise hand production the
		// preview's address — through the platform's own variables and
		// through a fromResourceClaim variable alike. Both paths are
		// asserted, because the two resolve the Secret separately and a fix
		// to one says nothing about the other.
		serveEnvironment(prodEnv, kitchenv1alpha1.EnvironmentProduction, kitchenv1alpha1.EnvironmentPreview)
		const name = "prices"
		createClaim(name, `{"service": {"project": "pricing", "offering": "pricing-api"}}`)
		reconcileOnce(name)
		claim := getClaim(name)
		Expect(claim.Status.SecretName).To(Equal(bindingFor(name, kitchenv1alpha1.EnvironmentPreview).SecretName))
		Expect(bindingFor(name, kitchenv1alpha1.EnvironmentProduction).SecretName).To(BeEmpty())

		prod := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: consumer + "-production", Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: consumer},
				Type:       kitchenv1alpha1.EnvironmentProduction,
			},
		}
		vars, unbound, err := serviceBindingEnv(ctx, k8sClient, prod, consumer, consumerNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(vars).To(BeEmpty())
		Expect(unbound).To(HaveLen(1))

		release := &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: consumer + "-rel-494b", Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: consumer},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: consumer + "-bld-494b"},
				Image:      "registry.example.com/checkout@sha256:" + strings.Repeat("c", 64),
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Env: []kitchenv1alpha1.EnvVar{{
						Name: "PRICES_HOST",
						FromResourceClaim: &kitchenv1alpha1.ResourceClaimKeySelector{
							Name: name, Key: service.BindingKeyHost,
						},
					}},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())
		defer func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, release))).To(Succeed()) }()
		prod.Spec.ReleaseRef = kitchenv1alpha1.ReleaseReference{Name: release.Name}

		environments := &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		resolved, effects, requeue, err := environments.resolveEnv(ctx, prod, release, consumerNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeFalse(), "a class nobody admitted is not a state to wait through")
		for _, v := range resolved {
			Expect(v.Name).NotTo(Equal("PRICES_HOST"),
				"production must not read the Secret a preview was admitted to")
		}
		Expect(effects.unboundHere).To(HaveLen(1))
		Expect(effects.unboundHere[0]).To(ContainSubstring(name))
	})
})

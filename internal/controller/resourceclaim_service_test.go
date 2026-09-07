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

		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: prodEnv, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: provider},
				Type:       kitchenv1alpha1.EnvironmentProduction,
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
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
		}
		vars, err := serviceBindingEnv(ctx, k8sClient, env, consumer, consumerNS)
		Expect(err).NotTo(HaveOccurred())
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
		}
		vars, err := serviceBindingEnv(ctx, k8sClient, env, consumer, consumerNS)
		Expect(err).NotTo(HaveOccurred())
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
})

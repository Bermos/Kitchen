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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/cache"
)

// fakeCache is a cache provider as the reconciler sees one: it records what
// the claim asked for and which of the two ways it was taken back. Whether a
// tenancy is really confined to its prefix is the provider package's test;
// what this one is for is that releasing a claim and destroying it are two
// different calls, and that the deletionPolicy decides which.
type fakeCache struct {
	asked     cache.Requirements
	released  []string
	destroyed []string
}

func (f *fakeCache) Provision(ctx context.Context, name string) (cache.Instance, error) {
	return f.ProvisionWith(ctx, name, cache.Requirements{})
}

func (f *fakeCache) ProvisionWith(_ context.Context, name string, req cache.Requirements) (cache.Instance, error) {
	f.asked = req
	tenancy := cache.TenancyShared
	if req.Tenancy == cache.TenancyDedicated {
		tenancy = cache.TenancyDedicated
	}
	return cache.Instance{
		ID: "shared:kitchen-caches/shared-cache/" + name,
		Binding: cache.Binding{
			URL:  "redis://" + name + ":secret@shared-cache.kitchen-caches.svc:6379",
			Host: "shared-cache.kitchen-caches.svc", Port: "6379",
			Username: name, Password: "secret", KeyPrefix: name + ":",
		},
		Provenance:  cache.ProvenanceProduction,
		Tenancy:     tenancy,
		TenancyNote: "a tenancy in the platform's shared shared-cache",
	}, nil
}

func (f *fakeCache) Release(_ context.Context, instanceID string) error {
	f.released = append(f.released, instanceID)
	return nil
}

func (f *fakeCache) Deprovision(_ context.Context, instanceID string) error {
	f.destroyed = append(f.destroyed, instanceID)
	return nil
}

func (f *fakeCache) CreateBranch(_ context.Context, instanceID, name string) (cache.Branch, error) {
	return cache.Branch{
		ID:         instanceID + "." + name,
		Binding:    cache.Binding{URL: "redis://preview", KeyPrefix: name + ":"},
		Provenance: cache.ProvenanceSynthetic,
		Tenancy:    cache.TenancyShared,
	}, nil
}

func (f *fakeCache) DeleteBranch(_ context.Context, _, branchID string) error {
	f.destroyed = append(f.destroyed, branchID)
	return nil
}

var _ = Describe("A redis claim", func() {
	const (
		projectName    = "clcache"
		claimName      = "clcache-jobs"
		connectionName = "clvalkey"
		namespace      = "default"
	)

	ctx := context.Background()
	claimKey := types.NamespacedName{Name: claimName, Namespace: namespace}

	var (
		reconciler *ResourceClaimReconciler
		provider   *fakeCache
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

	createClaim := func(config string, policy kitchenv1alpha1.ClaimDeletionPolicy) {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: projectName},
				ConnectionRef:  &kitchenv1alpha1.LocalObjectReference{Name: connectionName},
				Type:           kitchenv1alpha1.ClaimTypeRedis,
				DeletionPolicy: policy,
			},
		}
		if config != "" {
			claim.Spec.Config = &runtime.RawExtension{Raw: []byte(config)}
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	}

	deleteClaim := func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, getClaim()))).To(Succeed())
		reconcileOnce()
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, claimKey, &kitchenv1alpha1.ResourceClaim{}))).To(BeTrue())
	}

	BeforeEach(func() {
		provider = &fakeCache{}
		reconciler = &ResourceClaimReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Caches: func(cache.Options) (cache.Provisioner, error) { return provider, nil },
		}

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/clcache",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "reg"},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		connection := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace},
			Spec:       kitchenv1alpha1.ConnectionSpec{Provider: cache.ProviderValkey},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, connection))).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName, Namespace: namespace}, connection)).To(Succeed())
		connection.Status.Capabilities = []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityCache}
		Expect(k8sClient.Status().Update(ctx, connection)).To(Succeed())
	})

	AfterEach(func() {
		claim := &kitchenv1alpha1.ResourceClaim{}
		if err := k8sClient.Get(ctx, claimKey, claim); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey})
			Expect(err).NotTo(HaveOccurred())
		}
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("records which tenancy it was served by, and binds the prefix with it", func() {
		createClaim("", kitchenv1alpha1.ClaimRetain)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Tenancy).To(Equal(string(cache.TenancyShared)),
			"a claim that asked for nothing in particular shares a server")
		Expect(claim.Status.TenancyReason).NotTo(BeEmpty(), "which shape it got is stated, not inferred")

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: appNamespace(projectName), Name: claim.Status.SecretName,
		}, secret)).To(Succeed())
		instance := "kitchen-" + claimName
		Expect(string(secret.Data[cache.BindingKeyKeyPrefix])).To(Equal(instance+":"),
			"an application under a tenancy has to be told what to prefix its keys with")
		Expect(string(secret.Data[cache.BindingKeyUsername])).To(Equal(instance))
	})

	It("passes the claim's choice of tenancy through to the provider", func() {
		createClaim(`{"redis": {"usage": "queue", "tenancy": "dedicated"}}`, kitchenv1alpha1.ClaimRetain)
		reconcileOnce()

		Expect(provider.asked.Tenancy).To(Equal(cache.TenancyDedicated))
		Expect(provider.asked.Usage).To(Equal(cache.UsageQueue))
		Expect(getClaim().Status.Tenancy).To(Equal(string(cache.TenancyDedicated)))
	})

	It("releases the claim under Retain and destroys nothing", func() {
		createClaim("", kitchenv1alpha1.ClaimRetain)
		reconcileOnce()
		id := getClaim().Status.InstanceID

		deleteClaim()

		Expect(provider.released).To(ConsistOf(id),
			"a claim that no longer exists must not leave a working credential in a shared server")
		Expect(provider.destroyed).To(BeEmpty(),
			"destroying the data is what deletionPolicy Delete opts into, and Retain did not")
	})

	It("destroys what the claim held under Delete", func() {
		createClaim("", kitchenv1alpha1.ClaimDelete)
		reconcileOnce()
		id := getClaim().Status.InstanceID

		deleteClaim()

		Expect(provider.destroyed).To(ConsistOf(id))
		Expect(provider.released).To(BeEmpty(), "Delete is destruction, not a second release")
	})
})

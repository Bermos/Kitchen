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
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/database"
)

// plainProvisioner can be asked for a database and nothing else — the shape
// every provisioner has, and the shape a claim asking for capabilities cannot
// be served by.
type plainProvisioner struct {
	instance  database.Instance
	err       error
	branch    database.Branch
	branchErr error
}

func (p *plainProvisioner) Provision(context.Context, string) (database.Instance, error) {
	return p.instance, p.err
}
func (p *plainProvisioner) Deprovision(context.Context, string) error { return nil }
func (p *plainProvisioner) CreateBranch(context.Context, string, string) (database.Branch, error) {
	return p.branch, p.branchErr
}
func (p *plainProvisioner) DeleteBranch(context.Context, string, string) error { return nil }

// capableProvisioner answers requirements, and records the ones it was
// handed so the test can check what the claim's config actually became.
type capableProvisioner struct {
	plainProvisioner
	asked database.Requirements
	// refuse is returned from ProvisionWith, before anything would be
	// created.
	refuse error
}

func (p *capableProvisioner) ProvisionWith(
	_ context.Context,
	_ string,
	req database.Requirements,
) (database.Instance, error) {
	p.asked = req
	if p.refuse != nil {
		return database.Instance{}, p.refuse
	}
	return p.instance, p.err
}

var _ = Describe("A postgres claim that asks for a particular database", func() {
	const (
		projectName    = "clmaps"
		claimName      = "clmaps-db"
		connectionName = "clcnpg"
		namespace      = "default"
		previewEnv     = "clmaps-pr-9"
	)

	ctx := context.Background()
	claimKey := types.NamespacedName{Name: claimName, Namespace: namespace}
	appNS := "kitchen-" + projectName

	var (
		reconciler  *ResourceClaimReconciler
		provisioner database.Provisioner
	)

	bound := database.Instance{
		ID:         "kitchen-databases/kitchen-" + claimName,
		Binding:    database.Binding{URL: "postgresql://app:pw@host:5432/app", Host: "host", Port: "5432", User: "app", Password: "pw", Database: "app"},
		Provenance: database.ProvenanceProduction,
		Region:     "eu-central-2",
	}

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	getClaim := func() *kitchenv1alpha1.ResourceClaim {
		claim := &kitchenv1alpha1.ResourceClaim{}
		ExpectWithOffset(1, k8sClient.Get(ctx, claimKey, claim)).To(Succeed())
		return claim
	}

	// createClaim writes the claim through the API server, so its spec.config
	// travels exactly as the REST API would have written it.
	createClaim := func(config string) {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: projectName},
				ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: connectionName},
				Type:          kitchenv1alpha1.ClaimTypePostgres,
			},
		}
		if config != "" {
			claim.Spec.Config = &runtime.RawExtension{Raw: []byte(config)}
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	}

	BeforeEach(func() {
		reconciler = &ResourceClaimReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Databases: func(database.Options) (database.Provisioner, error) {
				return provisioner, nil
			},
		}

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/clmaps",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "reg"},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		// The self-hosted provider's Connection: no credentials secret at
		// all, which is the whole of what makes cnpg different.
		connection := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace},
			Spec:       kitchenv1alpha1.ConnectionSpec{Provider: database.ProviderCNPG},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, connection))).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName, Namespace: namespace}, connection)).To(Succeed())
		connection.Status.Capabilities = []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityDatabase}
		Expect(k8sClient.Status().Update(ctx, connection)).To(Succeed())
	})

	AfterEach(func() {
		claim := &kitchenv1alpha1.ResourceClaim{}
		if err := k8sClient.Get(ctx, claimKey, claim); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey})
			Expect(err).NotTo(HaveOccurred())
		}

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
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	// A connection with nothing in its credentials secret is not a connection
	// with a missing one, and the claim binds through it like any other.
	It("binds through a connection that holds no credential at all", func() {
		provisioner = &plainProvisioner{instance: bound}
		createClaim("")
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Residency).To(Equal("eu-central-2"))
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: claim.Status.SecretName}, secret)).To(Succeed())
		Expect(string(secret.Data["url"])).To(Equal(bound.Binding.URL))
	})

	It("hands the claim's version, extensions and storage to the provisioner", func() {
		capable := &capableProvisioner{plainProvisioner: plainProvisioner{instance: bound}}
		provisioner = capable
		createClaim(`{"postgres":{"version":"16","extensions":["postgis"],` +
			`"storage":{"size":"40Gi","storageClass":"fast"}}}`)
		reconcileOnce()

		Expect(getClaim().Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(capable.asked).To(Equal(database.Requirements{
			Version:      "16",
			Extensions:   []string{"postgis"},
			StorageSize:  "40Gi",
			StorageClass: "fast",
		}))
	})

	// The refusal is the feature: a claim that cannot be satisfied fails as a
	// claim, with the provider's own words, rather than binding and letting
	// the application die on a CREATE EXTENSION later.
	It("fails the claim, with the reason, when an extension cannot be supplied", func() {
		provisioner = &capableProvisioner{
			plainProvisioner: plainProvisioner{instance: bound},
			refuse: fmt.Errorf("%w: no image here supplies timescaledb on Postgres 17",
				database.ErrUnsatisfiable),
		}
		createClaim(`{"postgres":{"extensions":["timescaledb"]}}`)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		Expect(claim.Status.InstanceID).To(BeEmpty(), "nothing was provisioned")
		ready := meta.FindStatusCondition(claim.Status.Conditions, condReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("RequirementsUnsatisfiable"))
		Expect(ready.Message).To(ContainSubstring("timescaledb"))
	})

	// Asking a provisioner for something it has no way to answer is the same
	// mistake one provider along, so it is refused rather than provisioned as
	// though the requirements had not been written down.
	It("refuses requirements a provisioner cannot be asked for at all", func() {
		provisioner = &plainProvisioner{instance: bound}
		createClaim(`{"postgres":{"extensions":["postgis"]}}`)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		ready := meta.FindStatusCondition(claim.Status.Conditions, condReady)
		Expect(ready.Reason).To(Equal("RequirementsUnsatisfiable"))
		Expect(ready.Message).To(ContainSubstring(database.ProviderCNPG))
	})

	// A database the platform runs itself takes minutes. That is Pending, not
	// Failed — a claim that read Failed for every one of them would teach
	// everybody to ignore the word.
	It("waits Pending while the database is still coming up", func() {
		provisioner = &plainProvisioner{
			err: fmt.Errorf("%w: database kitchen-clmaps-db is still coming up (Setting up primary)",
				database.ErrNotReady),
		}
		createClaim("")
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimPending))
		ready := meta.FindStatusCondition(claim.Status.Conditions, condReady)
		Expect(ready.Reason).To(Equal("Provisioning"))
		Expect(ready.Message).To(ContainSubstring("coming up"))
	})

	// The preview half of the design, as the claim records it: a preview's
	// database is its own and empty, and the status says so per branch —
	// which is the value the policy engine judges the preview on.
	It("records a preview's database as synthetic", func() {
		provisioner = &plainProvisioner{
			instance: bound,
			branch: database.Branch{
				ID:         "kitchen-databases/kitchen-clmaps-db-" + previewEnv,
				Binding:    database.Binding{URL: "postgresql://app:pw@preview:5432/app"},
				Provenance: database.ProvenanceSynthetic,
			},
		}
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: previewEnv, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentPreview,
				Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 9, Branch: "feature/maps"},
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: projectName + "-rel-1"},
			},
		}
		Expect(k8sClient.Create(ctx, env)).To(Succeed())

		createClaim(`{"previewBranching":true}`)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Branches).To(HaveLen(1))
		Expect(claim.Status.Branches[0].Environment).To(Equal(previewEnv))
		Expect(claim.Status.Branches[0].Provenance).To(Equal(string(database.ProvenanceSynthetic)),
			"a preview's own empty database is synthetic, and that is what keeps production data out of previews")
		Expect(claim.Status.DataProvenance).To(Equal(string(database.ProvenanceProduction)),
			"the primary is still the production database")
	})

	// The same distinction the claim's own phase makes, one level down: a
	// preview database that is still coming up has not failed.
	It("says a preview database is coming up rather than that it failed", func() {
		provisioner = &plainProvisioner{
			instance: bound,
			branchErr: fmt.Errorf("%w: database kitchen-clmaps-db-clmaps-pr-9 is still coming up",
				database.ErrNotReady),
		}
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: previewEnv, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentPreview,
				Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 9, Branch: "feature/maps"},
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: projectName + "-rel-1"},
			},
		}
		Expect(k8sClient.Create(ctx, env)).To(Succeed())

		createClaim(`{"previewBranching":true}`)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound),
			"the shared binding works either way")
		branches := meta.FindStatusCondition(claim.Status.Conditions, condBranchesReady)
		Expect(branches).NotTo(BeNil())
		Expect(branches.Status).To(Equal(metav1.ConditionFalse))
		Expect(branches.Reason).To(Equal("BranchProvisioning"))
	})

	It("keeps ErrNotReady and ErrUnsatisfiable apart", func() {
		Expect(errors.Is(fmt.Errorf("x: %w", database.ErrNotReady), database.ErrUnsatisfiable)).To(BeFalse())
		Expect(errors.Is(fmt.Errorf("x: %w", database.ErrUnsatisfiable), database.ErrNotReady)).To(BeFalse())
	})
})

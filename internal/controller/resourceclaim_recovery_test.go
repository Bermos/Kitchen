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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/database"
)

// recoverableProvisioner is a provisioner whose provider can reconstruct data
// at a past moment — the optional interface, implemented, so the reconciler's
// two halves can be driven without a Neon account.
type recoverableProvisioner struct {
	plainProvisioner
	window database.RecoveryWindow
	// recovered is every RecoverTo the reconciler made, by branch name, with
	// the moment it asked for — which is what says a recovery was taken once
	// and at the right time.
	recovered map[string]time.Time
	deleted   []string
}

func (p *recoverableProvisioner) RecoveryWindow(context.Context, string) (database.RecoveryWindow, error) {
	return p.window, nil
}

func (p *recoverableProvisioner) RecoverTo(
	_ context.Context,
	_, name string,
	at time.Time,
) (database.Branch, error) {
	if p.recovered == nil {
		p.recovered = map[string]time.Time{}
	}
	if _, ok := p.recovered[name]; !ok {
		p.recovered[name] = at
	}
	return database.Branch{
		ID: "br-" + name,
		Binding: database.Binding{
			URL: "postgresql://app:pw@" + name + ":5432/app", Host: name,
			Port: "5432", User: "app", Password: "pw", Database: "app",
		},
		Provenance: database.ProvenanceProduction,
	}, nil
}

func (p *recoverableProvisioner) DeleteBranch(_ context.Context, _, branchID string) error {
	p.deleted = append(p.deleted, branchID)
	return nil
}

var _ = Describe("Recovering a claim to a point in time", func() {
	const (
		projectName    = "clrec"
		claimName      = "clrec-db"
		connectionName = "clrecconn"
		namespace      = "default"
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
		Binding:    database.Binding{URL: "postgresql://app:pw@primary:5432/app", Host: "primary", Port: "5432", User: "app", Password: "pw", Database: "app"},
		Provenance: database.ProvenanceProduction,
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

	secretData := func(name string) map[string][]byte {
		secret := &corev1.Secret{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: name}, secret)).To(Succeed())
		return secret.Data
	}

	createClaim := func() {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: projectName},
				ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: connectionName},
				Type:          kitchenv1alpha1.ClaimTypePostgres,
				DataClass:     kitchenv1alpha1.DataClassConfidential,
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	}

	// ask appends a recovery request the way the API's POST does.
	ask := func(name string, at time.Time) {
		claim := getClaim()
		claim.Spec.Recoveries = append(claim.Spec.Recoveries, kitchenv1alpha1.ClaimRecoveryRequest{
			Name: name, At: metav1.NewTime(at),
		})
		ExpectWithOffset(1, k8sClient.Update(ctx, claim)).To(Succeed())
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
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/clrec",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "reg"},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		// A hosted provider's Connection needs a credential; the factory
		// above is what actually answers, but the CRD refuses one without.
		credentials := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName + "-creds", Namespace: namespace},
			Data:       map[string][]byte{databaseTokenKey: []byte("token")},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, credentials))).To(Succeed())

		connection := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             database.ProviderNeon,
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: connectionName + "-creds"},
			},
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
		for _, obj := range []client.Object{
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: connectionName + "-creds", Namespace: namespace}},
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	// Observed, never declared: a provisioner that does not implement the
	// optional interface offers nothing, and says which provider and why
	// rather than showing a window it cannot honour.
	It("offers no recovery through a provider that cannot do it", func() {
		provisioner = &plainProvisioner{instance: bound}
		createClaim()
		reconcileOnce()

		recovery := getClaim().Status.Recovery
		Expect(recovery).NotTo(BeNil())
		Expect(recovery.Available).To(BeFalse())
		Expect(recovery.Window).To(BeNil())
		Expect(recovery.Reason).To(ContainSubstring(database.ProviderNeon))
	})

	// A provider that keeps no history is not a provider that cannot recover:
	// the interface is there, the window is empty, and the claim offers
	// nothing either way.
	It("offers no recovery over an empty window", func() {
		now := time.Now()
		provisioner = &recoverableProvisioner{
			plainProvisioner: plainProvisioner{instance: bound},
			window:           database.RecoveryWindow{Earliest: now, Latest: now},
		}
		createClaim()
		reconcileOnce()

		recovery := getClaim().Status.Recovery
		Expect(recovery.Available).To(BeFalse())
		Expect(recovery.Window).To(BeNil())
		Expect(recovery.Reason).To(ContainSubstring("keeps no history"))
	})

	It("records the window, recovers to a sibling, promotes it, and retains what it displaced", func() {
		provider := &recoverableProvisioner{
			plainProvisioner: plainProvisioner{instance: bound},
			window: database.RecoveryWindow{
				Earliest: time.Now().Add(-7 * 24 * time.Hour),
				Latest:   time.Now(),
			},
		}
		provisioner = provider
		createClaim()
		reconcileOnce()

		By("reading the window off the provider")
		claim := getClaim()
		Expect(claim.Status.Recovery.Available).To(BeTrue())
		Expect(claim.Status.Recovery.Window).NotTo(BeNil())
		Expect(claim.Status.Recovery.Window.Earliest.Time).To(BeTemporally("<", claim.Status.Recovery.Window.Latest.Time))

		By("recovering to a sibling with a binding of its own")
		at := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
		ask("before-the-migration", at)
		reconcileOnce()

		claim = getClaim()
		Expect(claim.Status.Recovery.Recoveries).To(HaveLen(1))
		recovered := claim.Status.Recovery.Recoveries[0]
		Expect(recovered.Phase).To(Equal(kitchenv1alpha1.ClaimRecoveryReady))
		Expect(recovered.ID).To(Equal("br-recovery-before-the-migration"))
		// Production data at an earlier moment, under the claim's own class:
		// a recovery is a new place the same data lives.
		Expect(recovered.Provenance).To(Equal(string(database.ProvenanceProduction)))
		Expect(recovered.DataClass).To(Equal(kitchenv1alpha1.DataClassConfidential))
		Expect(provider.recovered).To(HaveKey("recovery-before-the-migration"))
		Expect(provider.recovered["recovery-before-the-migration"]).To(BeTemporally("==", at))

		// The sibling's binding is its own, and the claim's is untouched:
		// recovering changes nothing the application is reading.
		Expect(string(secretData(recovered.SecretName)["host"])).To(Equal("recovery-before-the-migration"))
		Expect(string(secretData(claim.Status.SecretName)["host"])).To(Equal("primary"))

		By("promoting it")
		claim.Spec.PromotedRecovery = "before-the-migration"
		Expect(k8sClient.Update(ctx, claim)).To(Succeed())
		reconcileOnce()

		claim = getClaim()
		// The claim's own binding is now the recovery's, which is what rolls
		// every environment reading it onto the recovered database.
		Expect(string(secretData(claim.Status.SecretName)["host"])).To(Equal("recovery-before-the-migration"))
		Expect(claim.Status.Recovery.Recoveries[0].PromotedAt).NotTo(BeNil())
		// What it displaced is retained, and recorded so somebody can find
		// it: the database the claim was provisioned with.
		Expect(claim.Status.Recovery.Retained).To(HaveLen(1))
		Expect(claim.Status.Recovery.Retained[0].DisplacedBy).To(Equal("before-the-migration"))
		Expect(claim.Status.Recovery.Retained[0].Recovery).To(BeEmpty())
		// Nothing was destroyed on the way: retaining the original is the
		// whole of why promote is safe to get wrong.
		Expect(provider.deleted).To(BeEmpty())

		By("staying promoted through another reconcile")
		reconcileOnce()
		Expect(string(secretData(getClaim().Status.SecretName)["host"])).To(Equal("recovery-before-the-migration"))
		Expect(getClaim().Status.Recovery.Retained).To(HaveLen(1))
	})

	It("discards a recovery nobody promoted, with its database and its binding", func() {
		provider := &recoverableProvisioner{
			plainProvisioner: plainProvisioner{instance: bound},
			window: database.RecoveryWindow{
				Earliest: time.Now().Add(-24 * time.Hour),
				Latest:   time.Now(),
			},
		}
		provisioner = provider
		createClaim()
		reconcileOnce()
		ask("a-look", time.Now().Add(-time.Hour).Truncate(time.Second))
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Recovery.Recoveries).To(HaveLen(1))
		secretName := claim.Status.Recovery.Recoveries[0].SecretName

		claim.Spec.Recoveries = nil
		Expect(k8sClient.Update(ctx, claim)).To(Succeed())
		reconcileOnce()

		Expect(getClaim().Status.Recovery.Recoveries).To(BeEmpty())
		Expect(provider.deleted).To(ContainElement("br-recovery-a-look"))
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: secretName}, &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the discarded recovery's binding is gone")
	})
})

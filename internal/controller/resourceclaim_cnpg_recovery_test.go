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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/backup"
	"github.com/Bermos/Kitchen/internal/provider/database"
)

// A database this cluster runs is recoverable through its own archive and
// through nothing else, which makes the two halves of #247 and #245 one
// walk: no policy, no window; a policy with nothing in it yet, still no
// window; a base backup, and the claim can be recovered to a moment.
//
// The provisioner here is CloudNativePG's shape rather than CloudNativePG:
// the same two optional interfaces, the same sentinel errors, and the same
// order of answers. What it stands in for — a Cluster with a barmanObjectStore
// and a firstRecoverabilityPoint — is pinned in the provider's own tests,
// where there is a Cluster object to assert about.
type archivedProvisioner struct {
	plainProvisioner

	// policy is the last one the platform configured, which is what decides
	// whether there is an archive to recover from at all.
	policy database.BackupPolicy
	// firstPoint is what the destination reports it can reach back to; nil
	// until a base backup has been taken and read back.
	firstPoint *time.Time
	// replayed names the recoveries that have finished coming up. A database
	// this cluster runs takes minutes to recover, and the first answer for
	// every one of them is ErrNotReady.
	replayed map[string]bool
}

func (p *archivedProvisioner) ConfigureBackup(_ context.Context, _ string, policy database.BackupPolicy) error {
	p.policy = policy
	return nil
}

func (p *archivedProvisioner) BackupState(context.Context, string) (database.BackupState, error) {
	return database.BackupState{
		Configured:            p.policy.Enabled,
		Destination:           p.policy.Destination.Path(),
		Schedule:              p.policy.Schedule,
		FirstRecoverablePoint: p.firstPoint,
		Archiving:             database.ArchivingHealthy,
	}, nil
}

func (p *archivedProvisioner) RecoveryWindow(context.Context, string) (database.RecoveryWindow, error) {
	if !p.policy.Enabled {
		return database.RecoveryWindow{}, fmt.Errorf("%w: recovering a database this cluster runs means "+
			"bootstrapping a new one from its archive, and this one has no backup policy to archive to. "+
			"Give the claim one — spec.backup, or the platform's own backup destination",
			database.ErrUnsatisfiable)
	}
	if p.firstPoint == nil {
		return database.RecoveryWindow{}, fmt.Errorf("%w: it has not reported a recovery point yet, which "+
			"is the state a database is in until its first base backup has been taken and read back",
			database.ErrNotReady)
	}
	return database.RecoveryWindow{Earliest: *p.firstPoint, Latest: time.Now().Add(-5 * time.Minute)}, nil
}

func (p *archivedProvisioner) RecoverTo(
	_ context.Context, _, name string, at time.Time,
) (database.Branch, error) {
	if p.replayed == nil {
		p.replayed = map[string]bool{}
	}
	if !p.replayed[name] {
		p.replayed[name] = true
		return database.Branch{}, fmt.Errorf("%w: database %s is still coming up (Creating a new replica)",
			database.ErrNotReady, name)
	}
	return database.Branch{
		ID: "kitchen-databases/" + name,
		Binding: database.Binding{
			URL: "postgresql://app:pw@" + name + "-rw:5432/app", Host: name + "-rw",
			Port: "5432", User: "app", Password: "pw", Database: "app",
		},
		Provenance: database.ProvenanceProduction,
	}, nil
}

var _ = Describe("Recovering a database this cluster runs", func() {
	const (
		projectName    = "clcnpgrec"
		claimName      = "clcnpgrec-db"
		connectionName = "clcnpgrecconn"
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
		ID:         database.DefaultDatabaseNamespace + "/kitchen-" + claimName,
		Name:       "kitchen-" + claimName,
		Binding:    database.Binding{URL: "postgresql://app:pw@primary-rw:5432/app", Host: "primary-rw", Port: "5432", User: "app", Password: "pw", Database: "app"},
		Provenance: database.ProvenanceProduction,
	}

	reconcileOnce := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey})
		Expect(err).NotTo(HaveOccurred())
	}

	getClaim := func() *kitchenv1alpha1.ResourceClaim {
		GinkgoHelper()
		claim := &kitchenv1alpha1.ResourceClaim{}
		Expect(k8sClient.Get(ctx, claimKey, claim)).To(Succeed())
		return claim
	}

	secretData := func(name string) map[string][]byte {
		GinkgoHelper()
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: name}, secret)).To(Succeed())
		return secret.Data
	}

	// platformDestination is the one place an installation says where its
	// databases are archived to, which is also where their recovery windows
	// come from.
	platformDestination := func() {
		GinkgoHelper()
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Backup: kitchenv1alpha1.BackupSpec{
					Destination: &kitchenv1alpha1.BackupDestination{
						Type: kitchenv1alpha1.BackupDestinationS3,
						S3: &kitchenv1alpha1.S3Destination{
							Bucket: "kitchen-archive",
							Prefix: "prod",
							Region: "eu-central-1",
							CredentialsSecretRef: &kitchenv1alpha1.LocalObjectReference{
								Name: "clcnpgrec-destination",
							},
						},
					},
				},
			},
		})
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "clcnpgrec-destination"},
			Data: map[string][]byte{
				backup.CredentialKeyAccessKeyID:     []byte("AKIAEXAMPLE"),
				backup.CredentialKeySecretAccessKey: []byte("s3cr3t"),
			},
		}))).To(Succeed())
	}

	createClaim := func() {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: projectName},
				ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: connectionName},
				Type:          kitchenv1alpha1.ClaimTypePostgres,
				DataClass:     kitchenv1alpha1.DataClassConfidential,
			},
		})).To(Succeed())
	}

	ask := func(name string, at time.Time) {
		GinkgoHelper()
		claim := getClaim()
		claim.Spec.Recoveries = append(claim.Spec.Recoveries, kitchenv1alpha1.ClaimRecoveryRequest{
			Name: name, At: metav1.NewTime(at),
		})
		Expect(k8sClient.Update(ctx, claim)).To(Succeed())
	}

	BeforeEach(func() {
		reconciler = &ResourceClaimReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Databases: func(database.Options) (database.Provisioner, error) {
				return provisioner, nil
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: database.DefaultDatabaseNamespace},
		}))).To(Succeed())

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/clcnpgrec",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "reg"},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		// The one database Connection with no credential: the platform
		// provisions with its own account, into the cluster it was installed
		// in.
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
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "clcnpgrec-destination"}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	// The asymmetry #247 asks to be said out loud rather than shown as a
	// greyed-out button: this provider recovers from the archive the backup
	// policy writes, so a claim without one offers nothing and names the fix.
	It("offers no recovery until the database has an archive to recover from", func() {
		provisioner = &archivedProvisioner{plainProvisioner: plainProvisioner{instance: bound}}
		createClaim()
		reconcileOnce()

		claim := getClaim()
		// The claim is bound throughout: the application is reading its
		// database whatever the recovery surface says.
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Recovery).NotTo(BeNil())
		Expect(claim.Status.Recovery.Available).To(BeFalse())
		Expect(claim.Status.Recovery.Window).To(BeNil())
		Expect(claim.Status.Recovery.Reason).To(ContainSubstring("spec.backup"))
	})

	It("walks a claim with a backup policy from no window to a promoted recovery", func() {
		provider := &archivedProvisioner{plainProvisioner: plainProvisioner{instance: bound}}
		provisioner = provider
		platformDestination()
		createClaim()

		By("configuring the policy, which still recovers to nothing until a base backup is read back")
		reconcileOnce()
		claim := getClaim()
		Expect(claim.Status.Backup.Enabled).To(BeTrue())
		Expect(claim.Status.Backup.FirstRecoverablePoint).To(BeNil())
		Expect(claim.Status.Recovery.Available).To(BeFalse())
		Expect(claim.Status.Recovery.Reason).To(ContainSubstring("recovery point"))

		By("reading the window off the destination once there is one")
		first := time.Now().Add(-48 * time.Hour)
		provider.firstPoint = &first
		reconcileOnce()

		claim = getClaim()
		// The same moment the backup half publishes: one fact, read from the
		// database, reported by both halves.
		Expect(claim.Status.Backup.FirstRecoverablePoint).NotTo(BeNil())
		Expect(claim.Status.Recovery.Available).To(BeTrue())
		Expect(claim.Status.Recovery.Window).NotTo(BeNil())
		Expect(claim.Status.Recovery.Window.Earliest.Time).To(BeTemporally("~", first, time.Second))
		Expect(claim.Status.Recovery.Window.Latest.Time).To(BeTemporally("<", time.Now()))

		By("waiting for the sibling while CloudNativePG replays it")
		at := time.Now().Add(-6 * time.Hour).Truncate(time.Second)
		ask("before-the-migration", at)
		reconcileOnce()

		claim = getClaim()
		Expect(claim.Status.Recovery.Recoveries).To(HaveLen(1))
		Expect(claim.Status.Recovery.Recoveries[0].Phase).To(Equal(kitchenv1alpha1.ClaimRecoveryPending))
		Expect(claim.Status.Recovery.Recoveries[0].Message).To(ContainSubstring("still coming up"))
		// Nothing the application reads has changed while it comes up.
		Expect(string(secretData(claim.Status.SecretName)["host"])).To(Equal("primary-rw"))

		By("binding the sibling once it is serving")
		reconcileOnce()
		claim = getClaim()
		recovered := claim.Status.Recovery.Recoveries[0]
		Expect(recovered.Phase).To(Equal(kitchenv1alpha1.ClaimRecoveryReady))
		Expect(recovered.At.Time).To(BeTemporally("==", at))
		Expect(recovered.Provenance).To(Equal(string(database.ProvenanceProduction)))
		Expect(recovered.DataClass).To(Equal(kitchenv1alpha1.DataClassConfidential))
		Expect(string(secretData(recovered.SecretName)["host"])).To(Equal("recovery-before-the-migration-rw"))
		Expect(string(secretData(claim.Status.SecretName)["host"])).To(Equal("primary-rw"))

		By("promoting it, which moves the claim's binding and retains what it displaced")
		claim.Spec.PromotedRecovery = "before-the-migration"
		Expect(k8sClient.Update(ctx, claim)).To(Succeed())
		reconcileOnce()

		claim = getClaim()
		Expect(string(secretData(claim.Status.SecretName)["host"])).To(Equal("recovery-before-the-migration-rw"))
		Expect(claim.Status.Recovery.Recoveries[0].PromotedAt).NotTo(BeNil())
		Expect(claim.Status.Recovery.Retained).To(HaveLen(1))
		Expect(claim.Status.Recovery.Retained[0].DisplacedBy).To(Equal("before-the-migration"))
	})

	// A policy switched off does not strand the copies it made: the window
	// goes, the siblings stay, and discarding one still discards it.
	It("keeps the recoveries it has when the window goes away", func() {
		provider := &archivedProvisioner{plainProvisioner: plainProvisioner{instance: bound}}
		provisioner = provider
		platformDestination()
		createClaim()
		first := time.Now().Add(-48 * time.Hour)
		provider.firstPoint = &first
		reconcileOnce()

		ask("a-look", time.Now().Add(-time.Hour).Truncate(time.Second))
		reconcileOnce()
		reconcileOnce()
		Expect(getClaim().Status.Recovery.Recoveries[0].Phase).To(Equal(kitchenv1alpha1.ClaimRecoveryReady))

		By("switching the policy off")
		claim := getClaim()
		off := false
		claim.Spec.Backup = &kitchenv1alpha1.ClaimBackupSpec{Enabled: &off}
		Expect(k8sClient.Update(ctx, claim)).To(Succeed())
		reconcileOnce()

		claim = getClaim()
		Expect(claim.Status.Recovery.Available).To(BeFalse())
		Expect(claim.Status.Recovery.Window).To(BeNil())
		// The copy is still there, still bound, still listed — a window that
		// has gone says nothing about a database that exists.
		Expect(claim.Status.Recovery.Recoveries).To(HaveLen(1))
		Expect(claim.Status.Recovery.Recoveries[0].Phase).To(Equal(kitchenv1alpha1.ClaimRecoveryReady))
		Expect(secretData(claim.Status.Recovery.Recoveries[0].SecretName)).NotTo(BeEmpty())
	})
})

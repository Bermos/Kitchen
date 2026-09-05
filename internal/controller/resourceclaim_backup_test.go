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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/backup"
	"github.com/Bermos/Kitchen/internal/provider/database"
)

// backingProvisioner is a provider whose backups this platform configures. It
// records the policy it was handed, so a test can check the inheritance
// without a CloudNativePG to look at.
type backingProvisioner struct {
	plainProvisioner
	configured []database.BackupPolicy
	refuse     error
	state      database.BackupState
}

func (p *backingProvisioner) ConfigureBackup(
	_ context.Context, _ string, policy database.BackupPolicy,
) error {
	p.configured = append(p.configured, policy)
	return p.refuse
}

func (p *backingProvisioner) BackupState(context.Context, string) (database.BackupState, error) {
	return p.state, nil
}

// at is a pointer to a moment, which is how a provider reports the
// timestamps it has and omits the ones it does not.
func at(when time.Time) *time.Time { return &when }

// selfBackingProvisioner is a provider that keeps its own history — the shape
// Neon has, without the HTTP.
type selfBackingProvisioner struct{ plainProvisioner }

func (selfBackingProvisioner) ManagedBackupNote() string {
	return "this provider keeps continuous history of its own, for as long as its retention says"
}

var _ = Describe("The backup policy of a postgres claim", func() {
	const (
		projectName    = "clbak"
		claimName      = "clbak-db"
		connectionName = "clbakcnpg"
		namespace      = "default"
	)

	ctx := context.Background()
	claimKey := types.NamespacedName{Name: claimName, Namespace: namespace}
	databaseNS := database.DefaultDatabaseNamespace

	var (
		reconciler  *ResourceClaimReconciler
		provisioner database.Provisioner
	)

	bound := database.Instance{
		ID:         databaseNS + "/kitchen-" + claimName,
		Name:       "kitchen-" + claimName,
		Binding:    database.Binding{URL: "postgresql://app:pw@host:5432/app", User: "app", Password: "pw"},
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

	// platformDestination configures the singleton's own backup destination,
	// which is what every claim inherits when nothing narrower says otherwise.
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
								Name: "kitchen-backup-destination",
							},
						},
					},
				},
			},
		})
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "kitchen-backup-destination"},
			Data: map[string][]byte{
				backup.CredentialKeyAccessKeyID:     []byte("AKIAEXAMPLE"),
				backup.CredentialKeySecretAccessKey: []byte("s3cr3t"),
			},
		}))).To(Succeed())
	}

	createClaim := func(spec *kitchenv1alpha1.ClaimBackupSpec) {
		GinkgoHelper()
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: projectName},
				ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: connectionName},
				Type:          kitchenv1alpha1.ClaimTypePostgres,
				Backup:        spec,
			},
		}
		Expect(k8sClient.Create(ctx, claim)).To(Succeed())
	}

	// connectionWith sets the operator's default for every claim through this
	// Connection, in the `cnpg` slice of its config where the rest of the
	// per-installation defaults already live.
	connectionWith := func(config string) {
		GinkgoHelper()
		connection := &kitchenv1alpha1.Connection{}
		key := types.NamespacedName{Name: connectionName, Namespace: namespace}
		Expect(k8sClient.Get(ctx, key, connection)).To(Succeed())
		connection.Spec.Config = &runtime.RawExtension{Raw: []byte(config)}
		Expect(k8sClient.Update(ctx, connection)).To(Succeed())
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
			ObjectMeta: metav1.ObjectMeta{Name: databaseNS},
		}))).To(Succeed())

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/clbak",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "reg"},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

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
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "kitchen-backup-destination"}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("inherits the platform's destination and copies the credential where the database can read it", func() {
		backer := &backingProvisioner{
			plainProvisioner: plainProvisioner{instance: bound},
			state: database.BackupState{
				Configured:            true,
				Destination:           "s3://kitchen-archive/prod/databases",
				Schedule:              database.DefaultClaimBackupSchedule,
				FirstRecoverablePoint: at(time.Date(2026, 8, 1, 3, 14, 0, 0, time.UTC)),
				LastBackup:            at(time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC)),
				Archiving:             database.ArchivingHealthy,
			},
		}
		provisioner = backer
		platformDestination()
		createClaim(nil)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Backup).NotTo(BeNil())
		Expect(claim.Status.Backup.Enabled).To(BeTrue())
		Expect(claim.Status.Backup.ProviderManaged).To(BeFalse())
		Expect(claim.Status.Backup.Destination).To(Equal("s3://kitchen-archive/prod/databases"))
		// The number the whole phase exists for.
		Expect(claim.Status.Backup.FirstRecoverablePoint).NotTo(BeNil())
		Expect(claim.Status.Backup.FirstRecoverablePoint.Time).To(BeTemporally("==", time.Date(2026, 8, 1, 3, 14, 0, 0, time.UTC)))
		Expect(claim.Status.Backup.Archiving).To(Equal(kitchenv1alpha1.ClaimArchivingHealthy))

		Expect(backer.configured).To(HaveLen(1))
		policy := backer.configured[0]
		Expect(policy.Enabled).To(BeTrue())
		Expect(policy.Destination.Bucket).To(Equal("kitchen-archive"))
		// A prefix of the databases' own, so a bucket shared with the
		// platform archive reads as two things.
		Expect(policy.Destination.Prefix).To(Equal("prod/databases"))

		// The credential the API wrote lives in the platform namespace, and
		// CloudNativePG resolves a Secret reference in the Cluster's own — so
		// it is copied, exactly as the registry pull credential is.
		copied := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: databaseNS, Name: claimBackupSecretName(claimName),
		}, copied)).To(Succeed())
		Expect(string(copied.Data[backup.CredentialKeyAccessKeyID])).To(Equal("AKIAEXAMPLE"))
		Expect(string(copied.Data[backupCredentialRegionKey])).To(Equal("eu-central-1"))
		Expect(copied.Labels[labelManagedByKey]).To(Equal(labelManagedByValue))
	})

	It("says where to configure a destination when there is none anywhere", func() {
		provisioner = &backingProvisioner{plainProvisioner: plainProvisioner{instance: bound}}
		createClaim(nil)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Backup.Enabled).To(BeFalse())
		Expect(claim.Status.Backup.Reason).To(ContainSubstring("/platform/backup/destination"))
	})

	It("takes the connection's defaults and lets the claim override them", func() {
		backer := &backingProvisioner{plainProvisioner: plainProvisioner{instance: bound}}
		provisioner = backer
		platformDestination()
		connectionWith(`{"backup":{"schedule":"0 0 2 * * *","retentionPolicy":"14d"}}`)
		createClaim(&kitchenv1alpha1.ClaimBackupSpec{RetentionPolicy: "90d"})
		reconcileOnce()

		Expect(backer.configured).To(HaveLen(1))
		policy := backer.configured[0]
		// Field by field: the claim narrows what it names and inherits the
		// rest, which is what makes a Connection a default rather than an
		// all-or-nothing.
		Expect(policy.Schedule).To(Equal("0 0 2 * * *"))
		Expect(policy.RetentionPolicy).To(Equal("90d"))
	})

	It("switches off for a claim that asks to be left alone, and says the archives stay", func() {
		backer := &backingProvisioner{plainProvisioner: plainProvisioner{instance: bound}}
		provisioner = backer
		platformDestination()
		off := false
		createClaim(&kitchenv1alpha1.ClaimBackupSpec{Enabled: &off})
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Backup.Enabled).To(BeFalse())
		Expect(claim.Status.Backup.Reason).To(ContainSubstring("stays"))
		// The provisioner is still asked, with the policy off: that is what
		// takes an object store off a database that had one, and it is the
		// only thing switching off does — nothing at the destination moves.
		Expect(backer.configured).To(HaveLen(1))
		Expect(backer.configured[0].Enabled).To(BeFalse())
	})

	It("reports a database the platform did not create rather than writing to it", func() {
		// What ConfigureBackup answers for an adopted Cluster: a refusal, not
		// a failure, so the claim stays bound and the sentence lands where
		// somebody is already looking.
		backer := &backingProvisioner{
			plainProvisioner: plainProvisioner{instance: bound},
			refuse: fmt.Errorf("%w: database kitchen-clbak-db was handed to this platform rather than "+
				"created by it", database.ErrBackupNotManaged),
		}
		provisioner = backer
		platformDestination()
		createClaim(nil)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Backup.Enabled).To(BeFalse())
		Expect(claim.Status.Backup.Reason).To(ContainSubstring("handed to this platform"))
	})

	It("reports a provider that keeps its own history and asks it for nothing", func() {
		provisioner = &selfBackingProvisioner{plainProvisioner{instance: bound}}
		platformDestination()
		createClaim(nil)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Backup.ProviderManaged).To(BeTrue())
		Expect(claim.Status.Backup.Enabled).To(BeFalse())
		Expect(claim.Status.Backup.Reason).To(ContainSubstring("continuous history"))
		// Nothing was configured and no credential was copied: the platform
		// manages nothing it cannot.
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: databaseNS, Name: claimBackupSecretName(claimName),
		}, &corev1.Secret{})).NotTo(Succeed())
	})

	It("removes only its own bookkeeping when the claim goes", func() {
		backer := &backingProvisioner{plainProvisioner: plainProvisioner{instance: bound}}
		provisioner = backer
		platformDestination()
		createClaim(nil)
		reconcileOnce()

		copied := types.NamespacedName{Namespace: databaseNS, Name: claimBackupSecretName(claimName)}
		Expect(k8sClient.Get(ctx, copied, &corev1.Secret{})).To(Succeed())

		claim := getClaim()
		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
		reconcileOnce()

		Expect(k8sClient.Get(ctx, copied, &corev1.Secret{})).NotTo(Succeed())
		// Deleting the claim never asked the provisioner for anything: the
		// last policy it saw is the one that was in force, and backups
		// outlive the claim under either deletion policy.
		Expect(backer.configured).To(HaveLen(1))
		Expect(backer.configured[0].Enabled).To(BeTrue())
	})
})

var _ = Describe("A claim of a type nothing backs up", func() {
	ctx := context.Background()

	It("is refused a backup policy at admission", func() {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "clbak-bucket", Namespace: "default"},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: "clbak"},
				ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "store"},
				Type:          kitchenv1alpha1.ClaimTypeObjectStore,
				Backup:        &kitchenv1alpha1.ClaimBackupSpec{RetentionPolicy: "30d"},
			},
		}
		err := k8sClient.Create(ctx, claim)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("postgres"))
	})

	It("refuses a retention that is not a count and a unit", func() {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "clbak-badretention", Namespace: "default"},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: "clbak"},
				ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "pg"},
				Type:          kitchenv1alpha1.ClaimTypePostgres,
				Backup:        &kitchenv1alpha1.ClaimBackupSpec{RetentionPolicy: "forever"},
			},
		}
		Expect(k8sClient.Create(ctx, claim)).NotTo(Succeed())
	})
})

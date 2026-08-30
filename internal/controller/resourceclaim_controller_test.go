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
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/provider/database"
	"github.com/Bermos/Kitchen/internal/provider/database/databasetest"
)

// recordingSignedStore captures the declaration envelopes the reconciler
// would keep, in place of a ClickHouse.
type recordingSignedStore struct {
	records []clickhouse.SignedRecord
}

func (s *recordingSignedStore) InsertSignedRecord(_ context.Context, record clickhouse.SignedRecord) error {
	s.records = append(s.records, record)
	return nil
}

var _ = Describe("ResourceClaim Controller", func() {
	const (
		projectName     = "clshop"
		claimName       = "clshop-db"
		connectionName  = "clneon"
		credentialsName = "clneon-credentials"
		namespace       = "default"
		previewEnvName  = "clshop-pr-3"
	)

	ctx := context.Background()
	claimKey := types.NamespacedName{Name: claimName, Namespace: namespace}
	appNS := "kitchen-" + projectName

	var (
		fake       *databasetest.NeonServer
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

	readyCondition := func(claim *kitchenv1alpha1.ResourceClaim) *metav1.Condition {
		return meta.FindStatusCondition(claim.Status.Conditions, condReady)
	}

	// createClaim writes the claim through the API server, so CRD defaulting
	// (deletionPolicy: Retain) applies exactly as it would in production.
	createClaim := func(mutate func(*kitchenv1alpha1.ResourceClaim)) {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: projectName},
				ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: connectionName},
				Type:          "postgres",
			},
		}
		if mutate != nil {
			mutate(claim)
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	}

	setCapabilities := func(name string, capabilities ...kitchenv1alpha1.Capability) {
		conn := &kitchenv1alpha1.Connection{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, conn)).To(Succeed())
		conn.Status.Capabilities = capabilities
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, conn)).To(Succeed())
	}

	BeforeEach(func() {
		fake = databasetest.NewNeonServer()
		reconciler = &ResourceClaimReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			// The provisioner the factory hands back is the real Neon client;
			// only its API URL points at the fake.
			Databases: func(opts database.Options) (database.Provisioner, error) {
				return &database.Neon{APIURL: fake.URL(), Token: opts.Token}, nil
			},
		}

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/clshop",
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
			StringData: map[string]string{"token": "neon-token"},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, credentials))).To(Succeed())

		connection := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "neon",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: credentialsName},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, connection))).To(Succeed())
		// What the Connection reconciler will write once it validates; the
		// claim controller only ever matches on this status.
		setCapabilities(connectionName, kitchenv1alpha1.CapabilityDatabase)
	})

	AfterEach(func() {
		// The claim first, through its finalizer, while the fake provider is
		// still there to take the deletes.
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
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: credentialsName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("provisions through the plugin and binds the claim", func() {
		createClaim(nil)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.SecretName).To(Equal(claimName + "-binding"))
		Expect(claim.Status.InstanceID).NotTo(BeEmpty())
		Expect(claim.Spec.DeletionPolicy).To(Equal(kitchenv1alpha1.ClaimRetain), "the CRD default must be Retain")
		ready := readyCondition(claim)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		provisioned := meta.FindStatusCondition(claim.Status.Conditions, condProvisioned)
		Expect(provisioned).NotTo(BeNil())
		Expect(provisioned.Status).To(Equal(metav1.ConditionTrue))

		project := fake.ProjectNamed("kitchen-" + claimName)
		Expect(project).NotTo(BeNil(), "no instance was provisioned at the provider")

		// The provider's declaration reaches the status: what the data derives
		// from, and where the provider actually put it.
		Expect(claim.Status.DataProvenance).To(Equal("production"),
			"a Neon project IS the production database, and the claim must say so")
		Expect(claim.Status.Residency).To(Equal(databasetest.NeonRegion),
			"the provider's reported placement is the placement of record")

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: claim.Status.SecretName}, secret)).To(Succeed())
		Expect(secret.Labels).To(HaveKeyWithValue(labelManagedByKey, labelManagedByValue))
		Expect(secret.Labels).To(HaveKeyWithValue(labelClaim, claimName))
		branch := fake.BranchNamed("kitchen-"+claimName, "main")
		Expect(branch).NotTo(BeNil())
		Expect(string(secret.Data["host"])).To(Equal(branch.Host()))
		Expect(string(secret.Data["password"])).To(Equal(branch.Password()))
		Expect(string(secret.Data["user"])).To(Equal("neondb_owner"))
		Expect(string(secret.Data["database"])).To(Equal("neondb"))
		Expect(string(secret.Data["port"])).To(Equal("5432"))
		Expect(string(secret.Data["url"])).To(ContainSubstring(branch.Host()))
	})

	It("signs and stores the provider's data-class declaration on bind", func() {
		// The platform side of the declaration: a Kitchen with a store and a
		// signing key, and a fake record store capturing what would be kept.
		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Compliance: kitchenv1alpha1.ComplianceSpec{
					Attestation: kitchenv1alpha1.AttestationSpec{Enabled: true},
				},
				Observability: kitchenv1alpha1.ObservabilitySpec{
					ClickHouse: kitchenv1alpha1.ClickHouseSpec{
						SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: "clickhouse-conn"},
					},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())
		// The platform's own pieces live in the platform namespace: the store
		// secret and the signing key are read from there, not from wherever
		// the claim happens to be.
		platformNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, platformNS))).To(Succeed())
		storeSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "clickhouse-conn", Namespace: PlatformNamespace},
			StringData: map[string]string{
				"host": "clickhouse", "httpPort": "8123", "database": "kitchen", "username": "kitchen",
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, storeSecret))).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, storeSecret))).To(Succeed())
		})
		key, err := EnsureSigningKey(ctx, k8sClient, PlatformNamespace, SigningKeySecretName, true)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: SigningKeySecretName, Namespace: PlatformNamespace},
			}))).To(Succeed())
		})
		store := &recordingSignedStore{}
		reconciler.Records = func(clickhouse.Config) SignedRecordStore { return store }

		createClaim(nil)
		reconcileOnce()
		Expect(getClaim().Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))

		Expect(store.records).To(HaveLen(1), "one binding, one declaration record")
		record := store.records[0]
		Expect(record.Type).To(Equal(attestation.PredicateDataClass))
		Expect(record.Project).To(Equal(projectName))
		Expect(record.Subject).To(Equal(ClaimIdentityDigest(getClaim())),
			"the subject is the claim's identity digest — a claim has no OCI repository")

		// The envelope verifies under the platform's key and carries the
		// declaration whole: what, who said so, and when.
		envelope := attestation.Envelope{}
		Expect(json.Unmarshal([]byte(record.Envelope), &envelope)).To(Succeed())
		statement, err := envelope.Verify(key)
		Expect(err).NotTo(HaveOccurred(), "the stored envelope must verify under the platform's key")
		Expect(statement.PredicateType).To(Equal(attestation.PredicateDataClass))
		Expect(statement.Describes(record.Subject)).To(BeTrue())
		predicate := map[string]any{}
		Expect(json.Unmarshal(statement.Predicate, &predicate)).To(Succeed())
		Expect(predicate["provenance"]).To(Equal("production"))
		Expect(predicate["provider"]).To(Equal("neon"))
		Expect(predicate["claim"]).To(Equal(claimName))
		Expect(predicate["declaredAt"]).NotTo(BeEmpty())

		// A second reconcile of a bound claim mints nothing new: the record
		// belongs to the binding, not to the loop.
		reconcileOnce()
		Expect(store.records).To(HaveLen(1))
	})

	It("fails the claim with the provider's error, and recovers when the provider does", func() {
		fake.FailWith("the account's project quota is exhausted")
		createClaim(nil)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		ready := readyCondition(claim)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("ProvisionFailed"))
		Expect(ready.Message).To(ContainSubstring("the account's project quota is exhausted"))

		fake.FailWith("")
		reconcileOnce()
		Expect(getClaim().Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
	})

	It("stays pending while the connection has not been validated", func() {
		setCapabilities(connectionName) // the Connection reconciler has not reported yet
		createClaim(nil)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimPending))
		ready := readyCondition(claim)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("ConnectionNotValidated"))
		Expect(ready.Message).To(ContainSubstring("validated"))
		Expect(fake.ProjectNamed("kitchen-"+claimName)).To(BeNil(), "nothing may be provisioned before validation")
	})

	It("fails a claim whose connection cannot provision databases", func() {
		setCapabilities(connectionName, kitchenv1alpha1.CapabilityGitSource)
		createClaim(nil)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		ready := readyCondition(claim)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("CapabilityMissing"))
	})

	It("retains the instance on deletion by default, removing only the binding", func() {
		createClaim(nil)
		reconcileOnce()
		secretName := getClaim().Status.SecretName

		Expect(k8sClient.Delete(ctx, getClaim())).To(Succeed())
		reconcileOnce()

		Expect(errors.IsNotFound(k8sClient.Get(ctx, claimKey, &kitchenv1alpha1.ResourceClaim{}))).To(BeTrue())
		Expect(fake.ProjectNamed("kitchen-"+claimName)).NotTo(BeNil(), "Retain must keep the database")
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: secretName}, &corev1.Secret{})
		Expect(errors.IsNotFound(err)).To(BeTrue(), "the binding secret goes with the claim")
	})

	It("deprovisions on deletion when the policy says Delete", func() {
		createClaim(func(claim *kitchenv1alpha1.ResourceClaim) {
			claim.Spec.DeletionPolicy = kitchenv1alpha1.ClaimDelete
		})
		reconcileOnce()
		Expect(fake.ProjectNamed("kitchen-" + claimName)).NotTo(BeNil())

		Expect(k8sClient.Delete(ctx, getClaim())).To(Succeed())
		reconcileOnce()

		Expect(errors.IsNotFound(k8sClient.Get(ctx, claimKey, &kitchenv1alpha1.ResourceClaim{}))).To(BeTrue())
		Expect(fake.ProjectNamed("kitchen-"+claimName)).To(BeNil(), "Delete must deprovision the database")
	})

	Context("with preview branching", func() {
		envKey := types.NamespacedName{Name: previewEnvName, Namespace: namespace}

		createPreview := func() {
			env := &kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: previewEnvName, Namespace: namespace},
				Spec: kitchenv1alpha1.EnvironmentSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					Type:       kitchenv1alpha1.EnvironmentPreview,
					Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 3, Branch: "feature"},
					ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: projectName + "-rel-1"},
				},
			}
			ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
		}

		branchingClaim := func() {
			createClaim(func(claim *kitchenv1alpha1.ResourceClaim) {
				claim.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"previewBranching": true}`)}
			})
		}

		It("gives each preview its own branch and binding secret", func() {
			createPreview()
			branchingClaim()
			reconcileOnce()

			claim := getClaim()
			Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
			Expect(claim.Status.Branches).To(HaveLen(1))
			entry := claim.Status.Branches[0]
			Expect(entry.Environment).To(Equal(previewEnvName))
			Expect(entry.SecretName).To(Equal(claimName + "-binding-" + previewEnvName))

			branch := fake.BranchNamed("kitchen-"+claimName, previewEnvName)
			Expect(branch).NotTo(BeNil(), "no branch was created at the provider")
			Expect(entry.ID).To(Equal(branch.ID))
			Expect(entry.Provenance).To(Equal("production"),
				"a branch of a production database is production-derived, and the branch record must say so")

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: entry.SecretName}, secret)).To(Succeed())
			Expect(string(secret.Data["host"])).To(Equal(branch.Host()), "the per-environment binding must point at the branch")
			Expect(string(secret.Data["password"])).To(Equal(branch.Password()))

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(env, claimBranchFinalizer)).To(BeTrue(),
				"the claim controller must hold the Environment until its branch is gone")

			branches := meta.FindStatusCondition(claim.Status.Conditions, condBranchesReady)
			Expect(branches).NotTo(BeNil())
			Expect(branches.Status).To(Equal(metav1.ConditionTrue))
		})

		It("tears the branch down with its Environment and releases the finalizer", func() {
			createPreview()
			branchingClaim()
			reconcileOnce()
			secretName := getClaim().Status.Branches[0].SecretName

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			// The finalizer holds it: deletion is pending until the branch is
			// cleaned up.
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.DeletionTimestamp.IsZero()).To(BeFalse())

			reconcileOnce()

			Expect(fake.BranchNamed("kitchen-"+claimName, previewEnvName)).To(BeNil(), "the branch must go with the Environment")
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: secretName}, &corev1.Secret{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "the per-environment binding must go with the Environment")
			Expect(errors.IsNotFound(k8sClient.Get(ctx, envKey, &kitchenv1alpha1.Environment{}))).To(BeTrue(),
				"the Environment must be released once its branch is gone")
			Expect(getClaim().Status.Branches).To(BeEmpty())
		})

		It("cleans up branches when the claim itself is deleted", func() {
			createPreview()
			branchingClaim()
			reconcileOnce()

			Expect(k8sClient.Delete(ctx, getClaim())).To(Succeed())
			reconcileOnce()

			Expect(errors.IsNotFound(k8sClient.Get(ctx, claimKey, &kitchenv1alpha1.ResourceClaim{}))).To(BeTrue())
			// Retain keeps the instance but never the branches: they exist for
			// previews the platform runs, not for the application's data.
			Expect(fake.ProjectNamed("kitchen-" + claimName)).NotTo(BeNil())
			Expect(fake.BranchNamed("kitchen-"+claimName, previewEnvName)).To(BeNil())
			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(env, claimBranchFinalizer)).To(BeFalse(),
				"a deleted claim must not keep holding Environments")
		})

		It("injects the branch binding into the preview's workload", func() {
			kitchen := &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
				Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())
			release := &kitchenv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: projectName + "-rel-1", Namespace: namespace},
				Spec: kitchenv1alpha1.ReleaseSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: projectName + "-bld-1"},
					Image:      "registry.example.com/clshop@sha256:1234",
					ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
						Env: []kitchenv1alpha1.EnvVar{{
							Name:              "DATABASE_URL",
							FromResourceClaim: &kitchenv1alpha1.ResourceClaimKeySelector{Name: claimName, Key: "url"},
						}},
					},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, release))).To(Succeed())
			}()
			createPreview()
			branchingClaim()

			environmentReconciler := &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			reconcileEnv := func() {
				_, err := environmentReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
			}

			// Before the claim has branched for this preview, the environment
			// must wait rather than deploy against the shared database.
			reconcileEnv()
			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			ready := meta.FindStatusCondition(env.Status.Conditions, condReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Reason).To(Equal("ClaimNotBound"))
			Expect(errors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Namespace: appNS, Name: previewEnvName}, &appsv1.Deployment{}))).To(BeTrue())

			reconcileOnce() // the claim binds and branches
			reconcileEnv()

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: previewEnvName}, deploy)).To(Succeed())
			// The platform's own variables lead; the claim's binding is the
			// project's one variable, and it is the last of them.
			podEnv := deploy.Spec.Template.Spec.Containers[0].Env
			Expect(podEnv[0].Name).To(Equal("PORT"))
			Expect(podEnv[len(podEnv)-1].ValueFrom.SecretKeyRef.Name).To(
				Equal(claimName+"-binding-"+previewEnvName),
				"a branching claim's preview must read the branch binding, not the shared one")
		})
	})
})

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/policy"
)

// fakeEvidenceSetReader answers whatever a test configured, standing in for
// the registry read the reconciler materializes evidence from.
type fakeEvidenceSetReader struct {
	set attestation.EvidenceSet
	err error
}

func (f *fakeEvidenceSetReader) Evidence(
	_ context.Context, _ string, _ ...attestation.Verifier,
) (attestation.EvidenceSet, error) {
	if f.err != nil {
		return attestation.EvidenceSet{}, f.err
	}
	return f.set, nil
}

var _ = Describe("Promotion Controller", func() {
	// Everything lives in the platform namespace, because that is where the
	// reconciler resolves the registry connection, the credential and the
	// policy bundles from — the same place production runs it.
	const (
		namespace   = PlatformNamespace
		projectName = "promoshop"
		stagingEnv  = projectName + "-staging"
		prodEnv     = projectName + "-production"
		releaseA    = projectName + "-rel-a"
		digestA     = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		imageA      = "registry.example.com/kitchen/" + projectName + "@" + digestA
	)

	ctx := context.Background()

	var (
		reconciler *PromotionReconciler
		decisions  *fakeDecisionStore
		registry   *stubAttester
		evidence   *fakeEvidenceSetReader
	)

	key := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: namespace}
	}

	reconcileOnce := func(name string) {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	newPromotion := func(name, envName string) *kitchenv1alpha1.Promotion {
		return &kitchenv1alpha1.Promotion{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.PromotionSpec{
				ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: projectName},
				EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: envName},
				ReleaseRef:     kitchenv1alpha1.LocalObjectReference{Name: releaseA},
				RequestedBy:    "grace@example.com",
				Trigger:        kitchenv1alpha1.PromotionManual,
			},
		}
	}

	environmentWith := func(name string, requirements *kitchenv1alpha1.EnvironmentRequirements) *kitchenv1alpha1.Environment {
		return &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef:   kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:         kitchenv1alpha1.EnvironmentProduction,
				ReleaseRef:   kitchenv1alpha1.LocalObjectReference{Name: projectName + "-rel-old"},
				Requirements: requirements,
			},
		}
	}

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())

		// The platform: a singleton with a decision store and attestation on,
		// the signing key, and the registry the project pushes to.
		_, privatePEM, publicPEM, err := attestation.GenerateECDSAKey()
		Expect(err).NotTo(HaveOccurred())
		signingKey := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: SigningKeySecretName, Namespace: namespace},
			Data: map[string][]byte{
				attestation.SecretKeyPrivate: privatePEM,
				attestation.SecretKeyPublic:  publicPEM,
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, signingKey))).To(Succeed())
		store := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "promoshop-clickhouse", Namespace: namespace},
			Data: map[string][]byte{
				"host": []byte("clickhouse"), "httpPort": []byte("8123"),
				"database": []byte("kitchen"), "username": []byte("kitchen"), "password": []byte("s"),
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, store))).To(Succeed())
		credentials := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "promoshop-registry-creds", Namespace: namespace},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{
				corev1.DockerConfigJsonKey: []byte(`{"auths":{"registry.example.com":{"username":"robot"}}}`),
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, credentials))).To(Succeed())
		connection := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "promoshop-registry", Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "dockerRegistry",
				CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "promoshop-registry-creds"},
				Config:               &runtime.RawExtension{Raw: []byte(`{"url":"registry.example.com/kitchen"}`)},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, connection))).To(Succeed())
		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Observability: kitchenv1alpha1.ObservabilitySpec{
					ClickHouse: kitchenv1alpha1.ClickHouseSpec{
						SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: "promoshop-clickhouse"},
					},
				},
				Compliance: kitchenv1alpha1.ComplianceSpec{
					Attestation: kitchenv1alpha1.AttestationSpec{Enabled: true},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, kitchen))).To(Succeed())

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/promoshop",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "promoshop-registry"},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		build := &kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{Name: projectName + "-bld-a", Namespace: namespace},
			Spec: kitchenv1alpha1.BuildSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Git:        kitchenv1alpha1.GitRevision{SHA: "aaaa111122223333", Branch: "main"},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, build))).To(Succeed())
		Expect(k8sClient.Get(ctx, key(projectName+"-bld-a"), build)).To(Succeed())
		build.Status.Artifact = &kitchenv1alpha1.ArtifactStatus{
			Repository: "registry.example.com/kitchen/" + projectName,
			Digest:     digestA,
		}
		Expect(k8sClient.Status().Update(ctx, build)).To(Succeed())

		release := &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: releaseA, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: projectName + "-bld-a"},
				Image:      imageA,
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())

		decisions = &fakeDecisionStore{}
		registry = &stubAttester{}
		evidence = &fakeEvidenceSetReader{}
		reconciler = &PromotionReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Stores:          func(clickhouse.Config) DecisionStore { return decisions },
			Attesters:       func([]byte, string) (ArtifactAttester, error) { return registry, nil },
			EvidenceReaders: func([]byte, string) (EvidenceSetReader, error) { return evidence, nil },
		}
	})

	AfterEach(func() {
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Promotion{ObjectMeta: metav1.ObjectMeta{Name: "promo-allowed", Namespace: namespace}},
			&kitchenv1alpha1.Promotion{ObjectMeta: metav1.ObjectMeta{Name: "promo-blocked", Namespace: namespace}},
			&kitchenv1alpha1.Promotion{ObjectMeta: metav1.ObjectMeta{Name: "promo-mismatch", Namespace: namespace}},
			&kitchenv1alpha1.Promotion{ObjectMeta: metav1.ObjectMeta{Name: "promo-staging", Namespace: namespace}},
			&kitchenv1alpha1.Promotion{ObjectMeta: metav1.ObjectMeta{
				Name: automaticPromotionName(projectName, releaseA, prodEnv), Namespace: namespace}},
			&kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: stagingEnv, Namespace: namespace}},
			&kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: prodEnv, Namespace: namespace}},
			&kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: "stranger-env", Namespace: namespace}},
			&kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: releaseA, Namespace: namespace}},
			&kitchenv1alpha1.Build{ObjectMeta: metav1.ObjectMeta{Name: projectName + "-bld-a", Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "promoshop-registry", Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "promoshop-registry-creds", Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "promoshop-clickhouse", Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SigningKeySecretName, Namespace: namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		// The kitchen-system namespace stays: envtest namespaces never finish
		// terminating and would poison later specs.
	})

	It("applies an allowed promotion: the environment moves, the history and the decision say so", func() {
		Expect(k8sClient.Create(ctx, environmentWith(prodEnv, nil))).To(Succeed())
		Expect(k8sClient.Create(ctx, newPromotion("promo-allowed", prodEnv))).To(Succeed())

		reconcileOnce("promo-allowed")

		promotion := &kitchenv1alpha1.Promotion{}
		Expect(k8sClient.Get(ctx, key("promo-allowed"), promotion)).To(Succeed())
		Expect(promotion.Status.Phase).To(Equal(kitchenv1alpha1.PromotionApplied))
		Expect(promotion.Status.Verdict).To(Equal(policy.VerdictAllowed))
		Expect(promotion.Status.DecisionID).NotTo(BeEmpty())
		Expect(promotion.Status.AppliedAt).NotTo(BeNil())
		// No requirements means nothing was evaluated, and the status says so
		// rather than implying a bar was cleared.
		Expect(promotion.Status.Message).To(ContainSubstring("declares no requirements"))

		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, key(prodEnv), env)).To(Succeed())
		Expect(env.Spec.ReleaseRef.Name).To(Equal(releaseA))
		Expect(env.Status.History).To(HaveLen(1))
		Expect(env.Status.History[0].Release).To(Equal(projectName + "-rel-old"))
		Expect(env.Status.History[0].Reason).To(Equal(kitchenv1alpha1.ReleaseMovePromoted))
		Expect(env.Status.History[0].By).To(Equal("promo-allowed"), "the promotion is the mover")

		// Every promotion has a stored decision, this one included.
		Expect(decisions.decisions).To(HaveLen(1))
		Expect(decisions.decisions[0].ID).To(Equal(promotion.Status.DecisionID))
		Expect(decisions.decisions[0].Verdict).To(Equal(policy.VerdictAllowed))

		// The artifact carries both records: the decision, and the deployment.
		Expect(registry.predicates).To(ContainElement(attestation.PredicatePromotionDecision))
		Expect(registry.predicates).To(ContainElement(attestation.PredicateDeployment))
	})

	It("blocks a promotion whose artifact lacks the required evidence, naming the rules", func() {
		bundle := policy.DefaultBundle()
		Expect(k8sClient.Create(ctx, environmentWith(prodEnv, &kitchenv1alpha1.EnvironmentRequirements{
			BundleDigest: policy.Digest(bundle),
			Parameters:   map[string]string{"require-provenance": "true"},
		}))).To(Succeed())
		// The registry answers: the artifact carries nothing.
		evidence.set = attestation.EvidenceSet{Attestations: []attestation.Evidence{}}
		Expect(k8sClient.Create(ctx, newPromotion("promo-blocked", prodEnv))).To(Succeed())

		release := &kitchenv1alpha1.Release{}
		Expect(k8sClient.Get(ctx, key(releaseA), release)).To(Succeed())
		untouched := release.ResourceVersion

		reconcileOnce("promo-blocked")

		promotion := &kitchenv1alpha1.Promotion{}
		Expect(k8sClient.Get(ctx, key("promo-blocked"), promotion)).To(Succeed())
		Expect(promotion.Status.Phase).To(Equal(kitchenv1alpha1.PromotionBlocked))
		Expect(promotion.Status.Verdict).To(Equal(policy.VerdictBlocked))
		Expect(promotion.Status.UnmetRules).To(Equal([]string{"require-provenance"}))
		Expect(promotion.Status.Message).To(ContainSubstring("require-provenance"))
		Expect(promotion.Status.DecisionID).NotTo(BeEmpty(), "a refusal is a decision too")

		// The environment was never touched, and neither was the release: a
		// promotion moves a pointer or nothing — the artifact is pinned by
		// digest across every stage and is never rebuilt.
		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, key(prodEnv), env)).To(Succeed())
		Expect(env.Spec.ReleaseRef.Name).To(Equal(projectName + "-rel-old"))
		Expect(k8sClient.Get(ctx, key(releaseA), release)).To(Succeed())
		Expect(release.ResourceVersion).To(Equal(untouched))
		Expect(release.Spec.Image).To(Equal(imageA))

		// Blocked is terminal for this object: a second reconcile changes
		// nothing and decides nothing new.
		reconcileOnce("promo-blocked")
		Expect(decisions.decisions).To(HaveLen(1))
	})

	It("fails a promotion whose references do not tell one story", func() {
		stranger := environmentWith("stranger-env", nil)
		stranger.Spec.ProjectRef = kitchenv1alpha1.LocalObjectReference{Name: "somebody-else"}
		Expect(k8sClient.Create(ctx, stranger)).To(Succeed())
		Expect(k8sClient.Create(ctx, newPromotion("promo-mismatch", "stranger-env"))).To(Succeed())

		reconcileOnce("promo-mismatch")

		promotion := &kitchenv1alpha1.Promotion{}
		Expect(k8sClient.Get(ctx, key("promo-mismatch"), promotion)).To(Succeed())
		Expect(promotion.Status.Phase).To(Equal(kitchenv1alpha1.PromotionFailed))
		Expect(promotion.Status.Message).To(ContainSubstring("belongs to project"))
		Expect(decisions.decisions).To(BeEmpty(), "nothing judged the artifact")

		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, key("stranger-env"), env)).To(Succeed())
		Expect(env.Spec.ReleaseRef.Name).To(Equal(projectName + "-rel-old"))
	})

	It("chains the next stage's promotion when it auto-promotes", func() {
		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, key(projectName), project)).To(Succeed())
		project.Spec.Promotion = &kitchenv1alpha1.PromotionPolicySpec{Stages: []kitchenv1alpha1.PromotionStage{
			{Name: "staging", Environment: stagingEnv},
			{Name: "production", Environment: prodEnv, AutoPromote: true},
		}}
		Expect(k8sClient.Update(ctx, project)).To(Succeed())

		Expect(k8sClient.Create(ctx, environmentWith(stagingEnv, nil))).To(Succeed())
		Expect(k8sClient.Create(ctx, environmentWith(prodEnv, nil))).To(Succeed())
		Expect(k8sClient.Create(ctx, newPromotion("promo-staging", stagingEnv))).To(Succeed())

		reconcileOnce("promo-staging")

		// Stage one applied, and the platform asked for stage two by itself.
		next := &kitchenv1alpha1.Promotion{}
		Expect(k8sClient.Get(ctx, key(automaticPromotionName(projectName, releaseA, prodEnv)), next)).To(Succeed())
		Expect(next.Spec.Trigger).To(Equal(kitchenv1alpha1.PromotionAutomatic))
		Expect(next.Spec.ReleaseRef.Name).To(Equal(releaseA), "the same release — never rebuilt between stages")
		Expect(next.Spec.EnvironmentRef.Name).To(Equal(prodEnv))
		Expect(next.Spec.RequestedBy).To(Equal("system:controller/promotion"))

		// And applying it lands the identical digest on production: the image
		// deployed at the last stage is byte-identical to the one built once.
		reconcileOnce(next.Name)
		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, key(prodEnv), env)).To(Succeed())
		Expect(env.Spec.ReleaseRef.Name).To(Equal(releaseA))
		release := &kitchenv1alpha1.Release{}
		Expect(k8sClient.Get(ctx, key(releaseA), release)).To(Succeed())
		Expect(release.Spec.Image).To(Equal(imageA))
	})

	It("refuses to edit a promotion's spec, like a release's", func() {
		Expect(k8sClient.Create(ctx, environmentWith(prodEnv, nil))).To(Succeed())
		Expect(k8sClient.Create(ctx, newPromotion("promo-allowed", prodEnv))).To(Succeed())

		promotion := &kitchenv1alpha1.Promotion{}
		Expect(k8sClient.Get(ctx, key("promo-allowed"), promotion)).To(Succeed())
		promotion.Spec.Reason = "edited after the fact"
		err := k8sClient.Update(ctx, promotion)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Promotion spec is immutable"))
	})
})

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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/policy"
)

// Recording a decision is three writes in a fixed order — the audit record,
// the store, the artifact — and these tests pin what lands in each, with the
// same stub seams every other attestation test uses.

// fakeDecisionStore records what Record stored.
type fakeDecisionStore struct {
	decisions []clickhouse.Decision
	bundles   map[string]string
}

func (f *fakeDecisionStore) InsertDecision(_ context.Context, decision clickhouse.Decision) error {
	f.decisions = append(f.decisions, decision)
	return nil
}

func (f *fakeDecisionStore) InsertPolicyBundle(_ context.Context, digest, content string) error {
	if f.bundles == nil {
		f.bundles = map[string]string{}
	}
	f.bundles[digest] = content
	return nil
}

// decisionFixtures is a platform that can do everything Record wants: a store
// secret to resolve, a signing key, and a project with a registry connection.
func decisionFixtures(t *testing.T) (*DecisionRecorder, *fakeDecisionStore, *stubAttester, *kitchenv1alpha1.Kitchen) {
	t.Helper()

	_, privatePEM, publicPEM, err := attestation.GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	key := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SigningKeySecretName, Namespace: PlatformNamespace},
		Data: map[string][]byte{
			attestation.SecretKeyPrivate: privatePEM,
			attestation.SecretKeyPublic:  publicPEM,
		},
	}
	store := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kitchen-clickhouse", Namespace: PlatformNamespace},
		Data: map[string][]byte{
			"host": []byte("clickhouse"), "httpPort": []byte("8123"),
			"database": []byte("kitchen"), "username": []byte("kitchen"), "password": []byte("s"),
		},
	}
	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-credentials", Namespace: PlatformNamespace},
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"registry.example.com":{"username":"robot"}}}`),
		},
	}
	connection := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "dockerRegistry",
			CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "registry-credentials"},
			Config:               &runtime.RawExtension{Raw: []byte(`{"url":"registry.example.com/kitchen"}`)},
		},
	}
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source:   kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{Repo: "acme/shop", ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "github"}}},
			Registry: &kitchenv1alpha1.RegistrySpec{ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"}},
		},
	}
	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			BaseDomain: "apps.example.com",
			Observability: kitchenv1alpha1.ObservabilitySpec{
				ClickHouse: kitchenv1alpha1.ClickHouseSpec{
					SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: "kitchen-clickhouse"},
				},
			},
			Compliance: kitchenv1alpha1.ComplianceSpec{
				Attestation: kitchenv1alpha1.AttestationSpec{Enabled: true},
			},
		},
	}

	c := complianceClient(t, key, store, credentials, connection, project, kitchen)
	decisions := &fakeDecisionStore{}
	registry := &stubAttester{}
	recorder := &DecisionRecorder{
		Client:    c,
		Stores:    func(clickhouse.Config) DecisionStore { return decisions },
		Attesters: func([]byte, string) (ArtifactAttester, error) { return registry, nil },
	}
	return recorder, decisions, registry, kitchen
}

func promotionInput() policy.Input {
	return policy.Input{
		Kind:        policy.KindPromotion,
		At:          time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Parameters:  map[string]string{"require-sbom": "true"},
		Project:     policy.ProjectFacts{Name: "shop"},
		Environment: policy.EnvironmentFacts{Name: "shop-production", Type: "production"},
		Release: policy.ReleaseFacts{
			Name:   "shop-rel-7",
			Image:  "registry.example.com/kitchen/shop@sha256:" + strings.Repeat("a", 64),
			Digest: "sha256:" + strings.Repeat("a", 64),
		},
		DataSnapshot: "trivy-db-2026-08-20",
	}
}

func TestRecordStoresTheDecisionAndAttestsAPromotion(t *testing.T) {
	recorder, decisions, registry, kitchen := decisionFixtures(t)
	bundle := policy.DefaultBundle()
	input := promotionInput()
	result := policy.Result{
		Verdict: policy.VerdictBlocked,
		Fired:   []policy.FiredRule{{Rule: "require-sbom", Message: "no SBOM"}},
	}

	about := &kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: "shop-rel-7", Namespace: PlatformNamespace}}
	decisionID, err := recorder.Record(context.Background(), kitchen, about, input, result, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if decisionID == "" {
		t.Fatal("a recorded decision has an id")
	}

	// The store holds the decision with both digests, the whole input, and
	// the bundle bytes those digests can be replayed from.
	if len(decisions.decisions) != 1 {
		t.Fatalf("want one stored decision, got %d", len(decisions.decisions))
	}
	stored := decisions.decisions[0]
	wantInputDigest, err := input.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != decisionID || stored.Kind != policy.KindPromotion || stored.Verdict != policy.VerdictBlocked {
		t.Fatalf("stored %+v", stored)
	}
	if stored.BundleDigest != policy.Digest(bundle) || stored.InputDigest != wantInputDigest {
		t.Fatalf("the stored digests must be the real ones, got %+v", stored)
	}
	canonical, err := input.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Input != string(canonical) {
		t.Fatalf("the stored input must be the canonical bytes, got %s", stored.Input)
	}
	if stored.DataSnapshot != "trivy-db-2026-08-20" || stored.DecidedBy != "system:controller/policy" {
		t.Fatalf("stored %+v", stored)
	}
	persisted, held := decisions.bundles[stored.BundleDigest]
	if !held {
		t.Fatal("the bundle must be persisted under its digest for replay")
	}
	roundTrip := policy.Bundle{}
	if err := json.Unmarshal([]byte(persisted), &roundTrip); err != nil {
		t.Fatal(err)
	}
	if policy.Digest(roundTrip) != stored.BundleDigest {
		t.Fatal("the persisted bundle must digest back to what the decision cites")
	}

	// The artifact carries the decision: the verdict finally lives in a
	// predicate, this one, with the reproduction pointers and never the full
	// input.
	if len(registry.attached) != 1 || registry.predicate != attestation.PredicatePromotionDecision {
		t.Fatalf("want one promotion-decision attestation, got %d under %q",
			len(registry.attached), registry.predicate)
	}
	statement, err := registry.attached[0].Statement()
	if err != nil {
		t.Fatal(err)
	}
	if !statement.Describes("sha256:" + strings.Repeat("a", 64)) {
		t.Fatal("the attestation is not about the artifact")
	}
	predicate := map[string]any{}
	if err := json.Unmarshal(statement.Predicate, &predicate); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"decisionID":   decisionID,
		"verdict":      policy.VerdictBlocked,
		"bundleDigest": policy.Digest(bundle),
		"inputDigest":  wantInputDigest,
		"environment":  "shop-production",
		"dataSnapshot": "trivy-db-2026-08-20",
		"evaluatedAt":  "2026-08-20T12:00:00Z",
	} {
		if predicate[field] != want {
			t.Errorf("predicate %s = %v, want %v", field, predicate[field], want)
		}
	}
	if _, present := predicate["input"]; present {
		t.Error("the predicate must not carry the full input; the store holds it")
	}
	rules, ok := predicate["rulesFired"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("the predicate names what fired, got %v", predicate["rulesFired"])
	}
}

func TestAPromotionsDecisionIDIsStableAcrossARequeue(t *testing.T) {
	// A status update failing after the decision was stored re-enters Record
	// with a fresh evaluation — a later At, so a different input digest — and
	// the retry must not mint a second identity for the one decision this
	// promotion gets: the id derives from the promotion's UID, and the
	// store's insert recognises its own earlier row by it.
	recorder, decisions, _, kitchen := decisionFixtures(t)
	about := &kitchenv1alpha1.Promotion{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-promo-1", Namespace: PlatformNamespace, UID: "11111111-aaaa-bbbb-cccc-222222222222",
	}}
	result := policy.Result{Verdict: policy.VerdictAllowed, Fired: []policy.FiredRule{}}

	first, err := recorder.Record(context.Background(), kitchen, about, promotionInput(), result, policy.DefaultBundle())
	if err != nil {
		t.Fatal(err)
	}
	requeued := promotionInput()
	requeued.At = requeued.At.Add(time.Minute)
	second, err := recorder.Record(context.Background(), kitchen, about, requeued, result, policy.DefaultBundle())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("one promotion, one decision id: got %q then %q", first, second)
	}
	if len(decisions.decisions) != 2 || decisions.decisions[0].ID != decisions.decisions[1].ID {
		t.Fatalf("both store attempts must carry the one id — the store's insert dedupes on it — got %+v",
			decisions.decisions)
	}

	// Another promotion decides under an id of its own.
	other := &kitchenv1alpha1.Promotion{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-promo-2", Namespace: PlatformNamespace, UID: "33333333-aaaa-bbbb-cccc-444444444444",
	}}
	third, err := recorder.Record(context.Background(), kitchen, other, promotionInput(), result, policy.DefaultBundle())
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatalf("two promotions must not share a decision id, both got %q", first)
	}

	// A rescan is a fresh question each time it is asked and keeps random ids.
	rescan := promotionInput()
	rescan.Kind = policy.KindRescan
	fourth, err := recorder.Record(context.Background(), kitchen, about, rescan, result, policy.DefaultBundle())
	if err != nil {
		t.Fatal(err)
	}
	fifth, err := recorder.Record(context.Background(), kitchen, about, rescan, result, policy.DefaultBundle())
	if err != nil {
		t.Fatal(err)
	}
	if fourth == fifth {
		t.Fatalf("a rescan re-asked is a new decision, both got %q", fourth)
	}
}

func TestARescanDecisionIsStoredAndNotAttested(t *testing.T) {
	recorder, decisions, registry, kitchen := decisionFixtures(t)
	input := promotionInput()
	input.Kind = policy.KindRescan

	about := &kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: "shop-rel-7", Namespace: PlatformNamespace}}
	if _, err := recorder.Record(context.Background(), kitchen, about, input,
		policy.Result{Verdict: policy.VerdictAllowed, Fired: []policy.FiredRule{}}, policy.DefaultBundle()); err != nil {
		t.Fatal(err)
	}
	if len(decisions.decisions) != 1 {
		t.Fatalf("a rescan is stored, got %d", len(decisions.decisions))
	}
	if len(registry.attached) != 0 {
		t.Fatalf("a rescan asserts nothing new about the artifact, got %d attachments", len(registry.attached))
	}
}

func TestADecisionWithoutAStoreStillStands(t *testing.T) {
	recorder, decisions, registry, kitchen := decisionFixtures(t)
	// The installation has no telemetry store: the decision is still made,
	// still attested for a promotion, and the platform's compliance status is
	// what owns up to it being unreplayable.
	kitchen.Spec.Observability.ClickHouse.SecretRef = nil

	about := &kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: "shop-rel-7", Namespace: PlatformNamespace}}
	decisionID, err := recorder.Record(context.Background(), kitchen, about, promotionInput(),
		policy.Result{Verdict: policy.VerdictAllowed, Fired: []policy.FiredRule{}}, policy.DefaultBundle())
	if err != nil {
		t.Fatal(err)
	}
	if decisionID == "" {
		t.Fatal("the decision still has an id")
	}
	if len(decisions.decisions) != 0 {
		t.Fatalf("nothing can be stored without a store, got %+v", decisions.decisions)
	}
	if len(registry.attached) != 1 {
		t.Fatalf("the promotion is still attested, got %d attachments", len(registry.attached))
	}
}

// The policy block on Kitchen.status.compliance mirrors the audit posture:
// the engine always evaluates, and this is where an installation is told its
// decisions are not being kept.
func TestReconcilePolicyStoreReportsThePosture(t *testing.T) {
	// No store at all: not storing, and the message says what that costs.
	kitchen := &kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}}
	r := &KitchenReconciler{Client: complianceClient(t, kitchen)}
	status := r.reconcilePolicyStore(context.Background(), kitchen)
	if status.Storing || !strings.Contains(status.Message, "evaluated but not stored") {
		t.Fatalf("no store must read as not storing, loudly: %+v", status)
	}

	// A store that answers: the schema is ensured and decisions are kept.
	answering := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer answering.Close()
	endpoint, err := url.Parse(answering.URL)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kitchen-clickhouse", Namespace: PlatformNamespace},
		Data: map[string][]byte{
			"host": []byte(endpoint.Hostname()), "httpPort": []byte(endpoint.Port()),
			"database": []byte("kitchen"), "username": []byte("kitchen"), "password": []byte("s"),
		},
	}
	kitchen.Spec.Observability.ClickHouse.SecretRef = &kitchenv1alpha1.LocalObjectReference{Name: "kitchen-clickhouse"}
	r = &KitchenReconciler{Client: complianceClient(t, kitchen, secret)}
	status = r.reconcilePolicyStore(context.Background(), kitchen)
	if !status.Storing || status.Message != "" {
		t.Fatalf("an answering store must read as storing: %+v", status)
	}
}

// The transition is built apart from the recording so it can be held up to
// the light without a store — the same split every reconciler's records use.
func TestTheDecisionTransitionCarriesTheReproductionContract(t *testing.T) {
	input := promotionInput()
	result := policy.Result{
		Verdict: policy.VerdictAllowedWithException,
		Fired: []policy.FiredRule{
			{Rule: "require-sbom", Message: "no SBOM", Waived: true, Exception: "incident-441"},
		},
	}
	about := &kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: "shop-rel-7", Namespace: PlatformNamespace}}

	transition := decisionTransition(about, input, result, "decision-1",
		"sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64))
	if transition.Kind != "PromotionDecision" || transition.Controller != actorPolicyEngine {
		t.Fatalf("the record is the policy engine's, got %+v", transition)
	}
	if transition.Correlation != "decision-1" {
		t.Fatalf("the correlation is the way back to the stored row, got %q", transition.Correlation)
	}
	if transition.To != policy.VerdictAllowedWithException || transition.Project != "shop" {
		t.Fatalf("the transition carries the verdict and the project, got %+v", transition)
	}
	for field, want := range map[string]any{
		"decisionID":   "decision-1",
		"kind":         policy.KindPromotion,
		"verdict":      policy.VerdictAllowedWithException,
		"bundleDigest": "sha256:" + strings.Repeat("b", 64),
		"inputDigest":  "sha256:" + strings.Repeat("c", 64),
		"environment":  "shop-production",
		"release":      "shop-rel-7",
	} {
		if transition.Details[field] != want {
			t.Errorf("details %s = %v, want %v", field, transition.Details[field], want)
		}
	}
	waived, ok := transition.Details["waivedRules"].([]string)
	if !ok || len(waived) != 1 || waived[0] != "require-sbom" {
		t.Errorf("a waived rule is recorded as waived, got %v", transition.Details["waivedRules"])
	}
	if _, present := transition.Details["unmetRules"]; present {
		t.Errorf("nothing stands unmet here, got %v", transition.Details["unmetRules"])
	}
}

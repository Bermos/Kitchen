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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/policy"
	"github.com/Bermos/Kitchen/internal/provider"
)

// Recording a policy decision: the one sequence every reconciler-made verdict
// goes through, in the one order that keeps it honest.
//
//  1. The audit record, fail-closed, before anything else — a decision the
//     log could not record is a decision the platform does not act on, which
//     is the same contract every API write keeps.
//  2. The decision row and the bundle it cites, into the store. Best-effort
//     by design: the engine evaluated with the bundle and the input in its
//     hands, so a missing store does not un-make the decision — it makes the
//     platform say loudly (Kitchen.status.compliance.policy) that decisions
//     are not being stored.
//  3. For a promotion, the PromotionDecision attestation on the artifact:
//     the verdict finally lives somewhere, and it lives here rather than in
//     any gate's predicate — gates record facts, this records the decision.
//
// The promotion reconciler (#133) and the rescan sweep (#134) both call
// Record; the API's replay handler keeps the same order on its own side with
// s.recorded, because its audit actor is the caller rather than a controller.

// DecisionStore is the slice of the store a decision lands in. It is an
// interface so the recorder can be tested without a ClickHouse.
type DecisionStore interface {
	InsertDecision(ctx context.Context, decision clickhouse.Decision) error
	InsertPolicyBundle(ctx context.Context, digest, content string) error
}

// DecisionStoreFactory builds the store from the platform's own ClickHouse
// configuration. Nil means the real client.
type DecisionStoreFactory func(cfg clickhouse.Config) DecisionStore

func defaultDecisionStore(cfg clickhouse.Config) DecisionStore {
	return clickhouse.New(cfg)
}

// DecisionRecorder carries a policy verdict into the record: audit log,
// decision store, and — for promotions — the artifact itself.
type DecisionRecorder struct {
	Client client.Client
	// Audit is waited on and fail-closed; may be nil, which records nothing
	// and refuses nothing, exactly like every other reconciler's.
	Audit *audit.Recorder
	// Stores builds the decision store; nil resolves the real one from the
	// singleton's ClickHouse secret. Tests inject.
	Stores DecisionStoreFactory
	// Attesters builds the registry attester the promotion-decision
	// attestation is attached through; nil talks to the real registry with
	// the project's own push credential. Tests inject.
	Attesters AttesterFactory
}

// Record writes one decision everywhere it belongs and answers its id.
//
// `about` is the object the decision is about — the Promotion being applied,
// the Release being rescanned — and is what the audit record names. A non-nil
// error means the audit log refused the record, and the caller MUST NOT act
// on the verdict: return the error and requeue, like every other refused
// transition.
func (r *DecisionRecorder) Record(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	about client.Object,
	input policy.Input,
	result policy.Result,
	bundle policy.Bundle,
) (string, error) {
	decisionID := decisionIDFor(about, input.Kind)

	canonical, err := input.Canonical()
	if err != nil {
		return "", fmt.Errorf("the policy input could not be encoded: %w", err)
	}
	inputDigest, err := input.Digest()
	if err != nil {
		return "", err
	}
	bundleDigest := policy.Digest(bundle)
	rulesFired, err := json.Marshal(result.Fired)
	if err != nil {
		return "", fmt.Errorf("the fired rules could not be encoded: %w", err)
	}

	// The audit record gates everything: no record, no decision.
	if err := r.Audit.Record(ctx, decisionTransition(about, input, result, decisionID, bundleDigest, inputDigest)); err != nil {
		return "", err
	}

	log := logf.FromContext(ctx)

	// The store, best-effort and loud. Kitchen.status.compliance.policy is
	// what owns up to an installation where this branch never runs.
	if store := r.store(ctx, kitchen); store != nil {
		bundleContent, err := json.Marshal(map[string]string(bundle))
		if err == nil {
			if err := store.InsertPolicyBundle(ctx, bundleDigest, string(bundleContent)); err != nil {
				log.Error(err, "the policy bundle could not be persisted for replay", "bundleDigest", bundleDigest)
			}
		}
		if err := store.InsertDecision(ctx, clickhouse.Decision{
			ID:           decisionID,
			Timestamp:    input.At,
			Kind:         input.Kind,
			Project:      input.Project.Name,
			Environment:  input.Environment.Name,
			Release:      input.Release.Name,
			Artifact:     input.Release.Image,
			BundleDigest: bundleDigest,
			InputDigest:  inputDigest,
			DataSnapshot: input.DataSnapshot,
			Verdict:      result.Verdict,
			RulesFired:   string(rulesFired),
			Input:        string(canonical),
			DecidedBy:    audit.ControllerActor(actorPolicyEngine),
		}); err != nil {
			log.Error(err, "the decision could not be stored; it stands, unreplayable",
				"decision", decisionID, "verdict", result.Verdict)
		}
	} else {
		log.Info("no decision store: the decision stands, unreplayable",
			"decision", decisionID, "verdict", result.Verdict)
	}

	// The attestation, for promotions alone: a rescan re-attests nothing (its
	// scan evidence is already on the artifact and its decision is in the
	// store), and a replay asserts nothing new about the artifact at all.
	if input.Kind == policy.KindPromotion {
		if err := r.attestDecision(ctx, kitchen, input, result, decisionID, bundleDigest, inputDigest); err != nil {
			// Logged, not fatal: the decision is recorded and audited, and
			// attach is content-idempotent so nothing is lost by moving on —
			// while returning an error here would re-run the whole recording
			// and duplicate the audit trail.
			log.Error(err, "the promotion decision could not be attested on the artifact",
				"decision", decisionID, "artifact", input.Release.Image)
		}
	}
	return decisionID, nil
}

// decisionIDFor mints a decision's id. A promotion is decided exactly once —
// its spec is immutable and its phases terminal — so its decision id is a
// deterministic UUID derived from the promotion's UID: a requeue that
// re-records the decision (a status update failed after storage) produces
// the same id, and the store's insert recognises its own earlier row instead
// of keeping two. Every other kind is a fresh question each time it is asked
// — a rescan of the same release next week is a new decision — and gets a
// random id, as does anything without a UID to derive from.
func decisionIDFor(about client.Object, kind string) string {
	if kind != policy.KindPromotion || about == nil || about.GetUID() == "" {
		return string(uuid.NewUUID())
	}
	sum := sha256.Sum256([]byte("kitchen-promotion-decision\x00" + string(about.GetUID())))
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x50 // version 5: name-based
	id[8] = (id[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}

// decisionTransition is the audit record a decision appends before anything
// acts on it. Built apart from the recording so a test can hold it up to the
// light without a store: the correlation is the decision id (the way back to
// the stored row), and the details carry the two digests that make the
// verdict reproducible.
func decisionTransition(
	about client.Object,
	input policy.Input,
	result policy.Result,
	decisionID, bundleDigest, inputDigest string,
) audit.Transition {
	unmet := make([]string, 0, len(result.Fired))
	waived := make([]string, 0, len(result.Fired))
	for _, rule := range result.Fired {
		if rule.Waived {
			waived = append(waived, rule.Rule)
			continue
		}
		unmet = append(unmet, rule.Rule)
	}
	details := map[string]any{
		"decisionID":   decisionID,
		"kind":         input.Kind,
		"verdict":      result.Verdict,
		"bundleDigest": bundleDigest,
		"inputDigest":  inputDigest,
		"environment":  input.Environment.Name,
		"release":      input.Release.Name,
	}
	if len(unmet) > 0 {
		details["unmetRules"] = unmet
	}
	if len(waived) > 0 {
		details["waivedRules"] = waived
	}
	if input.DataSnapshot != "" {
		details["dataSnapshot"] = input.DataSnapshot
	}
	return audit.Transition{
		Object:      about,
		Kind:        audit.KindPromotionDecision,
		Controller:  actorPolicyEngine,
		Correlation: decisionID,
		To:          result.Verdict,
		Project:     input.Project.Name,
		Reason: fmt.Sprintf("policy %s of release %s for environment %s: %s",
			input.Kind, input.Release.Name, input.Environment.Name, result.Verdict),
		Details: details,
	}
}

// store resolves where decisions are kept, the way every store user does: off
// the singleton's ClickHouse secret. Nil means there is nowhere, which the
// caller treats as loud-but-not-fatal.
func (r *DecisionRecorder) store(ctx context.Context, kitchen *kitchenv1alpha1.Kitchen) DecisionStore {
	if kitchen == nil {
		return nil
	}
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		return nil
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}
	if err := r.Client.Get(ctx, key, secret); err != nil {
		logf.FromContext(ctx).Error(err, "the decision store's secret could not be read")
		return nil
	}
	cfg, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		logf.FromContext(ctx).Error(err, "the decision store's secret is not a store configuration")
		return nil
	}
	factory := r.Stores
	if factory == nil {
		factory = defaultDecisionStore
	}
	return factory(cfg)
}

// attestDecision mints the PromotionDecision attestation on the artifact: the
// decision id, the verdict, and the digests that reproduce it — never the
// full input, which the store holds. This predicate is where a verdict
// finally lives; the gate and source predicates stay verdict-free by test.
func (r *DecisionRecorder) attestDecision(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	input policy.Input,
	result policy.Result,
	decisionID, bundleDigest, inputDigest string,
) error {
	repository, digest, byDigest := strings.Cut(input.Release.Image, "@")
	if !byDigest || digest == "" {
		return fmt.Errorf("the release's image %q names no digest, so there is nothing to attach to",
			input.Release.Image)
	}

	signer, err := SigningKeyFor(ctx, r.Client, kitchen)
	if err != nil {
		return err
	}
	if signer == nil {
		// Not signing is a state, not a failure; the platform status says so.
		return nil
	}

	attester, err := r.registryAttester(ctx, input.Project.Name)
	if err != nil {
		return err
	}
	statement, err := attestation.NewStatement(
		repository, digest, attestation.PredicatePromotionDecision,
		decisionPredicate(input, result, decisionID, bundleDigest, inputDigest))
	if err != nil {
		return err
	}
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		return err
	}
	_, err = attester.Attach(ctx, repository+"@"+digest, envelope, statement.PredicateType)
	return err
}

// decisionPredicate is the promotion-decision predicate: everything needed to
// find and replay the decision, and nothing the store already holds whole.
func decisionPredicate(
	input policy.Input,
	result policy.Result,
	decisionID, bundleDigest, inputDigest string,
) map[string]any {
	rules := make([]map[string]any, 0, len(result.Fired))
	for _, rule := range result.Fired {
		fired := map[string]any{"rule": rule.Rule, "message": rule.Message, "waived": rule.Waived}
		if rule.Exception != "" {
			fired["exception"] = rule.Exception
		}
		rules = append(rules, fired)
	}
	predicate := map[string]any{
		"decisionID":   decisionID,
		"verdict":      result.Verdict,
		"bundleDigest": bundleDigest,
		"inputDigest":  inputDigest,
		"environment":  input.Environment.Name,
		"rulesFired":   rules,
		"evaluatedAt":  input.At.UTC().Format(time.RFC3339),
	}
	if input.DataSnapshot != "" {
		predicate["dataSnapshot"] = input.DataSnapshot
	}
	return predicate
}

// registryAttester resolves the registry the project's artifacts live in,
// through the project's own registry Connection — the same resolution
// gateTarget makes, from a project name instead of a build.
func (r *DecisionRecorder) registryAttester(ctx context.Context, projectName string) (ArtifactAttester, error) {
	project := &kitchenv1alpha1.Project{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: PlatformNamespace, Name: projectName,
	}, project); err != nil {
		return nil, err
	}
	connection := &kitchenv1alpha1.Connection{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: PlatformNamespace, Name: project.Spec.Registry.ConnectionRef.Name,
	}, connection); err != nil {
		return nil, err
	}
	registry, err := provider.Registry(connection)
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: connection.Spec.CredentialsSecretRef.Name}
	if err := r.Client.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("the registry credential could not be read: %w", err)
	}
	factory := r.Attesters
	if factory == nil {
		factory = defaultAttester
	}
	return factory(secret.Data[corev1.DockerConfigJsonKey], registry.Server)
}

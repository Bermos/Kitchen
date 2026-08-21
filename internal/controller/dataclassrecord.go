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
	"encoding/hex"
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The data-class declaration record: when a provider declares what a
// provisioned resource's data derives from, the platform signs the
// declaration as a kitchen.bermos.dev DataClass/v1 statement and keeps the
// envelope in the store's signed_records table.
//
// It is signed because it is the fact an auditor's first question hangs on —
// "is production data in that preview?" — and a fact worth acting on is worth
// being able to verify later, independent of whatever the claim's status says
// by then. It lives in the store rather than a registry because a claim has
// no OCI repository: nothing content-addressed exists to attach to, so the
// statement's subject is a claim identity digest (sha256 over
// namespace/name/uid) and the envelope is kept whole.
//
// Best-effort-loud, like decision storage: the declaration is recorded on the
// claim's status and audit-logged either way, a store or signer that is not
// there is logged loudly, and Kitchen.status.compliance.policy owns up to an
// installation that keeps no records.

// SignedRecordStore is the slice of the store a declaration envelope lands
// in. An interface so the recorder can be tested without a ClickHouse.
type SignedRecordStore interface {
	InsertSignedRecord(ctx context.Context, record clickhouse.SignedRecord) error
}

// SignedRecordStoreFactory builds the store from the platform's ClickHouse
// configuration. Nil means the real client.
type SignedRecordStoreFactory func(cfg clickhouse.Config) SignedRecordStore

func defaultSignedRecordStore(cfg clickhouse.Config) SignedRecordStore {
	return clickhouse.New(cfg)
}

// ClaimIdentityDigest is the stable identity a claim's signed records name as
// their subject: sha256 over namespace, name and UID, length-separated. A
// claim has no OCI repository and so no content digest — this is the identity
// that survives everything but the claim's own deletion, and the UID keeps a
// deleted-and-recreated claim from inheriting its predecessor's record.
func ClaimIdentityDigest(claim *kitchenv1alpha1.ResourceClaim) string {
	sum := sha256.Sum256([]byte(claim.Namespace + "\x00" + claim.Name + "\x00" + string(claim.UID)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// claimSubjectName is the statement subject's name half: a readable path to
// the claim, next to the digest that is the actual identity.
func claimSubjectName(claim *kitchenv1alpha1.ResourceClaim) string {
	return "kitchen.bermos.dev/resourceclaims/" + claim.Namespace + "/" + claim.Name
}

// recordDataClassDeclaration signs and stores one declaration: this claim's
// data (or this environment's branch of it) derives from `provenance`, on
// `provider`'s word. Environment is empty for the claim's primary resource
// and names the preview for a branch.
//
// Nothing here fails the bind. A signer the platform does not hold means the
// declaration stands unattested (status says the platform is not signing);
// a store that is not there is logged loudly and the compliance status owns
// it. Errors are the operator's to read, never the claim's to wedge on.
func (r *ResourceClaimReconciler) recordDataClassDeclaration(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	environment, provenance, provider string,
) {
	if provenance == "" {
		// No declaration was made; there is nothing to attest. The absence is
		// visible on the claim's status and in the inventory as "undeclared".
		return
	}
	log := logf.FromContext(ctx)

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		log.Error(err, "the data-class declaration could not be attested: no platform configuration",
			"claim", claim.Name)
		return
	}
	signer, err := SigningKeyFor(ctx, r.Client, kitchen)
	if err != nil {
		log.Error(err, "the data-class declaration could not be attested", "claim", claim.Name)
		return
	}
	if signer == nil {
		// Not signing is a state, not a failure; the platform status says so.
		return
	}

	predicate := map[string]any{
		"claim":      claim.Name,
		"project":    claim.Spec.ProjectRef.Name,
		"provenance": provenance,
		"provider":   provider,
		"declaredAt": time.Now().UTC().Format(time.RFC3339),
	}
	if environment != "" {
		predicate["environment"] = environment
	}
	statement, err := attestation.NewStatement(
		claimSubjectName(claim), ClaimIdentityDigest(claim), attestation.PredicateDataClass, predicate)
	if err != nil {
		log.Error(err, "the data-class statement could not be built", "claim", claim.Name)
		return
	}
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		log.Error(err, "the data-class statement could not be signed", "claim", claim.Name)
		return
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		log.Error(err, "the data-class envelope could not be encoded", "claim", claim.Name)
		return
	}

	store := r.signedRecordStore(ctx, kitchen)
	if store == nil {
		log.Info("no record store: the data-class declaration stands, unkept",
			"claim", claim.Name, "provenance", provenance)
		return
	}
	if err := store.InsertSignedRecord(ctx, clickhouse.SignedRecord{
		ID:        string(uuid.NewUUID()),
		Timestamp: time.Now().UTC(),
		Type:      attestation.PredicateDataClass,
		Subject:   ClaimIdentityDigest(claim),
		Project:   claim.Spec.ProjectRef.Name,
		Envelope:  string(encoded),
	}); err != nil {
		log.Error(err, "the data-class declaration could not be stored; it stands on the claim's status",
			"claim", claim.Name, "provenance", provenance)
	}
}

// signedRecordStore resolves where signed records are kept, the way every
// store user does: off the singleton's ClickHouse secret. Nil means there is
// nowhere, which the caller treats as loud-but-not-fatal.
func (r *ResourceClaimReconciler) signedRecordStore(
	ctx context.Context, kitchen *kitchenv1alpha1.Kitchen,
) SignedRecordStore {
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		return nil
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		logf.FromContext(ctx).Error(err, "the record store's secret could not be read")
		return nil
	}
	cfg, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		logf.FromContext(ctx).Error(err, "the record store's secret is not a store configuration")
		return nil
	}
	factory := r.Records
	if factory == nil {
		factory = defaultSignedRecordStore
	}
	return factory(cfg)
}

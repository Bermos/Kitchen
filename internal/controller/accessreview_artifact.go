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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The recertification artefact: the one deliverable of a cycle that has to
// outlive the object.
//
// A closed AccessReview already carries the whole review on its status, and
// that would be enough if the object were forever. It is not: an object can
// be deleted, a cluster can be rebuilt, and an institution asked "show me the
// access review you did last March" cannot answer with a custom resource that
// has since been garbage-collected. So the cycle's closing act is to sign a
// statement of the whole thing — the snapshot, every decision, who made it,
// which were self-reviews, which revocations were actually carried out — and
// keep the envelope in the store's signed_records table under no TTL.
//
// It is the resource claim's data-class declaration exactly (§12): a DSSE
// envelope wrapping an in-toto Statement whose subject is an identity digest,
// because a review has no OCI repository to attach anything to. It verifies
// with the platform's published public key and with Kitchen out of the loop,
// which is the whole point of the evidence being standard-shaped.
//
// Best-effort-loud, like every other signed record here: a platform with no
// signing key or no store still closes the cycle, still applies the
// revocations and still writes the audit record — what it cannot do is leave
// portable evidence, and status.artifact.message says so rather than leaving
// a blank field for a reader to interpret generously.

// AccessReviewIdentityDigest is the stable identity a cycle's signed record
// names as its subject: sha256 over namespace, name and UID, length-separated.
// The UID is what keeps a deleted-and-recreated cycle from inheriting its
// predecessor's evidence.
func AccessReviewIdentityDigest(review *kitchenv1alpha1.AccessReview) string {
	sum := sha256.Sum256([]byte(review.Namespace + "\x00" + review.Name + "\x00" + string(review.UID)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// accessReviewSubjectName is the statement subject's name half: a readable
// path to the cycle, beside the digest that is the actual identity.
func accessReviewSubjectName(review *kitchenv1alpha1.AccessReview) string {
	return "kitchen.bermos.dev/accessreviews/" + review.Namespace + "/" + review.Name
}

// AccessReviewPredicate is the statement's payload: the cycle whole, in a
// shape a reader who has never seen Kitchen can follow.
//
// It is built apart from the signing so that a test can hold it up to the
// light without a key or a store — the pattern every predicate here follows,
// and the one that makes "does the artefact actually name the reviewer and
// every decision" a unit test rather than an integration test.
func AccessReviewPredicate(review *kitchenv1alpha1.AccessReview, at time.Time) map[string]any {
	entries := make([]map[string]any, 0, len(review.Status.Entries))
	for i := range review.Status.Entries {
		entry := &review.Status.Entries[i]
		row := map[string]any{
			"subject":  entry.Subject,
			"grant":    entry.Grant,
			"role":     entry.Role,
			"decision": string(entry.Decision),
		}
		if entry.Email != "" {
			row["email"] = entry.Email
		}
		if entry.Decision == "" {
			// An undecided grant is part of the record, not an omission from
			// it: "nobody looked at this one" is exactly the finding an
			// examiner is reading the artefact for.
			row["decision"] = "undecided"
		}
		if entry.DecidedBy != "" {
			row["decidedBy"] = entry.DecidedBy
		}
		if entry.DecidedAt != nil {
			row["decidedAt"] = entry.DecidedAt.UTC().Format(time.RFC3339)
		}
		if entry.Note != "" {
			row["note"] = entry.Note
		}
		if entry.SelfReview {
			row["selfReview"] = true
		}
		if entry.LastActive != nil {
			row["lastActive"] = entry.LastActive.UTC().Format(time.RFC3339)
		}
		if entry.Orphaned {
			row["orphaned"] = true
		}
		if entry.Decision == kitchenv1alpha1.AccessRevoke {
			row["applied"] = entry.Applied
			if entry.ApplyMessage != "" {
				row["applyMessage"] = entry.ApplyMessage
			}
		}
		entries = append(entries, row)
	}

	predicate := map[string]any{
		"review":       review.Name,
		"scope":        string(review.Spec.Scope),
		"openedBy":     review.Spec.OpenedBy,
		"reviewers":    subjectsOfReviewers(review),
		"dueBy":        review.Spec.DueBy.UTC().Format(time.RFC3339),
		"closedBy":     review.Status.ClosedBy,
		"entries":      entries,
		"pending":      review.Status.Pending,
		"confirmed":    review.Status.Confirmed,
		"revoked":      review.Status.Revoked,
		"selfReviewed": review.Status.SelfReviewed,
		"orphaned":     review.Status.Orphaned,
		"attestedAt":   at.UTC().Format(time.RFC3339),
		"platform":     kitchenPlatformName,
	}
	if review.Spec.ProjectRef != nil {
		predicate["project"] = review.Spec.ProjectRef.Name
	}
	if review.Spec.Reason != "" {
		predicate["reason"] = review.Spec.Reason
	}
	if review.Status.SnapshotAt != nil {
		predicate["snapshotAt"] = review.Status.SnapshotAt.UTC().Format(time.RFC3339)
	}
	if review.Status.ClosedAt != nil {
		predicate["closedAt"] = review.Status.ClosedAt.UTC().Format(time.RFC3339)
	}
	return predicate
}

// kitchenPlatformName is what the predicate calls the producer. It is a
// constant rather than the version, because the artefact is a claim about
// access rather than about a build, and pinning it to a version would invite
// a reader to think the version mattered to the claim.
const kitchenPlatformName = "kitchen.bermos.dev"

// recordArtifact signs the closed cycle and keeps the envelope. It always
// sets status.artifact — with a message where there is nothing to keep —
// because a nil artefact on a closed cycle would read as "not attempted".
func (r *AccessReviewReconciler) recordArtifact(
	ctx context.Context, review *kitchenv1alpha1.AccessReview,
) {
	log := logf.FromContext(ctx)
	now := r.now()
	artifact := &kitchenv1alpha1.AccessReviewArtifact{
		Subject:       AccessReviewIdentityDigest(review),
		PredicateType: attestation.PredicateAccessReview,
	}
	review.Status.Artifact = artifact

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		artifact.Message = "the cycle closed unattested: no platform configuration could be read"
		log.Error(err, "the access review could not be attested", "review", review.Name)
		return
	}
	signer, err := SigningKeyFor(ctx, r.Client, kitchen)
	if err != nil {
		artifact.Message = "the cycle closed unattested: " + err.Error()
		log.Error(err, "the access review could not be attested", "review", review.Name)
		return
	}
	if signer == nil {
		artifact.Message = "the cycle closed unattested: this platform holds no signing key, so the " +
			"decisions stand on the object and in the audit log and nowhere portable"
		return
	}

	statement, err := attestation.NewStatement(
		accessReviewSubjectName(review), artifact.Subject,
		attestation.PredicateAccessReview, AccessReviewPredicate(review, now))
	if err != nil {
		artifact.Message = "the cycle closed unattested: " + err.Error()
		log.Error(err, "the access review statement could not be built", "review", review.Name)
		return
	}
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		artifact.Message = "the cycle closed unattested: " + err.Error()
		log.Error(err, "the access review statement could not be signed", "review", review.Name)
		return
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		artifact.Message = "the cycle closed unattested: " + err.Error()
		log.Error(err, "the access review envelope could not be encoded", "review", review.Name)
		return
	}

	store := r.signedRecordStore(ctx, kitchen)
	if store == nil {
		artifact.Message = "the cycle was signed but not kept: this installation has no telemetry store, " +
			"so there is nowhere to retain the artefact beyond the object itself"
		log.Info("no record store: the access review artefact stands, unkept", "review", review.Name)
		return
	}
	id := string(uuid.NewUUID())
	project := ""
	if review.Spec.ProjectRef != nil {
		project = review.Spec.ProjectRef.Name
	}
	if err := store.InsertSignedRecord(ctx, clickhouse.SignedRecord{
		ID:        id,
		Timestamp: now,
		Type:      attestation.PredicateAccessReview,
		Subject:   artifact.Subject,
		Project:   project,
		Envelope:  string(encoded),
	}); err != nil {
		artifact.Message = "the cycle was signed but not kept: " + err.Error()
		log.Error(err, "the access review artefact could not be stored", "review", review.Name)
		return
	}

	artifact.RecordID = id
	artifact.SignedAt = &metav1.Time{Time: now}
	log.Info("access review artefact retained", "review", review.Name, "record", id,
		"subject", artifact.Subject)
}

// signedRecordStore resolves where signed records are kept, the way every
// store user does: off the singleton's ClickHouse secret. Nil means there is
// nowhere, which the caller treats as loud-but-not-fatal.
func (r *AccessReviewReconciler) signedRecordStore(
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

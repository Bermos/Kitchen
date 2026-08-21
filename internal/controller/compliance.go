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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The platform singleton's half of the compliance suite: the audit log's
// table, the key attestations are signed under, and the status that says
// whether either is actually working.
//
// Both are reported rather than assumed. An installation that turned the audit
// log on but has no telemetry store, or that names a signing key secret which
// does not exist, is producing no evidence at all — and the failure mode of
// evidence is silence, so the platform says so on its own object instead of
// waiting for an auditor to notice.

const (
	condComplianceReady = "ComplianceReady"

	// defaultAuditRetentionDays matches the CRD default, for Kitchen objects
	// written before the field existed.
	defaultAuditRetentionDays = 365
)

// reconcileCompliance creates the audit log's table, keeps its retention in
// step with spec.compliance.audit.retentionDays, and publishes what the audit
// recorder and the signer are doing.
//
// It is separate from reconcileTelemetrySchema even though both talk to the
// same store, because they answer to different settings: telemetry retention
// is a disk decision and audit retention is a records one, and an installation
// that turns collection down must not thereby shorten its evidence.
func (r *KitchenReconciler) reconcileCompliance(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	status := &kitchenv1alpha1.ComplianceStatus{}
	kitchen.Status.Compliance = status

	signing := r.reconcileSigningKey(ctx, kitchen)
	status.Attestation = signing
	status.Policy = r.reconcilePolicyStore(ctx, kitchen)

	if !kitchen.Spec.Compliance.Audit.Enabled {
		status.Audit = &kitchenv1alpha1.AuditStatus{
			Message: "the audit log is turned off in spec.compliance.audit",
		}
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condComplianceReady)
		return true
	}

	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		// Enabled with nowhere to write. This is the one combination worth a
		// false condition rather than silence: the platform was asked for
		// evidence and cannot produce any.
		status.Audit = &kitchenv1alpha1.AuditStatus{
			Message: "spec.compliance.audit is on but this installation has no telemetry store to append to: " +
				"set spec.observability.clickhouse.secretRef, or turn the audit log off deliberately",
		}
		setCond(condComplianceReady, metav1.ConditionFalse, "NoAuditStore", status.Audit.Message)
		return false
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		status.Audit = &kitchenv1alpha1.AuditStatus{Message: err.Error()}
		setCond(condComplianceReady, metav1.ConditionFalse, "ConnectionSecretMissing", err.Error())
		return false
	}
	cfg, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		status.Audit = &kitchenv1alpha1.AuditStatus{Message: err.Error()}
		setCond(condComplianceReady, metav1.ConditionFalse, "ConnectionSecretInvalid", err.Error())
		return false
	}

	retention := kitchen.Spec.Compliance.Audit.RetentionDays
	if retention < 1 {
		retention = defaultAuditRetentionDays
	}
	if err := clickhouse.New(cfg).EnsureAuditSchema(ctx, retention); err != nil {
		status.Audit = &kitchenv1alpha1.AuditStatus{Message: err.Error()}
		setCond(condComplianceReady, metav1.ConditionFalse, "SchemaNotApplied", err.Error())
		return false
	}

	recording := r.Audit.Status()
	status.Audit = &kitchenv1alpha1.AuditStatus{
		Recording: recording.Recording,
		Message:   recording.Message,
	}
	// The sequence published here is the chain's, read from the head object
	// rather than from whatever this replica last appended — the number is
	// only worth publishing if it is the whole platform's.
	if sequence, err := r.Audit.Head(ctx); err == nil {
		status.Audit.Sequence = sequence
	}

	message := fmt.Sprintf("audit log is in place, retaining %d days", retention)
	if !signing.Signing {
		setCond(condComplianceReady, metav1.ConditionFalse, "NotSigning", signing.Message)
		return false
	}
	setCond(condComplianceReady, metav1.ConditionTrue, "ComplianceReady",
		message+"; artifacts are signed under key "+signing.KeyID)
	return true
}

// reconcilePolicyStore creates the policy engine's tables — decisions and the
// bundles they cite — and reports whether decisions are being stored at all.
//
// It mirrors the audit posture exactly, because the two substantiate each
// other: the engine evaluates whether or not a store exists (a bundle and an
// input in hand need nothing else), but a decision that leaves no replayable
// row behind is one this status has to own up to rather than leave to an
// auditor to discover. There is no enabled knob — the engine has no off
// switch — so unlike the audit block this never contributes a condition; it
// says what is true and the decision-making paths stay honest either way.
//
// Retention is the audit knob, not the telemetry one: see EnsurePolicySchema.
func (r *KitchenReconciler) reconcilePolicyStore(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
) *kitchenv1alpha1.PolicyStatus {
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		return &kitchenv1alpha1.PolicyStatus{
			Message: "policy decisions are evaluated but not stored: this installation has no telemetry store " +
				"to keep them in, so no decision can be replayed later. Set spec.observability.clickhouse.secretRef",
		}
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		return &kitchenv1alpha1.PolicyStatus{
			Message: "policy decisions are evaluated but not stored: " + err.Error(),
		}
	}
	cfg, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		return &kitchenv1alpha1.PolicyStatus{
			Message: "policy decisions are evaluated but not stored: " + err.Error(),
		}
	}
	retention := kitchen.Spec.Compliance.Audit.RetentionDays
	if retention < 1 {
		retention = defaultAuditRetentionDays
	}
	if err := clickhouse.New(cfg).EnsurePolicySchema(ctx, retention); err != nil {
		return &kitchenv1alpha1.PolicyStatus{
			Message: "policy decisions are evaluated but not stored: " + err.Error(),
		}
	}
	return &kitchenv1alpha1.PolicyStatus{Storing: true}
}

// SigningKeySecretName is where the attestation key lives when the platform
// generated it. An installation that brings its own names it in
// spec.compliance.attestation.signingKeyRef instead.
const SigningKeySecretName = "kitchen-attestation-key"

// signingKeySecret is the secret the platform's key is expected in.
func signingKeySecret(kitchen *kitchenv1alpha1.Kitchen) string {
	if ref := kitchen.Spec.Compliance.Attestation.SigningKeyRef; ref != nil && ref.Name != "" {
		return ref.Name
	}
	return SigningKeySecretName
}

// reconcileSigningKey makes sure there is a key to sign attestations with, and
// reports which one it is.
//
// A key the platform generated is created once and then left alone. Rotation
// is deliberately a manual act — replacing the secret — rather than something
// the operator does on a schedule: every attestation ever signed under the old
// key stays valid only for as long as its public half is still published, so
// rotating is a decision about the evidence already out there, not about the
// key.
func (r *KitchenReconciler) reconcileSigningKey(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
) *kitchenv1alpha1.AttestationStatus {
	if !kitchen.Spec.Compliance.Attestation.Enabled {
		return &kitchenv1alpha1.AttestationStatus{
			Message: "artifact attestation is turned off in spec.compliance.attestation",
		}
	}

	name := signingKeySecret(kitchen)
	// An installation that named its own key is never handed a generated
	// one: a key that appeared because the named secret was missing would be
	// a key nobody's custody rules cover.
	generate := kitchen.Spec.Compliance.Attestation.SigningKeyRef == nil
	key, err := EnsureSigningKey(ctx, r.Client, PlatformNamespace, name, generate)
	if err != nil {
		return &kitchenv1alpha1.AttestationStatus{SecretName: name, Message: err.Error()}
	}
	return &kitchenv1alpha1.AttestationStatus{
		Signing:    true,
		KeyID:      key.KeyID(),
		SecretName: name,
	}
}

// EnsureSigningKey reads the platform's attestation key, generating it when
// `generate` is set and the secret is not there yet.
//
// It is a package function rather than a method because three callers need the
// same key: the platform reconciler that publishes its id, the build reconciler
// that signs with it, and the REST API that verifies with it and hands out its
// public half.
func EnsureSigningKey(
	ctx context.Context,
	c client.Client,
	namespace, name string,
	generate bool,
) (*attestation.ECDSAKey, error) {
	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret)
	switch {
	case err == nil:
		key, loadErr := attestation.LoadECDSAKey(
			secret.Data[attestation.SecretKeyPrivate], secret.Data[attestation.SecretKeyPublic])
		if loadErr != nil {
			return nil, fmt.Errorf("the signing key in secret %s/%s cannot be used: %w", namespace, name, loadErr)
		}
		return key, nil
	case !apierrors.IsNotFound(err):
		return nil, err
	case !generate:
		return nil, fmt.Errorf(
			"spec.compliance.attestation.signingKeyRef names secret %q, which does not exist in %s: "+
				"create it with %s and %s, or clear the reference to have the platform generate one",
			name, namespace, attestation.SecretKeyPrivate, attestation.SecretKeyPublic)
	}

	key, privatePEM, publicPEM, err := attestation.GenerateECDSAKey()
	if err != nil {
		return nil, err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{labelManagedByKey: labelManagedByValue},
		},
		Data: map[string][]byte{
			attestation.SecretKeyPrivate: privatePEM,
			attestation.SecretKeyPublic:  publicPEM,
		},
	}
	if err := c.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Two reconciles raced to generate the first key. Whichever lost
			// reads the winner's rather than overwriting it — an overwrite
			// would orphan every attestation signed in between.
			return EnsureSigningKey(ctx, c, namespace, name, false)
		}
		return nil, err
	}
	return key, nil
}

// SigningKeyFor resolves and loads the key the platform is configured to sign
// under, without creating anything. It answers a nil key and no error when the
// installation has turned attestation off, so a caller can treat "not signing"
// as a state rather than as a failure.
func SigningKeyFor(
	ctx context.Context,
	c client.Client,
	kitchen *kitchenv1alpha1.Kitchen,
) (*attestation.ECDSAKey, error) {
	if kitchen == nil || !kitchen.Spec.Compliance.Attestation.Enabled {
		return nil, nil
	}
	return EnsureSigningKey(ctx, c, PlatformNamespace, signingKeySecret(kitchen), false)
}

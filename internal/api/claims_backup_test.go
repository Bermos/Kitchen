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

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/backup"
)

// The backup policy a claim carries: what a request may ask for, what it is
// refused, and what a read answers about it.
//
// The policy adds no route of its own — it is part of what a claim is rather
// than a second object beside it — so every test here goes through the claim's
// own create and the claim's own view.

func TestAClaimWithNoPolicyInheritsRatherThanDeclaring(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), cnpgConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-archive-db", "project": "shop", "connection": "postgres", "type": "postgres"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "shop-archive-db", claim); err != nil {
		t.Fatal(err)
	}
	// Nothing written down is the whole point: the connection's default and,
	// failing that, the platform's destination are what such a claim gets,
	// and an empty block on the spec would freeze today's answer as this
	// claim's own.
	if claim.Spec.Backup != nil {
		t.Errorf("a claim that asked for nothing carries a policy: %+v", claim.Spec.Backup)
	}
	// And a claim no operator has reconciled has no backup view at all,
	// rather than one reporting an unprotected database that may well be
	// protected a second later.
	if view := decode[claimView](t, recorder); view.Backup != nil {
		t.Errorf("the answer invented a state: %+v", view.Backup)
	}
}

func TestAClaimCanOverrideTheScheduleAndTheRetention(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), cnpgConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-archive-db", "project": "shop", "connection": "postgres", "type": "postgres",
		  "backup": {"enabled": true, "schedule": "0 30 2 * * *", "retentionPolicy": "30d"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "shop-archive-db", claim); err != nil {
		t.Fatal(err)
	}
	policy := claim.Spec.Backup
	if policy == nil || policy.Enabled == nil || !*policy.Enabled {
		t.Fatalf("the claim does not carry what it asked for: %+v", policy)
	}
	if policy.Schedule != "0 30 2 * * *" || policy.RetentionPolicy != "30d" {
		t.Errorf("schedule = %q retention = %q", policy.Schedule, policy.RetentionPolicy)
	}
	if policy.Destination != nil {
		t.Errorf("a claim that named no bucket takes the platform's: %+v", policy.Destination)
	}
}

func TestAClaimCanSwitchItsBackupsOff(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), cnpgConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "scratch-db", "project": "shop", "connection": "postgres", "type": "postgres",
		  "backup": {"enabled": false}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "scratch-db", claim); err != nil {
		t.Fatal(err)
	}
	// False is a decision and has to survive as one: an unset Enabled means
	// "whatever the connection and the platform say", which is the opposite
	// answer.
	if claim.Spec.Backup == nil || claim.Spec.Backup.Enabled == nil || *claim.Spec.Backup.Enabled {
		t.Errorf("switching backups off did not land: %+v", claim.Spec.Backup)
	}
}

func TestAFiveFieldScheduleIsRefusedWithTheOneThatWorks(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), cnpgConnection())...)

	// The refusal is the feature. "0 3 * * *" is a *valid* expression that
	// the database's own operator reads as every hour at three minutes past,
	// so accepting it would silently back this database up 24 times a day and
	// never at three in the morning.
	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-archive-db", "project": "shop", "connection": "postgres", "type": "postgres",
		  "backup": {"schedule": "0 3 * * *"}}`)
	message := errorOf(t, recorder.Body.String())
	if recorder.Code != http.StatusBadRequest || !strings.Contains(message, "six fields") {
		t.Fatalf("want a refusal naming the dialect: %d %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(message, "0 0 3 * * *") {
		t.Errorf("the refusal should carry the schedule that works: %s", message)
	}
}

func TestABackupPolicyIsRefusedABadRetentionAndHalfAKeyPair(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), cnpgConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-archive-db", "project": "shop", "connection": "postgres", "type": "postgres",
		  "backup": {"retentionPolicy": "a month or so"}}`)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(errorOf(t, recorder.Body.String()), "retentionPolicy") {
		t.Errorf("a retention that is not a count and a unit is refused by name: %d %s",
			recorder.Code, recorder.Body.String())
	}

	recorder = h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-archive-db", "project": "shop", "connection": "postgres", "type": "postgres",
		  "backup": {"destination": {"bucket": "shop-archive", "accessKeyId": "AKIAEXAMPLE"}}}`)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(errorOf(t, recorder.Body.String()), "secretAccessKey") {
		t.Errorf("half a key pair cannot authenticate and is refused here: %d %s",
			recorder.Code, recorder.Body.String())
	}

	recorder = h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-archive-db", "project": "shop", "connection": "postgres", "type": "postgres",
		  "backup": {"destination": {"prefix": "databases"}}}`)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(errorOf(t, recorder.Body.String()), "bucket") {
		t.Errorf("a destination with nowhere to write is refused: %d %s",
			recorder.Code, recorder.Body.String())
	}
}

func TestAClaimOfAnotherTypeIsRefusedABackupPolicy(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), s3Connection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-uploads", "project": "shop", "connection": "store", "type": "objectStore",
		  "backup": {"enabled": true}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(errorOf(t, recorder.Body.String()), "postgres") {
		t.Errorf("the refusal should say which type does have a policy: %s", recorder.Body.String())
	}
}

func TestABucketOfTheClaimsOwnIsStoredAndNeverEchoed(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), cnpgConnection())...)

	body := `{"name": "shop-archive-db", "project": "shop", "connection": "postgres", "type": "postgres",
		  "backup": {"destination": {"bucket": "shop-archive", "prefix": "db", "region": "eu-central-1",
		                             "endpoint": "https://minio.example.com", "forcePathStyle": true,
		                             "serverSideEncryption": "AES256",
		                             "accessKeyId": "AKIAEXAMPLE",
		                             "secretAccessKey": "s3cr3t-do-not-echo"}}}`
	recorder := h.do(t, http.MethodPost, "/api/v1/claims", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "s3cr3t-do-not-echo") ||
		strings.Contains(recorder.Body.String(), "AKIAEXAMPLE") {
		t.Fatalf("a key came back out of the API: %s", recorder.Body.String())
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "shop-archive-db", claim); err != nil {
		t.Fatal(err)
	}
	s3 := claim.Spec.Backup.Destination.S3
	if s3.Bucket != "shop-archive" || s3.Prefix != "db" || !s3.ForcePathStyle ||
		s3.Endpoint != "https://minio.example.com" || s3.ServerSideEncryption != "AES256" {
		t.Errorf("the destination on the claim: %+v", s3)
	}
	if s3.CredentialsSecretRef == nil {
		t.Fatal("the claim names no credential")
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: testNamespace, Name: s3.CredentialsSecretRef.Name}
	if err := h.server.Client.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("the credential was not written: %v", err)
	}
	if string(secret.Data[backup.CredentialKeySecretAccessKey]) != "s3cr3t-do-not-echo" {
		t.Errorf("the Secret does not carry the key pair: %v", secret.Data)
	}
	// It is the operator's to delete with the claim, which is what the label
	// says: a Secret anything else wrote is nobody's to remove.
	if secret.Labels[managedByLabelKey] != managedByLabelValue {
		t.Errorf("a credential this API wrote carries no managed-by label: %v", secret.Labels)
	}
}

func TestAClaimReportsWhatTheDestinationCanRecoverTo(t *testing.T) {
	first := metav1.NewTime(time.Date(2026, 8, 30, 3, 14, 0, 0, time.UTC))
	last := metav1.NewTime(time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC))
	claim := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-archive-db", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:          kitchenv1alpha1.ClaimTypePostgres,
			ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "postgres"},
		},
		Status: kitchenv1alpha1.ResourceClaimStatus{
			Backup: &kitchenv1alpha1.ClaimBackupStatus{
				Enabled:               true,
				Schedule:              "0 0 3 * * *",
				RetentionPolicy:       "30d",
				Destination:           "s3://kitchen-archive/databases",
				LastBackup:            &last,
				FirstRecoverablePoint: &first,
				Archiving:             kitchenv1alpha1.ClaimArchivingHealthy,
			},
		},
	}
	h := newHarness(t, nil, append(fixtures(), cnpgConnection(), claim)...)

	view := decode[claimView](t, h.do(t, http.MethodGet, "/api/v1/claims/shop-archive-db", ""))
	if view.Backup == nil || !view.Backup.Enabled {
		t.Fatalf("the claim reports no policy: %+v", view.Backup)
	}
	// The field the whole feature exists for: "backups are configured" is
	// worth nothing next to "we can restore to 03:14 on the 30th".
	if view.Backup.FirstRecoverablePoint == nil || !view.Backup.FirstRecoverablePoint.Equal(first.Time) {
		t.Errorf("firstRecoverablePoint = %v", view.Backup.FirstRecoverablePoint)
	}
	if view.Backup.Destination != "s3://kitchen-archive/databases" || view.Backup.Archiving != "healthy" {
		t.Errorf("the policy in force: %+v", view.Backup)
	}
}

func TestAProviderThatKeepsItsOwnBackupsIsNotReportedAsUnprotected(t *testing.T) {
	claim := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-db", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:          kitchenv1alpha1.ClaimTypePostgres,
			ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "neon"},
		},
		Status: kitchenv1alpha1.ResourceClaimStatus{
			Backup: &kitchenv1alpha1.ClaimBackupStatus{
				ProviderManaged: true,
				Reason:          "Neon keeps continuous history of this database itself",
			},
		},
	}
	h := newHarness(t, nil, append(fixtures(), neonConnection(), claim)...)

	view := decode[claimView](t, h.do(t, http.MethodGet, "/api/v1/claims/orders-db", ""))
	if view.Backup == nil || !view.Backup.ProviderManaged || view.Backup.Enabled {
		t.Fatalf("the honest third state, between backed up and not: %+v", view.Backup)
	}
	if !strings.Contains(view.Backup.Reason, "Neon") {
		t.Errorf("the provider's own sentence about what it keeps: %q", view.Backup.Reason)
	}
}

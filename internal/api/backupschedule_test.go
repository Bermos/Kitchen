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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/backup"
	"github.com/Bermos/Kitchen/internal/controller"
)

const (
	destinationPath = "/api/v1/platform/backup/destination"
	runsPath        = "/api/v1/platform/backup/runs"

	// nightly is a schedule these tests set and read back. A quiet hour is
	// what the documentation asks for, and this is one.
	nightly = "0 3 * * *"

	// testArchiveKey is the key archives are encrypted under here — 32 bytes
	// as base64, which is what `openssl rand -base64 32` prints. Every write
	// of a first destination carries one or says out loud that it wants none,
	// because that is now the choice the route insists on.
	testArchiveKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
)

// setDestination writes a destination through the route and hands back the
// view the route answered with.
func setDestination(t *testing.T, h *harness, body string) backupScheduleView {
	t.Helper()
	recorder := h.do(t, http.MethodPut, destinationPath, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT destination answered %d: %s", recorder.Code, recorder.Body.String())
	}
	view := backupScheduleView{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func singleton(t *testing.T, h *harness) *kitchenv1alpha1.Kitchen {
	t.Helper()
	kitchen := &kitchenv1alpha1.Kitchen{}
	key := types.NamespacedName{Name: controller.KitchenSingletonName}
	if err := h.server.Client.Get(context.Background(), key, kitchen); err != nil {
		t.Fatal(err)
	}
	return kitchen
}

// The whole reason the destination has an address of its own: it carries a
// credential, and nothing ever hands one back.
func TestBackupDestinationNeverEchoesTheCredential(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	body := `{"type":"s3","s3":{"bucket":"kitchen-backups","prefix":"prod/","region":"eu-central-1",
		"endpoint":"https://minio.example.com","forcePathStyle":true,"serverSideEncryption":"AES256",
		"accessKeyId":"AKIAEXAMPLE","secretAccessKey":"s3cr3t-do-not-echo"},
		"encryption":{"key":"` + testArchiveKey + `"}}`
	view := setDestination(t, h, body)

	answered, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"AKIAEXAMPLE", "s3cr3t-do-not-echo", "accessKeyId", "secretAccessKey"} {
		if strings.Contains(string(answered), secret) {
			t.Errorf("the response carries %q; the API never reads a credential back: %s", secret, answered)
		}
	}
	if view.Destination == nil {
		t.Fatal("the response describes no destination")
	}
	if view.Destination.Bucket != "kitchen-backups" || view.Destination.Prefix != "prod" {
		t.Errorf("the bucket and prefix are echoed, and they read as %+v", view.Destination)
	}
	if view.Destination.Described != "s3://kitchen-backups/prod" {
		t.Errorf("the described destination is %q", view.Destination.Described)
	}
	if view.Destination.Credential != backupCredentialStored {
		t.Errorf("a destination given a key pair authenticates with %q", view.Destination.Credential)
	}

	// Writing the key into a Secret this API owns
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: testNamespace, Name: BackupDestinationSecretName}
	if err := h.server.Client.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("the credential was not written: %v", err)
	}
	if string(secret.Data[backup.CredentialKeyAccessKeyID]) != "AKIAEXAMPLE" ||
		string(secret.Data[backup.CredentialKeySecretAccessKey]) != "s3cr3t-do-not-echo" {
		t.Errorf("the Secret does not carry the key pair: %v", secret.Data)
	}
	if secret.Labels[managedByLabelKey] != managedByLabelValue {
		t.Errorf("a credential this API wrote carries no managed-by label: %v", secret.Labels)
	}

	// Pointing the singleton at it
	kitchen := singleton(t, h)
	destination := kitchen.Spec.Backup.Destination
	if destination == nil || destination.S3 == nil || destination.S3.CredentialsSecretRef == nil {
		t.Fatalf("the singleton names no credential: %+v", destination)
	}
	if destination.S3.CredentialsSecretRef.Name != BackupDestinationSecretName {
		t.Errorf("the singleton names %q", destination.S3.CredentialsSecretRef.Name)
	}
}

// The API never reads a credential back, so a form that redisplays a
// destination cannot send the key it never received. An edit that mentions no
// key must therefore leave the stored one exactly where it is.
func TestBackupDestinationKeepsAnUnmentionedCredential(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	setDestination(t, h, `{"s3":{"bucket":"kitchen-backups","accessKeyId":"AK","secretAccessKey":"SK"},
		"encryption":{"key":"`+testArchiveKey+`"}}`)

	view := setDestination(t, h, `{"s3":{"bucket":"kitchen-backups","prefix":"nightly"}}`)
	if view.Destination.Credential != backupCredentialStored {
		t.Fatalf("editing the prefix dropped the credential: %+v", view.Destination)
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: testNamespace, Name: BackupDestinationSecretName}
	if err := h.server.Client.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("the credential is gone: %v", err)
	}

	// And moving onto the ambient chain is explicit, and deletes it
	view = setDestination(t, h, `{"s3":{"bucket":"kitchen-backups","ambientCredentials":true}}`)
	if view.Destination.Credential != backupCredentialAmbient {
		t.Errorf("the destination still authenticates with %q", view.Destination.Credential)
	}
	if err := h.server.Client.Get(context.Background(), key, secret); err == nil {
		t.Error("the stored key survived a move onto the ambient credential chain")
	}
}

// Half a key pair is a destination that cannot authenticate, and a key
// alongside "use the ambient chain" is two answers to one question.
func TestBackupDestinationRefusesAHalfConfiguration(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for _, tc := range []struct{ name, body, says string }{
		{"no bucket", `{"s3":{"bucket":""}}`, "bucket is required"},
		{"no s3 block", `{"type":"s3"}`, "needs an s3 block"},
		{"an unknown kind", `{"type":"gcs","s3":{"bucket":"b"}}`, "type must be"},
		{"half a key pair", `{"s3":{"bucket":"b","accessKeyId":"AK"}}`, "go together"},
		{"a key and the ambient chain", `{"s3":{"bucket":"b","accessKeyId":"AK","secretAccessKey":"SK",
			"ambientCredentials":true}}`, "takes no accessKeyId"},
		{"an encryption nothing implements", `{"s3":{"bucket":"b","serverSideEncryption":"rot13"}}`,
			"serverSideEncryption must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPut, destinationPath, tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("answered %d, want 400: %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.says) {
				t.Errorf("the refusal does not say what to fix: %s", recorder.Body.String())
			}
		})
	}
}

// Removing a destination takes the credential with it — and answers with what
// to clear first where a schedule still points at it, rather than handing back
// a CEL rule's message.
func TestBackupDestinationRemoval(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	setDestination(t, h, `{"s3":{"bucket":"kitchen-backups","accessKeyId":"AK","secretAccessKey":"SK"},
		"encryption":{"key":"`+testArchiveKey+`"}}`)

	if recorder := h.do(t, http.MethodPatch, settingsPath,
		`{"backupSchedule":"0 3 * * *","backupKeepLast":7}`); recorder.Code != http.StatusOK {
		t.Fatalf("setting the schedule answered %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodDelete, destinationPath, "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("removing the destination under a live schedule answered %d, want 409: %s",
			recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Clear the schedule first") {
		t.Errorf("the refusal does not say what to do: %s", recorder.Body.String())
	}

	if recorder := h.do(t, http.MethodPatch, settingsPath, `{"backupSchedule":""}`); recorder.Code != http.StatusOK {
		t.Fatalf("clearing the schedule answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodDelete, destinationPath, ""); recorder.Code != http.StatusOK {
		t.Fatalf("removing the destination answered %d: %s", recorder.Code, recorder.Body.String())
	}

	kitchen := singleton(t, h)
	if kitchen.Spec.Backup.Destination != nil {
		t.Errorf("the destination survived its own removal: %+v", kitchen.Spec.Backup.Destination)
	}
	// A retention with nothing to prune overrides nothing, and admission
	// refuses the pair — so it goes with the destination it belonged to.
	if kitchen.Spec.Backup.Retention.KeepLast != nil || kitchen.Spec.Backup.Retention.KeepDays != nil {
		t.Errorf("the retention outlived its destination: %+v", kitchen.Spec.Backup.Retention)
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: testNamespace, Name: BackupDestinationSecretName}
	if err := h.server.Client.Get(context.Background(), key, secret); err == nil {
		t.Error("the credential outlived the destination it belonged to")
	}
}

// The schedule and the retention are ordinary settings and go through the
// route that already edits this object. The destination does not, because
// PATCH /settings must never carry a credential.
func TestSettingsCarryTheBackupSchedule(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	// This one wants no encryption at all, which is a decision it has to make
	// out loud — the destination route refuses to write a destination whose
	// archives are neither encrypted nor deliberately not.
	setDestination(t, h, `{"s3":{"bucket":"kitchen-backups","prefix":"prod"},"encryption":{"mode":"none"}}`)

	recorder := h.do(t, http.MethodPatch, settingsPath,
		`{"backupSchedule":"0 3 * * *","backupKeepLast":30,"backupKeepDays":90}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", recorder.Code, recorder.Body.String())
	}
	view := settingsView{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Backup.Schedule != nightly {
		t.Errorf("the schedule reads back as %q", view.Backup.Schedule)
	}
	if view.Backup.KeepLast == nil || *view.Backup.KeepLast != 30 {
		t.Errorf("keepLast reads back as %v", view.Backup.KeepLast)
	}

	// And 0 removes a bound, which is the only way back to keeping everything
	recorder = h.do(t, http.MethodPatch, settingsPath, `{"backupKeepLast":0,"backupKeepDays":0}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", recorder.Code, recorder.Body.String())
	}
	kitchen := singleton(t, h)
	if kitchen.Spec.Backup.Retention.KeepLast != nil || kitchen.Spec.Backup.Retention.KeepDays != nil {
		t.Errorf("a retention that was cleared reads as %+v", kitchen.Spec.Backup.Retention)
	}
	if kitchen.Spec.Backup.Schedule != nightly {
		t.Error("a patch that did not mention the schedule changed it")
	}
}

// The two combinations admission would refuse, refused here in the words that
// name the fix rather than as a CEL rule's message.
func TestSettingsRefuseAScheduleWithNowhereToWriteTo(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for _, tc := range []struct{ name, body, says string }{
		{"a schedule with no destination", `{"backupSchedule":"0 3 * * *"}`, "needs somewhere to write to"},
		{"a retention with nothing to prune", `{"backupKeepLast":7}`, "needs a destination to apply to"},
		{"a schedule that is not one", `{"backupSchedule":"every night"}`, "five-field cron expression"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, settingsPath, tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("answered %d, want 400: %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.says) {
				t.Errorf("the refusal does not say what to fix: %s", recorder.Body.String())
			}
		})
	}
}

// A run has to have something to run to. Both routes say what is missing
// rather than failing at the destination.
func TestBackupRunsNeedADestination(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, runsPath, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("listing with no destination answered %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no backup destination") {
		t.Errorf("the refusal does not say what is missing: %s", recorder.Body.String())
	}

	recorder = h.do(t, http.MethodPost, runsPath, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("running with no schedule answered %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no scheduled backup to run") {
		t.Errorf("the refusal does not say what is missing: %s", recorder.Body.String())
	}
}

// Every route on this surface reads or writes the platform's own credentials,
// so every one of them is the operator's.
func TestBackupScheduleRoutesAreRefusedToAMember(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)

	for _, call := range []struct {
		method, path, body string
	}{
		{http.MethodPut, destinationPath, `{"s3":{"bucket":"b"}}`},
		{http.MethodDelete, destinationPath, ""},
		{http.MethodGet, runsPath, ""},
		{http.MethodPost, runsPath, ""},
	} {
		recorder := h.do(t, call.method, call.path, call.body)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s %s answered %d, want 403: %s",
				call.method, call.path, recorder.Code, recorder.Body.String())
		}
	}
}

// The archive is every credential this platform holds, and a destination is
// somebody's bucket. So a destination that would leave it readable there is
// refused at the door: the endpoint has to be https, and either the archive is
// encrypted under a key this installation supplied or the installation has
// said out loud that it would rather it were not.
func TestBackupDestinationRefusesToLeaveTheArchiveInTheOpen(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for _, tc := range []struct{ name, body, says string }{
		{"a plaintext endpoint", `{"s3":{"bucket":"b","endpoint":"http://minio.internal:9000"},
			"encryption":{"key":"` + testArchiveKey + `"}}`, "must be an https:// URL"},
		{"an endpoint with no scheme at all", `{"s3":{"bucket":"b","endpoint":"minio.internal:9000"},
			"encryption":{"key":"` + testArchiveKey + `"}}`, "must be an https:// URL"},
		{"no key and no decision", `{"s3":{"bucket":"b"}}`, "no key to encrypt them with"},
		{"a key that is not a key", `{"s3":{"bucket":"b"},"encryption":{"key":"hunter2"}}`,
			"must be 32 bytes"},
		{"an encryption nothing implements", `{"s3":{"bucket":"b"},"encryption":{"mode":"rot13"}}`,
			"encryption.mode must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPut, destinationPath, tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("answered %d, want 400: %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.says) {
				t.Errorf("the refusal does not say what to fix: %s", recorder.Body.String())
			}
		})
	}

	// And the opt-in is what makes the plaintext endpoint possible, for the
	// installation whose store really is on a network it trusts.
	view := setDestination(t, h, `{"s3":{"bucket":"b","endpoint":"http://minio.internal:9000",
		"allowInsecureEndpoint":true},"encryption":{"key":"`+testArchiveKey+`"}}`)
	if !view.Destination.AllowInsecureEndpoint {
		t.Errorf("the opt-in is not on the destination that was written: %+v", view.Destination)
	}
}

// The key is written into a Secret this platform owns, is never read back, and
// is what the archive is actually encrypted under.
func TestBackupEncryptionKeyIsStoredAndNeverEchoed(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	view := setDestination(t, h, `{"s3":{"bucket":"kitchen-backups"},
		"encryption":{"key":"`+testArchiveKey+`"}}`)

	if view.Encryption.Mode != string(kitchenv1alpha1.BackupEncryptionAES256GCM) {
		t.Errorf("archives are encrypted by default, and this platform says %q", view.Encryption.Mode)
	}
	if view.Encryption.Key != backupKeyStored {
		t.Errorf("the key was supplied and the platform reports %q", view.Encryption.Key)
	}
	answered, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(answered), testArchiveKey) {
		t.Errorf("the response carries the archive's key: %s", answered)
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: testNamespace, Name: backup.EncryptionKeySecretName}
	if err := h.server.Client.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("the key was not written: %v", err)
	}
	parsed, err := backup.ParseEncryptionKey(string(secret.Data[backup.EncryptionKeySecretKey]))
	if err != nil {
		t.Fatalf("the stored key cannot be used: %v", err)
	}
	if len(parsed) != backup.EncryptionKeyBytes {
		t.Errorf("the stored key is %d bytes", len(parsed))
	}
	if secret.Labels[managedByLabelKey] != managedByLabelValue {
		t.Errorf("a credential this API wrote carries no managed-by label: %v", secret.Labels)
	}

	kitchen := singleton(t, h)
	if ref := kitchen.Spec.Backup.Encryption.KeySecretRef; ref == nil || ref.Name != backup.EncryptionKeySecretName {
		t.Fatalf("the singleton names no encryption key: %+v", kitchen.Spec.Backup.Encryption)
	}
	if !kitchen.Spec.Backup.Encryption.Encrypted() {
		t.Error("the singleton does not say its archives are encrypted")
	}

	// Editing the bucket mentions no key, and the stored one survives — the
	// rule the destination's own credential already keeps.
	view = setDestination(t, h, `{"s3":{"bucket":"kitchen-backups","prefix":"nightly"}}`)
	if view.Encryption.Key != backupKeyStored {
		t.Errorf("editing the prefix dropped the archive's key: %+v", view.Encryption)
	}

	// And turning it off is a decision somebody makes, not a state to drift
	// into: the mode says so afterwards, and the key stays where it is so the
	// archives already in the bucket are still openable.
	view = setDestination(t, h, `{"s3":{"bucket":"kitchen-backups"},"encryption":{"mode":"none"}}`)
	if view.Encryption.Mode != string(kitchenv1alpha1.BackupEncryptionNone) {
		t.Errorf("the opt-out did not take: %+v", view.Encryption)
	}
	if err := h.server.Client.Get(context.Background(), key, secret); err != nil {
		t.Errorf("turning encryption off destroyed the key that opens the archives already written: %v", err)
	}
}

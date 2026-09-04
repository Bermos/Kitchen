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
		"accessKeyId":"AKIAEXAMPLE","secretAccessKey":"s3cr3t-do-not-echo"}}`
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
	setDestination(t, h, `{"s3":{"bucket":"kitchen-backups","accessKeyId":"AK","secretAccessKey":"SK"}}`)

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
	setDestination(t, h, `{"s3":{"bucket":"kitchen-backups","accessKeyId":"AK","secretAccessKey":"SK"}}`)

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
	setDestination(t, h, `{"s3":{"bucket":"kitchen-backups","prefix":"prod"}}`)

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

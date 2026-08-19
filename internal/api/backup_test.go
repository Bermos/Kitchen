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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/accountsdb"
	"github.com/Bermos/Kitchen/internal/backup"
)

const backupPath = "/api/v1/platform/backup"

// stubAccounts stands in for the identity provider's Postgres.
type stubAccounts struct{ dump accountsdb.Dump }

func (s *stubAccounts) Database() string { return s.dump.Database }

func (s *stubAccounts) Dump(context.Context) (accountsdb.Dump, error) { return s.dump, nil }

func (s *stubAccounts) Close(context.Context) {}

func withAccounts(h *harness, accounts accountsConnection, message string) {
	h.server.accountsDB = func(context.Context, *kitchenv1alpha1.Kitchen) (accountsConnection, string) {
		return accounts, message
	}
}

func fixtureAccounts() *stubAccounts {
	return &stubAccounts{dump: accountsdb.Dump{
		Database: "kitchen",
		Tables: []accountsdb.Table{{
			Name: "user", Columns: []string{"id", "email"}, Rows: 1,
			Data: []byte("1\tada@example.com\n"),
		}},
	}}
}

func snapshotClass(name string) *unstructured.Unstructured {
	class := &unstructured.Unstructured{}
	class.SetAPIVersion("snapshot.storage.k8s.io/v1")
	class.SetKind("VolumeSnapshotClass")
	class.SetName(name)
	return class
}

func TestBackupDescribesWhatItWouldCarry(t *testing.T) {
	objects := append(fixtures(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-token", Namespace: testNamespace},
			Data:       map[string][]byte{"api-token": []byte("cf")},
		},
		// The cluster's, not the platform's: neither is counted.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.kitchen.v1", Namespace: testNamespace},
			Type:       corev1.SecretType("helm.sh/release.v1"),
		},
		snapshotClass("csi-hostpath"),
	)
	h := newHarness(t, nil, objects...)
	withAccounts(h, fixtureAccounts(), "")

	recorder := h.do(t, http.MethodGet, backupPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := backupView{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}

	if view.Resources["projects"] == 0 {
		t.Errorf("the screen says an export would carry no projects: %v", view.Resources)
	}
	if view.Secrets != 1 {
		t.Errorf("counted %d secrets, want the one that is the platform's", view.Secrets)
	}
	if !view.Accounts.Available || view.Accounts.Database != "kitchen" {
		t.Errorf("the accounts database reads as %+v", view.Accounts)
	}
	if len(view.Excluded) == 0 {
		t.Error("the screen does not say what a backup leaves out")
	}
	// The one thing an operator must not have to discover during an incident.
	if !strings.Contains(strings.Join(view.Excluded, " "), "ClickHouse") {
		t.Errorf("the exclusions do not mention telemetry: %v", view.Excluded)
	}
	if !view.Snapshots.Supported || len(view.Snapshots.Classes) != 1 {
		t.Errorf("snapshot support reads as %+v", view.Snapshots)
	}
	if !strings.HasSuffix(view.Filename, ".tar.gz") {
		t.Errorf("the download is called %q", view.Filename)
	}
}

// Issue #64's cluster: the snapshot API is registered and there is no class to
// snapshot into, so a VolumeSnapshot would be accepted and never taken.
func TestBackupReportsASnapshotAPIWithNoClasses(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	withAccounts(h, nil, "no identity provider")

	recorder := h.do(t, http.MethodGet, backupPath, "")
	view := backupView{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Snapshots.Supported {
		t.Error("a cluster with no VolumeSnapshotClass was reported as able to snapshot")
	}
	if !strings.Contains(view.Snapshots.Message, "VolumeSnapshotClass") {
		t.Errorf("the message does not name what is missing: %q", view.Snapshots.Message)
	}
}

// The archive itself: a stream a restore can read, carrying the projects, the
// credentials and the accounts.
func TestBackupStreamsARestorableArchive(t *testing.T) {
	objects := append(fixtures(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-token", Namespace: testNamespace},
		Data:       map[string][]byte{"api-token": []byte("cf")},
	})
	h := newHarness(t, nil, objects...)
	h.server.Version = "0.9.0"
	withAccounts(h, fixtureAccounts(), "")

	recorder := h.do(t, http.MethodPost, backupPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/gzip" {
		t.Errorf("content type %q", got)
	}
	disposition := recorder.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, "kitchen-backup-") {
		t.Errorf("content disposition %q", disposition)
	}

	archive, err := backup.Read(bytes.NewReader(recorder.Body.Bytes()))
	if err != nil {
		t.Fatalf("the archive the API streamed is not readable: %v", err)
	}
	if archive.Manifest.PlatformVersion != "0.9.0" {
		t.Errorf("the manifest says version %q", archive.Manifest.PlatformVersion)
	}
	if len(archive.Resources["projects"]) == 0 {
		t.Error("the archive carries no projects")
	}
	var carriedToken bool
	for _, secret := range archive.Secrets {
		if secret.Name == "cloudflare-token" && string(secret.Data["api-token"]) == "cf" {
			carriedToken = true
		}
	}
	if !carriedToken {
		t.Error("the archive carries no credentials, so a restore would bring back a platform that cannot talk")
	}
	if archive.Accounts == nil || archive.Accounts.Rows() != 1 {
		t.Errorf("the archive carries no accounts: %+v", archive.Accounts)
	}
}

// An installation with no identity provider has no accounts to take, which is
// not a fault — but the archive has to say so, because the difference between
// that and a database nobody could reach is only visible at restore time.
func TestBackupSaysWhyItCarriesNoAccounts(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	withAccounts(h, nil, "the accounts database refused the connection")

	recorder := h.do(t, http.MethodPost, backupPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	archive, err := backup.Read(bytes.NewReader(recorder.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if archive.Accounts != nil {
		t.Error("an archive with no accounts carried some")
	}
	if !strings.Contains(archive.Manifest.AccountsMessage, "refused the connection") {
		t.Errorf("the manifest does not say why: %q", archive.Manifest.AccountsMessage)
	}
}

// The archive is every credential the platform holds, so both routes are the
// operator's — and a member is refused rather than handed a smaller archive.
func TestBackupIsRefusedToAMember(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		recorder := h.do(t, method, backupPath, "")
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s answered %d, want 403: %s", method, recorder.Code, recorder.Body.String())
		}
	}
}

// The accounts database is resolved off the Kitchen object, and an
// installation whose object predates the field still has one to find.
func TestAccountsSecretFallsBackToTheConventionalName(t *testing.T) {
	if got := accountsdb.SecretName(&kitchenv1alpha1.Kitchen{}); got != accountsdb.DefaultSecretName {
		t.Errorf("an object naming no secret resolved to %q", got)
	}
	named := &kitchenv1alpha1.Kitchen{Spec: kitchenv1alpha1.KitchenSpec{
		Auth: kitchenv1alpha1.AuthSpec{DatabaseSecretRef: &kitchenv1alpha1.LocalObjectReference{Name: "elsewhere"}},
	}}
	if got := accountsdb.SecretName(named); got != "elsewhere" {
		t.Errorf("an object naming a secret resolved to %q", got)
	}
}

// An installation with no identity provider has no accounts database, and an
// archive that leaves them out has to say which of the two reasons it was —
// "there are none" or "there are and I could not reach them".
//
// It is checked below the HTTP layer on purpose: an installation with no
// identity provider has no issuer either, so no caller can authenticate and
// the route can never be reached on one. What is under test is the resolution,
// which the export path calls before it writes anything.
func TestNoIdentityProviderMeansNoAccountsToBackUp(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	kitchen := &kitchenv1alpha1.Kitchen{
		Spec: kitchenv1alpha1.KitchenSpec{Auth: kitchenv1alpha1.AuthSpec{Enabled: false}},
	}
	accounts, message := h.server.accountsDatabase(context.Background(), kitchen)
	if accounts != nil {
		t.Fatal("an installation with no identity provider produced an accounts connection")
	}
	if !strings.Contains(message, "no identity provider") {
		t.Errorf("the message reads %q", message)
	}

	// And one that has an identity provider whose secret is not there is a
	// fault, said in the words that name the fix.
	enabled := &kitchenv1alpha1.Kitchen{
		Spec: kitchenv1alpha1.KitchenSpec{Auth: kitchenv1alpha1.AuthSpec{Enabled: true}},
	}
	if _, message = h.server.accountsDatabase(context.Background(), enabled); !strings.Contains(
		message, accountsdb.DefaultSecretName) {
		t.Errorf("the message does not name the secret it looked for: %q", message)
	}
}

// The filename is what an archive is called on somebody's disk months later,
// and it goes into a quoted header, so it carries neither a quote nor a slash.
func TestBackupFilenameIsSafeAndSaysWhichPlatform(t *testing.T) {
	kitchen := &kitchenv1alpha1.Kitchen{Spec: kitchenv1alpha1.KitchenSpec{ClusterName: `pro"d/staging`}}
	name := backupFilename(kitchen, metav1.Now().Time.UTC())
	if strings.ContainsAny(name, `"/`) {
		t.Errorf("the filename carries a quote or a slash: %q", name)
	}
	if !strings.Contains(name, "pro-d-staging") {
		t.Errorf("the filename does not name the installation: %q", name)
	}
}

// A guard against the archive quietly losing a kind: every kind the operator
// reconciles has to be in it, or a restore brings back a platform missing
// something nobody will notice until they look for it.
func TestEveryCustomResourceKindIsInTheArchive(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	carried := map[string]bool{}
	for _, kind := range backup.Kinds {
		carried[kind.Kind] = true
	}
	// PlatformUpdate is the deliberate omission: it is the upgrade history of
	// a cluster that will not exist by the time anybody restores.
	carried["PlatformUpdate"] = true

	for gvk, goType := range scheme.AllKnownTypes() {
		if gvk.Group != kitchenv1alpha1.GroupVersion.Group || strings.HasSuffix(gvk.Kind, "List") {
			continue
		}
		// The scheme also carries metav1's own option kinds under every group
		// it knows. Only the types declared in api/v1alpha1 are the platform's.
		if goType.PkgPath() != reflect.TypeOf(kitchenv1alpha1.Project{}).PkgPath() {
			continue
		}
		if !carried[gvk.Kind] {
			t.Errorf("%s is reconciled by the operator and is not in a backup", gvk.Kind)
		}
	}
}

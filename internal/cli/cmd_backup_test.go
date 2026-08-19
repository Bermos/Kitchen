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

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/backup"
)

// archive builds a real backup archive, the way the operator's export endpoint
// does — so what the command writes to disk is a file a restore could read,
// rather than a fixture that only looks like one.
func archive(t *testing.T) []byte {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", ClusterName: "prod"},
		},
		&kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "kitchen-system"},
		},
	).Build()

	exporter := &backup.Exporter{
		Client:          cluster,
		Namespace:       "kitchen-system",
		Version:         "0.9.0",
		ClusterName:     "prod",
		BaseDomain:      "apps.example.com",
		AccountsMessage: "this installation has no identity provider",
	}
	buffer := &bytes.Buffer{}
	if _, err := exporter.WriteTo(context.Background(), buffer); err != nil {
		t.Fatalf("building the archive: %v", err)
	}
	return buffer.Bytes()
}

func TestBackupWritesAnArchiveTheRestoreCouldRead(t *testing.T) {
	h := newHarness(t)
	h.platform.backup = archive(t)
	h.platform.backupFilename = "kitchen-backup-prod-2026-08-19T090000Z.tar.gz"

	if code := h.run("backup", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	taken := backupTaken{}
	h.answer(&taken)

	// The name the platform suggested, in the working directory — which is
	// what makes an archive found on a disk months later say where it is from.
	want := filepath.Join(h.work, h.platform.backupFilename)
	if taken.File != want {
		t.Errorf("wrote %q, want %q", taken.File, want)
	}
	written, err := os.ReadFile(taken.File)
	if err != nil {
		t.Fatalf("the file the command reported is not there: %v", err)
	}
	if int64(len(written)) != taken.Bytes {
		t.Errorf("reported %d bytes and wrote %d", taken.Bytes, len(written))
	}
	if _, err := backup.Read(bytes.NewReader(written)); err != nil {
		t.Fatalf("what it wrote is not a readable archive: %v", err)
	}

	if taken.PlatformVersion != "0.9.0" || taken.Objects == 0 {
		t.Errorf("the summary does not describe the archive: %+v", taken)
	}
	// The exclusions come from the archive rather than from this command, so
	// the one thing nobody should discover during an incident is in the answer
	// a cron job reads.
	if !strings.Contains(strings.Join(taken.Excluded, " "), "ClickHouse") {
		t.Errorf("the summary does not say telemetry is left out: %v", taken.Excluded)
	}
	if !strings.Contains(taken.AccountsMessage, "no identity provider") {
		t.Errorf("the summary does not carry why there are no accounts: %q", taken.AccountsMessage)
	}

	// Nothing partial left behind: the archive is written to a temporary file
	// and renamed, so a directory holding a `.partial` after a success would
	// mean the rename never happened.
	entries, err := os.ReadDir(h.work)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".partial") {
			t.Errorf("left a partial archive behind: %s", entry.Name())
		}
	}
}

// An archive is a credential, and overwriting one by accident is how a good
// backup is lost to a failed one.
func TestBackupWillNotOverwriteWithoutForce(t *testing.T) {
	h := newHarness(t)
	h.platform.backup = archive(t)
	h.platform.backupFilename = "kitchen-backup.tar.gz"

	target := filepath.Join(h.work, "existing.tar.gz")
	if err := os.WriteFile(target, []byte("the one that worked"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := h.run("backup", target, "--json"); code == 0 {
		t.Fatal("overwrote an existing archive without --force")
	}
	if failed := h.failure(); failed.Code != codeConflict {
		t.Errorf("failed with %q, want %q", failed.Code, codeConflict)
	}
	kept, err := os.ReadFile(target)
	if err != nil || string(kept) != "the one that worked" {
		t.Errorf("the existing archive was not left alone: %q (%v)", kept, err)
	}

	if code := h.run("backup", target, "--force", "--json"); code != 0 {
		t.Fatalf("--force did not overwrite: %s", h.stderr.String())
	}
	if _, err := backup.Read(bytes.NewReader(read(t, target))); err != nil {
		t.Fatalf("--force wrote something that is not an archive: %v", err)
	}
}

// The whole platform's credentials are the operator's, and a refusal must
// leave nothing on disk that looks like a backup.
func TestBackupRefusedLeavesNoFile(t *testing.T) {
	h := newHarness(t)
	// A nil archive is how the fake platform refuses a member.
	if code := h.run("backup", "--json"); code == 0 {
		t.Fatal("a member was given the platform's credentials")
	}
	entries, err := os.ReadDir(h.work)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused backup left %d files behind", len(entries))
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

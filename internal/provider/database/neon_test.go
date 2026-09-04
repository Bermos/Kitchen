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

package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Bermos/Kitchen/internal/provider/database/databasetest"
)

func neonAgainstFake(t *testing.T) (*Neon, *databasetest.NeonServer) {
	t.Helper()
	fake := databasetest.NewNeonServer()
	t.Cleanup(fake.Close)
	return &Neon{APIURL: fake.URL(), Token: "neon-token"}, fake
}

func TestProvisionCreatesAProjectAndReadsItsBinding(t *testing.T) {
	neon, fake := neonAgainstFake(t)

	instance, err := neon.Provision(context.Background(), shopDB)
	if err != nil {
		t.Fatal(err)
	}
	project := fake.ProjectNamed("kitchen-shop-db")
	if project == nil {
		t.Fatal("no project was created")
	}
	if instance.ID != project.ID {
		t.Fatalf("instance ID %q is not the project's %q", instance.ID, project.ID)
	}
	branch := fake.BranchNamed("kitchen-shop-db", "main")
	if branch == nil {
		t.Fatal("the project has no default branch")
	}
	binding := instance.Binding
	if binding.User != "neondb_owner" || binding.Database != "neondb" ||
		binding.Password != branch.Password() || binding.Host != branch.Host() || binding.Port != "5432" {
		t.Fatalf("unexpected binding: %+v", binding)
	}
	if !strings.Contains(binding.URL, branch.Host()) || !strings.Contains(binding.URL, "sslmode=require") {
		t.Fatalf("unexpected connection URL: %q", binding.URL)
	}
	if auth := fake.LastAuthorization(); auth != "Bearer neon-token" {
		t.Fatalf("the token did not reach the API: %q", auth)
	}
	// The data-class half of the contract: a claim's Neon project IS the
	// production database, and its placement is Neon's own answer.
	if instance.Provenance != ProvenanceProduction {
		t.Fatalf("a provisioned Neon project is production data, got %q", instance.Provenance)
	}
	if instance.Region != databasetest.NeonRegion {
		t.Fatalf("the reported placement must come through, got %q", instance.Region)
	}
}

// A branch of a production database is production-derived — the whole of the
// classification story hangs on the provider saying so, cheap copy or not.
func TestNeonDeclaresBranchesProductionDerived(t *testing.T) {
	neon, _ := neonAgainstFake(t)
	ctx := context.Background()

	instance, err := neon.Provision(ctx, shopDB)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := neon.CreateBranch(ctx, instance.ID, "shop-pr-7")
	if err != nil {
		t.Fatal(err)
	}
	if branch.Provenance != ProvenanceProduction {
		t.Fatalf("a branch of a production database is production-derived, got %q", branch.Provenance)
	}
	// Finding the branch again declares the same thing: the declaration is a
	// property of what the branch is, not of which code path returned it.
	again, err := neon.CreateBranch(ctx, instance.ID, "shop-pr-7")
	if err != nil {
		t.Fatal(err)
	}
	if again.Provenance != ProvenanceProduction {
		t.Fatalf("the found-again branch lost its declaration: %q", again.Provenance)
	}

	// The idempotent Provision path reports the region too.
	found, err := neon.Provision(ctx, shopDB)
	if err != nil {
		t.Fatal(err)
	}
	if found.Region != databasetest.NeonRegion || found.Provenance != ProvenanceProduction {
		t.Fatalf("the found-again project lost its declaration: %+v", found)
	}
}

// A second Provision of the same name must find the first project again — a
// reconcile that lost its status update provisions once, not twice.
func TestProvisionIsIdempotentByName(t *testing.T) {
	neon, fake := neonAgainstFake(t)
	ctx := context.Background()

	first, err := neon.Provision(ctx, shopDB)
	if err != nil {
		t.Fatal(err)
	}
	second, err := neon.Provision(ctx, shopDB)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("a second Provision made a second project: %q then %q", first.ID, second.ID)
	}
	if second.Binding.Password != first.Binding.Password {
		t.Fatal("the reused instance's binding does not match the created one's")
	}
	_ = fake
}

func TestBranchLifecycle(t *testing.T) {
	neon, fake := neonAgainstFake(t)
	ctx := context.Background()

	instance, err := neon.Provision(ctx, shopDB)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := neon.CreateBranch(ctx, instance.ID, "shop-pr-7")
	if err != nil {
		t.Fatal(err)
	}
	if fake.BranchNamed("kitchen-shop-db", "shop-pr-7") == nil {
		t.Fatal("no branch was created")
	}
	if branch.Binding.Host == instance.Binding.Host {
		t.Fatal("the branch binding points at the primary")
	}

	again, err := neon.CreateBranch(ctx, instance.ID, "shop-pr-7")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != branch.ID {
		t.Fatalf("a second CreateBranch made a second branch: %q then %q", branch.ID, again.ID)
	}

	if err := neon.DeleteBranch(ctx, instance.ID, branch.ID); err != nil {
		t.Fatal(err)
	}
	if fake.BranchNamed("kitchen-shop-db", "shop-pr-7") != nil {
		t.Fatal("the branch is still there")
	}
	// Deleting an already-absent branch is not an error.
	if err := neon.DeleteBranch(ctx, instance.ID, branch.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDeprovisionRemovesTheProject(t *testing.T) {
	neon, fake := neonAgainstFake(t)
	ctx := context.Background()

	instance, err := neon.Provision(ctx, shopDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := neon.Deprovision(ctx, instance.ID); err != nil {
		t.Fatal(err)
	}
	if fake.ProjectNamed("kitchen-shop-db") != nil {
		t.Fatal("the project is still there")
	}
	// Deleting an already-absent project is not an error.
	if err := neon.Deprovision(ctx, instance.ID); err != nil {
		t.Fatal(err)
	}
}

func TestProviderErrorsCarryTheAPIsDiagnostic(t *testing.T) {
	neon, fake := neonAgainstFake(t)
	fake.FailWith("branches are locked")

	_, err := neon.Provision(context.Background(), shopDB)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "branches are locked") {
		t.Fatalf("the API's own words are missing from %q", err.Error())
	}
	if strings.Contains(err.Error(), "neon-token") {
		t.Fatal("the error leaks the credential")
	}
}

// The window is the project's own retention, read from the provider rather
// than assumed: a date picker over a window that does not exist is worse than
// no feature.
func TestNeonReadsItsRecoveryWindowFromTheProject(t *testing.T) {
	neon, fake := neonAgainstFake(t)
	ctx := context.Background()

	instance, err := neon.Provision(ctx, shopDB)
	if err != nil {
		t.Fatal(err)
	}
	window, err := neon.RecoveryWindow(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if window.Empty() {
		t.Fatal("a project with retention has a window")
	}
	reach := window.Latest.Sub(window.Earliest)
	if want := time.Duration(databasetest.NeonRetention) * time.Second; reach != want {
		t.Fatalf("the window reaches back %s, want the project's retention %s", reach, want)
	}
	if !window.Contains(window.Latest.Add(-time.Hour)) {
		t.Fatal("an hour ago is inside a seven-day window")
	}
	if window.Contains(window.Earliest.Add(-time.Second)) {
		t.Fatal("a moment before the earliest is outside the window")
	}

	// Retention turned off is a provider that keeps no history, and the
	// window says so rather than pretending to a moment ago.
	fake.SetRetention(instance.ID, 0)
	window, err = neon.RecoveryWindow(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !window.Empty() {
		t.Fatalf("a project with no retention has no window, got %+v", window)
	}
}

// The whole of Neon's recovery: one field on the request CreateBranch already
// posts. The test asserts the field reaches the API, because that is the
// difference between a recovery and a branch of the present.
func TestNeonRecoverToSendsTheParentTimestamp(t *testing.T) {
	neon, fake := neonAgainstFake(t)
	ctx := context.Background()

	instance, err := neon.Provision(ctx, shopDB)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	branch, err := neon.RecoverTo(ctx, instance.ID, "recovery-before-the-migration", at)
	if err != nil {
		t.Fatal(err)
	}
	created := fake.BranchNamed("kitchen-shop-db", "recovery-before-the-migration")
	if created == nil {
		t.Fatal("no branch was created")
	}
	if created.ParentTimestamp != at.Format(time.RFC3339) {
		t.Fatalf("parent_timestamp did not reach the API: %q", created.ParentTimestamp)
	}
	// A recovery of a production database is production data at an earlier
	// moment, and the binding is the sibling's rather than the primary's.
	if branch.Provenance != ProvenanceProduction {
		t.Fatalf("a recovery of a production database is production-derived, got %q", branch.Provenance)
	}
	if branch.Binding.Host == instance.Binding.Host {
		t.Fatal("the recovery's binding points at the original")
	}

	// Idempotent by name, like every other create here: a reconcile that runs
	// twice recovers once, and never takes a second copy a minute later.
	again, err := neon.RecoverTo(ctx, instance.ID, "recovery-before-the-migration", at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != branch.ID {
		t.Fatalf("a second RecoverTo made a second database: %q then %q", branch.ID, again.ID)
	}

	// An ordinary branch still carries no timestamp: recovery is the one
	// caller that sets it.
	if _, err := neon.CreateBranch(ctx, instance.ID, "shop-pr-9"); err != nil {
		t.Fatal(err)
	}
	if preview := fake.BranchNamed("kitchen-shop-db", "shop-pr-9"); preview == nil || preview.ParentTimestamp != "" {
		t.Fatalf("a preview branch is not a recovery: %+v", preview)
	}
}

// A recovery with no moment to recover to is refused before anything is
// created — the field is the whole operation.
func TestNeonRecoverToNeedsAMoment(t *testing.T) {
	neon, _ := neonAgainstFake(t)
	ctx := context.Background()

	instance, err := neon.Provision(ctx, shopDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := neon.RecoverTo(ctx, instance.ID, "recovery-nowhen", time.Time{}); err == nil {
		t.Fatal("want a refusal")
	}
}

// Neon implements the optional interface; the compiler is the assertion.
var _ RecoverableProvisioner = (*Neon)(nil)

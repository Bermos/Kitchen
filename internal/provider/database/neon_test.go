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

	instance, err := neon.Provision(context.Background(), "kitchen-shop-db")
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

	instance, err := neon.Provision(ctx, "kitchen-shop-db")
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
	found, err := neon.Provision(ctx, "kitchen-shop-db")
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

	first, err := neon.Provision(ctx, "kitchen-shop-db")
	if err != nil {
		t.Fatal(err)
	}
	second, err := neon.Provision(ctx, "kitchen-shop-db")
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

	instance, err := neon.Provision(ctx, "kitchen-shop-db")
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

	instance, err := neon.Provision(ctx, "kitchen-shop-db")
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

	_, err := neon.Provision(context.Background(), "kitchen-shop-db")
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

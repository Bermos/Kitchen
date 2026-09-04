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
	"strings"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/database"
	"github.com/Bermos/Kitchen/internal/provider/oidcclient"
)

// What a preview binds to is decided in one place, from the provider's
// declaration and the claim's own choice, and every rule of that decision
// is a case here.
func TestResolvePreviewMode(t *testing.T) {
	postgres, _ := kitchenv1alpha1.LookupClaimType(kitchenv1alpha1.ClaimTypePostgres)
	oidc, _ := kitchenv1alpha1.LookupClaimType(kitchenv1alpha1.ClaimTypeOIDCClient)
	branching := contract.Declaration{Preview: contract.PreviewBranch, PreviewNote: "a copy"}
	fresh := contract.Declaration{Preview: contract.PreviewFresh, PreviewNote: "an empty one"}
	sharing := contract.Declaration{Preview: contract.PreviewShared, PreviewNote: "the one there is"}

	for _, testCase := range []struct {
		name        string
		claimType   kitchenv1alpha1.ClaimType
		declaration contract.Declaration
		declared    bool
		choice      string
		want        contract.PreviewMode
		says        string
	}{
		{"no choice takes the declaration", postgres, branching, true, "", contract.PreviewBranch, "a copy"},
		{"a fresh provider gives fresh", postgres, fresh, true, "", contract.PreviewFresh, "an empty one"},
		{"the declared mode by name is that mode", postgres, fresh, true, "fresh", contract.PreviewFresh, "an empty one"},
		{"shared is always available and says what it means", postgres, fresh, true, "shared", contract.PreviewShared,
			"reads and writes the same database production does"},
		{"none is always available", postgres, branching, true, "none", contract.PreviewNone, "nothing"},
		{"a shared provider is never a default where data is held", postgres, sharing, true, "", contract.PreviewNone,
			"previewMode: shared"},
		{"a shared provider is the default where nothing is held", oidc, sharing, true, "", contract.PreviewShared,
			"the one there is"},
		{"a mode the provider cannot give is nothing, and says what it gives", postgres, fresh, true, "branch",
			contract.PreviewNone, "it gives fresh"},
		{"a provider that declared nothing gives nothing", postgres, contract.Declaration{}, false, "",
			contract.PreviewNone, "has not declared"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mode, reason := resolvePreviewMode(testCase.claimType, "acme", testCase.declaration, testCase.declared,
				testCase.choice)
			if mode != testCase.want {
				t.Fatalf("want %s, got %s (%s)", testCase.want, mode, reason)
			}
			if !strings.Contains(reason, testCase.says) {
				t.Fatalf("the reason does not say %q: %q", testCase.says, reason)
			}
		})
	}
}

// declare writes the decision and the provider's workload declarations on
// the claim, which is what the environment reconciler acts on.
func TestDeclareRecordsTheProviderOnTheClaim(t *testing.T) {
	claim := &kitchenv1alpha1.ResourceClaim{}
	claim.Spec.Type = kitchenv1alpha1.ClaimTypePostgres
	postgres, _ := claim.Type()
	if mode := declare(claim, postgres, database.ProviderCNPG); mode != contract.PreviewFresh {
		t.Fatalf("CloudNativePG gives previews a fresh database, got %s", mode)
	}
	if claim.Status.PreviewMode != "fresh" || claim.Status.PreviewReason == "" {
		t.Errorf("the decision must land on the status with its reason: %+v", claim.Status)
	}
	if claim.Status.KeepsPodsRunning || claim.Status.ForcesRecreate {
		t.Error("a database constrains nothing about the workload that reads it")
	}

	client := &kitchenv1alpha1.ResourceClaim{}
	client.Spec.Type = kitchenv1alpha1.ClaimTypeOIDCClient
	oidc, _ := client.Type()
	if mode := declare(client, oidc, oidcclient.ProviderName); mode != contract.PreviewShared {
		t.Fatalf("every environment signs in through the one client, got %s", mode)
	}
}

// fakeIdler records what the branch machinery asked of a provider that can
// park.
type fakeIdler struct{ idled, woken []string }

func (f *fakeIdler) idleBranch(_ context.Context, branchID string) error {
	f.idled = append(f.idled, branchID)
	return nil
}

func (f *fakeIdler) wakeBranch(_ context.Context, branchID string) error {
	f.woken = append(f.woken, branchID)
	return nil
}

// A preview's own infrastructure follows the preview: parked when it parks,
// running when it wakes, and asked for nothing when it has not moved.
func TestParkBranchFollowsTheEnvironment(t *testing.T) {
	env := &kitchenv1alpha1.Environment{}
	branch := &kitchenv1alpha1.ClaimBranch{Environment: "shop-pr-41", ID: "kitchen-databases/br-41"}
	idler := &fakeIdler{}

	// Awake and recorded awake: nothing to ask.
	if err := parkBranch(context.Background(), idler, env, branch); err != nil {
		t.Fatal(err)
	}
	if len(idler.idled)+len(idler.woken) != 0 {
		t.Fatalf("an environment that has not moved asked the provider for something: %+v", idler)
	}

	env.Status.Idle = true
	if err := parkBranch(context.Background(), idler, env, branch); err != nil {
		t.Fatal(err)
	}
	if len(idler.idled) != 1 || idler.idled[0] != branch.ID || !branch.Idle {
		t.Fatalf("a parked preview did not park its own resource: %+v %+v", idler, branch)
	}

	// Parked and recorded parked: nothing to ask again.
	if err := parkBranch(context.Background(), idler, env, branch); err != nil {
		t.Fatal(err)
	}
	if len(idler.idled) != 1 {
		t.Fatalf("the same park was asked for twice: %+v", idler)
	}

	env.Status.Idle = false
	if err := parkBranch(context.Background(), idler, env, branch); err != nil {
		t.Fatal(err)
	}
	if len(idler.woken) != 1 || idler.woken[0] != branch.ID || branch.Idle {
		t.Fatalf("a woken preview did not wake its own resource: %+v %+v", idler, branch)
	}
}

// A provider with nothing to park is never asked, and its branches read as
// awake whatever the environment is doing — which is the honest record: the
// claim's own idleReason is where the provider says why.
func TestParkBranchAsksNothingOfAProviderThatCannot(t *testing.T) {
	env := &kitchenv1alpha1.Environment{}
	env.Status.Idle = true
	branch := &kitchenv1alpha1.ClaimBranch{Environment: "shop-pr-41", ID: "br-41", Idle: true}
	if err := parkBranch(context.Background(), nil, env, branch); err != nil {
		t.Fatal(err)
	}
	if branch.Idle {
		t.Error("a branch nothing can park must not be recorded as parked")
	}
}

// What a claim says about idling is the provider's declaration read against
// the preview mode in force: a claim whose previews bind production's
// resource parks nothing, whatever its provider can do.
func TestResolveIdlingIsTheDeclarationAgainstThePreviewMode(t *testing.T) {
	parks := contract.Declaration{CanIdle: true, IdleNote: "the Cluster hibernates"}
	cannot := contract.Declaration{IdleNote: "Neon suspends its own compute"}

	if canIdle, reason := resolveIdling(parks, contract.PreviewFresh); !canIdle ||
		!strings.Contains(reason, "hibernates") {
		t.Fatalf("a preview's own resource parks: %v %q", canIdle, reason)
	}
	if canIdle, reason := resolveIdling(parks, contract.PreviewShared); canIdle ||
		!strings.Contains(reason, "rather than a resource of their own") {
		t.Fatalf("a shared preview must not park production's: %v %q", canIdle, reason)
	}
	if canIdle, reason := resolveIdling(cannot, contract.PreviewBranch); canIdle ||
		!strings.Contains(reason, "suspends its own compute") {
		t.Fatalf("a provider that cannot park says why: %v %q", canIdle, reason)
	}
}

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

	ctrl "sigs.k8s.io/controller-runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/declarations"
)

// A claim's type decides who provisions it and what its binding holds, and
// that is the whole of what differs between one kind of claim and another.
// Everything else — the finalizer, the Connection and its capability, the
// Pending/Bound/Failed transitions with their audit records, the binding
// Secret's removal, the watches — is the claim's own and is shared.
//
// So a claim type is a contract registered here, and the reconciler never
// asks what type it is holding: it looks the contract up and hands over. A
// new kind of dependency is a row in kitchenv1alpha1.ClaimTypes, a package
// beside internal/provider/database holding its Binding, its Requirements
// and its provisioner interface, a file beside this one implementing
// claimContract over it, and one entry in claimContracts. The test on this
// file refuses a row without an entry and an entry without a row.

// claimContract is one kind of claim as the reconciler sees it.
type claimContract interface {
	// reconcile drives the claim to Bound, or records why it cannot be yet,
	// through the reconciler's pending, failed and bind. conn is the
	// Connection the claim names, already checked to carry the type's
	// capability; it is nil for a type the platform provisions itself.
	reconcile(
		ctx context.Context,
		r *ResourceClaimReconciler,
		claim *kitchenv1alpha1.ResourceClaim,
		project *kitchenv1alpha1.Project,
		conn *kitchenv1alpha1.Connection,
	) (ctrl.Result, error)

	// finalize takes back what the contract put into the world — under the
	// claim's deletionPolicy where the resource holds data — before the
	// reconciler removes the binding Secret and lets the claim go. A
	// returned error is retried; a provider that is already gone is not an
	// error, because a claim that cannot be deleted blocks its project's
	// teardown behind it.
	finalize(ctx context.Context, r *ResourceClaimReconciler, claim *kitchenv1alpha1.ResourceClaim) error
}

// claimContracts is the registry: one contract per row of
// kitchenv1alpha1.ClaimTypes.
var claimContracts = map[string]claimContract{
	kitchenv1alpha1.ClaimTypePostgres:    postgresContract{},
	kitchenv1alpha1.ClaimTypeOIDCClient:  oidcContract{},
	kitchenv1alpha1.ClaimTypeObjectStore: objectStoreContract{},
	kitchenv1alpha1.ClaimTypeVolume:      volumeContract{},
	kitchenv1alpha1.ClaimTypeInngest:     inngestContract{},
}

// claimContractFor resolves a claim's type and its contract. ok is false
// for a type the table or the registry does not know, which the CRD's enum
// keeps to objects written before the type was removed.
func claimContractFor(claim *kitchenv1alpha1.ResourceClaim) (kitchenv1alpha1.ClaimType, claimContract, bool) {
	claimType, ok := claim.Type()
	if !ok {
		return kitchenv1alpha1.ClaimType{}, nil, false
	}
	contract, ok := claimContracts[claimType.Name]
	if !ok {
		return claimType, nil, false
	}
	return claimType, contract, true
}

// declare records on the claim's status what its provider declares — what a
// preview binds to, resolved against the claim's own choice, and what the
// binding does to the workload — and answers the resolved preview mode. It
// is the one place a declaration meets a claim, so that every contract
// records the same answer the same way.
//
// The rules, in order:
//
//   - A provider that has declared nothing gives previews nothing. The
//     claim binds for production and says so for previews.
//   - No choice takes the provider's declaration — unless that is shared
//     and the type holds data, because a preview reading production's data
//     is never a default: previews then get nothing, and the reason names
//     the choice that would accept it.
//   - shared is always available and always explicit; none is always
//     available.
//   - The provider's own mode by name is that mode; any other mode is one
//     this provider cannot give, and previews get nothing rather than the
//     wrong thing. The API refuses such a claim at creation, so this is
//     reachable only by a claim whose Connection changed under it.
func declare(claim *kitchenv1alpha1.ResourceClaim, claimType kitchenv1alpha1.ClaimType, provider string) contract.PreviewMode {
	declaration, declared := declarations.Lookup(claimType.Name, provider)
	mode, reason := resolvePreviewMode(claimType, provider, declaration, declared, claim.PreviewChoice())
	claim.Status.PreviewMode = string(mode)
	claim.Status.PreviewReason = reason
	claim.Status.KeepsPodsRunning = declaration.KeepsPodsRunning
	claim.Status.ForcesRecreate = declaration.ForcesRecreate
	return mode
}

func resolvePreviewMode(
	claimType kitchenv1alpha1.ClaimType,
	provider string,
	declaration contract.Declaration,
	declared bool,
	choice string,
) (contract.PreviewMode, string) {
	if !declared {
		return contract.PreviewNone, fmt.Sprintf("%s has not declared what a preview environment gets from a %s "+
			"claim, so previews get nothing", provider, claimType.Name)
	}
	switch contract.PreviewMode(choice) {
	case "":
		if declaration.Preview == contract.PreviewShared && claimType.HoldsData {
			return contract.PreviewNone, fmt.Sprintf("%s gives a preview environment production's %s itself (%s). "+
				"A preview reading production data is never a default: set previewMode: shared on the claim to "+
				"accept it, or previewMode: none to say previews get nothing", provider, claimType.Resource,
				declaration.PreviewNote)
		}
		return declaration.Preview, declaration.PreviewNote
	case contract.PreviewShared:
		if declaration.ForcesRecreate {
			// A resource that attaches to one pod at a time cannot be
			// shared between production and a preview without taking it
			// from production. The API refuses the choice at the door;
			// this is the same answer for a claim written another way.
			return contract.PreviewNone, fmt.Sprintf("the claim asks previews to share production's %s, and %s "+
				"declares that it attaches to one pod at a time: a preview mounting it would take it from "+
				"production, so previews get nothing", claimType.Resource, provider)
		}
		return contract.PreviewShared, fmt.Sprintf("the claim asks previews to bind production's %s itself: a "+
			"preview environment reads and writes the same %s production does", claimType.Resource,
			claimType.Resource)
	case contract.PreviewNone:
		return contract.PreviewNone, "the claim asks for nothing in preview environments"
	case declaration.Preview:
		return declaration.Preview, declaration.PreviewNote
	default:
		return contract.PreviewNone, fmt.Sprintf("the claim asks previews for %q, which %s cannot give: it gives "+
			"%s (%s), so previews get nothing rather than the wrong thing", choice, provider, declaration.Preview,
			declaration.PreviewNote)
	}
}

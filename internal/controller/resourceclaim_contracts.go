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

	ctrl "sigs.k8s.io/controller-runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
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
	kitchenv1alpha1.ClaimTypePostgres:   postgresContract{},
	kitchenv1alpha1.ClaimTypeOIDCClient: oidcContract{},
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

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
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// TestEveryClaimTypeHasAContract is what makes a claim type a registration
// rather than a branch: a row in kitchenv1alpha1.ClaimTypes without a
// contract here would admit claims nothing provisions, and a contract
// without a row would provision claims the CRD refuses.
func TestEveryClaimTypeHasAContract(t *testing.T) {
	for _, claimType := range kitchenv1alpha1.ClaimTypes {
		if _, ok := claimContracts[claimType.Name]; !ok {
			t.Errorf("claim type %q is in kitchenv1alpha1.ClaimTypes but has no contract in claimContracts",
				claimType.Name)
		}
	}
	for name := range claimContracts {
		if _, ok := kitchenv1alpha1.LookupClaimType(name); !ok {
			t.Errorf("claimContracts registers %q, which kitchenv1alpha1.ClaimTypes does not admit", name)
		}
	}
}

func TestClaimContractForRefusesAnUnknownType(t *testing.T) {
	claim := &kitchenv1alpha1.ResourceClaim{}
	claim.Spec.Type = "mainframe"
	if _, _, ok := claimContractFor(claim); ok {
		t.Error("a type the table does not know must have no contract")
	}
	claim.Spec.Type = kitchenv1alpha1.ClaimTypePostgres
	claimType, contract, ok := claimContractFor(claim)
	if !ok || contract == nil || claimType.Capability != kitchenv1alpha1.CapabilityDatabase {
		t.Errorf("postgres should resolve to its contract and the database capability, got %+v", claimType)
	}
}

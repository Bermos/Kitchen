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

package declarations

import (
	"os"
	"path/filepath"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/database"
	"github.com/Bermos/Kitchen/internal/provider/oidcclient"
)

// docsPage is where the matrix is published, relative to this package.
const docsPage = "../../../docs/api/claims.md"

// TestEveryProviderDeclares is what makes a declaration mandatory: a
// provider that can fulfil a claim type without a row here would decide
// what previews get in private, which is the state this package exists to
// end.
func TestEveryProviderDeclares(t *testing.T) {
	for _, claimType := range kitchenv1alpha1.ClaimTypes {
		if len(providersOf(claimType.Name)) == 0 {
			t.Errorf("claim type %q has no providers listed; nothing can fulfil it", claimType.Name)
		}
	}
	for _, d := range All() {
		if !d.Preview.Known() {
			t.Errorf("%s via %s declares preview mode %q, which is not one of %v", d.Type, d.Provider, d.Preview,
				contract.PreviewModes)
		}
		if d.PreviewNote == "" {
			t.Errorf("%s via %s says what previews get but not why", d.Type, d.Provider)
		}
		if (d.KeepsPodsRunning || d.ForcesRecreate) && d.WorkloadNote == "" {
			t.Errorf("%s via %s constrains the workload without saying why", d.Type, d.Provider)
		}
		if _, ok := kitchenv1alpha1.LookupClaimType(d.Type); !ok {
			t.Errorf("%q is declared for and is not a claim type", d.Type)
		}
	}
	// Every database provider Default() can build has a declaration: the two
	// maps are written side by side and the test is what keeps them level.
	for provider := range database.Declarations {
		if _, ok := Lookup(kitchenv1alpha1.ClaimTypePostgres, provider); !ok {
			t.Errorf("database provider %q declares and is not listed", provider)
		}
	}
}

func TestTheTwoShippedProvidersDeclareWhatTheIssueSays(t *testing.T) {
	neon, ok := Lookup(kitchenv1alpha1.ClaimTypePostgres, database.ProviderNeon)
	if !ok || neon.Preview != contract.PreviewBranch {
		t.Errorf("Neon branches production for a preview; it declares %q", neon.Preview)
	}
	cnpg, ok := Lookup(kitchenv1alpha1.ClaimTypePostgres, database.ProviderCNPG)
	if !ok || cnpg.Preview != contract.PreviewFresh {
		t.Errorf("CloudNativePG gives a preview a fresh, empty database; it declares %q", cnpg.Preview)
	}
	oidc, ok := Lookup(kitchenv1alpha1.ClaimTypeOIDCClient, oidcclient.ProviderName)
	if !ok || oidc.Preview != contract.PreviewShared {
		t.Errorf("every environment signs in through the one client; it declares %q", oidc.Preview)
	}
	if _, ok := Lookup(kitchenv1alpha1.ClaimTypePostgres, "mainframe"); ok {
		t.Error("a provider nothing declares for must not be found")
	}
}

// TestDocsMatrixIsFresh is the drift check: the matrix in docs/api/claims.md
// is generated from these declarations, and a declaration that moved
// without the page moving with it fails here. Run `make claim-matrix`.
func TestDocsMatrixIsFresh(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(docsPage))
	if err != nil {
		t.Fatalf("reading %s: %v", docsPage, err)
	}
	fresh, err := Splice(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if fresh != string(raw) {
		t.Fatalf("%s is stale: the matrix between the generated markers no longer matches the declarations. "+
			"Run `make claim-matrix` and commit the result", docsPage)
	}
}

func TestSpliceRefusesAPageWithoutMarkers(t *testing.T) {
	if _, err := Splice("# Claims\n\nnothing generated here\n"); err == nil {
		t.Error("a page without markers cannot be kept honest and must be refused")
	}
	out, err := Splice("before\n" + BeginMarker + "\nstale\n" + EndMarker + "\nafter\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "before\n"+BeginMarker+"\n"+Matrix()+EndMarker+"\nafter\n" {
		t.Errorf("the block between the markers was not replaced:\n%s", out)
	}
}

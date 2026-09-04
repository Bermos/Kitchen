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
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
	"github.com/Bermos/Kitchen/internal/provider/oidcclient"
	"github.com/Bermos/Kitchen/internal/provider/volume"
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
		// Both answers to "does a preview's own resource park with the
		// preview" need a sentence: one has to say what survives the park,
		// the other has to say why an open pull request keeps paying for it.
		if d.IdleNote == "" {
			t.Errorf("%s via %s says nothing about what an idle preview does to what it provisioned",
				d.Type, d.Provider)
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
	for provider := range objectstore.Declarations {
		if _, ok := Lookup(kitchenv1alpha1.ClaimTypeObjectStore, provider); !ok {
			t.Errorf("object store provider %q declares and is not listed", provider)
		}
	}
}

func TestTheTwoShippedProvidersDeclareWhatTheIssueSays(t *testing.T) {
	neon, ok := Lookup(kitchenv1alpha1.ClaimTypePostgres, database.ProviderNeon)
	if !ok || neon.Preview != contract.PreviewBranch {
		t.Errorf("Neon branches production for a preview; it declares %q", neon.Preview)
	}
	if neon.CanIdle {
		t.Error("Neon suspends its own compute; the platform must not claim to park it")
	}
	cnpg, ok := Lookup(kitchenv1alpha1.ClaimTypePostgres, database.ProviderCNPG)
	if !ok || cnpg.Preview != contract.PreviewFresh {
		t.Errorf("CloudNativePG gives a preview a fresh, empty database; it declares %q", cnpg.Preview)
	}
	if !cnpg.CanIdle {
		t.Error("a CloudNativePG preview Cluster hibernates with its preview; the declaration should say so")
	}
	oidc, ok := Lookup(kitchenv1alpha1.ClaimTypeOIDCClient, oidcclient.ProviderName)
	if !ok || oidc.Preview != contract.PreviewShared {
		t.Errorf("every environment signs in through the one client; it declares %q", oidc.Preview)
	}
	s3, ok := Lookup(kitchenv1alpha1.ClaimTypeObjectStore, objectstore.ProviderS3)
	if !ok || s3.Preview != contract.PreviewFresh {
		t.Errorf("a preview gets its own empty bucket; s3 declares %q", s3.Preview)
	}
	vol, ok := Lookup(kitchenv1alpha1.ClaimTypeVolume, volume.ProviderName)
	if !ok || vol.Preview != contract.PreviewFresh || !vol.ForcesRecreate {
		t.Errorf("a volume gives a preview a fresh, empty volume and forces a recreate; it declares %+v", vol)
	}
	bound, ok := Lookup(kitchenv1alpha1.ClaimTypeVolume, volume.BoundProviderName)
	if !ok || bound.Preview != contract.PreviewShared || !bound.SharedIsReadOnly {
		t.Errorf("a bound volume gives a preview the same volume read-only; it declares %+v", bound)
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

// A kubebuilder marker cannot read a Go constant, so the CRD's enum for a
// volume's source is written out by hand next to the field. This is what
// holds the two spellings together: a value renamed in one place and not the
// other would be a claim the API server admits and the provider does not
// know, or the reverse.
func TestTheVolumeSourcesAreSpelledOneWay(t *testing.T) {
	pairs := map[volume.Source]kitchenv1alpha1.VolumeSource{
		volume.SourceProvision: kitchenv1alpha1.VolumeProvision,
		volume.SourceBind:      kitchenv1alpha1.VolumeBind,
	}
	if len(pairs) != len(volume.Sources) {
		t.Fatalf("every source is held against the API's: %v", volume.Sources)
	}
	for provider, api := range pairs {
		if string(provider) != string(api) {
			t.Errorf("the provider says %q and the CRD says %q", provider, api)
		}
	}
}

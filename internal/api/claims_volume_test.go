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
	"net/http"
	"strings"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/volume"
)

// The volume claim through the API: no connection, a required block naming
// the process, and a refusal for anything the reconciler would only refuse
// later.

func TestAVolumeClaimNamesTheProcessThatMountsIt(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-data", "project": "shop", "type": "volume", "deletionPolicy": "Delete",
			"volume": {"process": "web", "size": "5Gi", "mountPath": "/data", "storageClass": "fast"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	created := decode[claimView](t, recorder)
	if created.Connection != "" || created.Secret != "" {
		t.Errorf("a volume claim has no connection and no binding secret: %+v", created)
	}
	if created.Volume == nil || created.Volume.Process != "web" || created.Volume.MountPath != "/data" ||
		created.Volume.Size != "5Gi" || created.Volume.StorageClass != "fast" {
		t.Errorf("the answer carries what was asked: %+v", created.Volume)
	}
	if created.DeletionPolicy != "Delete" {
		t.Errorf("a volume holds data, so deletionPolicy is its to choose: %q", created.DeletionPolicy)
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "shop-data", claim); err != nil {
		t.Fatal(err)
	}
	if got := claim.Volume(); got.Process != "web" || got.MountPath != "/data" || got.Size != "5Gi" {
		t.Errorf("spec.config carries the volume block as the reconciler reads it: %+v", got)
	}
}

func TestTheShapeOfAVolumeRequestIsRefusedHere(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, testCase := range map[string]struct {
		body string
		says string
	}{
		"no block": {
			`{"name": "v", "project": "shop", "type": "volume"}`,
			"volume is required",
		},
		"no process": {
			`{"name": "v", "project": "shop", "type": "volume", "volume": {"size": "1Gi", "mountPath": "/data"}}`,
			"volume.process is required",
		},
		"a process the project does not have": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"process": "worker", "size": "1Gi", "mountPath": "/data"}}`,
			"not one of project shop's processes",
		},
		"a relative mount path": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"process": "web", "size": "1Gi", "mountPath": "data"}}`,
			"volume.mountPath must be an absolute",
		},
		"a size that is not a quantity": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"process": "web", "size": "lots", "mountPath": "/data"}}`,
			"volume.size is a Kubernetes quantity",
		},
		"a connection": {
			`{"name": "v", "project": "shop", "type": "volume", "connection": "postgres",
				"volume": {"process": "web", "size": "1Gi", "mountPath": "/data"}}`,
			"takes no connection",
		},
		"sharing production's volume with previews": {
			`{"name": "v", "project": "shop", "type": "volume", "previewMode": "shared",
				"volume": {"process": "web", "size": "1Gi", "mountPath": "/data"}}`,
			"previewMode \"shared\" is refused",
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/claims", testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, testCase.says) {
				t.Fatalf("the refusal does not say %q: %q", testCase.says, got)
			}
		})
	}
}

// The catalogue says, before the claim exists, that a volume forces a
// recreate and offers previews a fresh volume or nothing — never
// production's own.
func TestTheCatalogueDeclaresWhatAVolumeCosts(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/claim-types", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	for _, view := range decode[[]claimTypeView](t, recorder) {
		if view.Type != kitchenv1alpha1.ClaimTypeVolume {
			continue
		}
		if view.Capability != "" || !view.HoldsData || len(view.Providers) != 1 {
			t.Fatalf("a volume takes no connection, holds data and has one provider: %+v", view)
		}
		provider := view.Providers[0]
		if provider.Provider != volume.ProviderName || provider.PreviewMode != string(contract.PreviewFresh) {
			t.Errorf("the StorageClass gives previews a fresh volume: %+v", provider)
		}
		if !provider.ForcesRecreate || provider.WorkloadNote == "" {
			t.Errorf("a volume declares the downtime it costs: %+v", provider)
		}
		for _, choice := range provider.PreviewChoices {
			if choice == string(contract.PreviewShared) {
				t.Errorf("a volume cannot be shared with previews, and must not be offered: %v", provider.PreviewChoices)
			}
		}
		return
	}
	t.Fatal("the catalogue does not list the volume type")
}

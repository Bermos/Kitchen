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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/declarations"
)

// The claim catalogue: every kind of claim the platform admits, and what
// each provider that can fulfil it declares about previews and about the
// workload. It is the same table the docs matrix is generated from and the
// reconciler acts on, served so that the dashboard shows a developer what
// they are choosing at the moment they choose it, rather than after the
// claim has bound.

// claimTypeView is one row of kitchenv1alpha1.ClaimTypes with its providers'
// declarations.
type claimTypeView struct {
	Type string `json:"type"`
	// Resource is the noun for what the claim provisions — "database",
	// "OAuth client".
	Resource string `json:"resource"`
	// Capability is what a Connection must provide to fulfil the type; empty
	// for a type the platform provisions itself, which takes no connection.
	Capability string `json:"capability,omitempty"`
	// HoldsData says whether deletionPolicy protects anything: a type that
	// holds none is always deprovisioned with its claim.
	HoldsData bool `json:"holdsData"`
	// Providers is each provider that can fulfil the type, with what it
	// declares.
	Providers []claimProviderView `json:"providers"`
}

// claimProviderView is one provider's declaration, as contract.Declaration
// says it, plus which preview modes a claim may ask this provider for.
type claimProviderView struct {
	Provider string `json:"provider"`
	// PreviewMode is what a preview gets when the claim asks for a resource
	// of its own: branch, fresh, shared or none. PreviewNote is why.
	PreviewMode string `json:"previewMode"`
	PreviewNote string `json:"previewNote"`
	// PreviewChoices is every previewMode a claim through this provider may
	// name: the provider's own, shared and none. Shared is listed, never
	// preselected — a preview reading production data is asked for.
	PreviewChoices []string `json:"previewChoices"`
	// KeepsPodsRunning and ForcesRecreate are what the binding does to the
	// workload that reads it; WorkloadNote is why, empty when neither.
	KeepsPodsRunning bool   `json:"keepsPodsRunning,omitempty"`
	ForcesRecreate   bool   `json:"forcesRecreate,omitempty"`
	WorkloadNote     string `json:"workloadNote,omitempty"`
}

func (s *Server) listClaimTypes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, claimTypeViews())
}

func claimTypeViews() []claimTypeView {
	views := make([]claimTypeView, 0, len(kitchenv1alpha1.ClaimTypes))
	for _, claimType := range kitchenv1alpha1.ClaimTypes {
		view := claimTypeView{
			Type:       claimType.Name,
			Resource:   claimType.Resource,
			Capability: string(claimType.Capability),
			HoldsData:  claimType.HoldsData,
			Providers:  []claimProviderView{},
		}
		for _, d := range declarations.All() {
			if d.Type != claimType.Name {
				continue
			}
			view.Providers = append(view.Providers, claimProviderView{
				Provider:         d.Provider,
				PreviewMode:      string(d.Preview),
				PreviewNote:      d.PreviewNote,
				PreviewChoices:   previewChoices(d.Preview, d.ForcesRecreate),
				KeepsPodsRunning: d.KeepsPodsRunning,
				ForcesRecreate:   d.ForcesRecreate,
				WorkloadNote:     d.WorkloadNote,
			})
		}
		views = append(views, view)
	}
	return views
}

// previewChoices is what a claim may name as its previewMode against a
// provider that declares the given mode: the mode itself, shared, and none,
// each once and in that order. A provider whose resource attaches to one pod
// at a time is not offered shared: a preview mounting production's would
// take it from production, and the API refuses the choice.
func previewChoices(declared contract.PreviewMode, attachesOnce bool) []string {
	choices := []string{}
	modes := []contract.PreviewMode{declared, contract.PreviewShared, contract.PreviewNone}
	if attachesOnce {
		modes = []contract.PreviewMode{declared, contract.PreviewNone}
	}
	for _, mode := range modes {
		if mode == "" {
			continue
		}
		known := false
		for _, have := range choices {
			if have == string(mode) {
				known = true
			}
		}
		if !known {
			choices = append(choices, string(mode))
		}
	}
	return choices
}

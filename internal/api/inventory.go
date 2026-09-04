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
	"context"
	"net/http"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The classification inventory: every environment and every resource claim,
// with its data class, its data's provenance and its location, in one
// request. FINMA expects the storage location of critical data to be known
// continuously, and "in the cluster" is not a location — this endpoint is
// where the platform answers with what it actually knows, absences included.
//
// Three words carry the absences, and they are words rather than empty
// strings because an export read by an auditor must not leave a blank cell
// open to a generous reading:
//
//   - "unclassified": nobody has declared a data class. Never defaulted.
//   - "undeclared": the claim's provider made no statement about what the
//     provisioned data derives from.
//   - "unknown": no residency is declared or reported anywhere along the
//     fallback chain.
//
// Since #309 it also carries the **outsourcing** half: every image running
// here that somebody else built, with the upstream reference it was taken
// from, the digest that reference resolved to, and who admitted it onto this
// platform. Determining that an outsourcing is *material* is the
// institution's judgement and stays theirs (§3); what a platform can supply
// is the list it is made against, and a vendored image running in production
// is the clearest entry that list has.

const (
	inventoryUnclassified = "unclassified"
	inventoryUndeclared   = "undeclared"
	inventoryUnknown      = "unknown"
)

// inventoryItemView is one row: an environment or a claim, with the three
// data facts. `type` is the item's own vocabulary — production/preview for an
// environment, postgres/oidcClient for a claim.
type inventoryItemView struct {
	Kind      string `json:"kind"`
	Project   string `json:"project"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	DataClass string `json:"dataClass"`
	// Upstream, Digest, AdmittedBy and Signature are the outsourcing facts,
	// on `vendoredImage` rows alone: where the image came from, what the
	// reference resolved to, who brought it onto this platform, and what
	// became of the vendor's own signature. Empty on every other kind of
	// row, which is why they are omitted rather than worded — an environment
	// has no upstream, and "unknown" would suggest it might.
	Upstream   string `json:"upstream,omitempty"`
	Digest     string `json:"digest,omitempty"`
	AdmittedBy string `json:"admittedBy,omitempty"`
	Signature  string `json:"signature,omitempty"`
	// Provenance is what the item's data derives from. Claims only — an
	// environment's data story is its claims' — and "undeclared" when the
	// provider said nothing.
	Provenance string `json:"provenance,omitempty"`
	Residency  string `json:"residency"`
}

// inventoryBody is the whole answer, exportable as it is: the rows, sorted
// stably, and the platform's declared default residency so a reader knows
// what an environment's "unknown" would fall back to if it were set.
type inventoryBody struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// DefaultResidency is the Kitchen object's declared residency — the
	// platform-wide answer an environment without one inherits. Declared,
	// not observed; empty when nobody has declared one.
	DefaultResidency string              `json:"defaultResidency,omitempty"`
	Items            []inventoryItemView `json:"items"`
}

// complianceInventory answers GET /api/v1/compliance/inventory: the caller's
// visible slice of the classification inventory. An operator reads the whole
// install; a member reads their projects' rows — the same filtering every
// cross-project read applies.
func (s *Server) complianceInventory(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	scope := scopeFrom(ctx)

	defaultResidency := ""
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err == nil {
		defaultResidency = kitchen.Spec.Residency
	}

	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := s.Client.List(ctx, environments, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := s.Client.List(ctx, claims, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}

	items := []inventoryItemView{}
	for i := range environments.Items {
		env := &environments.Items[i]
		if !scope.allows(env.Spec.ProjectRef.Name) {
			continue
		}
		residency := env.Spec.Residency
		if residency == "" {
			residency = defaultResidency
		}
		items = append(items, inventoryItemView{
			Kind:      "environment",
			Project:   env.Spec.ProjectRef.Name,
			Name:      env.Name,
			Type:      string(env.Spec.Type),
			DataClass: orWord(string(env.Spec.DataClass), inventoryUnclassified),
			Residency: orWord(residency, inventoryUnknown),
		})
	}
	// The outsourcing rows: what each environment is actually running that
	// this platform did not build. They are keyed off the deployed release
	// rather than off the project's declaration, because a declaration says
	// what is wanted and a Release says what is running — and it is the
	// second one an auditor is asking about.
	items = append(items, s.vendoredInventory(ctx, environments.Items, scope, defaultResidency)...)

	for i := range claims.Items {
		claim := &claims.Items[i]
		if !scope.allows(claim.Spec.ProjectRef.Name) {
			continue
		}
		items = append(items, inventoryItemView{
			Kind:      "claim",
			Project:   claim.Spec.ProjectRef.Name,
			Name:      claim.Name,
			Type:      claim.Spec.Type,
			DataClass: orWord(string(claim.Spec.DataClass), inventoryUnclassified),
			// The claim's provenance is what its provider declared (#138);
			// until a declaration exists it reads undeclared, loudly.
			Provenance: orWord(claimProvenance(claim), inventoryUndeclared),
			// A claim's residency is the provider's reported placement — the
			// actual one, which is why it does not fall back to the declared
			// platform default the way an environment's does.
			Residency: orWord(claim.Status.Residency, inventoryUnknown),
		})
	}

	// A stable order is what makes two exports diffable: project, then kind
	// (claims after environments), then name.
	sort.Slice(items, func(i, j int) bool {
		if items[i].Project != items[j].Project {
			return items[i].Project < items[j].Project
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Name < items[j].Name
	})

	writeJSON(w, http.StatusOK, inventoryBody{
		GeneratedAt:      time.Now().UTC(),
		DefaultResidency: defaultResidency,
		Items:            items,
	})
}

// vendoredInventory is one row per image running in an environment that
// somebody else built.
//
// It walks environment → release → build because that is where the adoption
// record is: the Build is still the object an acquisition is recorded as
// (#306), so `status.artifact.upstream` is the one place that says which
// upstream reference this digest came from and who admitted it. A degraded
// read — a pruned build, a release that is gone — drops the row rather than
// inventing one, and the environment's own row is still there saying
// something runs here.
func (s *Server) vendoredInventory(
	ctx context.Context,
	environments []kitchenv1alpha1.Environment,
	scope projectScope,
	defaultResidency string,
) []inventoryItemView {
	releases := &kitchenv1alpha1.ReleaseList{}
	if err := s.Client.List(ctx, releases, client.InNamespace(s.Namespace)); err != nil {
		return nil
	}
	builds := &kitchenv1alpha1.BuildList{}
	if err := s.Client.List(ctx, builds, client.InNamespace(s.Namespace)); err != nil {
		return nil
	}
	releaseByName := map[string]*kitchenv1alpha1.Release{}
	for i := range releases.Items {
		releaseByName[releases.Items[i].Name] = &releases.Items[i]
	}
	buildByName := map[string]*kitchenv1alpha1.Build{}
	for i := range builds.Items {
		buildByName[builds.Items[i].Name] = &builds.Items[i]
	}

	rows := []inventoryItemView{}
	for i := range environments {
		env := &environments[i]
		if !scope.allows(env.Spec.ProjectRef.Name) || env.Spec.ReleaseRef.Name == "" {
			continue
		}
		release, held := releaseByName[env.Spec.ReleaseRef.Name]
		if !held {
			continue
		}
		build, held := buildByName[release.Spec.BuildRef.Name]
		if !held {
			continue
		}
		residency := env.Spec.Residency
		if residency == "" {
			residency = defaultResidency
		}
		for _, artifact := range build.VendoredArtifacts() {
			row := inventoryItemView{
				Kind:    "vendoredImage",
				Project: env.Spec.ProjectRef.Name,
				// The environment and which of its images: a unit ships
				// several and a row that named only the project would
				// describe a mixed unit as wholly outsourced.
				Name: env.Name + "/" + artifact.Name(),
				Type: "image",
				// An image's data facts are the environment's it runs in:
				// nothing is declared about an image, and an outsourcing
				// inventory that said "unclassified" beside a component
				// handling confidential data would be answering the wrong
				// question.
				DataClass: orWord(string(env.Spec.DataClass), inventoryUnclassified),
				Residency: orWord(residency, inventoryUnknown),
				Digest:    artifact.Artifact.Digest,
			}
			if upstream := artifact.Artifact.Upstream; upstream != nil {
				row.Upstream = upstream.Reference
				row.AdmittedBy = orWord(upstream.AdmittedBy, inventoryUnknown)
				row.Signature = orWord(string(upstream.Signature.Result), inventoryUnknown)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// orWord answers the value, or the stated absence.
func orWord(value, absent string) string {
	if value == "" {
		return absent
	}
	return value
}

// claimProvenance is the claim's declared data provenance — the provider's
// statement, recorded on the status at bind (#138). Empty means the provider
// declared nothing, which the caller words as "undeclared".
func claimProvenance(claim *kitchenv1alpha1.ResourceClaim) string {
	return claim.Status.DataProvenance
}

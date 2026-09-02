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
	"encoding/json"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The objectStore half of the claim API: a bucket from an
// objectStore-capable Connection, optionally versioned, publicly readable or
// held to a size, with a fresh bucket per preview Environment.

// objectStoreClaimShaper is the claimShaper for type objectStore.
type objectStoreClaimShaper struct{}

func (objectStoreClaimShaper) fields() []claimField {
	return []claimField{
		{
			name:  "objectStore",
			set:   func(body *createClaimRequest) bool { return body.ObjectStore != nil },
			lacks: "no versioning, no public reads and no size",
		},
	}
}

// config is spec.config as this API writes it for an objectStore claim: the
// objectStore block the provisioner reads, nothing when nothing was asked.
func (objectStoreClaimShaper) config(w http.ResponseWriter, body *createClaimRequest) (*runtime.RawExtension, bool) {
	cfg, ok := validObjectStoreConfig(w, body.ObjectStore)
	if !ok {
		return nil, false
	}
	if cfg == nil {
		return nil, true
	}
	raw, err := json.Marshal(struct {
		ObjectStore *kitchenv1alpha1.ObjectStoreConfig `json:"objectStore"`
	}{cfg})
	if err != nil {
		badRequest(w, "%s", err.Error())
		return nil, false
	}
	return &runtime.RawExtension{Raw: raw}, true
}

func (objectStoreClaimShaper) view(claim *kitchenv1alpha1.ResourceClaim, view *claimView) {
	view.ObjectStore = objectStoreOf(claim)
}

func (objectStoreClaimShaper) deletionOutcome(claim *kitchenv1alpha1.ResourceClaim) string {
	if claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete {
		return "the bucket, its objects and its credential are deleted at the store"
	}
	return "the bucket and its objects are kept at the store"
}

// claimObjectStoreView is what the claim asked its bucket to be, as it
// answered it.
type claimObjectStoreView struct {
	Versioning bool   `json:"versioning,omitempty"`
	PublicRead bool   `json:"publicRead,omitempty"`
	Size       string `json:"size,omitempty"`
}

// objectStoreOf is the claim's bucket requirements, and nothing at all for
// a claim that asked for nothing.
func objectStoreOf(claim *kitchenv1alpha1.ResourceClaim) *claimObjectStoreView {
	cfg := claim.ObjectStore()
	if !cfg.Versioning && !cfg.PublicRead && cfg.Size == "" {
		return nil
	}
	return &claimObjectStoreView{Versioning: cfg.Versioning, PublicRead: cfg.PublicRead, Size: cfg.Size}
}

// validObjectStoreConfig checks the shape of what an objectStore claim asks
// of its bucket: a size that is a quantity. Whether the store can honour any
// of it — public reads at a store nothing outside the cluster reaches, a
// quota at a store with no admin API — is the provisioner's to answer, and
// it lands on the claim's status as a refusal naming what could not be
// supplied.
func validObjectStoreConfig(
	w http.ResponseWriter,
	cfg *kitchenv1alpha1.ObjectStoreConfig,
) (*kitchenv1alpha1.ObjectStoreConfig, bool) {
	if cfg == nil {
		return nil, true
	}
	out := kitchenv1alpha1.ObjectStoreConfig{
		Versioning: cfg.Versioning,
		PublicRead: cfg.PublicRead,
		Size:       strings.TrimSpace(cfg.Size),
	}
	if out.Size != "" {
		quantity, err := resource.ParseQuantity(out.Size)
		if err != nil {
			badRequest(w, "objectStore.size is a Kubernetes quantity — \"50Gi\" (got %q): %s", cfg.Size, err.Error())
			return nil, false
		}
		if quantity.Sign() <= 0 {
			badRequest(w, "objectStore.size must be more than nothing (got %q)", cfg.Size)
			return nil, false
		}
	}
	if !out.Versioning && !out.PublicRead && out.Size == "" {
		return nil, true
	}
	return &out, true
}

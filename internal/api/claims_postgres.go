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
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The postgres half of the claim API: a database from a database-capable
// Connection, optionally naming the Postgres version, extensions and storage
// it needs, and branched per preview Environment when asked.

// postgresClaimShaper is the claimShaper for type postgres.
type postgresClaimShaper struct{}

func (postgresClaimShaper) fields() []claimField {
	return []claimField{
		{
			name:  "previewBranching",
			set:   func(body *createClaimRequest) bool { return body.PreviewBranching },
			lacks: "no branch per preview",
		},
		{
			name:  "postgres",
			set:   func(body *createClaimRequest) bool { return body.Postgres != nil },
			lacks: "no version, no extensions and no volume",
		},
	}
}

// config is spec.config as this API writes it for a postgres claim: the
// platform's previewBranching, and the postgres block the provisioner reads.
func (postgresClaimShaper) config(w http.ResponseWriter, body *createClaimRequest) (*runtime.RawExtension, bool) {
	postgres, ok := validPostgresConfig(w, body.Postgres)
	if !ok {
		return nil, false
	}
	if !body.PreviewBranching && postgres == nil {
		return nil, true
	}
	raw, err := json.Marshal(claimConfigBody{
		PreviewBranching: body.PreviewBranching,
		Postgres:         postgres,
	})
	if err != nil {
		badRequest(w, "%s", err.Error())
		return nil, false
	}
	return &runtime.RawExtension{Raw: raw}, true
}

func (postgresClaimShaper) view(claim *kitchenv1alpha1.ResourceClaim, view *claimView) {
	view.Postgres = postgresOf(claim)
}

func (postgresClaimShaper) deletionOutcome(claim *kitchenv1alpha1.ResourceClaim) string {
	if claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete {
		return "the database is deprovisioned"
	}
	return "the database is kept at the provider"
}

// claimConfigBody is spec.config as this API writes it. It is the platform's
// own slice of that object — the plugin's half is what the provisioner reads —
// and it is spelled here rather than reused from the CRD package because the
// CRD's copy is unexported on purpose: what is written into a RawExtension is
// the API's contract with its callers, and it should have to change on
// purpose.
type claimConfigBody struct {
	PreviewBranching bool                            `json:"previewBranching,omitempty"`
	Postgres         *kitchenv1alpha1.PostgresConfig `json:"postgres,omitempty"`
}

// claimPostgresView is the claim's database requirements as it answered
// them, flattened one level so a caller reads storage without a nested
// object it never has to build.
type claimPostgresView struct {
	Version      string   `json:"version,omitempty"`
	Extensions   []string `json:"extensions,omitempty"`
	StorageSize  string   `json:"storageSize,omitempty"`
	StorageClass string   `json:"storageClass,omitempty"`
}

// postgresOf is the claim's database requirements, and nothing at all for a
// claim that asked for nothing.
func postgresOf(claim *kitchenv1alpha1.ResourceClaim) *claimPostgresView {
	cfg := claim.Postgres()
	if cfg.Version == "" && len(cfg.Extensions) == 0 &&
		cfg.Storage.Size == "" && cfg.Storage.StorageClass == "" {
		return nil
	}
	return &claimPostgresView{
		Version:      cfg.Version,
		Extensions:   cfg.Extensions,
		StorageSize:  cfg.Storage.Size,
		StorageClass: cfg.Storage.StorageClass,
	}
}

// validPostgresConfig checks the shape of what a postgres claim asks of its
// database, and normalizes it: an empty block is nothing rather than an empty
// object on the spec.
//
// Only shape. Whether the version exists and whether an extension can be
// supplied is the provisioner's to answer against the images its Connection
// was configured with, and the answer lands on the claim's status as a
// failure naming what could not be supplied. The division matters: this layer
// refuses what is not a version, that layer refuses what is not available.
func validPostgresConfig(
	w http.ResponseWriter,
	cfg *kitchenv1alpha1.PostgresConfig,
) (*kitchenv1alpha1.PostgresConfig, bool) {
	if cfg == nil {
		return nil, true
	}
	out := kitchenv1alpha1.PostgresConfig{
		Version: strings.TrimSpace(cfg.Version),
		Storage: kitchenv1alpha1.PostgresStorage{
			Size:         strings.TrimSpace(cfg.Storage.Size),
			StorageClass: strings.TrimSpace(cfg.Storage.StorageClass),
		},
	}
	if out.Version != "" {
		major, err := strconv.Atoi(out.Version)
		if err != nil || major < 9 || major > 99 {
			badRequest(w, "postgres.version is a major version and nothing else — \"17\", not %q. Which majors "+
				"this connection can actually serve is the connection's answer, and a version it cannot serve "+
				"fails the claim with the list", cfg.Version)
			return nil, false
		}
	}
	for _, extension := range cfg.Extensions {
		extension = strings.TrimSpace(extension)
		if extension == "" {
			continue
		}
		if !extensionNamePattern.MatchString(extension) {
			badRequest(w, "postgres.extensions are the identifiers CREATE EXTENSION takes — letters, digits "+
				"and underscores (got %q)", extension)
			return nil, false
		}
		out.Extensions = append(out.Extensions, extension)
	}
	if out.Storage.Size != "" {
		quantity, err := resource.ParseQuantity(out.Storage.Size)
		if err != nil {
			badRequest(w, "postgres.storage.size is a Kubernetes quantity — \"10Gi\" (got %q): %s",
				cfg.Storage.Size, err.Error())
			return nil, false
		}
		if quantity.Sign() <= 0 {
			badRequest(w, "postgres.storage.size must be more than nothing (got %q)", cfg.Storage.Size)
			return nil, false
		}
	}
	if out.Storage.StorageClass != "" {
		if errs := validation.IsDNS1123Subdomain(out.Storage.StorageClass); len(errs) > 0 {
			badRequest(w, "postgres.storage.storageClass must be a StorageClass name (got %q): %s",
				cfg.Storage.StorageClass, strings.Join(errs, "; "))
			return nil, false
		}
	}
	if out.Version == "" && len(out.Extensions) == 0 && out.Storage.Size == "" && out.Storage.StorageClass == "" {
		return nil, true
	}
	return &out, true
}

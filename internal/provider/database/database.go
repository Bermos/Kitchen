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

// Package database abstracts database provisioners behind a small interface
// so Connections can plug in different implementations, the way
// internal/gitprovider does for git hosting. The operator matches on the
// Connection's capabilities; this package supplies the database behavior a
// ResourceClaim is bound through.
//
// The interface is deliberately generic (docs/SCOPE.md, open decision 2):
// identifiers are opaque strings the implementation mints, so a provisioner
// does not have to be a cloud API. Neon is the implementation that ships;
// CloudNativePG is the second consumer the shape is held against, mapping as:
//
//   - Provision: create a Cluster custom resource and wait for it, returning
//     the namespaced name as the instance ID and the credentials cnpg writes
//     into its app Secret as the Binding.
//   - Deprovision: delete the Cluster, which drops its storage.
//   - CreateBranch: a Cluster bootstrapped from the parent (pg_basebackup or
//     a backup object), returning its own namespaced name as the branch ID —
//     a heavier copy than Neon's copy-on-write branch, behind the same verbs.
//   - DeleteBranch: delete that Cluster.
//
// cnpg stays unimplemented until its operator is something the platform
// installs — `cnpg` is deliberately not in the Connection provider enum, and
// Default answers ErrUnsupportedProvider for everything it cannot actually
// provision.
package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// ErrUnsupportedProvider is returned by Default for providers without an
// implementation yet.
var ErrUnsupportedProvider = errors.New("unsupported database provider")

// DataProvenance is a provisioner's declaration of what the data in a
// provisioned resource derives from. It is the provider contract's compliance
// half: production-derived data in a preview environment is the finding an
// auditor reaches first, and the way to make it a state the system cannot
// reach is for the *provider* to say what it handed over — the platform
// records the declaration, attests it, and the policy engine enforces it.
//
// The zero value is deliberate: a provisioner that cannot declare returns ""
// (undeclared), and undeclared data is usable only where policy would accept
// production data — the unknown is treated as the worst case, never as clean.
type DataProvenance string

const (
	// ProvenanceProduction: the data IS production data, or derives from it —
	// a fresh production database, and equally a branch or clone of one. A
	// copy-on-write branch of a production database is production-derived,
	// however cheap the copy was.
	ProvenanceProduction DataProvenance = "production"
	// ProvenanceMasked: derived from production data through a masking or
	// anonymization step the provider vouches for.
	ProvenanceMasked DataProvenance = "masked"
	// ProvenanceSynthetic: generated data that never derived from production —
	// a fresh empty database included.
	ProvenanceSynthetic DataProvenance = "synthetic"
)

// Binding is everything an application needs to reach a provisioned database.
// The fields become the keys of the claim's binding Secret verbatim: url,
// host, port, user, password, database.
type Binding struct {
	// URL is the single-string form (postgresql://...); the other fields are
	// the same connection taken apart for applications that want pieces.
	URL      string
	Host     string
	Port     string
	User     string
	Password string
	Database string
}

// Instance is one provisioned database resource.
type Instance struct {
	// ID is the provider-side identifier the other operations address the
	// instance by. Opaque to the caller: a cloud project ID, a namespaced
	// object name — whatever the implementation can find it again under.
	ID string
	// Binding reaches the instance's primary branch.
	Binding Binding
	// Provenance declares what the instance's data derives from. Empty means
	// the provider cannot say (undeclared) — see DataProvenance for what that
	// costs the claim at policy time.
	Provenance DataProvenance
	// Region is where the provider actually placed the instance, in the
	// provider's own vocabulary (a Neon region id). Empty when the provider
	// reports no placement. It is recorded on the claim's status as the
	// placement of record — reported, not declared.
	Region string
}

// Branch is a copy of an instance's data under its own address, cheap where
// the provider supports copy-on-write and merely possible where it does not.
type Branch struct {
	// ID is the provider-side branch identifier, opaque like Instance.ID.
	ID string
	// Binding reaches the branch instead of the primary.
	Binding Binding
	// Provenance declares what the branch's data derives from — for a branch
	// of a production database, production, because a branch is the parent's
	// data under a new address. A provisioner that masks or synthesizes while
	// branching declares masked or synthetic instead; empty means it cannot
	// say.
	Provenance DataProvenance
}

// Provisioner is a database provider bound to one Connection.
//
// All operations are idempotent by name or tolerant of absence: Provision and
// CreateBranch return the existing instance or branch when one under that
// name is already there (a reconcile may run twice), and the two Delete
// operations treat already-absent as success.
//
// The results carry the data-class half of the contract. Provision and
// CreateBranch declare, on the Instance or Branch they return, what the data
// derives from (Provenance) and — when the provider knows — where it actually
// is (Region). Both are optional in the weakest sense only: a provisioner
// that returns them empty has declared nothing, and its claims are then
// usable only in environments whose policy would accept production data. A
// third-party provisioner that masks or synthesizes is implemented by
// returning masked or synthetic here; nothing else in the platform needs to
// know it exists. See docs/CRDS.md, "The provider contract".
type Provisioner interface {
	// Provision creates (or finds) the database instance of the given name
	// and returns its binding.
	Provision(ctx context.Context, name string) (Instance, error)
	// Deprovision destroys the instance and its data.
	Deprovision(ctx context.Context, instanceID string) error
	// CreateBranch creates (or finds) a branch of the instance's data under
	// the given name and returns its binding.
	CreateBranch(ctx context.Context, instanceID, name string) (Branch, error)
	// DeleteBranch removes a branch and its data.
	DeleteBranch(ctx context.Context, instanceID, branchID string) error
}

// Factory builds a Provisioner for a Connection. The token comes from the
// Connection's credentials secret.
type Factory func(conn *kitchenv1alpha1.Connection, token string) (Provisioner, error)

// Default resolves the built-in providers.
func Default(conn *kitchenv1alpha1.Connection, token string) (Provisioner, error) {
	switch conn.Spec.Provider {
	case "neon":
		apiURL := DefaultNeonAPIURL
		if conn.Spec.Config != nil {
			var cfg struct {
				APIURL string `json:"apiUrl"`
			}
			if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
				return nil, fmt.Errorf("invalid neon config: %w", err)
			}
			if cfg.APIURL != "" {
				apiURL = cfg.APIURL
			}
		}
		return &Neon{APIURL: apiURL, Token: token}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProvider, conn.Spec.Provider)
	}
}

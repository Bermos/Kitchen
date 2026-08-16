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
}

// Branch is a copy of an instance's data under its own address, cheap where
// the provider supports copy-on-write and merely possible where it does not.
type Branch struct {
	// ID is the provider-side branch identifier, opaque like Instance.ID.
	ID string
	// Binding reaches the branch instead of the primary.
	Binding Binding
}

// Provisioner is a database provider bound to one Connection.
//
// All operations are idempotent by name or tolerant of absence: Provision and
// CreateBranch return the existing instance or branch when one under that
// name is already there (a reconcile may run twice), and the two Delete
// operations treat already-absent as success.
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

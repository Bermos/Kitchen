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
// The interface is deliberately generic (docs/SCOPE.md, decision 2):
// identifiers are opaque strings the implementation mints, so a provisioner
// does not have to be a cloud API. Two implementations ship:
//
//   - Neon, a SaaS Postgres reached over HTTP with an API token.
//   - CloudNativePG, which provisions into the cluster Kitchen is installed
//     in, so that a database needs no account anywhere. Provision creates a
//     Cluster custom resource and returns its namespaced name as the instance
//     ID, with the credentials cnpg writes into its app Secret as the
//     Binding; Deprovision deletes the Cluster, which takes its volumes with
//     it; CreateBranch and DeleteBranch are a preview's own Cluster.
//
// The one place the two disagree in kind rather than in degree is branching,
// and the disagreement is deliberate. Neon's branch is a copy-on-write copy
// of the parent's data, and so declares ProvenanceProduction. cnpg has no
// copy-on-write anything, and a preview is not worth a pg_basebackup of a
// production database: its Cluster is a *fresh, empty* database, and it
// declares ProvenanceSynthetic. That is the true answer and the useful one —
// it keeps production data out of previews by construction rather than by
// policy.
//
// Capabilities are the other thing an in-cluster provisioner can answer that
// a SaaS one cannot. A claim names a Postgres major version and the
// extensions its first migration will call for; CapableProvisioner resolves
// those to an image before it creates anything, and refuses — ErrUnsatisfiable
// — when no image it can reach supplies one. The refusal is the feature: a
// claim that cannot be satisfied should fail as a claim, with a message,
// rather than as a CREATE EXTENSION in a crash loop three minutes later.
package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// ErrUnsupportedProvider is returned by Default for providers without an
// implementation yet.
var ErrUnsupportedProvider = errors.New("unsupported database provider")

// ErrUnsatisfiable marks a claim no image the provisioner can reach can
// serve: a Postgres major version it does not publish, or an extension none
// of its images ships. It is a refusal rather than a failure — nothing was
// created, and retrying without changing the claim will refuse again — which
// is why the reconciler lands it on the claim as a claim failure with the
// message attached, instead of requeueing forever.
var ErrUnsatisfiable = errors.New("claim cannot be satisfied")

// ErrNotReady marks a provisioned resource that exists but is not serving
// yet. It is the opposite of ErrUnsatisfiable: nothing is wrong, the answer
// is simply not there yet, and the reconciler holds the claim Pending and
// looks again. A database the platform runs itself takes minutes to come up,
// where a SaaS API answers in one request — which is the whole reason this
// error exists.
var ErrNotReady = errors.New("not ready yet")

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

// Requirements are what a claim asks of its database beyond a name: which
// Postgres it wants, what the application will call for once it is up, and
// how much room it needs. They come off the claim's spec.config, which is why
// they are plain strings — the platform reads them, the provisioner resolves
// them, and a provisioner that cannot answer them says so before it creates
// anything.
type Requirements struct {
	// Version is the Postgres major, "17" or "16". Empty takes the
	// provisioner's own default, which is what most claims do.
	Version string
	// Extensions the application needs to be able to use — "postgis",
	// "vector", "pg_trgm". They are resolved to an image that ships them and
	// created in the database at bootstrap, so an application never needs the
	// right to CREATE EXTENSION itself.
	Extensions []string
	// StorageSize is a Kubernetes quantity ("10Gi"). Empty takes the
	// provisioner's default.
	StorageSize string
	// StorageClass names the class the volume is cut from. Empty takes the
	// cluster's default StorageClass, which Kitchen requires anyway.
	StorageClass string
}

// Empty reports whether the claim asked for nothing in particular, in which
// case any provisioner can serve it.
func (r Requirements) Empty() bool {
	return r.Version == "" && len(r.Extensions) == 0 && r.StorageSize == "" && r.StorageClass == ""
}

// CapableProvisioner is a Provisioner that can be asked for capabilities and
// not merely for a database — the shape internal/gitprovider uses for
// StatusReporter, and for the same reason: it is a real difference between
// implementations rather than a method every one of them has to fake.
//
// A provisioner that does not implement it is asked nothing, and a claim that
// names requirements it cannot carry is refused by the reconciler rather than
// provisioned as though the requirements had not been written down.
type CapableProvisioner interface {
	Provisioner
	// ProvisionWith is Provision with the claim's requirements applied. It
	// resolves them to an image *before* it creates anything and answers an
	// error wrapping ErrUnsatisfiable when it cannot, naming what it could
	// not supply and what it could.
	ProvisionWith(ctx context.Context, name string, req Requirements) (Instance, error)
}

// Options is what a Provisioner is built from. It is a struct rather than an
// argument list because the two implementations need different halves of it:
// a SaaS provisioner needs a token and never touches the cluster, and an
// in-cluster one is the other way round.
type Options struct {
	// Connection the claim provisions through.
	Connection *kitchenv1alpha1.Connection
	// Token from the Connection's credentials secret, empty for a provider
	// that has no credential because it provisions into this cluster with the
	// operator's own account.
	Token string
	// Cluster is the platform's own cluster. Nil for the providers that never
	// touch it; an in-cluster provisioner is refused without it.
	Cluster client.Client
	// Namespace the in-cluster provisioners put their objects in.
	Namespace string
}

// Factory builds a Provisioner for a Connection.
type Factory func(opts Options) (Provisioner, error)

// The provider names the Connection enum admits for a database.
const (
	// ProviderNeon is Postgres as somebody else's service.
	ProviderNeon = "neon"
	// ProviderCNPG is Postgres in this cluster, run by CloudNativePG. It is
	// the one database provider with no credential: the platform provisions
	// with the operator's own account, into the cluster it was installed in.
	ProviderCNPG = "cnpg"
)

// Default resolves the built-in providers.
func Default(opts Options) (Provisioner, error) {
	conn := opts.Connection
	switch conn.Spec.Provider {
	case ProviderNeon:
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
		return &Neon{APIURL: apiURL, Token: opts.Token}, nil
	case ProviderCNPG:
		return NewCNPG(opts)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProvider, conn.Spec.Provider)
	}
}

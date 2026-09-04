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

// Package cache is the contract of the redis claim type: somewhere an
// application can put what it can afford to recompute, and somewhere it can
// put work it cannot afford to lose. Those are opposite configurations of
// the same server, which is why this contract's requirement is not the
// version.
//
// It is shaped after internal/provider/database: a Binding whose fields
// become the keys of the claim's Secret, typed Requirements a provisioner
// refuses before creating anything when it cannot honour them, and a
// Provisioner whose identifiers are opaque.
//
// Two implementations ship:
//
//   - Valkey, one instance per claim in the cluster Kitchen is installed in,
//     written directly as a StatefulSet, a Service and a Secret. No operator,
//     no CRD, no admission webhook and no install job — which is why this
//     contract was the cheapest place to find out whether the vocabulary
//     generalises past Postgres.
//   - An external server behind a redis Connection: Upstash, ElastiCache,
//     Aiven, or the Valkey a team already runs.
//
// The in-cluster provisioner gives one instance per claim, never a logical
// database number or a key prefix inside a shared server. A database number
// is not an isolation boundary — one tenant's FLUSHALL empties another's,
// there is no per-tenant memory limit, and keyspaces collide — and the
// requirement that matters here cannot be satisfied in a shared server at
// all: maxmemory-policy is server-wide, so one instance cannot offer
// noeviction to a queue and allkeys-lru to a cache.
//
// At a server somebody else runs there is no choice: nobody can ask another
// team's Redis for a second process, so the external provisioner hands out
// logical databases — one per claim and one per preview, allocated and
// recorded rather than hashed out of a name, and refused when the server has
// none left. What that buys is written down where a person choosing a
// connection reads it (docs/api/claims.md): the separation is logical and
// not cryptographic, because every claim through one Connection is handed
// the same password.
package cache

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// ErrUnsupportedProvider is returned by Default for a provider this package
// has no provisioner for.
var ErrUnsupportedProvider = errors.New("unsupported cache provider")

// ErrUnsatisfiable marks a claim the provisioner cannot honour as asked: a
// queue against a server configured to evict, a version nothing publishes an
// image for, a memory limit a hosted plan does not let anybody set. Nothing
// was created and retrying without changing the claim refuses again, so the
// reconciler lands it on the claim as a failure with the message attached.
//
// It is the point of this contract. A Sidekiq or BullMQ Redis configured
// with LRU eviction drops jobs under memory pressure and reports nothing:
// the queue is empty, the work is gone, and the application is none the
// wiser. Refusing the claim is how that stops being possible.
var ErrUnsatisfiable = errors.New("claim cannot be satisfied")

// ErrNotReady marks an instance that exists and is not serving yet — a
// Valkey the cluster is still starting. Nothing is wrong; the reconciler
// holds the claim Pending and looks again.
var ErrNotReady = errors.New("not ready yet")

// Usage is what the application intends to do with the instance, and the one
// requirement that decides how it is configured.
type Usage string

const (
	// UsageCache is data that can be recomputed: the instance evicts the
	// least recently used key when it fills up (allkeys-lru) and keeps
	// nothing on disk. Losing a key is a miss, and nobody notices.
	UsageCache Usage = "cache"

	// UsageQueue is work that cannot be recomputed: the instance refuses
	// writes when it fills up (noeviction) and appends every write to disk.
	// Memory pressure fails the enqueue — loudly, where the application can
	// retry — instead of silently dropping a job.
	UsageQueue Usage = "queue"
)

// Usages is every value, for the refusals that list them.
var Usages = []Usage{UsageCache, UsageQueue}

// Known reports whether the value is one of the two.
func (u Usage) Known() bool { return u == UsageCache || u == UsageQueue }

// Durable reports whether this usage needs what it holds to survive a
// restart — which is what decides whether the instance gets a volume.
func (u Usage) Durable() bool { return u == UsageQueue }

// DataProvenance is the provisioner's declaration of what an instance's data
// derives from, on the same terms as the other contracts'.
type DataProvenance string

const (
	// ProvenanceProduction: the instance is production's own.
	ProvenanceProduction DataProvenance = "production"
	// ProvenanceSynthetic: a fresh, empty instance that never held
	// production's keys — every preview's.
	ProvenanceSynthetic DataProvenance = "synthetic"
)

// Binding is everything an application needs to reach its instance. The
// fields become the keys of the claim's binding Secret verbatim.
type Binding struct {
	// URL is the single-string form (redis://:password@host:port), which is
	// what every client library takes; the other fields are the same
	// connection taken apart for the ones that want pieces.
	URL      string
	Host     string
	Port     string
	Password string
	// Database is the logical database the binding selects, as a number in
	// a string: "0" at an instance of the claim's own, the one allocated to
	// this claim at a server shared with other claims. It is in the URL as
	// well; it is a field of its own because a client given the host, the
	// port and the password alone connects to database 0, which at a shared
	// server is somebody else's keyspace.
	Database string
	// TLS says whether the connection is encrypted — "true" or "false" as
	// the Secret carries it. An application that assumes wrong fails to
	// connect at all, which is the good failure, but it should not have to
	// guess.
	TLS bool
}

// The keys of the binding Secret, spelled once.
const (
	BindingKeyURL      = "url"
	BindingKeyHost     = "host"
	BindingKeyPort     = "port"
	BindingKeyPassword = "password"
	BindingKeyDatabase = "database"
	BindingKeyTLS      = "tls"
)

// Data is the binding as the Secret carries it.
func (b Binding) Data() map[string][]byte {
	tls := "false"
	if b.TLS {
		tls = "true"
	}
	database := b.Database
	if database == "" {
		database = "0"
	}
	return map[string][]byte{
		BindingKeyURL:      []byte(b.URL),
		BindingKeyHost:     []byte(b.Host),
		BindingKeyPort:     []byte(b.Port),
		BindingKeyPassword: []byte(b.Password),
		BindingKeyDatabase: []byte(database),
		BindingKeyTLS:      []byte(tls),
	}
}

// Instance is one provisioned cache or queue.
type Instance struct {
	// ID is the provider-side identifier the other operations address the
	// instance by. Opaque: a namespaced object name for the in-cluster
	// provisioner, the server's own address for an external one.
	ID string
	// Name is what the provisioner called the instance, recorded on the
	// claim so that a resource provisioned under one naming rule keeps its
	// name when the rule changes; see internal/provider/naming.
	Name string
	// Binding reaches the instance.
	Binding Binding
	// Provenance declares what the instance's data derives from.
	Provenance DataProvenance
	// Region is where the provider reports the instance to be; empty when it
	// reports nothing.
	Region string
}

// Branch is a preview's own instance: empty, configured like the parent,
// torn down with the preview.
type Branch struct {
	ID         string
	Binding    Binding
	Provenance DataProvenance
}

// Provisioner is a cache provider bound to one Connection.
//
// All operations are idempotent by name or tolerant of absence: Provision
// and CreateBranch return the instance already under that name, and the two
// Delete operations treat already-absent as success. Both may answer
// ErrNotReady while an instance the cluster has to start is coming up.
type Provisioner interface {
	// Provision creates (or finds) the claim's instance and returns its
	// binding. The provisioner names it — naming.Resolve out of the claim's
	// project and its own budget — rather than being told what to call it.
	Provision(ctx context.Context, res naming.Resource) (Instance, error)
	// Deprovision destroys the instance and everything in it.
	Deprovision(ctx context.Context, instanceID string) error
	// CreateBranch creates (or finds) a preview's own instance beside the
	// claim's and returns its binding.
	CreateBranch(ctx context.Context, instanceID, name string) (Branch, error)
	// DeleteBranch removes a preview's instance.
	DeleteBranch(ctx context.Context, instanceID, branchID string) error
}

// Requirements are what a claim asks of its instance beyond a name.
type Requirements struct {
	// Usage decides the eviction policy and whether anything is written to
	// disk. Empty takes the provisioner's default, which is cache — the
	// safe default, because a cache that turns out to be a queue loses work
	// where a queue that turns out to be a cache only costs a volume.
	Usage Usage
	// MaxMemory is a Kubernetes quantity ("512Mi") the instance may not grow
	// past. Empty takes the provisioner's default.
	MaxMemory string
	// Version is the Valkey major, "8". Empty takes the provisioner's own
	// default, which is what most claims do.
	Version string
}

// Empty reports whether the claim asked for nothing in particular, in which
// case any provisioner can serve it.
func (r Requirements) Empty() bool {
	return r.Usage == "" && r.MaxMemory == "" && r.Version == ""
}

// CapableProvisioner is a Provisioner that can be asked for requirements —
// the same optional-interface shape internal/gitprovider uses, and for the
// same reason: it is a real difference between implementations rather than a
// method every one of them has to fake.
type CapableProvisioner interface {
	Provisioner
	// ProvisionWith is Provision with the claim's requirements applied. It
	// answers an error wrapping ErrUnsatisfiable, before creating anything,
	// when it cannot honour one.
	ProvisionWith(ctx context.Context, res naming.Resource, req Requirements) (Instance, error)
}

// The provider names the Connection enum admits for a cache.
const (
	// ProviderValkey is a cache this cluster runs: one Valkey per claim,
	// written by the provisioner itself. It is the second Connection
	// provider with no credential — it provisions with the operator's own
	// account, into the cluster Kitchen was installed in.
	ProviderValkey = "valkey"

	// ProviderRedis is a server somebody else runs, reached over the URL its
	// Connection holds: Upstash, ElastiCache, Aiven, or the Valkey a team
	// already has.
	ProviderRedis = "redis"
)

// The keys an external server's Connection credential Secret carries.
const (
	CredentialKeyURL = "url"
)

// Declarations is what each cache provider says about itself before it has
// provisioned anything, written next to Default so that a provider and its
// declaration are added together.
var Declarations = map[string]contract.Declaration{
	ProviderValkey: {
		Preview: contract.PreviewFresh,
		PreviewNote: "a new, empty instance of the preview's own, configured like production's and torn " +
			"down with the preview: the branch declares provenance synthetic",
	},
	ProviderRedis: {
		Preview: contract.PreviewFresh,
		PreviewNote: "a logical database of the preview's own at the same server, allocated to it alone and " +
			"handed back when the preview closes: the branch declares provenance synthetic — it never holds " +
			"production's keys, though a server the platform does not run cannot be emptied, so a database is " +
			"handed out again only once every untouched one is gone",
	},
}

// Options is what a Provisioner is built from. It is a struct rather than an
// argument list because the two implementations need different halves of it:
// an external server needs a URL and never touches the cluster, and the
// in-cluster one is the other way round.
type Options struct {
	// Connection the claim provisions through.
	Connection *kitchenv1alpha1.Connection
	// URL of an external server, from its credentials Secret. Empty for the
	// in-cluster provider, which has no credential because it provisions
	// with the operator's own account.
	URL string
	// Cluster is the platform's own cluster. Nil for a provider that never
	// touches it; the in-cluster one is refused without it.
	Cluster client.Client
	// Namespace the in-cluster provisioner puts its instances in.
	Namespace string
}

// Factory builds a Provisioner for a Connection.
type Factory func(opts Options) (Provisioner, error)

// Default resolves the built-in providers.
func Default(opts Options) (Provisioner, error) {
	conn := opts.Connection
	if conn == nil {
		return nil, fmt.Errorf("%w: no connection", ErrUnsupportedProvider)
	}
	switch conn.Spec.Provider {
	case ProviderValkey:
		return NewValkey(opts)
	case ProviderRedis:
		return NewExternal(opts)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProvider, conn.Spec.Provider)
	}
}

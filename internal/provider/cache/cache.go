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
//   - Valkey, in the cluster Kitchen is installed in, written directly as a
//     StatefulSet, a Service and a Secret. No operator, no CRD, no admission
//     webhook and no install job — which is why this contract was the
//     cheapest place to find out whether the vocabulary generalises past
//     Postgres.
//   - An external server behind a redis Connection: Upstash, ElastiCache,
//     Aiven, or the Valkey a team already runs.
//
// # Two tenancies, chosen by requirement
//
// A platform whose selling point is an environment per pull request cannot
// afford a server per claim per environment on the single-node clusters it
// supports: four claims and five open pull requests is twenty-four Valkeys.
// So the in-cluster provider gives a claim a *tenancy* in a server the
// platform already runs, and only creates a server of its own when the claim
// asks for something a shared one cannot give.
//
// A tenancy is an ACL user restricted to a key prefix, with a password of
// its own:
//
//	ACL SETUSER kitchen-shop-jobs reset on >… ~kitchen-shop-jobs:* &kitchen-shop-jobs:* +@all -@admin -@dangerous +info
//
// That is a real boundary and a different one from a logical database
// number, which is why the number was rejected and this was not: the tenant
// cannot read, write or subscribe outside its own prefix, and FLUSHALL,
// FLUSHDB, KEYS, SWAPDB and CONFIG are not commands it has. The binding
// carries the prefix (BindingKeyKeyPrefix) because the application has to write
// under it — every client library has a prefix option, and this is the cost
// of the shape.
//
// The eviction policy is what the old objection was really about, and it is
// answered by *which* server a tenancy goes in rather than by giving up:
// maxmemory-policy is server-wide, so the platform runs one shared server
// per usage — an evicting one for caches, a write-refusing one with an
// append log for queues — and a claim's usage picks the server. What a
// shared server genuinely cannot give is a per-tenant memory limit, and a
// claim naming maxMemory is therefore given a server of its own. That is the
// whole of the resolution: see ResolveTenancy.
//
// What is honestly given up by sharing is the failure domain. One shared
// server is one process to lose, and on a queue server one tenant filling
// memory fails every tenant's writes rather than only its own. A claim that
// cannot accept that says tenancy: dedicated and is given the old shape
// back, unchanged.
package cache

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
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

// Tenancy is which of the two shapes a claim is served by, and the answer
// #275 settled: shared tenancy is not a second provisioning *interface* —
// Instance.ID was always opaque, and a tenancy handle is exactly what it can
// hold — it is a second implementation behind the same one.
type Tenancy string

const (
	// TenancyShared is a tenancy in a server the platform already runs: an
	// ACL user restricted to a key prefix, with a password of its own. The
	// default, because a server per claim per environment does not fit on
	// the clusters this platform supports.
	TenancyShared Tenancy = "shared"

	// TenancyDedicated is a server of the claim's own. What a shared server
	// cannot give — a memory limit that is this claim's alone, a version
	// nothing else at the installation runs, a failure domain of one — is
	// what this is for.
	TenancyDedicated Tenancy = "dedicated"
)

// Tenancies is every value, for the refusals that list them.
var Tenancies = []Tenancy{TenancyShared, TenancyDedicated}

// Known reports whether the value is one of the two.
func (t Tenancy) Known() bool { return t == TenancyShared || t == TenancyDedicated }

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
	// Username is the ACL user the connection authenticates as, and it is
	// how a tenancy is told apart from a server of one's own: a tenant has
	// one, a dedicated instance authenticates as the server's default user
	// and leaves this empty. A client given host, port and password alone
	// would authenticate as default and be refused, which is why it is a
	// key of its own rather than only a piece of the URL.
	Username string
	// KeyPrefix is what every key this connection may touch has to start
	// with — empty for a dedicated instance, which owns its whole keyspace.
	// It is not advisory: the tenant's ACL admits no key outside it, so an
	// application that ignores it is refused on its first write. Every
	// client library has a prefix option to set it in one place.
	KeyPrefix string
	// TLS says whether the connection is encrypted — "true" or "false" as
	// the Secret carries it. An application that assumes wrong fails to
	// connect at all, which is the good failure, but it should not have to
	// guess.
	TLS bool
}

// The keys of the binding Secret, spelled once.
const (
	BindingKeyURL       = "url"
	BindingKeyHost      = "host"
	BindingKeyPort      = "port"
	BindingKeyPassword  = "password"
	BindingKeyUsername  = "username"
	BindingKeyKeyPrefix = "keyPrefix"
	BindingKeyTLS       = "tls"
)

// Data is the binding as the Secret carries it.
func (b Binding) Data() map[string][]byte {
	tls := "false"
	if b.TLS {
		tls = "true"
	}
	return map[string][]byte{
		BindingKeyURL:       []byte(b.URL),
		BindingKeyHost:      []byte(b.Host),
		BindingKeyPort:      []byte(b.Port),
		BindingKeyPassword:  []byte(b.Password),
		BindingKeyUsername:  []byte(b.Username),
		BindingKeyKeyPrefix: []byte(b.KeyPrefix),
		BindingKeyTLS:       []byte(tls),
	}
}

// Instance is one provisioned cache or queue.
type Instance struct {
	// ID is the provider-side identifier the other operations address the
	// instance by. Opaque: a namespaced object name for the in-cluster
	// provisioner, the server's own address for an external one.
	ID string
	// Binding reaches the instance.
	Binding Binding
	// Provenance declares what the instance's data derives from.
	Provenance DataProvenance
	// Region is where the provider reports the instance to be; empty when it
	// reports nothing.
	Region string
	// Tenancy is which shape the provisioner actually served the claim
	// with, and TenancyNote is the sentence behind it. Both are recorded on
	// the claim: whether a dependency is alone on a server or sharing one is
	// a fact about its failure domain, and the platform states it rather
	// than leaving it to be inferred from the shape of the ID.
	Tenancy     Tenancy
	TenancyNote string
}

// Branch is a preview's own instance or tenancy: empty, configured like the
// parent, torn down with the preview.
type Branch struct {
	ID          string
	Binding     Binding
	Provenance  DataProvenance
	Tenancy     Tenancy
	TenancyNote string
}

// Provisioner is a cache provider bound to one Connection.
//
// All operations are idempotent by name or tolerant of absence: Provision
// and CreateBranch return the instance already under that name, and the two
// Delete operations treat already-absent as success. Both may answer
// ErrNotReady while an instance the cluster has to start is coming up.
type Provisioner interface {
	// Provision creates (or finds) the instance of the given name and
	// returns its binding.
	Provision(ctx context.Context, name string) (Instance, error)
	// Release takes back the access and leaves the data. It runs when the
	// claim goes, under either deletionPolicy, and is what keeps a tenancy
	// in a server other projects are reading from outliving the claim that
	// asked for it: the ACL user is deleted, the keys are not. For a server
	// of the claim's own there is nothing to take back — the credential
	// admits nobody to anything the claim did not already own — and it is a
	// no-op.
	//
	// It is deliberately not Deprovision under another name: destroying
	// data is what deletionPolicy Delete opts into, and releasing a claim
	// must never imply it.
	Release(ctx context.Context, instanceID string) error
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
	// Tenancy is the claim's own choice of shape, and empty is the usual
	// case: the provisioner resolves one from what else the claim asked
	// for. Naming it is how a claim insists — on a failure domain of its
	// own, or on not costing the cluster a server.
	Tenancy Tenancy
}

// Empty reports whether the claim asked for nothing in particular, in which
// case any provisioner can serve it.
func (r Requirements) Empty() bool {
	return r.Usage == "" && r.MaxMemory == "" && r.Version == "" && r.Tenancy == ""
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
	ProvisionWith(ctx context.Context, name string, req Requirements) (Instance, error)
}

// The provider names the Connection enum admits for a cache.
const (
	// ProviderValkey is a cache this cluster runs, written by the
	// provisioner itself: a tenancy in a server the platform already keeps,
	// or a server of the claim's own where it asked for one. It is the
	// second Connection provider with no credential — it provisions with the
	// operator's own account, into the cluster Kitchen was installed in.
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
		PreviewNote: "a new, empty keyspace of the preview's own — its own ACL user and key prefix in the " +
			"platform's shared server, or an instance of its own where the claim asked for one — configured " +
			"like production's and torn down with the preview: the branch declares provenance synthetic",
	},
	ProviderRedis: {
		Preview: contract.PreviewFresh,
		PreviewNote: "a new, empty logical database of the preview's own at the same server, torn down with " +
			"the preview: the branch declares provenance synthetic",
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
	// Keyspaces dials a running server to make and unmake tenancies in it.
	// Nil takes the real client; the tests inject a server that answers
	// without one running, which is the same seam objectstore's AdminAPI is.
	Keyspaces KeyspaceFactory
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

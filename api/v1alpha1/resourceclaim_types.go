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

package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClaimDeletionPolicy says what happens to the provisioned resource when its
// claim is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type ClaimDeletionPolicy string

const (
	// ClaimRetain keeps the provisioned resource at the provider. Only the
	// platform's own bookkeeping — binding Secrets and preview branches — is
	// removed with the claim.
	ClaimRetain ClaimDeletionPolicy = "Retain"
	// ClaimDelete deprovisions the resource, destroying its data.
	ClaimDelete ClaimDeletionPolicy = "Delete"
)

// Claim types, which are what decides who provisions the resource.
const (
	// ClaimTypePostgres is a database from a database-capable Connection.
	ClaimTypePostgres = "postgres"

	// ClaimTypeOIDCClient is an OAuth client at the platform's own identity
	// provider, so that a deployed application signs its users in with the
	// same accounts the dashboard uses. It is a claim type with no
	// Connection: the provider is the issuer the platform is already
	// configured with, and the operator registers the client with the
	// service credential it holds for it.
	ClaimTypeOIDCClient = "oidcClient"

	// ClaimTypeObjectStore is a bucket from an objectStore-capable
	// Connection: somewhere an application can put a file it did not build
	// into its image — user uploads, generated exports, anything it writes
	// and expects to read back. The bundled MinIO, or any S3-compatible
	// store somebody else runs.
	ClaimTypeObjectStore = "objectStore"
	// ClaimTypeVolume is a persistent volume mounted into one of the
	// project's processes, for the workload that writes where it was told
	// to write rather than where a cloud-native rewrite would put it. It is
	// the odd contract: every other one produces credentials, this one
	// produces a mount. It takes no Connection — the provider is the
	// cluster's StorageClass, which Kitchen requires of every cluster
	// anyway — and it shares the claim lifecycle (deletionPolicy, preview
	// teardown, dataClass) and none of the binding-Secret machinery.
	ClaimTypeVolume = "volume"
	// ClaimTypeInngest is an Inngest app: durable background work — retries,
	// sleeps, fan-out, concurrency limits and cron — run by Inngest, with
	// the application's worker holding an outbound connection to it. The
	// claim binds the keys a worker connects with, read from the Inngest
	// account behind a backgroundJobs-capable Connection.
	ClaimTypeInngest = "inngest"

	// ClaimTypeRedis is a Redis-speaking cache or queue from a
	// cache-capable Connection: the Valkey this cluster runs one of per
	// claim, or a server somebody else runs. What it is *for* — a cache
	// that may evict, or a queue that must not — is the claim's own
	// requirement and the one that decides how it is configured.
	ClaimTypeRedis = "redis"
)

// ClaimType is what the platform knows about one kind of claim before any
// provisioner is involved: what a Connection has to be able to do for it,
// what to call the thing it provisions, and whether deleting it can destroy
// data.
//
// It is a table rather than a set of switch statements because the same
// three facts are needed in four places — the CRD's admission rules, the
// claim reconciler, the REST API and the dashboard — and a type that is
// registered once cannot be half-added. The CRD's `type` enum and its two
// CEL rules are markers, which cannot read a Go value, so a test holds them
// to this table instead (see resourceclaim_types_test.go).
type ClaimType struct {
	// Name is the value of spec.type.
	Name string

	// Capability is what a Connection must be able to do to provision this
	// type, and the reconciler matches Connections on it — never on a
	// provider name. Empty for a type the platform provisions itself, which
	// is exactly the set of types that take no connectionRef.
	Capability Capability

	// Resource is the noun for what the claim provisions — "database",
	// "OAuth client" — in every sentence the platform writes about it.
	Resource string

	// HoldsData says whether the provisioned resource holds data that
	// spec.deletionPolicy exists to protect. A type that does not — an OAuth
	// client holds permission to sign people in, not data — is always
	// deprovisioned with its claim, and refuses a deletionPolicy.
	HoldsData bool
}

// TakesConnection reports whether a claim of this type names a Connection.
func (t ClaimType) TakesConnection() bool { return t.Capability != "" }

// ClaimTypes is every kind of claim the platform admits. A new type is a row
// here, a contract package beside internal/provider/database, and a
// registration in the reconciler and the API; the tests on each of those
// refuse a row without its registration and a registration without its row.
var ClaimTypes = []ClaimType{
	{Name: ClaimTypePostgres, Capability: CapabilityDatabase, Resource: "database", HoldsData: true},
	{Name: ClaimTypeOIDCClient, Resource: "OAuth client"},
	{Name: ClaimTypeObjectStore, Capability: CapabilityObjectStore, Resource: "bucket", HoldsData: true},
	{Name: ClaimTypeVolume, Resource: "volume", HoldsData: true},
	// An Inngest app holds no data the platform could destroy: event history
	// and function runs live at Inngest under the account's own retention,
	// the keys are the account's, and a preview's branch environment is
	// archived rather than deleted — archiving deletes nothing there. So
	// deleting the claim always takes back only what the platform put into
	// the world, and deletionPolicy has nothing to say.
	{Name: ClaimTypeInngest, Capability: CapabilityBackgroundJobs, Resource: "Inngest app"},
	{Name: ClaimTypeRedis, Capability: CapabilityCache, Resource: "cache", HoldsData: true},
}

// LookupClaimType finds a claim type by the value of spec.type.
func LookupClaimType(name string) (ClaimType, bool) {
	for _, t := range ClaimTypes {
		if t.Name == name {
			return t, true
		}
	}
	return ClaimType{}, false
}

// ClaimTypeNames is every admitted spec.type, in table order — what the
// CRD's enum has to be, and what a refusal lists.
func ClaimTypeNames() []string {
	names := make([]string, 0, len(ClaimTypes))
	for _, t := range ClaimTypes {
		names = append(names, t.Name)
	}
	return names
}

// ClaimTypesWithoutConnection is the set of types the platform provisions
// itself — the set the CRD's connectionRef rules are written against.
func ClaimTypesWithoutConnection() []string {
	names := make([]string, 0, len(ClaimTypes))
	for _, t := range ClaimTypes {
		if !t.TakesConnection() {
			names = append(names, t.Name)
		}
	}
	return names
}

// Type is the claim's ClaimType, and false for a type the table does not
// know — which the CRD's enum refuses at admission, so it is reachable only
// by an object written before the type was removed.
func (c *ResourceClaim) Type() (ClaimType, bool) {
	return LookupClaimType(c.Spec.Type)
}

// ResourceClaimSpec is a Project's request for a provisioned resource — a
// Postgres database from a capable Connection, or an OAuth client from the
// platform's own identity provider.
//
// The two rules below are written against the set of types that take no
// Connection — ClaimTypesWithoutConnection — rather than against a named
// exception, and the refusal names the type it refused. The set in the
// markers is held to the table by a test, since a marker cannot read one.
// +kubebuilder:validation:XValidation:rule="self.type in ['oidcClient', 'volume'] || has(self.connectionRef)",message="connectionRef is required: it names the Connection that provisions the resource. Only a claim the platform provisions itself goes without one.",messageExpression="'connectionRef is required: it names the Connection that provisions a ' + self.type + ' claim. Only a claim of a type the platform provisions itself (oidcClient, volume) goes without one.'"
// +kubebuilder:validation:XValidation:rule="!(self.type in ['oidcClient', 'volume']) || !has(self.connectionRef)",message="this claim type takes no connectionRef: the platform provisions it itself, and a Connection here would name a provider nothing would ask.",messageExpression="'a ' + self.type + ' claim takes no connectionRef: the platform provisions it itself, and a Connection here would name a provider nothing would ask.'"
type ResourceClaimSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	// Connection with a capability matching Type (e.g. database). Required
	// for every type that takes one, and refused on the types the platform
	// provisions itself — see ClaimTypes.
	// +optional
	ConnectionRef *LocalObjectReference `json:"connectionRef,omitempty"`

	// Kind of resource requested. It is immutable: the type decides who
	// provisions the resource and what the binding Secret holds, so changing
	// it on a bound claim would leave a database behind while the
	// application's environment quietly started reading OAuth credentials
	// out of the same keys. Ask for the other one and delete this.
	// +kubebuilder:validation:Enum=postgres;oidcClient;objectStore;volume;inngest;redis
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable: delete the claim and ask for the other kind"
	Type string `json:"type"`

	// DeletionPolicy is what deleting the claim does to the provisioned
	// resource. Retain is the default because a claim can own a production
	// database: destroying data has to be opted into, never implied by
	// removing the platform object in front of it. Preview branches and the
	// binding Secrets are cleaned up under either policy.
	//
	// It says nothing about an oidcClient claim, whose client is always
	// deregistered: the policy exists to protect data from a deletion nobody
	// meant, and an OAuth client holds none — what it holds is permission to
	// sign people in, which is the thing that must not outlive the claim.
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy ClaimDeletionPolicy `json:"deletionPolicy,omitempty"`

	// Provider-specific configuration, validated by the plugin.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Config *runtime.RawExtension `json:"config,omitempty"`

	// DataClass is the sensitivity classification of the data this claim's
	// resource holds. It may not exceed the project's own class — children
	// narrow a classification, never widen it — which the API enforces when
	// the claim is created. Absent means unclassified, surfaced as such and
	// never defaulted.
	// +optional
	DataClass DataClass `json:"dataClass,omitempty"`
}

// claimConfig is the contract-agnostic slice of spec.config the platform
// itself reads. Everything else in there belongs to one contract, which
// reads its own slice through DecodeConfig — this struct names no contract,
// so that adding one is a new package rather than a new field here.
type claimConfig struct {
	// PreviewMode is the claim's own choice of what its previews bind to,
	// against what the provider declares it can give them: the provider's
	// declared mode, "shared" — the production resource itself, which is
	// why it has to be asked for by name — or "none". Empty takes the
	// provider's declaration, unless that is shared and the type holds
	// data, in which case previews get nothing and the status says why.
	PreviewMode string `json:"previewMode,omitempty"`

	// PreviewBranching is what the choice used to be called, before a
	// provider declared what a branch even was: true asked for a resource
	// of the preview's own, and absent quietly gave previews production.
	// It is still read, as the provider's own mode, so a claim written
	// under the old name keeps its branches.
	PreviewBranching bool `json:"previewBranching,omitempty"`
}

// DecodeConfig reads spec.config into v, and reports whether there was a
// readable config to decode. A claim with no config, or one the platform
// cannot read, leaves v as it was and answers false: the API validates a
// config before it is written, and a claim that reached the cluster another
// way is better provisioned plainly than not at all.
//
// It is the one door to spec.config. A contract reads its own slice through
// it — the postgres block, the oidcClient block — and the platform reads
// claimConfig, and neither has to know what else is in there.
func (c *ResourceClaim) DecodeConfig(v any) bool {
	if c.Spec.Config == nil || len(c.Spec.Config.Raw) == 0 {
		return false
	}
	return json.Unmarshal(c.Spec.Config.Raw, v) == nil
}

// PostgresConfig is the `postgres` slice of a postgres claim's spec.config:
// what the application needs of the database beyond its existence.
//
// It exists because "Postgres" is not one thing. An application that needs
// PostGIS, pgvector or a time-series extension has otherwise no way to ask
// for one: the claim binds, the URL arrives, and the application dies on a
// CREATE EXTENSION in its first migration — which is the wrong end of the
// process to find out at. Naming it here means the provisioner resolves it to
// an image before it creates anything, and a claim it cannot satisfy fails as
// a claim, with a message saying what could not be supplied.
//
// Everything in here is applied when the database is *created*. A major
// version is not something to change under a live Postgres and a volume is
// not something to shrink, so editing this on a bound claim does not re-image
// or re-cut anything: asking for a different database means asking for a
// different database.
type PostgresConfig struct {
	// Version is the Postgres major, as a number: "17". Empty takes the
	// platform's default, which is what most claims want.
	// +optional
	Version string `json:"version,omitempty"`

	// Extensions the application needs — "postgis", "vector", "pg_trgm".
	// They are created in the database when it is bootstrapped, as superuser,
	// so the application never needs the right to CREATE EXTENSION itself;
	// and an extension no image the platform can run supplies is a refusal
	// rather than a database that does not have it.
	// +optional
	Extensions []string `json:"extensions,omitempty"`

	// Storage is the volume behind the database.
	// +optional
	Storage PostgresStorage `json:"storage,omitempty"`
}

// PostgresStorage is the volume a self-hosted database is cut from. It means
// nothing to a provider that has no volumes — Neon bills by usage and is not
// asked.
type PostgresStorage struct {
	// Size is a Kubernetes quantity: "10Gi". Empty takes the platform's
	// default.
	// +optional
	Size string `json:"size,omitempty"`

	// StorageClass the volume comes from. Empty takes the cluster's default
	// StorageClass, which Kitchen requires of every cluster anyway.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
}

// ObjectStoreConfig is the `objectStore` slice of an objectStore claim's
// spec.config: what the application needs of its bucket beyond its
// existence.
//
// Every field is applied when the bucket is *created*, and a provider that
// cannot honour one refuses the claim before it creates anything — the
// refusal names what could not be supplied, which is the whole point of
// asking here rather than finding out from an application that assumed its
// uploads were versioned.
type ObjectStoreConfig struct {
	// Versioning keeps every version of an object rather than the latest
	// alone, so an overwrite or a delete can be undone at the store.
	// +optional
	Versioning bool `json:"versioning,omitempty"`

	// PublicRead lets anyone read the bucket's objects without a credential
	// — a bucket that serves images straight to browsers. Only a store that
	// is actually on the internet can honour it; the bundled one is reached
	// inside the cluster alone and refuses it, saying so.
	// +optional
	PublicRead bool `json:"publicRead,omitempty"`

	// Size is how much the bucket may hold, as a Kubernetes quantity:
	// "50Gi". It is a hint the provider turns into a quota where it can set
	// one, and refuses where it cannot; empty asks for no limit.
	// +optional
	Size string `json:"size,omitempty"`
}

// RedisConfig is the `redis` slice of a redis claim's spec.config: what the
// instance has to be, beyond its existence.
//
// `usage` is the field this contract exists for. A cache and a queue are
// opposite configurations of the same server — one evicts the least recently
// used key when it fills up, the other refuses the write — and a queue
// served by an evicting instance drops jobs under memory pressure and
// reports nothing. Naming it here means the provisioner applies it when the
// instance is created, and a provider that cannot honour it refuses the
// claim rather than binding something that will lose work.
//
// Everything here is applied when the instance is *created*. Changing the
// usage of a live instance would mean reconfiguring a server somebody's
// application is already reading, so asking for a different instance means
// asking for a different instance.
type RedisConfig struct {
	// Usage is what the application intends to do with it: `cache` for what
	// it can recompute, `queue` for work it cannot. Empty takes the
	// provisioner's default, which is `cache` — the safe one, because a
	// cache that turns out to be a queue loses work where a queue that
	// turns out to be a cache only costs a volume.
	// +kubebuilder:validation:Enum=cache;queue
	// +optional
	Usage string `json:"usage,omitempty"`

	// MaxMemory is a Kubernetes quantity ("512Mi") the instance may not
	// grow past. Empty takes the platform's default.
	// +optional
	MaxMemory string `json:"maxMemory,omitempty"`

	// Version is the Valkey major, as a number: "8". Empty takes the
	// platform's default. A version no image the platform can run supplies
	// is a refusal rather than an instance that is not it.
	// +optional
	Version string `json:"version,omitempty"`
}

// OIDCClientConfig is the oidcClient slice of spec.config: what the operator
// registers the client with, and what it keeps its redirect list made of.
type OIDCClientConfig struct {
	// CallbackPaths are appended to every URL the project's Environments are
	// reachable at to build the client's redirect list. They are paths and
	// not URLs because that is the whole point of the type: the operator owns
	// the URLs, and a preview that does not exist yet cannot be written down.
	// +optional
	CallbackPaths []string `json:"callbackPaths,omitempty"`

	// RedirectURIs are registered verbatim alongside the generated ones, for
	// the addresses the platform does not own — a developer's
	// http://localhost:3000/auth/callback being the whole of why this exists.
	// +optional
	RedirectURIs []string `json:"redirectURIs,omitempty"`

	// Scopes the client may ask for.
	// +optional
	Scopes []string `json:"scopes,omitempty"`
}

// DefaultOIDCCallbackPaths is where an application is assumed to receive the
// authorization code when the claim does not say.
//
// Two of them, because there is no one answer and the cost of a redirect URI
// nothing serves is nothing: every generated URI is on the application's own
// origin, so the worst an unused one can do is land a code the application
// never asked for on a page it does not have. The first is the plain
// convention; the second is what Auth.js builds for a provider called
// kitchen, which is the framework most likely to be on the other end.
// Naming callbackPaths replaces both.
var DefaultOIDCCallbackPaths = []string{"/auth/callback", "/api/auth/callback/kitchen"}

// DefaultOIDCScopes is what a client is registered for when the claim does
// not say. offline_access is included because an application that cannot
// refresh a token signs its users out every hour.
var DefaultOIDCScopes = []string{"openid", "profile", "email", "offline_access"}

// VolumeConfig is the `volume` slice of a volume claim's spec.config: which
// process mounts the volume, where, and what the volume has to be.
//
// The process is the one thing a volume claim cannot go without. Every pod an
// Environment materializes — the web process, its workers, its scheduled runs
// — carries the environment label, and a ReadWriteOnce volume can be attached
// to one of them at a time; two mounting it is a Multi-Attach failure that
// looks like a rollout that never finishes. So the claim names one process,
// and only that process gets the mount. The web process is named "web" —
// the one name ProcessSpec refuses, for exactly this reason.
//
// Everything in here is applied when the volume is *created*, like a
// database's storage: a volume is not something to shrink, and a mount path
// that moves under a running process is a process that has lost its data.
type VolumeConfig struct {
	// Process is the project's process that mounts the volume: "web" for
	// the web process (Project.spec.runtime), or the name of one of
	// Project.spec.processes. Required; a claim naming none, or naming a
	// process the project does not have, is refused.
	Process string `json:"process"`

	// Size is a Kubernetes quantity: "10Gi". Required — a volume has no
	// sensible default size, and one that was defaulted is the one that
	// fills up first.
	Size string `json:"size"`

	// StorageClass the volume is cut from. Empty takes the cluster's
	// default StorageClass, which Kitchen requires of every cluster anyway.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// MountPath is the absolute path inside the process's container the
	// volume appears at: "/data".
	MountPath string `json:"mountPath"`
}

// WebProcessName is what a volume claim calls the web process — the one
// process a Project has that is not in spec.processes, and the one name
// ProcessSpec refuses, so that the two can never collide.
const WebProcessName = "web"

// Volume is what the claim asks of its volume, empty for a claim of another
// type or one that reached the cluster without a readable config — the API
// validates a config before it is written, and the reconciler refuses an
// empty one rather than guessing at a mount path.
func (c *ResourceClaim) Volume() VolumeConfig {
	if c.Spec.Type != ClaimTypeVolume {
		return VolumeConfig{}
	}
	var cfg struct {
		Volume *VolumeConfig `json:"volume,omitempty"`
	}
	if !c.DecodeConfig(&cfg) || cfg.Volume == nil {
		return VolumeConfig{}
	}
	return *cfg.Volume
}

// OIDCClient is the claim's OAuth client configuration with the defaults
// filled in. Config the platform cannot read gets the defaults whole — a
// malformed config is refused by the API before it is written, and a claim
// that reached the cluster another way is better registered conventionally
// than not at all.
func (c *ResourceClaim) OIDCClient() OIDCClientConfig {
	cfg := OIDCClientConfig{}
	if !c.DecodeConfig(&cfg) {
		cfg = OIDCClientConfig{}
	}
	if len(cfg.CallbackPaths) == 0 {
		cfg.CallbackPaths = DefaultOIDCCallbackPaths
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = DefaultOIDCScopes
	}
	return cfg
}

// InngestConfig is the `inngest` slice of an inngest claim's spec.config:
// which app the worker connects as, which Inngest environment production
// binds to, and how the worker reaches Inngest.
//
// Everything Inngest needs to know about the functions themselves is in the
// application: a connect worker syncs its functions when it connects, and
// the app comes into existence at Inngest the first time one does. So this
// block names things rather than creating them, and the platform's part is
// the keys — read from the account the Connection holds, for the named
// environment and, per preview, for a branch environment of the preview's
// own.
type InngestConfig struct {
	// App is the Inngest app ID the application's client is created with
	// (`new Inngest({ id })`). It is what the claim's status reports on —
	// whether that app has connected yet — and it is not something the
	// platform can set on the application's behalf. Empty takes the claim's
	// own name.
	// +optional
	App string `json:"app,omitempty"`

	// Environment is the Inngest environment production binds to: an
	// account's `production`, or a custom environment created in the
	// Inngest dashboard. Preview environments never bind to it — they get
	// a branch environment each. Empty means production.
	// +optional
	Environment string `json:"environment,omitempty"`

	// Mode is how the worker reaches Inngest. `connect` — the only mode the
	// platform provisions — has the worker hold an outbound WebSocket to
	// Inngest's gateway, which is what makes a protected preview work and
	// what stops the project idling. `serve`, where Inngest calls the
	// application over HTTP, is refused: the call would meet the preview
	// gate and get a login page, and the sync it needs would have the
	// platform hand Inngest a URL per deploy. Empty means connect.
	// +optional
	Mode string `json:"mode,omitempty"`
}

// InngestModeConnect is the one mode an inngest claim is provisioned in.
const InngestModeConnect = "connect"

// InngestDefaultEnvironment is the Inngest environment production binds to
// when the claim names none.
const InngestDefaultEnvironment = "production"

// Inngest is the claim's Inngest configuration with the defaults filled in:
// the claim's own name for the app, production for the environment, connect
// for the mode. Config the platform cannot read gets the defaults whole,
// for the same reason OIDCClient does.
func (c *ResourceClaim) Inngest() InngestConfig {
	var cfg struct {
		Inngest *InngestConfig `json:"inngest,omitempty"`
	}
	out := InngestConfig{}
	if c.DecodeConfig(&cfg) && cfg.Inngest != nil {
		out = *cfg.Inngest
	}
	if out.App == "" {
		out.App = c.Name
	}
	if out.Environment == "" {
		out.Environment = InngestDefaultEnvironment
	}
	if out.Mode == "" {
		out.Mode = InngestModeConnect
	}
	return out
}

// Connection is the Connection the claim provisions through, empty for a
// claim type that has none.
func (c *ResourceClaim) Connection() string {
	if c.Spec.ConnectionRef == nil {
		return ""
	}
	return c.Spec.ConnectionRef.Name
}

// PreviewChoice is what the claim asked its previews to bind to: a preview
// mode by name, or empty for the provider's own declaration. Config the
// platform cannot read counts as asking for nothing in particular — the
// API validates a config before it is written.
//
// The old previewBranching flag reads as no choice at all, deliberately: it
// asked for the preview's own resource, and that is what the provider's
// declaration now gives by default.
func (c *ResourceClaim) PreviewChoice() string {
	var cfg claimConfig
	if !c.DecodeConfig(&cfg) {
		return ""
	}
	return cfg.PreviewMode
}

// Postgres is what the claim asks of its database, empty for a claim of
// another type or one that asked for nothing in particular. Config the
// platform cannot read counts as asking for nothing — the API validates it
// before it is written, and a claim that reached the cluster another way is
// better provisioned plainly than not at all.
func (c *ResourceClaim) Postgres() PostgresConfig {
	if c.Spec.Type != ClaimTypePostgres {
		return PostgresConfig{}
	}
	var cfg struct {
		Postgres *PostgresConfig `json:"postgres,omitempty"`
	}
	if !c.DecodeConfig(&cfg) || cfg.Postgres == nil {
		return PostgresConfig{}
	}
	return *cfg.Postgres
}

// ObjectStore is what the claim asks of its bucket, empty for a claim of
// another type or one that asked for nothing in particular — read through
// the same door as the postgres slice, for the same reasons.
func (c *ResourceClaim) ObjectStore() ObjectStoreConfig {
	if c.Spec.Type != ClaimTypeObjectStore {
		return ObjectStoreConfig{}
	}
	var cfg struct {
		ObjectStore *ObjectStoreConfig `json:"objectStore,omitempty"`
	}
	if !c.DecodeConfig(&cfg) || cfg.ObjectStore == nil {
		return ObjectStoreConfig{}
	}
	return *cfg.ObjectStore
}

// Redis is what the claim asks of its instance, empty for a claim of
// another type or one that asked for nothing in particular. Config the
// platform cannot read counts as asking for nothing — the API validates it
// before it is written.
func (c *ResourceClaim) Redis() RedisConfig {
	if c.Spec.Type != ClaimTypeRedis {
		return RedisConfig{}
	}
	var cfg struct {
		Redis *RedisConfig `json:"redis,omitempty"`
	}
	if !c.DecodeConfig(&cfg) || cfg.Redis == nil {
		return RedisConfig{}
	}
	return *cfg.Redis
}

// ClaimPhase is the coarse lifecycle summary of a ResourceClaim.
// +kubebuilder:validation:Enum=Pending;Bound;Failed
type ClaimPhase string

const (
	ClaimPending ClaimPhase = "Pending"
	ClaimBound   ClaimPhase = "Bound"
	ClaimFailed  ClaimPhase = "Failed"
)

// ClaimBranch records one provider-side resource the claim created for a
// preview Environment — a database branch, a preview's own bucket — and the
// Secret its binding was written into.
// ClaimBranch records one provider-side branch the claim created for a
// preview Environment — a database branch, an Inngest branch environment —
// and the Secret its binding was written into.
type ClaimBranch struct {
	// Environment is the preview Environment the branch belongs to.
	Environment string `json:"environment"`

	// ID is the provider-side branch identifier.
	ID string `json:"id"`

	// SecretName is the per-environment binding Secret in the project
	// namespace.
	SecretName string `json:"secretName"`

	// Provenance is the provider's declaration of what this branch's data
	// derives from — production for a branch of a production database,
	// however cheap the copy. Empty means the provider declared nothing.
	// It is per-branch because it is the branch, not the primary, that a
	// preview's workload reads, and the policy engine judges the preview on
	// exactly this value.
	// +kubebuilder:validation:Enum=production;masked;synthetic
	// +optional
	Provenance string `json:"provenance,omitempty"`
}

// ClaimVolumeStatus is what a volume claim materialized: the
// PersistentVolumeClaim in the application namespace the named process
// mounts, and one more per preview Environment under preview mode fresh.
//
// It is a list of its own rather than a reuse of Branches, because a branch
// is a provider-side identifier with a binding Secret — required, and read
// by the environment reconciler as the preview's credentials — and a volume
// has neither: what a preview gets is a PersistentVolumeClaim with a name,
// mounted by the pod spec rather than read out of a Secret.
type ClaimVolumeStatus struct {
	// Process is the project's process that mounts the volume ("web" for
	// the web process), and MountPath where it appears in its container.
	// Both echo the claim's config, so a reader of the status does not have
	// to decode spec.config to know what the environment reconciler does.
	Process   string `json:"process"`
	MountPath string `json:"mountPath"`

	// StorageClass is the class the volume was actually cut from: the one
	// the claim named, or the cluster's default at the time.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// AccessMode is what the StorageClass was detected to support:
	// ReadWriteOnce, which caps the process at one replica and forces a
	// recreate on every deploy, or ReadWriteMany, which lifts both. It is
	// detected from the class's provisioner and its annotations, never
	// assumed; AccessModeReason says what decided it.
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany
	AccessMode string `json:"accessMode"`
	// +optional
	AccessModeReason string `json:"accessModeReason,omitempty"`

	// ClaimName is the production PersistentVolumeClaim, in the project's
	// application namespace.
	ClaimName string `json:"claimName"`

	// PersistentVolume is the PersistentVolume the production claim is
	// bound to, once it is. Under deletionPolicy Retain the reconciler
	// patches that volume's reclaim policy to Retain and labels it with the
	// claim, which is what lets it outlive the application namespace — and
	// what a later claim of the same name re-binds to.
	// +optional
	PersistentVolume string `json:"persistentVolume,omitempty"`

	// Previews are the per-preview PersistentVolumeClaims, one per live
	// preview Environment, each fresh and empty, torn down with the
	// preview.
	// +optional
	Previews []ClaimPreviewVolume `json:"previews,omitempty"`
}

// ClaimPreviewVolume is one preview Environment's own volume.
type ClaimPreviewVolume struct {
	// Environment is the preview Environment the volume belongs to.
	Environment string `json:"environment"`

	// ClaimName is the PersistentVolumeClaim in the application namespace.
	ClaimName string `json:"claimName"`

	// Provenance is what the volume's data derives from: synthetic, always,
	// because a preview's volume is created empty and never copied from
	// production.
	// +kubebuilder:validation:Enum=production;masked;synthetic
	// +optional
	Provenance string `json:"provenance,omitempty"`
}

// ResourceClaimStatus defines the observed state of a ResourceClaim.
type ResourceClaimStatus struct {
	// +optional
	Phase ClaimPhase `json:"phase,omitempty"`

	// Secret in the project namespace holding the binding keys
	// (url, host, password, ...), per environment where applicable.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// InstanceID is the provider-side identifier of the provisioned resource,
	// opaque to the platform. It is what deprovisioning and branch operations
	// address.
	// +optional
	InstanceID string `json:"instanceID,omitempty"`

	// Branches are the provider-side branches created for preview
	// Environments, torn down with them.
	// +optional
	Branches []ClaimBranch `json:"branches,omitempty"`

	// DataProvenance is the provider's declaration of what the provisioned
	// data derives from: production, masked or synthetic. Empty means the
	// provider declared nothing (undeclared) — surfaced as such, and treated
	// by policy as the worst case rather than as clean. It is a declaration
	// the platform records and attests (a signed
	// kitchen.bermos.dev DataClass/v1 statement), never something it
	// inspects the data to establish.
	// +kubebuilder:validation:Enum=production;masked;synthetic
	// +optional
	DataProvenance string `json:"dataProvenance,omitempty"`

	// Residency is where the provisioned resource actually is, as the
	// provider reported it — a Neon region id, for the provider that ships.
	// It is recorded from the provider's answer rather than from anything
	// declared, which is what makes it the placement of record: an
	// environment's residency says where data is meant to be, this says
	// where this resource's data is. Empty when the provider reports no
	// placement, and read as "unknown" by the inventory rather than
	// defaulted.
	// +optional
	Residency string `json:"residency,omitempty"`

	// PreviewMode is what a preview Environment of the claim's project binds
	// to, as the reconciler resolved it from the provider's declaration and
	// the claim's own choice: branch, fresh, shared or none. It is written
	// here rather than read off the provider each time because it is the
	// fact a preview's workload, the policy engine and the screen all act
	// on, and they should act on one answer. Empty until the claim has been
	// reconciled by an operator that declares.
	// +kubebuilder:validation:Enum=branch;fresh;shared;none
	// +optional
	PreviewMode string `json:"previewMode,omitempty"`

	// PreviewReason is why previews bind nothing, when PreviewMode is none,
	// and the sentence behind the mode otherwise — the provider's own words
	// about what a preview gets.
	// +optional
	PreviewReason string `json:"previewReason,omitempty"`

	// KeepsPodsRunning is the provider's declaration that this claim's
	// binding holds the workload up — a worker holding an outbound
	// connection, say — so no environment reading it idles to zero. The
	// environment reconciler reads it and says so on the environment.
	// +optional
	KeepsPodsRunning bool `json:"keepsPodsRunning,omitempty"`

	// ForcesRecreate is the provider's declaration that what it provisions
	// can be attached to one pod at a time, so the workload reading it is
	// deployed by recreation — stopped, then started — with a gap in serving
	// on every deploy. The environment reconciler reads it and sets the
	// strategy.
	// +optional
	ForcesRecreate bool `json:"forcesRecreate,omitempty"`

	// Volume is what a volume claim materialized, and empty for every other
	// type: a volume claim binds to a mount rather than to a Secret, so
	// SecretName stays empty on it and this is where the environment
	// reconciler reads what to mount.
	// +optional
	Volume *ClaimVolumeStatus `json:"volume,omitempty"`

	// RedirectURIs is the redirect list an oidcClient claim's client is
	// registered with, as the operator last wrote it. It is what a reconcile
	// compares the project's environment URLs against, so that a preview
	// appearing costs one call to the issuer and a reconcile that changes
	// nothing costs none.
	// +optional
	RedirectURIs []string `json:"redirectURIs,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ResourceClaim is the Schema for the resourceclaims API.
type ResourceClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourceClaimSpec   `json:"spec,omitempty"`
	Status ResourceClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ResourceClaimList contains a list of ResourceClaim.
type ResourceClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceClaim{}, &ResourceClaimList{})
}

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

// Package backup writes and reads the platform's own state: one gzipped tar
// carrying the Kitchen custom resources, the Secrets they depend on and the
// identity provider's database.
//
// Three stores hold everything Kitchen knows, and they need three different
// answers:
//
//   - **etcd**, through the CRDs, is the system of record for every Project,
//     Connection, Release and Environment (docs/SCOPE.md item 9). It is copied
//     out object by object.
//   - **The identity provider's Postgres** holds the accounts, sessions and
//     OAuth clients that deliberately do not live in CRDs, and is the one half
//     no sweep of custom resources can recover. See internal/accountsdb.
//   - **ClickHouse** holds telemetry, which is expendable and has a TTL. It is
//     *not* in the archive, and the manifest says so out loud — the thing an
//     operator must not discover during an incident is which of these three
//     they were not backing up.
//
// The credentials are the part that matters most and the part that is easiest
// to leave out. A restore without the Cloudflare token, the git app keys, the
// attestation signing key and the identity provider's own signing secret
// brings back a platform that cannot talk to anything, so the Secrets in the
// platform namespace travel with the objects that reference them. That makes
// the archive itself a credential: it is written to nowhere, kept nowhere, and
// only ever streamed to an operator who asked for it.
package backup

import (
	"time"
)

// Format is the archive layout's version. It goes in the manifest so that a
// reader can refuse an archive it does not understand, rather than restoring
// three quarters of one.
const Format = 1

// The paths inside the archive. They are a flat, obvious tree on purpose: an
// archive somebody has to restore by hand at three in the morning should be
// one `tar tzf` away from making sense.
const (
	// ManifestPath describes the archive: what it holds, what it does not,
	// and which release wrote it.
	ManifestPath = "manifest.json"
	// ResourcesDir holds one JSON document per custom resource, under the
	// plural name of its kind.
	ResourcesDir = "resources/"
	// SecretsDir holds the platform namespace's Secrets, one per file.
	SecretsDir = "secrets/"
	// AccountsDir holds the identity provider's database: a table listing and
	// one COPY-format file per table.
	AccountsDir = "accounts/"
	// AccountsManifestPath lists the tables, in the order they restore in.
	AccountsManifestPath = AccountsDir + "tables.json"
)

// maxArchiveBytes bounds what a restore will read. An archive is the platform's
// configuration and its accounts, not its telemetry or its images, so a
// quarter of a gigabyte is already far past anything real — and the reader
// holds it in memory, because tar is sequential and a restore needs the
// manifest before it knows what the rest of the entries are.
const maxArchiveBytes = 256 << 20

// Kind is one custom resource kind the archive carries.
type Kind struct {
	// Kind as the API server spells it.
	Kind string
	// Plural is the resource name, and the directory the objects go in.
	Plural string
	// ClusterScoped objects have no namespace to list within.
	ClusterScoped bool
}

// Kinds is every kitchen.bermos.dev kind in the archive, in the order a
// restore applies them: the platform's own configuration first, then the
// credentials projects name, then the projects, then everything that hangs off
// one. Nothing here owner-references anything else, so the order is a courtesy
// to whoever is watching the reconcilers catch up rather than a requirement.
//
// PlatformUpdate is deliberately absent. It is the upgrade history of a
// cluster that no longer exists by the time anyone is restoring, and every
// record in it names a Job that was reaped long ago.
//
// Promotion is deliberately absent too, and for a sharper reason: a
// promotion is a *request* to move an environment, and its status — the
// evaluated verdict — does not travel through a restore. Restored requests
// would arrive statusless, be re-evaluated, and re-apply themselves in
// whatever order the reconciler met them, racing each other to point every
// environment somewhere it pointed once. The outcome a restore should carry
// is already in it: `Environment.spec.releaseRef`. The decisions themselves
// live in the decision store and the audit log, which is where the history
// belongs.
//
// Exception IS carried, unlike Promotion, because it is a record rather than
// a request: a break-glass grant is part of the compliance register and must
// survive the cluster it was granted on. One caveat travels with that: phase
// is status, and status does not restore — an expired grant re-expires on the
// spot (expiry lives in spec), but a *resolved*, still-unexpired one comes
// back Active until somebody resolves it again. The resolution itself is in
// the audit log, which is the authoritative history either way.
var Kinds = []Kind{
	{Kind: "Kitchen", Plural: "kitchens", ClusterScoped: true},
	{Kind: "Connection", Plural: "connections"},
	{Kind: "Project", Plural: "projects"},
	{Kind: "Build", Plural: "builds"},
	{Kind: "Release", Plural: "releases"},
	{Kind: "Environment", Plural: "environments"},
	{Kind: "Domain", Plural: "domains"},
	{Kind: "ResourceClaim", Plural: "resourceclaims"},
	{Kind: "Exception", Plural: "exceptions"},
	{Kind: "SavedQuery", Plural: "savedqueries"},
}

// Manifest describes an archive. It is the first entry in the tar and the
// first thing a reader parses.
type Manifest struct {
	// Format is the layout version; see Format.
	Format int `json:"format"`

	// CreatedAt is when the export ran.
	CreatedAt time.Time `json:"createdAt"`

	// PlatformVersion is the release that wrote the archive. A restore into a
	// different one is refused unless it is asked for explicitly: the accounts
	// half is a data-only dump into a schema the identity provider migrates
	// for itself, so the two releases have to agree about what that schema is.
	PlatformVersion string `json:"platformVersion"`

	// ClusterName and BaseDomain identify the installation, so that an archive
	// found on a disk somewhere says which platform it came from before
	// anybody restores it over another one.
	ClusterName string `json:"clusterName,omitempty"`
	BaseDomain  string `json:"baseDomain,omitempty"`

	// Namespace the objects and secrets were read from.
	Namespace string `json:"namespace"`

	// Resources is how many objects of each kind the archive holds, keyed by
	// the plural name.
	Resources map[string]int `json:"resources"`

	// Secrets is how many Secrets travelled with them.
	Secrets int `json:"secrets"`

	// Accounts describes the identity provider's half.
	Accounts *AccountsSummary `json:"accounts,omitempty"`

	// AccountsMessage explains an archive with no accounts in it. An
	// installation brought up without an identity provider has none to take,
	// which is not a fault; a database that could not be reached is, and this
	// is where the difference is written down.
	AccountsMessage string `json:"accountsMessage,omitempty"`

	// Excluded is what this archive deliberately does not carry, in the words
	// somebody planning a recovery needs to read them in.
	Excluded []string `json:"excluded"`
}

// AccountsSummary is the identity provider's database, as the manifest
// reports it.
type AccountsSummary struct {
	Database string `json:"database"`
	Tables   int    `json:"tables"`
	Rows     int64  `json:"rows"`
}

// Excluded is what every archive leaves out, and why. It is written into the
// manifest rather than only into the documentation, because the manifest is
// what somebody reads when the documentation is on the cluster that died.
var Excluded = []string{
	"telemetry: logs, metrics, traces and flow data in ClickHouse are not backed up and are not " +
		"expected to survive. They already expire on their own retention.",
	"the audit log: it lives in the same ClickHouse and goes with it. Export it separately if a " +
		"retention policy requires it to outlive the cluster.",
	"container images: builds push them to a registry, which is backed up — or not — wherever it runs. " +
		"The bundled registry's volume is not in this archive.",
	"application data: databases a ResourceClaim provisioned belong to the provider that runs them. " +
		"The claim is restored; what it points at is that provider's to keep.",
	"the platform's upgrade history (PlatformUpdate objects), which describes a cluster that will not " +
		"exist by the time this is restored.",
	"Secrets outside the platform namespace: the registry pull credential each application namespace " +
		"holds is a copy the operator syncs, and it is written again on the next build.",
}

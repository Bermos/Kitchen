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

package controller

import (
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The catalogue of platform dependencies the operator can install.
//
// It is compiled in, and that is the whole design rather than a limitation.
// An install job is bound to an account that can apply CRDs and ClusterRoles;
// an entry arriving from a request — a repository URL, a chart name, a
// version, a values file — would make that grant unbounded and reduce the
// audit record to "the platform installed something". What an Addon
// contributes to the argv is a namespace, checked against a DNS label first.
//
// Two entries live here, and they were two near-identical files before it:
// 1,185 lines carrying one engine twice, whose second copy's own header said
// so. What differs between KEDA and CloudNativePG is the twelve fields below.
// What was the same is addon_controller.go.

// addonGrant is what the install job's account has to be able to do, declared
// per entry rather than assumed.
//
// The distinction is the one self-update and restore already draw in the
// chart: an account whose list of kinds is unbounded takes cluster-admin,
// because "a role that merely looks narrow while granting the power to write
// ClusterRoles is not narrow"; an account applying an enumerable list takes
// the enumerated role. A compiled catalogue entry is enumerable by
// construction — but only if the chart it installs is, and neither of today's
// is: both ship CRDs, ClusterRoles and, in CloudNativePG's case, a webhook
// configuration, and Kubernetes' own escalation prevention means an account
// that grants those must already hold them.
type addonGrant struct {
	// ClusterAdmin is whether the entry's account is bound to cluster-admin.
	ClusterAdmin bool
	// Because is why, in the words that belong in a review of the grant.
	Because string
}

// addonChart is one helm release an entry installs. An entry installs them in
// order, which is what an operator can express and a single Helm release
// cannot: the KEDA HTTP add-on ships a ScaledObject of KEDA's own CRD, so the
// CRD has to be established before the second chart is built at all.
type addonChart struct {
	// Release is upstream's own instruction's release name. Using anything
	// else would make an installation that later takes the release over by
	// hand harder to reason about, for no gain.
	Release string
	// Chart in the entry's repository.
	Chart string
	// DefaultVersion is the pin. Pinned rather than floated, and bumped by
	// reading whatever else the pin decides — see each entry's own file.
	DefaultVersion string
	// VersionLabel is where an install job records the version it installed,
	// so ownership survives the reconcile that read it.
	VersionLabel string
}

// addonEntry is one dependency the platform knows how to install.
type addonEntry struct {
	// ID is the entry's name and the Addon object's name, which is what
	// makes two Addons for one dependency impossible to write.
	ID string

	// Title and Summary are what a screen says about it.
	Title   string
	Summary string

	// Repository the charts are pulled from. Both of today's publish classic
	// HTTP repositories rather than OCI artifacts, so this is handed to helm
	// with --repo, which needs no `helm repo add` and so no writable cache.
	Repository string

	// Charts installed, in order.
	Charts []addonChart

	// Probe is the kind whose presence means the dependency is serving. A
	// CRD that is not installed is a RESTMapper no-match, not an error.
	Probe schema.GroupVersionKind

	// Partial is a kind that means somebody else installed half of this
	// entry — KEDA without its HTTP add-on. Installing over it is what helm
	// would find out half-way through, so the operator refuses and says what
	// to do instead. Nil for an entry with no such half-state.
	Partial *addonPartial

	// DefaultNamespace is where upstream's own documentation installs it.
	DefaultNamespace string

	// DependsOn names entries that must be Ready before this one installs.
	// The ordering is enforced by the reconciler and not by luck: an entry
	// whose dependency is not serving waits, with the dependency named.
	DependsOn []string

	// Component names the install job in collected logs, and is what the
	// dashboard filters on to show what helm said.
	Component string

	// Providers are the Connection providers this entry serves. They are
	// what an uninstall is refused over: a release nothing depends on can
	// go, one a claim provisions through cannot.
	Providers []string

	// BlastRadius is what stops working when the entry is uninstalled,
	// beyond the dependents that refuse it outright. It is stated before the
	// uninstall rather than discovered after it.
	BlastRadius string

	// ChartValue is the value that grants the operator an account to install
	// this entry with. It is named in the refusal, so that an installation
	// told "not permitted" is told the one thing that would permit it.
	ChartValue string

	// Grant is what that account can do, and why.
	Grant addonGrant

	// Timeout is what each of the entry's helm runs is given. They --wait, so
	// it is time for workloads to become ready and not merely for manifests
	// to be accepted.
	Timeout time.Duration
}

// addonPartial is an entry's half-installed state.
type addonPartial struct {
	// Probe is the kind that is served when only part of the entry is here.
	Probe schema.GroupVersionKind
	// Reason and Message say what was found and what to do about it.
	Reason  string
	Message string
}

// chartVersions is the entry's pins, keyed by chart name, as the operator
// compiles them in.
func (e addonEntry) chartVersions() map[string]string {
	versions := make(map[string]string, len(e.Charts))
	for _, chart := range e.Charts {
		versions[chart.Chart] = chart.DefaultVersion
	}
	return versions
}

// addonCatalogue is every entry, by ID. Entries register themselves from
// their own files, so adding one is adding a file rather than editing a table
// two branches are both appending to.
var addonCatalogue = map[string]addonEntry{}

// registerAddon adds an entry to the catalogue. It panics on a duplicate ID,
// which is a programming error caught at process start rather than a
// catalogue that silently holds one of two entries.
func registerAddon(entry addonEntry) {
	if _, taken := addonCatalogue[entry.ID]; taken {
		panic(fmt.Sprintf("addon catalogue: %q is registered twice", entry.ID))
	}
	addonCatalogue[entry.ID] = entry
}

// lookupAddon finds an entry by ID.
func lookupAddon(id string) (addonEntry, bool) {
	entry, ok := addonCatalogue[id]
	return entry, ok
}

// addonIDs is every entry in the catalogue, in name order, so that a message
// listing them reads the same twice.
func addonIDs() []string {
	ids := make([]string, 0, len(addonCatalogue))
	for id := range addonCatalogue {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// addonEntries is every entry, in name order.
func addonEntries() []addonEntry {
	entries := make([]addonEntry, 0, len(addonCatalogue))
	for _, id := range addonIDs() {
		entries = append(entries, addonCatalogue[id])
	}
	return entries
}

// AddonCatalogueEntry is one entry as everything outside this package sees
// it: what the dashboard lists and the API answers with.
//
// It is a projection rather than the entry itself, and deliberately so — the
// probe GVKs, the release names and the grant are the installer's business,
// and an API that published them would invite a request that set them.
type AddonCatalogueEntry struct {
	// ID is the entry's name, and so the Addon's.
	ID string `json:"id"`
	// Title and Summary say what it is and what it buys.
	Title   string `json:"title"`
	Summary string `json:"summary"`
	// Charts are the pins this operator would install, in order.
	Charts []AddonCatalogueChart `json:"charts"`
	// DefaultNamespace is where it goes when the Addon names no namespace.
	DefaultNamespace string `json:"defaultNamespace"`
	// DependsOn names the entries that go in first.
	DependsOn []string `json:"dependsOn,omitempty"`
	// ChartValue is the value that would permit the install, which is what a
	// refusal has to name to be actionable.
	ChartValue string `json:"chartValue"`
	// ClusterAdmin and GrantBecause are what the install account can do and
	// why, so the grant can be read before it is made.
	ClusterAdmin bool   `json:"clusterAdmin"`
	GrantBecause string `json:"grantBecause"`
	// BlastRadius is what stops working if it is removed.
	BlastRadius string `json:"blastRadius"`
}

// AddonCatalogueChart is one chart of an entry and the version pinned for it.
type AddonCatalogueChart struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// AddonCatalogue is every entry this operator can install, in name order.
func AddonCatalogue() []AddonCatalogueEntry {
	entries := make([]AddonCatalogueEntry, 0, len(addonCatalogue))
	for _, entry := range addonEntries() {
		charts := make([]AddonCatalogueChart, 0, len(entry.Charts))
		for _, chart := range entry.Charts {
			charts = append(charts, AddonCatalogueChart{Name: chart.Chart, Version: chart.DefaultVersion})
		}
		entries = append(entries, AddonCatalogueEntry{
			ID:               entry.ID,
			Title:            entry.Title,
			Summary:          entry.Summary,
			Charts:           charts,
			DefaultNamespace: entry.DefaultNamespace,
			DependsOn:        entry.DependsOn,
			ChartValue:       entry.ChartValue,
			ClusterAdmin:     entry.Grant.ClusterAdmin,
			GrantBecause:     entry.Grant.Because,
			BlastRadius:      entry.BlastRadius,
		})
	}
	return entries
}

// LookupAddonCatalogue finds one entry as the API sees it.
func LookupAddonCatalogue(id string) (AddonCatalogueEntry, bool) {
	for _, entry := range AddonCatalogue() {
		if entry.ID == id {
			return entry, true
		}
	}
	return AddonCatalogueEntry{}, false
}

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
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CloudNativePG: the catalogue entry that lets the platform answer "give me a
// database" with a database rather than with "install this yourself first".
//
// A chart cannot safely bundle it — it ships CRDs and an admission webhook of
// its own, and it is a popular thing for a cluster to already have, which
// Helm will not adopt — while the premise Kitchen is built on says a platform
// that owns its cluster should not make that somebody else's errand.
//
// It differs from the KEDA entry in every field and in one behaviour worth
// naming: a CloudNativePG somebody else installed is still *used*. A postgres
// claim provisions through whichever one is serving. What the ownership
// record decides is who may upgrade it, never who may use it.

const (
	// AddonCNPG is the entry's ID, and so the Addon object's name. It is
	// upstream's own chart name rather than the Connection provider's
	// shorthand, because it is the release an operator will go looking for.
	AddonCNPG = "cloudnative-pg"

	// DefaultCNPGChartRepository is where the chart is pulled from.
	// CloudNativePG publishes a classic HTTP repository rather than OCI
	// artifacts, so this is a repository URL handed to helm with --repo,
	// which needs no `helm repo add` and so no writable repository cache.
	DefaultCNPGChartRepository = "https://cloudnative-pg.github.io/charts"

	// DefaultCNPGChartVersion is pinned rather than floated, and it is pinned
	// next to the thing that depends on it: the operator version this chart
	// installs is what decides which Cluster fields exist and which images
	// are current, and the image catalogue those claims resolve against is
	// database.DefaultPostgresImages. A bump reads that list again — an
	// entry there is a promise the platform refuses claims on.
	DefaultCNPGChartVersion = "0.29.0"

	// DefaultCNPGInstallTimeout is what the helm run is given. It --waits, so
	// this is time for the operator's pods to become ready and not merely for
	// manifests to be accepted.
	DefaultCNPGInstallTimeout = 10 * time.Minute

	// DefaultCNPGOperatorNamespace is where CloudNativePG's own documentation
	// installs it, which is what an installation taking it over by hand later
	// will expect.
	DefaultCNPGOperatorNamespace = "cnpg-system"

	// cnpgInstallComponent names the job in collected logs, and is what the
	// dashboard filters on to show what helm said.
	cnpgInstallComponent = "cnpg-install"

	// cnpgReleaseName is upstream's own instruction's release name, and
	// cnpgChartName the chart in that repository.
	cnpgReleaseName = "cnpg"
	cnpgChartName   = "cloudnative-pg"
)

// cnpgClusterGVK is CloudNativePG's database kind. Kitchen writes these — one
// per claim — but here it is only a probe: a cluster that serves this kind
// runs CloudNativePG, and so must not be installed into.
func cnpgClusterGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
}

// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch

func init() {
	registerAddon(addonEntry{
		ID:    AddonCNPG,
		Title: "CloudNativePG",
		Summary: "Provisions Postgres into this cluster. Without it a postgres claim needs a connection to a " +
			"database somebody else runs.",
		Repository: DefaultCNPGChartRepository,
		Charts: []addonChart{{
			Release:        cnpgReleaseName,
			Chart:          cnpgChartName,
			DefaultVersion: DefaultCNPGChartVersion,
			VersionLabel:   labelInstallVersion,
		}},
		Probe:            cnpgClusterGVK(),
		DefaultNamespace: DefaultCNPGOperatorNamespace,
		Component:        cnpgInstallComponent,
		// This is the entry an uninstall is refused over. A cnpg Connection
		// is a statement that the platform provisions databases here, and
		// every claim through it is a database that would go with the
		// operator.
		Providers: []string{"cnpg"},
		BlastRadius: "No project will be able to claim a Postgres from this cluster. Databases already " +
			"provisioned are Clusters this operator reconciles, so removing it stops them being managed.",
		ChartValue: "databases.install.enabled",
		Grant: addonGrant{
			ClusterAdmin: true,
			Because: "installing CloudNativePG applies CRDs, ClusterRoles and a webhook configuration, which " +
				"is not an enumerable list",
		},
		Timeout: DefaultCNPGInstallTimeout,
	})
}

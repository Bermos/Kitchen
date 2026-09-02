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

// KEDA and its HTTP add-on: the catalogue entry that lets an environment idle
// to zero.
//
// The feature exists because the reason the *chart* cannot bundle them is
// Helm's and not Kubernetes': Helm builds and validates a release's entire
// manifest before applying any of it, so the add-on's own ScaledObject cannot
// resolve against a CRD arriving in the same release. An operator applies in
// whatever order it likes and can wait in between, which is the same reason
// the cert-manager ClusterIssuer and Certificate are the operator's rather
// than the chart's.
//
// It is why an entry installs a *list* of charts rather than one: the two go
// in in order, as two containers of one job, and the second is built against
// a CRD the first established.

const (
	// AddonKeda is the entry's ID, and so the Addon object's name.
	AddonKeda = "keda"

	// DefaultKedaChartRepository is where the two charts are pulled from.
	// KEDA publishes them as a classic HTTP repository rather than as OCI
	// artifacts, so this is a repository URL and helm is given it with
	// --repo, which needs no `helm repo add` and so no writable repository
	// cache to add it to.
	DefaultKedaChartRepository = "https://kedacore.github.io/charts"

	// DefaultKedaChartVersion and DefaultKedaHTTPChartVersion are pinned, and
	// pinned *as a pair*: the add-on tracks KEDA's own CRD and API closely
	// enough that the two versions are chosen together, the way
	// BuildpacksBuilderImage is pinned rather than floated. Bumping one means
	// checking the other's compatibility note.
	//
	// The interceptor's Service name and port follow from this pair too. At
	// these versions the add-on names its proxy
	// "keda-add-ons-http-interceptor-proxy" on port 8080, which is what
	// InterceptorSpec defaults to — a bump that moved either would have to
	// move those defaults with it.
	DefaultKedaChartVersion     = "2.20.2"
	DefaultKedaHTTPChartVersion = "0.15.0"

	// DefaultKedaInstallTimeout is what each of the two helm runs is given.
	// Both --wait, so this is time for pods to become ready and not merely
	// for manifests to be accepted.
	DefaultKedaInstallTimeout = 10 * time.Minute

	// kedaInstallComponent names the job in collected logs, and is what the
	// dashboard filters on to show what helm said.
	kedaInstallComponent = "keda-install"

	// kedaChartName and kedaHTTPChartName are the charts; kedaReleaseName and
	// kedaHTTPReleaseName the release names upstream's own instructions use.
	// Using anything else would make an installation that later wants to take
	// KEDA over by hand harder to reason about, for no gain.
	kedaChartName       = "keda"
	kedaHTTPChartName   = "keda-add-ons-http"
	kedaReleaseName     = "keda"
	kedaHTTPReleaseName = "keda-add-ons-http"
)

// scaledObjectGVK is KEDA's own scaling record. Kitchen never writes one —
// the HTTP add-on does, for its own interceptor fleet — but its presence is
// how the operator recognises a cluster that already runs KEDA without the
// add-on, and so must not be installed over.
func scaledObjectGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}
}

// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create

func init() {
	registerAddon(addonEntry{
		ID:    AddonKeda,
		Title: "KEDA and its HTTP add-on",
		Summary: "Scales an idle environment to zero and back on the first request. Without it every " +
			"environment keeps a pod running.",
		Repository: DefaultKedaChartRepository,
		Charts: []addonChart{
			{
				Release:        kedaReleaseName,
				Chart:          kedaChartName,
				DefaultVersion: DefaultKedaChartVersion,
				VersionLabel:   labelInstallVersion,
			},
			{
				Release:        kedaHTTPReleaseName,
				Chart:          kedaHTTPChartName,
				DefaultVersion: DefaultKedaHTTPChartVersion,
				VersionLabel:   labelInstallAddOnVersion,
			},
		},
		// The add-on's API is the only thing the platform actually needs;
		// KEDA alone gives it nothing.
		Probe: HTTPScaledObjectGVK(),
		Partial: &addonPartial{
			Probe:  scaledObjectGVK(),
			Reason: "KedaNotOurs",
			Message: "KEDA is already installed in this cluster but its HTTP add-on is not, and the platform " +
				"will not install over a release it does not own. Install the add-on beside your KEDA " +
				"(`helm install keda-add-ons-http kedacore/keda-add-ons-http`), and point " +
				"spec.scaleToZero.interceptor at it if it is not in " + defaultInterceptorNamespace,
		},
		DefaultNamespace: defaultInterceptorNamespace,
		Component:        kedaInstallComponent,
		// Nothing provisions *through* KEDA — it is not behind a Connection —
		// so an uninstall is never refused. What it costs is stated instead.
		Providers: nil,
		BlastRadius: "Every environment that idles will keep its pods running instead, and the first request " +
			"to a parked one will no longer wake it; nothing is deleted and no data is lost.",
		ChartValue: "scaleToZero.install.enabled",
		Grant: addonGrant{
			ClusterAdmin: true,
			Because: "installing KEDA applies CRDs, ClusterRoles and the aggregated roles it adds to the " +
				"cluster's own, which is not an enumerable list",
		},
		Timeout: DefaultKedaInstallTimeout,
	})
}

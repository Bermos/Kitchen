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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/platformhost"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

const (
	// ObjectStoreConnectionName is the s3 Connection the operator seeds so a
	// fresh installation's objectStore claims have a store to pick rather
	// than an empty list.
	ObjectStoreConnectionName = "kitchen-objectstore"

	// ObjectStoreRouteName is the HTTPRoute publishing the bundled store on
	// the shared Gateway, and ObjectStoreBackendTLSName the policy that
	// tells the Gateway to reach it over TLS. The chart owns the store; both
	// of these are the operator's, because the Gateway is.
	ObjectStoreRouteName      = "kitchen-objectstore"
	ObjectStoreBackendTLSName = "kitchen-objectstore"

	// ObjectStoreAdminRouteName is the route that keeps MinIO's admin API
	// off the published address. See applyObjectStoreAdminRoute.
	ObjectStoreAdminRouteName = "kitchen-objectstore-admin"

	// objectStoreAdminPath is MinIO's admin API, established against the
	// pinned image rather than from memory: every version of it lives under
	// this prefix, and `/minio/admin/` itself is answered by the admin
	// router — an unauthenticated request there gets 403 or a version
	// mismatch, never the S3 handler. Nothing else under `/minio/` is the
	// admin API: `/minio/health/live` is the liveness probe and
	// `/minio/v3/...` falls through to S3.
	//
	// Two neighbours are deliberately *not* carved out, and both are worth
	// knowing before somebody widens this. `/minio/v2/metrics/` wants a
	// bearer token unless `MINIO_PROMETHEUS_AUTH_TYPE=public` is set, which
	// the chart never sets — but `objectStore.extraEnv` can, and on a
	// published store that turns the bucket inventory into an anonymous
	// read. And `/minio/{storage,peer,lock,bootstrap}/` are the inter-node
	// APIs, which do not exist in the single-node mode the chart runs; they
	// would appear on this same port if it ever went distributed, and would
	// need carving out then.
	objectStoreAdminPath = "/minio/admin/"

	// ObjectStoreHostPrefix is the subdomain the store is published on:
	// objectstore.<baseDomain>. It comes from platformhost because the same
	// label is what a project may not be named (#423).
	ObjectStoreHostPrefix = platformhost.ObjectStore

	// ObjectStoreCredentialsSecretName holds that Connection's credential,
	// under the prefix the REST API uses for the credentials it writes, so
	// deleting the seeded connection from the connections page removes its
	// secret with it, exactly as for one someone created by hand.
	ObjectStoreCredentialsSecretName = "kitchen-connection-" + ObjectStoreConnectionName

	// defaultObjectStoreService, defaultObjectStorePort and
	// defaultObjectStoreSecret match what the chart writes under the
	// conventional release name, for the same upgrade reason the registry's
	// defaults exist: the singleton is not re-applied by default, and an
	// installation that switched the store on reads back the CRD's defaults.
	defaultObjectStoreService = "kitchen-objectstore"
	defaultObjectStorePort    = int32(9000)
	defaultObjectStoreSecret  = "kitchen-objectstore"

	condObjectStoreReady = "ObjectStoreReady"
)

// platformObjectStore is a resolved bundled object store: where it answers
// inside the cluster and where its root credential is.
type platformObjectStore struct {
	// Service and Port in the platform namespace.
	Service string
	Port    int32
	// Region the store reports.
	Region string
	// SecretName holds the store's root access key pair.
	SecretName string

	// PublicHost is where the store is published on the shared Gateway, and
	// empty for an installation that publishes it nowhere — `tls.mode: none`
	// or no base domain. It is a *second* address, not a replacement: the
	// Service address above is still what every server-side request uses,
	// and this one exists because an AWS SigV4 presigned URL signs the host
	// it is made for (#601).
	PublicHost string
	// PublicScheme is the scheme every published URL of the platform's
	// carries, resolved once from the TLS mode rather than written down
	// here — the rule every other public URL in this repository follows.
	PublicScheme string

	// Scheme is how the store is reached: https where the chart asked the
	// operator to issue it a certificate, http where somebody chose to leave
	// it in the clear. It is read from the store's own secret rather than
	// decided here, because whether the bundled store serves TLS is a chart
	// value and the secret is where the chart says so.
	Scheme string
	// CAFile is the PEM bundle that certificate is verified against, at the
	// path this pod mounts it. Empty for a store reached in the clear, and
	// for one whose certificate is somebody else's.
	CAFile string
}

// endpoint is the store's URL inside the cluster, on the Service address. It
// is what every server-side request uses and it does not change: an
// application's own reads and writes have no business leaving the cluster and
// coming back through the Gateway. What a browser cannot resolve it is
// publicEndpoint below that answers (#601).
//
// The scheme is the store's own (#382). It used to be `http` unconditionally,
// which is what made every object, every upload and every claim's credential
// readable to anything that landed in the namespace or watched the node.
func (o platformObjectStore) endpoint() string {
	scheme := o.Scheme
	if scheme == "" {
		// A secret written by a chart older than the certificate is
		// describing a store that really does answer in the clear.
		scheme = objectstore.SchemeHTTP
	}
	return fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", scheme, o.Service, PlatformNamespace, o.Port)
}

// publicEndpoint is the store's URL from outside the cluster, on the shared
// Gateway's own name and port, or "" for an installation that publishes it
// nowhere.
//
// It carries no port because the Gateway serves the scheme's own, and the
// scheme is the platform's rather than a constant: `kitchen.tls.mode` decides
// what every published URL is, so this goes through TLSMode.Scheme like the
// API's external URL and every generated application URL. In practice that is
// always https today, because the one mode it is not — `none` — publishes the
// store nowhere at all, and a fourth mode should not have to remember to come
// back here.
func (o platformObjectStore) publicEndpoint() string {
	if o.PublicHost == "" {
		return ""
	}
	scheme := o.PublicScheme
	if scheme == "" {
		scheme = kitchenv1alpha1.TLSModeACME.Scheme()
	}
	return fmt.Sprintf("%s://%s", scheme, o.PublicHost)
}

// host is the address the certificate is issued for, and the one the chart
// writes into the store's secret: `<service>.<namespace>.svc`, which
// serviceDNSNames turns into every shortening a cluster resolver accepts —
// the `.cluster.local` form the endpoint above uses among them.
func (o platformObjectStore) host() string {
	return fmt.Sprintf("%s.%s.svc", o.Service, PlatformNamespace)
}

// resolveObjectStore describes the bundled store, or nil when the platform
// runs none.
func resolveObjectStore(kitchen *kitchenv1alpha1.Kitchen) *platformObjectStore {
	spec := kitchen.Spec.ObjectStore
	if !spec.Enabled {
		return nil
	}
	store := &platformObjectStore{
		Service: spec.Service,
		Port:    spec.Port,
		Region:  spec.Region,
	}
	if spec.SecretRef != nil {
		store.SecretName = spec.SecretRef.Name
	}
	if store.Service == "" {
		store.Service = defaultObjectStoreService
	}
	if store.Port == 0 {
		store.Port = defaultObjectStorePort
	}
	if store.Region == "" {
		store.Region = objectstore.DefaultRegion
	}
	if store.SecretName == "" {
		store.SecretName = defaultObjectStoreSecret
	}
	store.PublicHost = objectStorePublicHost(kitchen)
	store.PublicScheme = kitchen.Spec.TLS.Mode.Scheme()
	return store
}

// objectStorePublicHost is the name the store is published under, and "" when
// it is published nowhere. The two reasons for that are quite different and
// objectStoreUnpublishedReason below is what tells them apart.
func objectStorePublicHost(kitchen *kitchenv1alpha1.Kitchen) string {
	if kitchen.Spec.TLS.Mode == kitchenv1alpha1.TLSModeNone {
		return ""
	}
	if host := kitchen.Spec.ObjectStore.Host; host != "" {
		return host
	}
	if kitchen.Spec.BaseDomain == "" {
		return ""
	}
	return fmt.Sprintf("%s.%s", ObjectStoreHostPrefix, kitchen.Spec.BaseDomain)
}

// objectStoreUnpublishedReason says, in the platform's own voice, why a store
// that is running has no address outside the cluster. Empty when it has one.
//
// It goes to `status.objectStore.unpublished` and onto the ObjectStoreReady
// message, which is a Platform-scope screen: the operator's own conditions
// table, and nowhere a developer holding a claim will see it. So it may name
// `tls.mode` and `spec.objectStore.host`, which are things its reader can
// actually change — and the claim screen has to say the same thing in its own
// words rather than assuming this sentence reached anybody (#601).
func objectStoreUnpublishedReason(kitchen *kitchenv1alpha1.Kitchen) string {
	switch {
	case kitchen.Spec.TLS.Mode == kitchenv1alpha1.TLSModeNone:
		return "the store is reached inside the cluster only: publishing it rides the platform's own " +
			"publicly trusted certificate, and tls.mode none has none, so an address published here would " +
			"carry every presigned request in the clear. Bindings carry no " + objectstore.BindingKeyPublicEndpoint +
			", and a URL presigned for somebody else's browser cannot be made"
	case objectStorePublicHost(kitchen) == "":
		return "the store is reached inside the cluster only: it has nowhere to be published, because the " +
			"platform has no base domain and spec.objectStore.host names no address either. Bindings carry " +
			"no " + objectstore.BindingKeyPublicEndpoint
	}
	return ""
}

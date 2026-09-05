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
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

const (
	// ObjectStoreConnectionName is the s3 Connection the operator seeds so a
	// fresh installation's objectStore claims have a store to pick rather
	// than an empty list.
	ObjectStoreConnectionName = "kitchen-objectstore"

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

// endpoint is the store's URL inside the cluster, on the Service address:
// nothing outside the cluster reaches it, and the node's container runtime —
// the reason the registry needs a publicly trusted certificate — is not in
// the path of an application's own requests.
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
	return store
}

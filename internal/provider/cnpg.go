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

package provider

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/cache"
)

// ProviderCNPG is the one provider that is not somewhere else. It provisions
// Postgres into the cluster Kitchen is installed in, through CloudNativePG,
// with the operator's own service account — so it has no credential, and its
// "is the provider reachable" question is a question about this cluster.
const ProviderCNPG = "cnpg"

// CNPGProbe answers that question: does this cluster serve
// postgresql.cnpg.io, and so is there anything here that could provision a
// database.
//
// It reports the answer in the same three parts every other probe does, and
// deliberately so — the connections page shows one shape, and a database
// operator that is not installed should read the way a provider that is down
// reads. The credential half is trivially valid: there is no credential, and
// the operator's own account is what writes, so a reachable provider is an
// accepted one.
type CNPGProbe struct {
	// Reader is the cluster. Nil is a probe that cannot run rather than one
	// that answers optimistically.
	Reader client.Reader
}

// Probe reports whether CloudNativePG is serving.
func (p *CNPGProbe) Probe(ctx context.Context) Result {
	if p.Reader == nil {
		return Result{Message: "this operator has no client to check the cluster with"}
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(cnpgClusterListGVK())
	err := p.Reader.List(ctx, list, client.Limit(1))
	switch {
	case err == nil:
		return accepted("CloudNativePG is serving in this cluster; databases are provisioned with the " +
			"operator's own account, and this connection holds no credential")
	case meta.IsNoMatchError(err), apierrors.IsNotFound(err):
		return unreachableBecause("this cluster does not serve postgresql.cnpg.io/v1, so CloudNativePG is not " +
			"installed. Set spec.databases.install on the Kitchen object to have the platform install it, or " +
			"install the Helm release yourself")
	default:
		return unreachableBecause("could not tell whether CloudNativePG is installed: " + err.Error())
	}
}

// WithCluster is the probe factory the operator and the API use: the two
// providers that are this cluster are resolved against it, and everything else
// goes to Default.
//
// It exists because Factory takes a Connection and a Secret, which is the
// right shape for every provider that lives somewhere else and no shape at all
// for the one that is here. Composing it in one place beats widening a
// signature five implementations share.
func WithCluster(reader client.Reader) Factory {
	return func(conn *kitchenv1alpha1.Connection, creds *corev1.Secret) (Probe, error) {
		switch conn.Spec.Provider {
		case ProviderCNPG:
			return &CNPGProbe{Reader: reader}, nil
		case cache.ProviderValkey:
			return &ValkeyProbe{}, nil
		}
		return Default(conn, creds)
	}
}

// ThirdParty reports whether a provider is somebody else. Every one is except
// cnpg and valkey, which provision into the cluster the platform is installed
// in — so a resilience register that listed either among the third parties a
// function depends on would be naming the platform as its own supplier, which
// is both wrong and the sort of wrong an auditor notices.
func ThirdParty(providerName string) bool {
	return providerName != ProviderCNPG && providerName != cache.ProviderValkey
}

// NeedsCredentials reports whether a provider has a credential to store at
// all. It is what lets the reconciler and the API stop looking for a Secret
// that is not meant to exist, rather than reporting its absence as a fault.
// The set of providers without one is the API package's, because the CRD's
// admission rule is written against the same set.
func NeedsCredentials(providerName string) bool {
	return kitchenv1alpha1.ProviderNeedsCredential(providerName)
}

func cnpgClusterListGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "ClusterList"}
}

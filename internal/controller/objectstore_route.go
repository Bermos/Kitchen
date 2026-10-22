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
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

// Publishing the bundled object store on the shared Gateway.
//
// The registry is the precedent and most of this is its shape: a reserved
// label, a hostname under the base domain, an HTTPRoute the operator writes
// because the Gateway is the operator's, and the platform's own wildcard
// certificate in front. The reason is different, though, and it is worth
// keeping straight. The registry is published because the *node's* container
// runtime is the client. The store is published because an AWS SigV4
// presigned URL signs the host it is made for: a URL signed against
// `kitchen-objectstore.kitchen-system.svc.cluster.local` is invalid at every
// other name and unresolvable in the browser it was handed to (#601).
//
// Two things follow and neither is optional:
//
//   - **Publishing an address publishes no object.** MinIO admits nobody
//     anonymously, and nothing here changes that: an unsigned request to the
//     published name is refused exactly as it is inside the cluster, and a
//     claim asking for a publicly readable bucket is still refused.
//   - **The two trust paths stay apart.** The public leg rides the wildcard
//     certificate the platform already holds. The leg from the Gateway to the
//     store is the *internal* one, on the platform's own CA — which is why
//     there is a BackendTLSPolicy below rather than a plaintext hop: the
//     store serves HTTPS on a `.svc` name and Envoy has to be told both to
//     speak TLS to it and what vouches for it.
//
// backendTLSPolicyGVK is addressed as an unstructured object for the reason
// cert-manager's kinds are: it keeps the build off another project's release
// cadence. BackendTLSPolicy is `v1` in the Gateway API standard channel from
// v1.4 on, which is what Cilium 1.20 — the Gateway implementation Kitchen
// targets — reads; the Go module this repository pins is older and still
// spells it v1alpha3, a version that release no longer serves.
var backendTLSPolicyGVK = schema.GroupVersionKind{
	Group:   gatewayv1.GroupName,
	Version: "v1",
	Kind:    "BackendTLSPolicy",
}

// applyObjectStoreRoute publishes the store on the shared Gateway. Every path
// goes to it: the S3 API is the whole of the URL space below the host, and
// with path-style addressing the bucket is the first segment of it.
func (r *KitchenReconciler) applyObjectStoreRoute(
	ctx context.Context,
	store *platformObjectStore,
	section *gatewayv1.SectionName,
) error {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Name: ObjectStoreRouteName, Namespace: PlatformNamespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		route.Labels = objectStoreLabels()
		route.Spec.CommonRouteSpec = gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{
				Name:        SharedGatewayName,
				Namespace:   ptr.To(gatewayv1.Namespace(PlatformNamespace)),
				SectionName: section,
			}},
		}
		route.Spec.Hostnames = []gatewayv1.Hostname{gatewayv1.Hostname(store.PublicHost)}
		route.Spec.Rules = []gatewayv1.HTTPRouteRule{{
			// `/` is what an absent match already defaults to. It is
			// written out because it is half of an argument made across two
			// objects: the admin route beside this one carries a longer
			// prefix, and a longer prefix is what out-precedences this rule
			// for those paths. An implicit match makes that unreadable.
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path: &gatewayv1.HTTPPathMatch{
					Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
					Value: ptr.To("/"),
				},
			}},
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(store.Service),
						Port: ptr.To(gatewayv1.PortNumber(store.Port)),
					},
				},
			}},
		}}
		return nil
	})
	return err
}

// applyObjectStoreAdminRoute keeps MinIO's admin API off the published
// address, and is the reason publishing port 9000 is not publishing the whole
// server.
//
// The S3 API is the entire URL space below the host — with path-style
// addressing a bucket is the first segment — so the route in front of it
// cannot enumerate what it serves and must match `/`. That sweeps up
// `/minio/admin/`, which is the one part of the port that is not the S3 API:
// user creation, policy attachment, service accounts, the server's own
// configuration. It answers 403 without an admin credential, and it should
// not be a handler the internet can reach at all.
//
// Gateway API cannot say "deny", and this repository already knows the shape
// of the workaround (#573): a carve-out from a route can only be made by a
// **more specific route out-precedencing it**. Hostname specificity is
// weighed first and both routes carry the same hostname, so path specificity
// is what does the work — `/minio/admin/` is a longer prefix than `/`, and
// the longest prefix wins across routes on a listener, not only within one.
//
// What it answers is the second half. A rule with no backendRefs and no
// response-producing filter answers 500, which would report a platform fault
// for a request the platform is deliberately refusing, and there is no
// fixed-response filter in the standard channel. A redirect is the one
// response the Gateway will produce itself, so the admin path is bounced to
// the store's root — where an anonymous caller that follows it is refused by
// the store, and one that does not follow it has still reached no admin
// handler. Either way nothing of the admin API is served.
//
// In-cluster administration is untouched: the operator's own provisioner
// talks to the Service address, which this route is not in front of.
func (r *KitchenReconciler) applyObjectStoreAdminRoute(
	ctx context.Context,
	store *platformObjectStore,
	section *gatewayv1.SectionName,
) error {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Name: ObjectStoreAdminRouteName, Namespace: PlatformNamespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		route.Labels = objectStoreLabels()
		route.Spec.CommonRouteSpec = gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{
				Name:        SharedGatewayName,
				Namespace:   ptr.To(gatewayv1.Namespace(PlatformNamespace)),
				SectionName: section,
			}},
		}
		route.Spec.Hostnames = []gatewayv1.Hostname{gatewayv1.Hostname(store.PublicHost)}
		route.Spec.Rules = []gatewayv1.HTTPRouteRule{{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path: &gatewayv1.HTTPPathMatch{
					Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
					Value: ptr.To(objectStoreAdminPath),
				},
			}},
			Filters: []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Path: &gatewayv1.HTTPPathModifier{
						Type:            gatewayv1.FullPathHTTPPathModifier,
						ReplaceFullPath: ptr.To("/"),
					},
					StatusCode: ptr.To(302),
				},
			}},
			// No backendRefs on purpose: the filter above is the whole of
			// the response, and anything here would be the store.
		}}
		return nil
	})
	return err
}

// applyObjectStoreBackendTLS tells the Gateway to reach the store over TLS,
// and what to verify its certificate against.
//
// Without it Envoy opens a plaintext connection to port 9000, where a store
// serving HTTPS answers nothing an S3 client can read — the published address
// would exist and never work. The hostname it validates is the store's `.svc`
// name, which is what the certificate is issued for; it is *not* the public
// name in front, and the CA is the platform's own rather than a public root.
//
// A store left in the clear needs none of this, and a store whose certificate
// somebody else issued is verified against the host's roots. Both are written
// as the absence of the CA reference rather than as a policy that turns
// verification off, because there is no such value here.
func (r *KitchenReconciler) applyObjectStoreBackendTLS(ctx context.Context, store *platformObjectStore) error {
	if store.Scheme != objectstore.SchemeHTTPS {
		return r.removeObjectStoreBackendTLS(ctx)
	}
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(backendTLSPolicyGVK)
	policy.SetName(ObjectStoreBackendTLSName)
	policy.SetNamespace(PlatformNamespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.SetLabels(objectStoreLabels())
		validation := map[string]any{"hostname": store.host()}
		if store.CAFile != "" {
			validation["caCertificateRefs"] = []any{map[string]any{
				"group": "",
				"kind":  "ConfigMap",
				"name":  InternalCAConfigMapName,
			}}
		} else {
			validation["wellKnownCACertificates"] = "System"
		}
		return unstructured.SetNestedMap(policy.Object, map[string]any{
			"targetRefs": []any{map[string]any{
				"group": "",
				"kind":  "Service",
				"name":  store.Service,
			}},
			"validation": validation,
		}, "spec")
	})
	return err
}

// removeObjectStoreRoute takes the published address down. It is called both
// when the store goes away and when it is still running and no longer
// publishable — `tls.mode` moved to none, the base domain went — because a
// route left behind answers for a name the platform no longer terminates TLS
// for.
func (r *KitchenReconciler) removeObjectStoreRoute(ctx context.Context) error {
	// The admin carve-out goes with the route it carves out of: on its own
	// it would redirect a hostname the platform no longer answers for.
	for _, name := range []string{ObjectStoreRouteName, ObjectStoreAdminRouteName} {
		route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: PlatformNamespace,
		}}
		if err := r.Delete(ctx, route); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return r.removeObjectStoreBackendTLS(ctx)
}

// removeObjectStoreBackendTLS drops the policy. A cluster whose Gateway API
// CRDs predate BackendTLSPolicy has no such kind, and a delete of a kind that
// does not exist is not a failure to report: there is nothing there.
func (r *KitchenReconciler) removeObjectStoreBackendTLS(ctx context.Context) error {
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(backendTLSPolicyGVK)
	policy.SetName(ObjectStoreBackendTLSName)
	policy.SetNamespace(PlatformNamespace)
	err := r.Delete(ctx, policy)
	switch {
	case err == nil, apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return nil
	default:
		return err
	}
}

func objectStoreLabels() map[string]string {
	return platformLabels(ObjectStoreRouteName, "objectstore")
}

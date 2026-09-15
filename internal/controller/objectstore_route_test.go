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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

// BackendTLSPolicy is `v1` in the Gateway API standard channel from v1.4 on,
// and the cluster Kitchen targets has it — but a cluster whose CRDs are older
// does not, and there is no version of this feature that works there: the
// store serves TLS on its Service name, so a Gateway told nothing would open a
// plaintext connection to it and the published address would answer nothing.
//
// What must not happen is the whole object store failing to reconcile over it.
// The store is what an installation is actually running; publishing it is the
// addition. So a missing kind leaves the store unpublished, with the reason in
// words, and everything that worked before goes on working.
//
// The envtest suite cannot reach this path — its CRD directory has the kind —
// so it is driven through a fake client that answers the way an API server
// with no such resource does.
func TestAGatewayApiWithoutBackendTlsPolicyLeavesTheStoreUnpublished(t *testing.T) {
	ctx := context.Background()
	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			BaseDomain: "apps.example.com",
			TLS:        kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeACME},
		},
	}
	// What the RESTMapper answers for a kind the cluster has no CRD for.
	noMatch := &meta.NoKindMatchError{
		GroupKind:        backendTLSPolicyGVK.GroupKind(),
		SearchedVersions: []string{backendTLSPolicyGVK.Version},
	}
	routeScheme := runtime.NewScheme()
	if err := gatewayv1.Install(routeScheme); err != nil {
		t.Fatal(err)
	}
	r := &KitchenReconciler{Client: fake.NewClientBuilder().
		WithScheme(routeScheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind() == backendTLSPolicyGVK {
					return noMatch
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.DeleteOption) error {
				if obj.GetObjectKind().GroupVersionKind() == backendTLSPolicyGVK {
					return noMatch
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()}

	store := &platformObjectStore{
		Service: defaultObjectStoreService,
		Port:    defaultObjectStorePort,
		Scheme:  objectstore.SchemeHTTPS,
		CAFile:  "/etc/kitchen/internal-ca/ca.crt",
	}
	store.PublicHost = objectStorePublicHost(kitchen)
	if store.PublicHost == "" {
		t.Fatal("the fixture publishes nowhere, so this test is checking nothing")
	}

	unpublished, err := r.publishObjectStore(ctx, kitchen, store)
	if err != nil {
		t.Fatalf("a Gateway API without the kind is not a reconcile failure: %v", err)
	}
	if !strings.Contains(unpublished, "BackendTLSPolicy") {
		t.Errorf("the reason names the kind the cluster does not have: %q", unpublished)
	}
	if !strings.Contains(unpublished, objectstore.BindingKeyPublicEndpoint) {
		t.Errorf("and the binding key that is therefore absent: %q", unpublished)
	}
	if store.PublicHost != "" {
		t.Errorf("nothing downstream may offer an address that is not being served, got %q",
			store.PublicHost)
	}
	if store.publicEndpoint() != "" {
		t.Errorf("so the seeded connection and every binding carry none, got %q", store.publicEndpoint())
	}

	route := &gatewayv1.HTTPRoute{}
	err = r.Get(ctx, client.ObjectKey{Name: ObjectStoreRouteName, Namespace: PlatformNamespace}, route)
	if err == nil {
		t.Error("no route is published for an address the Gateway cannot serve")
	}
}

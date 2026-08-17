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

package flows

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Bermos/Kitchen/internal/controller"
)

// The names the fixtures share. An assertion that disagrees with the route it
// is about is the one mistake these tests cannot catch.
const (
	hostProject    = "shop"
	hostProduction = "production"
	hostPreview    = "pr-41"
	productionHost = "shop.apps.example.com"
	previewHost    = "shop-pr-41.apps.example.com"
	customHost     = "www.shop.example.com"
	dashboardHost  = "kitchen.example.com"
	appNamespace   = "kitchen-app-shop"
	platformNS     = "kitchen-system"
	unroutedHost   = "abandoned.apps.example.com"
)

func routeFor(name, project, environment string, hostnames ...string) gatewayv1.HTTPRoute {
	route := gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNamespace}}
	if project != "" {
		route.Labels = map[string]string{
			controller.LabelProject:     project,
			controller.LabelEnvironment: environment,
		}
	}
	for _, hostname := range hostnames {
		route.Spec.Hostnames = append(route.Spec.Hostnames, gatewayv1.Hostname(hostname))
	}
	return route
}

// platformRoutes are the ones the chart and the Kitchen reconciler publish:
// real hostnames, no project.
func platformRoute() gatewayv1.HTTPRoute {
	route := routeFor("kitchen-api", "", "", dashboardHost)
	route.Namespace = platformNS
	return route
}

func TestHostsFromRoutesAttributesEveryPublishedHostname(t *testing.T) {
	table := hostsFromRoutes([]gatewayv1.HTTPRoute{
		// One route carries the generated URL and every verified custom
		// domain, which is why one listing is the whole map.
		routeFor(hostProduction, hostProject, hostProduction, productionHost, customHost),
		routeFor(hostPreview, hostProject, hostPreview, previewHost),
		platformRoute(),
	})

	for _, tc := range []struct {
		name, authority, project, environment string
	}{
		{"the generated url", productionHost, hostProject, hostProduction},
		{"a verified custom domain", customHost, hostProject, hostProduction},

		// A protected preview's requests reach the shared forward-auth gate,
		// not the application — attribution keyed on the destination endpoint
		// would file every preview on the platform under the gate. The Host
		// header is what survives that hop, and it is what this looks up.
		{"a protected preview behind the gate", previewHost, hostProject, hostPreview},

		// An idling environment's requests reach KEDA's interceptor for the
		// same reason and with the same answer; the interceptor itself routes
		// on nothing but this header.
		{"an idling environment behind the interceptor", previewHost, hostProject, hostPreview},

		// A `Host` header is whatever the client felt like sending.
		{"a port", productionHost + ":443", hostProject, hostProduction},
		{"shouted", "SHOP.Apps.Example.COM", hostProject, hostProduction},
		{"fully qualified", productionHost + ".", hostProject, hostProduction},

		// The unrouted bucket: a stale DNS entry, or a scanner guessing.
		{"a host nobody published", unroutedHost, "", ""},
		{"no host at all", "", "", ""},

		// The platform's own surfaces belong to no project, and giving them
		// one would put platform traffic on a project's charts.
		{"the dashboard", dashboardHost, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := table.lookup(tc.authority)
			if owner.project != tc.project || owner.environment != tc.environment {
				t.Errorf("lookup(%q) = %+v, want %s/%s", tc.authority, owner, tc.project, tc.environment)
			}
		})
	}
}

func TestHostsFromRoutesReadsWildcardHostnames(t *testing.T) {
	// Nothing Kitchen writes publishes a wildcard, but Gateway API permits one
	// and nothing refuses it at admission — and a wildcard route whose traffic
	// all landed in the unrouted bucket would look exactly like a
	// platform-wide DNS fault on the operator's Edge screen.
	table := hostsFromRoutes([]gatewayv1.HTTPRoute{
		routeFor(hostProduction, hostProject, hostProduction, "*.shop.example.com"),
	})
	if owner := table.lookup("checkout.shop.example.com"); owner.project != hostProject {
		t.Errorf("wildcard lookup = %+v, want %s", owner, hostProject)
	}
	// The dot in the suffix is load-bearing: a wildcard is a label, not a
	// string prefix.
	if owner := table.lookup("notshop.example.com"); owner.project != "" {
		t.Errorf("lookup(notshop.example.com) = %+v, want the unrouted bucket", owner)
	}
}

func TestHostIndexRefreshesWhenAHostMisses(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	published := routeFor(hostProduction, hostProject, hostProduction, productionHost)
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	index := newHostIndex(context.Background(), reader, logf.Log)

	// Nothing is published yet, so the first request is unattributed and the
	// miss is what arms the rebuild.
	if owner := index.lookup(context.Background(), productionHost); owner.project != "" {
		t.Errorf("lookup before publication = %+v, want the unrouted bucket", owner)
	}

	if err := reader.Create(context.Background(), &published); err != nil {
		t.Fatalf("publishing the route: %v", err)
	}

	// A hostname the operator published seconds ago is indistinguishable from
	// one nobody ever published until the routes are read again, and the first
	// requests to a new preview are exactly the ones somebody is watching for.
	// The floor between rebuilds has to be stepped over for the test to see it.
	index.missed = index.missed.Add(-2 * hostMissInterval)
	if owner := index.lookup(context.Background(), productionHost); owner.project != hostProject {
		t.Errorf("lookup after publication = %+v, want %s", owner, hostProject)
	}
}

func TestHostIndexKeepsWhatItHasWhenTheListingFails(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	published := routeFor(hostProduction, hostProject, hostProduction, productionHost)

	// The listing fails only after the index has read it once, so that a
	// rebuild which somehow succeeded would be visible as an emptied table
	// rather than as a table that happened to still be right.
	unreachable := false
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&published).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if unreachable {
					return errors.New("the api server is not answering")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	index := newHostIndex(context.Background(), reader, logf.Log)

	// A stale attribution puts a project's traffic on the right charts a
	// little late; an emptied one puts every project's traffic in the unrouted
	// bucket and fires the operator's unrouted-hosts signal for the whole
	// platform at once.
	unreachable = true
	index.rebuild(context.Background())

	if owner := index.table.lookup(productionHost); owner.project != hostProject {
		t.Errorf("lookup after a failed listing = %+v, want %s", owner, hostProject)
	}
}

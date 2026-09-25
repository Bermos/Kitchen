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

package api

import (
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The architecture overview: the declared graph read off the objects, and the
// flow collector's edges laid over it.

const (
	pricingEnvironment = "pricing-production"
	cacheInstance      = "kitchen-shop-cache-1a2b"
	httpProtocol       = "HTTP"
)

// topologyFixtures are the shop fixtures plus a second project whose offering
// shop's production binds, a cache behind the shop, and a worker process.
func topologyFixtures(phase kitchenv1alpha1.ClaimPhase) []runtime.Object {
	provider := pricingProject(openOffering())
	providerEnv := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: pricingEnvironment, Namespace: testNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: providerProject},
			Type:       kitchenv1alpha1.EnvironmentProduction,
		},
		Status: kitchenv1alpha1.EnvironmentStatus{Phase: kitchenv1alpha1.EnvironmentLive},
	}
	binding := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-pricing", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: feedProject},
			Type:       kitchenv1alpha1.ClaimTypeService,
			Config: &runtime.RawExtension{
				Raw: []byte(`{"service": {"project": "pricing", "offering": "pricing-api"}}`),
			},
		},
		Status: kitchenv1alpha1.ResourceClaimStatus{
			Phase: phase,
			Service: &kitchenv1alpha1.ClaimServiceStatus{Bindings: []kitchenv1alpha1.ClaimServiceBinding{
				{Consumer: kitchenv1alpha1.EnvironmentProduction, Environment: pricingEnvironment},
			}},
		},
	}
	cache := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-cache", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: feedProject},
			ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "valkey"},
			Type:          kitchenv1alpha1.ClaimTypeRedis,
		},
		Status: kitchenv1alpha1.ResourceClaimStatus{Phase: kitchenv1alpha1.ClaimBound, InstanceID: cacheInstance},
	}
	objects := []runtime.Object{provider, providerEnv, binding, cache}
	return append(objects, fixtures()...)
}

func nodesByID(nodes []topologyNode) map[string]topologyNode {
	out := make(map[string]topologyNode, len(nodes))
	for _, node := range nodes {
		out[node.ID] = node
	}
	return out
}

func edgesByID(edges []topologyEdge) map[string]topologyEdge {
	out := make(map[string]topologyEdge, len(edges))
	for _, edge := range edges {
		out[edge.ID] = edge
	}
	return out
}

func edgeID(kind, from, to string) string { return kind + ":" + from + "->" + to }

func TestTheTopologyIsReadOffTheObjects(t *testing.T) {
	h := newHarness(t, nil, topologyFixtures(kitchenv1alpha1.ClaimBound)...)

	res := h.do(t, http.MethodGet, "/api/v1/topology", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /topology = %d: %s", res.Code, res.Body.String())
	}
	graph := decode[topologyGraph](t, res)
	nodes := nodesByID(graph.Nodes)
	edges := edgesByID(graph.Edges)

	shopEnv := environmentNodeID(testEnvironment)
	offering := offeringNodeID(providerProject, openOfferingName)
	for _, id := range []string{
		projectNodeID(feedProject), projectNodeID(providerProject), shopEnv, environmentNodeID(pricingEnvironment),
		domainNodeID("shop.example.com"), topologyInternet, resourceNodeID("shop-db"), providerNodeID("neon"),
		resourceNodeID("shop-cache"), offering,
	} {
		if _, ok := nodes[id]; !ok {
			t.Errorf("the graph should have %s, got %v", id, graph.Nodes)
		}
	}
	if env := nodes[shopEnv]; env.Project != feedProject || env.Type != string(kitchenv1alpha1.EnvironmentProduction) || env.URL == "" {
		t.Errorf("an environment carries its project, type and address: %+v", env)
	}
	if node := nodes[offering]; node.Foreign || node.Type != "http" || node.Detail != "open" {
		t.Errorf("an operator's offering is nobody's foreign one and says what it speaks: %+v", node)
	}

	for _, id := range []string{
		edgeID(topologyRoutes, topologyInternet, shopEnv),
		edgeID(topologyRoutes, topologyInternet, domainNodeID("shop.example.com")),
		edgeID(topologyRoutes, domainNodeID("shop.example.com"), shopEnv),
		edgeID(topologyUses, shopEnv, resourceNodeID("shop-db")),
		edgeID(topologyProvidedBy, resourceNodeID("shop-db"), providerNodeID("neon")),
		edgeID(topologyConsumes, shopEnv, offering),
		edgeID(topologyServes, offering, environmentNodeID(pricingEnvironment)),
	} {
		edge, ok := edges[id]
		if !ok {
			t.Errorf("the graph should have edge %s, got %v", id, graph.Edges)
			continue
		}
		if edge.State != "" {
			t.Errorf("a bound edge is in effect, got state %q on %s", edge.State, id)
		}
	}
	// The binding reaches only production, so it is the only class drawn;
	// nothing about a service claim is a resource of the consumer's.
	if _, ok := nodes[resourceNodeID("shop-pricing")]; ok {
		t.Errorf("a service claim is an edge to an offering, not a resource")
	}
}

// A member sees their own project whole, and the other end of a binding as a
// name: no environment of somebody else's, and nothing of a project that
// nothing of theirs names.
func TestTheTopologyNarrowsToTheCallersProjects(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer,
		append(topologyFixtures(kitchenv1alpha1.ClaimBound)[:4], blogFixtures()...)...)

	res := h.do(t, http.MethodGet, "/api/v1/topology", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /topology = %d: %s", res.Code, res.Body.String())
	}
	graph := decode[topologyGraph](t, res)
	nodes := nodesByID(graph.Nodes)

	if _, ok := nodes[environmentNodeID(pricingEnvironment)]; ok {
		t.Errorf("another project's environment is not the member's to see")
	}
	for _, id := range []string{projectNodeID(otherProject), environmentNodeID("blog-production")} {
		if _, ok := nodes[id]; ok {
			t.Errorf("a project nothing of the member's names is not on their graph: %s", id)
		}
	}
	provider, ok := nodes[projectNodeID(providerProject)]
	if !ok || !provider.Foreign {
		t.Fatalf("the provider of a bound offering is drawn as a foreign name, got %+v", provider)
	}
	offering := offeringNodeID(providerProject, openOfferingName)
	if node := nodes[offering]; !node.Foreign {
		t.Errorf("an offering of a project the member cannot see is foreign: %+v", node)
	}
	edges := edgesByID(graph.Edges)
	if _, ok := edges[edgeID(topologyServes, offering, projectNodeID(providerProject))]; !ok {
		t.Errorf("a foreign offering is served by its project as a whole, got %v", graph.Edges)
	}
}

// The provider's side of the same binding: who depends on me, as a name.
func TestTheTopologyNamesTheConsumersOfTheCallersOfferings(t *testing.T) {
	h := newHarness(t, nil, topologyFixtures(kitchenv1alpha1.ClaimBound)...)
	h.demoteCaller(t)
	h.grant(t, providerProject, kitchenv1alpha1.AccessRoleViewer)

	graph := decode[topologyGraph](t, h.do(t, http.MethodGet, "/api/v1/topology", ""))
	nodes := nodesByID(graph.Nodes)
	if consumer := nodes[projectNodeID(feedProject)]; !consumer.Foreign {
		t.Errorf("the consumer is a foreign name to the provider, got %+v", consumer)
	}
	if _, ok := nodes[resourceNodeID("shop-db")]; ok {
		t.Errorf("the consumer's own resources are not the provider's to see")
	}
	edges := edgesByID(graph.Edges)
	offering := offeringNodeID(providerProject, openOfferingName)
	if _, ok := edges[edgeID(topologyConsumes, projectNodeID(feedProject), offering)]; !ok {
		t.Errorf("the consumer binds the offering as a project, got %v", graph.Edges)
	}
}

func TestATopologyFocusKeepsOneProjectAndItsNeighbours(t *testing.T) {
	h := newHarness(t, nil, topologyFixtures(kitchenv1alpha1.ClaimBound)...)

	graph := decode[topologyGraph](t, h.do(t, http.MethodGet, "/api/v1/topology?project=pricing", ""))
	nodes := nodesByID(graph.Nodes)
	if _, ok := nodes[environmentNodeID(testEnvironment)]; !ok {
		t.Errorf("the consumer of the focused project's offering is one edge away and stays")
	}
	if _, ok := nodes[resourceNodeID("shop-db")]; ok {
		t.Errorf("the consumer's own database is two edges away and goes")
	}
}

func TestAPendingBindingIsDrawnAsWaiting(t *testing.T) {
	h := newHarness(t, nil, topologyFixtures(kitchenv1alpha1.ClaimPendingApproval)...)

	graph := decode[topologyGraph](t, h.do(t, http.MethodGet, "/api/v1/topology", ""))
	edge := edgesByID(graph.Edges)[edgeID(topologyConsumes, environmentNodeID(testEnvironment),
		offeringNodeID(providerProject, openOfferingName))]
	if edge.State != string(kitchenv1alpha1.ClaimPendingApproval) {
		t.Errorf("a binding waiting on the provider says so, got %+v", edge)
	}
}

func TestObservedTrafficIsAttributedOntoTheGraph(t *testing.T) {
	h := newHarness(t, nil, append(topologyFixtures(kitchenv1alpha1.ClaimBound), blogFixtures()...)...)
	shop := controller.AppNamespace(feedProject)
	h.logs.edges = []clickhouse.TrafficEdge{
		// Visitors, through a published address.
		{Source: worldEndpoint, Destination: testEnvironment, DestinationNamespace: shop,
			Protocol: httpProtocol, Flows: 600, RPS: 10},
		// The binding, used.
		{Source: testEnvironment, SourceNamespace: shop, Destination: pricingEnvironment,
			DestinationNamespace: controller.AppNamespace(providerProject), Protocol: httpProtocol, Flows: 60, RPS: 1},
		// The cache, by the name its provider ran it under.
		{Source: testEnvironment + "-worker", SourceNamespace: shop, Destination: cacheInstance + "-0",
			DestinationNamespace: "kitchen-caches", Protocol: "TCP", Flows: 30, RPS: 0.5},
		// Somebody calling the shop with nothing saying they may.
		{Source: "blog-production", SourceNamespace: controller.AppNamespace(otherProject),
			Destination: testEnvironment, DestinationNamespace: shop, Protocol: httpProtocol, Flows: 6, RPS: 0.1,
			Errors: 2},
		// Off the platform.
		{Source: testEnvironment, SourceNamespace: shop, Destination: "api.stripe.com",
			Protocol: httpProtocol, Flows: 3, RPS: 0.05},
		// The platform's own components.
		{Source: "preview-gate", SourceNamespace: testNamespace, Destination: testEnvironment,
			DestinationNamespace: shop, Protocol: httpProtocol, Flows: 2},
	}

	res := h.do(t, http.MethodGet, "/api/v1/topology/traffic", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /topology/traffic = %d: %s", res.Code, res.Body.String())
	}
	answer := decode[topologyTraffic](t, res)
	if answer.Since.IsZero() || !answer.Until.After(answer.Since) {
		t.Errorf("the answer says which window its rates are over, got %v to %v", answer.Since, answer.Until)
	}

	shopEnv := environmentNodeID(testEnvironment)
	offering := offeringNodeID(providerProject, openOfferingName)
	byPair := map[string]observedEdge{}
	for _, edge := range answer.Edges {
		byPair[edge.From+" "+edge.To] = edge
	}
	for pair, want := range map[string]struct {
		status string
		along  []string
	}{
		topologyInternet + " " + shopEnv: {observedDeclared,
			[]string{edgeID(topologyRoutes, topologyInternet, shopEnv)}},
		shopEnv + " " + environmentNodeID(pricingEnvironment): {observedDeclared, []string{
			edgeID(topologyConsumes, shopEnv, offering),
			edgeID(topologyServes, offering, environmentNodeID(pricingEnvironment)),
		}},
		shopEnv + " " + resourceNodeID("shop-cache"): {observedDeclared,
			[]string{edgeID(topologyUses, shopEnv, resourceNodeID("shop-cache"))}},
		environmentNodeID("blog-production") + " " + shopEnv: {observedUndeclared, nil},
		shopEnv + " " + externalNodeID("api.stripe.com"):     {observedExternal, nil},
		topologyPlatform + " " + shopEnv:                     {observedPlatform, nil},
	} {
		edge, ok := byPair[pair]
		if !ok {
			t.Errorf("no observed edge %s, got %+v", pair, answer.Edges)
			continue
		}
		if edge.Status != want.status || len(edge.Along) != len(want.along) {
			t.Errorf("%s = %s along %v, want %s along %v", pair, edge.Status, edge.Along, want.status, want.along)
			continue
		}
		for i := range want.along {
			if edge.Along[i] != want.along[i] {
				t.Errorf("%s runs along %v, want %v", pair, edge.Along, want.along)
			}
		}
	}

	extra := nodesByID(answer.Nodes)
	for _, id := range []string{topologyPlatform, externalNodeID("api.stripe.com")} {
		if _, ok := extra[id]; !ok {
			t.Errorf("a node only the flows reach is answered with them: %s, got %v", id, answer.Nodes)
		}
	}
	if _, ok := extra[shopEnv]; ok {
		t.Errorf("a node the declared graph already has is not repeated")
	}
}

// A member is told that somebody calls their project, and who — a project, by
// name — but not which of that project's environments.
func TestObservedTrafficNamesAnotherProjectAsAWhole(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, blogFixtures()...)
	h.logs.edges = []clickhouse.TrafficEdge{{
		Source: "blog-production", SourceNamespace: controller.AppNamespace(otherProject),
		Destination: testEnvironment, DestinationNamespace: controller.AppNamespace(feedProject),
		Protocol: httpProtocol, Flows: 6,
	}}

	answer := decode[topologyTraffic](t, h.do(t, http.MethodGet, "/api/v1/topology/traffic", ""))
	if len(answer.Edges) != 1 || answer.Edges[0].From != projectNodeID(otherProject) {
		t.Fatalf("the caller is a project as a whole, got %+v", answer.Edges)
	}
	if node := nodesByID(answer.Nodes)[projectNodeID(otherProject)]; !node.Foreign {
		t.Errorf("a project the member cannot see is foreign, got %+v", node)
	}
}

func TestObservedTrafficAnswersANoStoreInstallationPlainly(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.trafficErr = errNoLogStore

	res := h.do(t, http.MethodGet, "/api/v1/topology/traffic", "")
	if res.Code == http.StatusOK {
		t.Fatalf("a failed store read is not an empty graph: %s", res.Body.String())
	}
}

// Narrowed to one project, the traffic still names both ends of every pair it
// answers with, even where the focused graph left one of them out.
func TestFocusedTrafficCarriesTheEndsTheFocusLeftOut(t *testing.T) {
	h := newHarness(t, nil, topologyFixtures(kitchenv1alpha1.ClaimBound)...)
	h.logs.edges = []clickhouse.TrafficEdge{{
		Source: testEnvironment + "-worker", SourceNamespace: controller.AppNamespace(feedProject),
		Destination: cacheInstance + "-0", DestinationNamespace: "kitchen-caches", Protocol: "TCP", Flows: 30,
	}}

	answer := decode[topologyTraffic](t, h.do(t, http.MethodGet, "/api/v1/topology/traffic?project=pricing", ""))
	nodes := nodesByID(answer.Nodes)
	if _, ok := nodes[resourceNodeID("shop-cache")]; !ok {
		t.Errorf("a resource two edges from the focus is answered with the traffic reaching it, got %v", answer.Nodes)
	}
	if _, ok := nodes[environmentNodeID(testEnvironment)]; ok {
		t.Errorf("an end the focused graph already has is not repeated")
	}
}

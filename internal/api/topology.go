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
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The architecture overview: what the projects on this platform are made of
// and what they depend on, drawn as one graph (docs/spikes/
// service-topology-2026-09.md, "Topology").
//
// **The declared half is read off objects that already exist.** Nothing holds
// the graph and nothing has to be kept in step with it: an environment, a
// claim, an offering, a domain and a connection each already say which other
// object they point at, and the edges are those pointers. That is why the
// route has no write beside it — the graph is changed by changing the objects
// it is read from, through the routes those objects already have.
//
// **The observed half is the flow collector's**, attributed onto the same
// nodes. A Hubble flow names workloads; the platform named those workloads
// after the environments and claims that materialized them, so the join is
// the controller's naming read backwards. A flow that runs along a declared
// edge animates it; one that does not is the spike's *undeclared edge* — A
// calls B with nothing saying it may — and is drawn as a finding rather than
// quietly merged into the picture.
//
// Both routes are cross-project reads narrowed to what the caller may see.
// A project the caller holds no role on appears only where one of their own
// objects already names it — the provider of an offering they bind, or the
// consumer of one they make, which the offering catalogue and the Offerings
// pane answer already — and then as a name and nothing else.

// The kinds of node the graph is made of.
const (
	topologyProject     = "project"
	topologyEnvironment = "environment"
	topologyOffering    = "offering"
	topologyResource    = "resource"
	topologyProvider    = "provider"
	topologyDomain      = "domain"
	// topologyInternet is the one node standing for everybody outside the
	// platform: where a published environment's visitors come from.
	topologyInternet = "internet"
	// topologyPlatform is the platform's own components, as one node. The
	// observed graph needs it — a protected preview is reached through the
	// forward-auth gate and an idling environment through the interceptor —
	// and a developer's screen has no use for which of them it was.
	topologyPlatform = "platform"
	// topologyExternal is a host off the platform that a workload called,
	// by the name it was resolved under.
	topologyExternal = "external"
)

// The kinds of edge. Every edge points the way a dependency does — from the
// thing that would break to the thing it would break on — which is also the
// way a request travels, so the observed flows run along them.
const (
	// topologyRoutes is a published address reaching an environment.
	topologyRoutes = "routes"
	// topologyConsumes is an environment binding another project's offering.
	topologyConsumes = "consumes"
	// topologyServes is an offering answered by an environment.
	topologyServes = "serves"
	// topologyUses is an environment reading one of its project's resources.
	topologyUses = "uses"
	// topologyProvidedBy is a resource provisioned through a connection.
	topologyProvidedBy = "providedBy"
)

// How an observed edge relates to the declared graph.
const (
	// observedDeclared runs along declared edges.
	observedDeclared = "declared"
	// observedUndeclared runs between two projects' workloads with nothing
	// declaring it: the finding the observed half exists for.
	observedUndeclared = "undeclared"
	// observedPlatform touches the platform's own components, which no
	// binding governs.
	observedPlatform = "platform"
	// observedExternal leaves the platform or arrives from off it without a
	// published address that explains it.
	observedExternal = "external"
)

// worldEndpoint is what the flow collector calls an endpoint it could name
// no other way.
const worldEndpoint = "world"

// topologyNode is one box on the diagram.
type topologyNode struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Project is the project the node belongs to, empty for the nodes that
	// belong to nobody — the internet, a connection, the platform.
	Project string `json:"project,omitempty"`
	// Foreign marks a project, or an offering, the caller holds no role on.
	// It is on the diagram because one of the caller's own objects names it,
	// and it carries its name and nothing else.
	Foreign bool `json:"foreign,omitempty"`
	// Type is the node's own classification: an environment's type, a
	// resource's claim type, an offering's protocol, a connection's provider.
	Type string `json:"type,omitempty"`
	// Phase is the object's phase, where it has one.
	Phase string `json:"phase,omitempty"`
	// URL is where an environment is published.
	URL string `json:"url,omitempty"`
	// Idle is an environment scaled to zero while nobody is asking for it.
	Idle bool `json:"idle,omitempty"`
	// Processes are what an environment runs.
	Processes []topologyProcess `json:"processes,omitempty"`
	// Detail is one more fact worth a line under the name: an offering's
	// visibility, a resource's instance.
	Detail string `json:"detail,omitempty"`
}

// topologyProcess is one process an environment runs.
type topologyProcess struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// topologyEdge is one declared dependency.
type topologyEdge struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	From string `json:"from"`
	To   string `json:"to"`
	// State is empty on an edge that is in effect, and otherwise says why it
	// is not: `PendingApproval` for a binding waiting on the provider,
	// `unresolved` for a class of environment the binding reaches nothing
	// for, or the claim's own phase.
	State string `json:"state,omitempty"`
	// Reason is the sentence behind a State, where the object gave one.
	Reason string `json:"reason,omitempty"`
}

// topologyGraph is the declared graph.
type topologyGraph struct {
	Nodes []topologyNode `json:"nodes"`
	Edges []topologyEdge `json:"edges"`
}

// observedEdge is one talking pair in the window, attributed onto the graph.
type observedEdge struct {
	From     string  `json:"from"`
	To       string  `json:"to"`
	Protocol string  `json:"protocol"`
	Flows    uint64  `json:"flows"`
	RPS      float64 `json:"rps"`
	Errors   uint64  `json:"errors"`
	Drops    uint64  `json:"drops"`
	// P95Ms is the slowest of the aggregated pairs' own p95 — an upper bound
	// rather than a percentile of the merged traffic, which the rows the
	// store answers with cannot produce.
	P95Ms float64 `json:"p95Ms"`
	// Status is how the edge relates to the declared graph.
	Status string `json:"status"`
	// Along is the declared edges this traffic runs over, in order. Empty
	// unless Status is `declared`.
	Along []string `json:"along,omitempty"`
}

// topologyTraffic is the observed half: the edges, and whatever nodes they
// reach that the declared graph does not have.
type topologyTraffic struct {
	Nodes []topologyNode `json:"nodes"`
	Edges []observedEdge `json:"edges"`
	// Since and Until are the window the rates are averaged over, with the
	// defaults filled in, so that a reader can say "per second over what".
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// topologyInputs is everything the graph is read off.
type topologyInputs struct {
	projects     []kitchenv1alpha1.Project
	environments []kitchenv1alpha1.Environment
	claims       []kitchenv1alpha1.ResourceClaim
	domains      []kitchenv1alpha1.Domain
	connections  []kitchenv1alpha1.Connection
}

// readTopologyInputs lists the five kinds the graph is made of.
func (s *Server) readTopologyInputs(ctx context.Context) (topologyInputs, error) {
	in := topologyInputs{}
	projects := &kitchenv1alpha1.ProjectList{}
	environments := &kitchenv1alpha1.EnvironmentList{}
	claims := &kitchenv1alpha1.ResourceClaimList{}
	domains := &kitchenv1alpha1.DomainList{}
	connections := &kitchenv1alpha1.ConnectionList{}
	for _, list := range []client.ObjectList{projects, environments, claims, domains, connections} {
		if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
			return in, err
		}
	}
	in.projects = projects.Items
	in.environments = environments.Items
	in.claims = claims.Items
	in.domains = domains.Items
	in.connections = connections.Items
	return in, nil
}

// Node identifiers, spelled once each. They are paths so that no two kinds
// can collide, and stable so that the dashboard can keep a layout between
// polls.
func projectNodeID(name string) string           { return "project/" + name }
func environmentNodeID(name string) string       { return "environment/" + name }
func offeringNodeID(project, name string) string { return "offering/" + project + "/" + name }
func resourceNodeID(claim string) string         { return "resource/" + claim }
func providerNodeID(connection string) string    { return "provider/" + connection }
func domainNodeID(hostname string) string        { return "domain/" + hostname }
func externalNodeID(host string) string          { return "external/" + host }

func internetNode() topologyNode {
	return topologyNode{ID: topologyInternet, Kind: topologyInternet, Name: "Internet"}
}

func platformNode() topologyNode {
	return topologyNode{ID: topologyPlatform, Kind: topologyPlatform, Name: "Kitchen platform"}
}

// graphBuilder collects nodes and edges without repeats.
type graphBuilder struct {
	nodes map[string]topologyNode
	edges map[string]topologyEdge
}

func newGraphBuilder() *graphBuilder {
	return &graphBuilder{nodes: map[string]topologyNode{}, edges: map[string]topologyEdge{}}
}

// node adds a node unless one with its identifier is already there. The first
// spelling wins, which is why the full nodes are added before anything that
// only refers to them.
func (b *graphBuilder) node(node topologyNode) {
	if _, ok := b.nodes[node.ID]; !ok {
		b.nodes[node.ID] = node
	}
}

func (b *graphBuilder) edge(kind, from, to, state, reason string) {
	id := kind + ":" + from + "->" + to
	if _, ok := b.edges[id]; ok {
		return
	}
	b.edges[id] = topologyEdge{ID: id, Kind: kind, From: from, To: to, State: state, Reason: reason}
}

func (b *graphBuilder) graph() topologyGraph {
	graph := topologyGraph{
		Nodes: make([]topologyNode, 0, len(b.nodes)),
		Edges: make([]topologyEdge, 0, len(b.edges)),
	}
	for _, node := range b.nodes {
		graph.Nodes = append(graph.Nodes, node)
	}
	for _, edge := range b.edges {
		graph.Edges = append(graph.Edges, edge)
	}
	sort.Slice(graph.Nodes, func(i, j int) bool { return graph.Nodes[i].ID < graph.Nodes[j].ID })
	sort.Slice(graph.Edges, func(i, j int) bool { return graph.Edges[i].ID < graph.Edges[j].ID })
	return graph
}

// environmentType is an environment's type with the CRD's default applied.
func environmentType(env *kitchenv1alpha1.Environment) kitchenv1alpha1.EnvironmentType {
	if env.Spec.Type == "" {
		return kitchenv1alpha1.EnvironmentProduction
	}
	return env.Spec.Type
}

// buildTopology reads the declared graph off the platform's objects, narrowed
// to what `scope` may see.
func buildTopology(in topologyInputs, scope projectScope) topologyGraph {
	b := newGraphBuilder()

	projects := make(map[string]*kitchenv1alpha1.Project, len(in.projects))
	for i := range in.projects {
		project := &in.projects[i]
		projects[project.Name] = project
		if scope.allows(project.Name) {
			b.node(topologyNode{ID: projectNodeID(project.Name), Kind: topologyProject, Name: project.Name,
				Project: project.Name})
		}
	}
	// foreign is a project the caller cannot see, drawn because something of
	// theirs names it: a name, and nothing else.
	foreign := func(name string) string {
		id := projectNodeID(name)
		b.node(topologyNode{ID: id, Kind: topologyProject, Name: name, Project: name, Foreign: true})
		return id
	}

	// Environments, and the addresses they are published at.
	environments := map[string][]*kitchenv1alpha1.Environment{}
	known := map[string]bool{}
	for i := range in.environments {
		env := &in.environments[i]
		project := env.Spec.ProjectRef.Name
		if !scope.allows(project) {
			continue
		}
		environments[project] = append(environments[project], env)
		known[env.Name] = true
		node := topologyNode{
			ID:      environmentNodeID(env.Name),
			Kind:    topologyEnvironment,
			Name:    env.Name,
			Project: project,
			Type:    string(environmentType(env)),
			Phase:   string(env.Status.Phase),
			URL:     env.Status.URL,
			Idle:    env.Status.Idle,
		}
		for _, process := range env.Status.Processes {
			node.Processes = append(node.Processes, topologyProcess{Name: process.Name, Type: string(process.Type)})
		}
		b.node(node)
		if env.Status.URL != "" {
			b.node(internetNode())
			b.edge(topologyRoutes, topologyInternet, node.ID, "", "")
		}
	}
	for i := range in.domains {
		domain := &in.domains[i]
		if !known[domain.Spec.EnvironmentRef.Name] {
			continue
		}
		target := environmentNodeID(domain.Spec.EnvironmentRef.Name)
		project := b.nodes[target].Project
		phase := "Verified"
		if !domain.Status.Verified {
			phase = "Unverified"
		}
		id := domainNodeID(domain.Spec.Hostname)
		b.node(topologyNode{ID: id, Kind: topologyDomain, Name: domain.Spec.Hostname, Project: project, Phase: phase})
		b.node(internetNode())
		b.edge(topologyRoutes, topologyInternet, id, "", "")
		b.edge(topologyRoutes, id, target, "", "")
	}

	// Offerings a visible project makes, and what answers them.
	offering := func(provider *kitchenv1alpha1.Project, offer kitchenv1alpha1.ServiceOffering) string {
		id := offeringNodeID(provider.Name, offer.Name)
		visible := scope.allows(provider.Name)
		b.node(topologyNode{
			ID:      id,
			Kind:    topologyOffering,
			Name:    offer.Name,
			Project: provider.Name,
			Foreign: !visible,
			Type:    string(offer.Protocol()),
			Detail:  string(offer.Visibility()),
		})
		if !visible {
			b.edge(topologyServes, id, foreign(provider.Name), "", "")
			return id
		}
		serving := offeringEnvironmentName(provider, offer)
		if known[serving] {
			b.edge(topologyServes, id, environmentNodeID(serving), "", "")
		} else {
			b.edge(topologyServes, id, projectNodeID(provider.Name), "unresolved",
				"the environment this offering serves does not exist yet")
		}
		return id
	}
	for _, project := range projects {
		if !scope.allows(project.Name) {
			continue
		}
		for _, offer := range project.Spec.Offers {
			offering(project, offer)
		}
	}

	// Claims: what each visible project's environments depend on.
	connections := make(map[string]*kitchenv1alpha1.Connection, len(in.connections))
	for i := range in.connections {
		connections[in.connections[i].Name] = &in.connections[i]
	}
	for i := range in.claims {
		claim := &in.claims[i]
		owner := claim.Spec.ProjectRef.Name
		if claim.Spec.Type == kitchenv1alpha1.ClaimTypeService {
			addServiceClaim(b, claim, projects, environments, scope, offering, foreign)
			continue
		}
		if !scope.allows(owner) {
			continue
		}
		id := resourceNodeID(claim.Name)
		b.node(topologyNode{
			ID:      id,
			Kind:    topologyResource,
			Name:    claim.Name,
			Project: owner,
			Type:    claim.Spec.Type,
			Phase:   string(claim.Status.Phase),
			Detail:  claim.Status.InstanceName,
		})
		if len(environments[owner]) == 0 {
			b.edge(topologyUses, projectNodeID(owner), id, "", "")
		}
		for _, env := range environments[owner] {
			b.edge(topologyUses, environmentNodeID(env.Name), id, "", "")
		}
		if ref := claim.Spec.ConnectionRef; ref != nil && ref.Name != "" {
			provider := topologyNode{ID: providerNodeID(ref.Name), Kind: topologyProvider, Name: ref.Name}
			if connection := connections[ref.Name]; connection != nil {
				provider.Type = connection.Spec.Provider
			}
			b.node(provider)
			b.edge(topologyProvidedBy, id, provider.ID, "", "")
		}
	}
	return b.graph()
}

// addServiceClaim draws one binding to an offering: from each of the
// consumer's environments, in the state the binding is in for that class of
// environment.
func addServiceClaim(
	b *graphBuilder,
	claim *kitchenv1alpha1.ResourceClaim,
	projects map[string]*kitchenv1alpha1.Project,
	environments map[string][]*kitchenv1alpha1.Environment,
	scope projectScope,
	offering func(*kitchenv1alpha1.Project, kitchenv1alpha1.ServiceOffering) string,
	foreign func(string) string,
) {
	consumer := claim.Spec.ProjectRef.Name
	service := claim.Service()
	if service.Project == "" || service.Offering == "" {
		return
	}
	// A claim neither end of which the caller can see is not theirs to be
	// told about.
	if !scope.allows(consumer) && !scope.allows(service.Project) {
		return
	}

	target := offeringNodeID(service.Project, service.Offering)
	if provider := projects[service.Project]; provider != nil {
		found := false
		for _, offer := range provider.Spec.Offers {
			if offer.Name == service.Offering {
				offering(provider, offer)
				found = true
			}
		}
		if !found {
			// The claim names an offering that has since been withdrawn. It
			// is still what the consumer depends on, which is the point of
			// drawing it.
			b.node(topologyNode{ID: target, Kind: topologyOffering, Name: service.Offering,
				Project: service.Project, Foreign: !scope.allows(service.Project), Phase: "Withdrawn"})
		}
	} else {
		b.node(topologyNode{ID: target, Kind: topologyOffering, Name: service.Offering,
			Project: service.Project, Foreign: !scope.allows(service.Project), Phase: "Withdrawn"})
	}

	state, reason := "", ""
	if claim.Status.Phase != "" && claim.Status.Phase != kitchenv1alpha1.ClaimBound {
		state = string(claim.Status.Phase)
	}

	if !scope.allows(consumer) {
		// Somebody else's project binds the caller's offering: the provider
		// is owed the name of who depends on it, and the Offerings pane
		// already answers it.
		b.edge(topologyConsumes, foreign(consumer), target, state, "")
		return
	}
	if len(environments[consumer]) == 0 {
		b.edge(topologyConsumes, projectNodeID(consumer), target, state, "")
		return
	}
	for _, env := range environments[consumer] {
		edgeState, edgeReason := state, reason
		if edgeState == "" && claim.Status.Service != nil {
			for _, binding := range claim.Status.Service.Bindings {
				if binding.Consumer != environmentType(env) {
					continue
				}
				if binding.Environment == "" {
					edgeState, edgeReason = "unresolved", binding.Reason
				}
			}
		}
		b.edge(topologyConsumes, environmentNodeID(env.Name), target, edgeState, edgeReason)
	}
}

// focusTopology narrows a graph to one project and everything one edge away
// from it — the other ends of its bindings, the connections behind its
// resources, the internet in front of it.
func focusTopology(graph topologyGraph, project string) topologyGraph {
	inside := map[string]bool{}
	for _, node := range graph.Nodes {
		if node.Project == project {
			inside[node.ID] = true
		}
	}
	keep := map[string]bool{}
	for id := range inside {
		keep[id] = true
	}
	out := topologyGraph{Nodes: []topologyNode{}, Edges: []topologyEdge{}}
	for _, edge := range graph.Edges {
		if inside[edge.From] || inside[edge.To] {
			out.Edges = append(out.Edges, edge)
			keep[edge.From] = true
			keep[edge.To] = true
		}
	}
	for _, node := range graph.Nodes {
		if keep[node.ID] {
			out.Nodes = append(out.Nodes, node)
		}
	}
	return out
}

// topology serves the declared graph.
func (s *Server) topology(w http.ResponseWriter, req *http.Request) {
	project := strings.TrimSpace(req.URL.Query().Get("project"))
	if !s.visibleProject(w, req, project) {
		return
	}
	in, err := s.readTopologyInputs(req.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	graph := buildTopology(in, scopeFrom(req.Context()))
	if project != "" {
		graph = focusTopology(graph, project)
	}
	writeJSON(w, http.StatusOK, graph)
}

// trafficIndex turns a flow's endpoint back into the node that materialized
// it: the controller's naming, read backwards.
type trafficIndex struct {
	scope projectScope
	// workloads maps namespace/workload to a node: an environment's own
	// Deployment and each of its processes' workloads.
	workloads map[string]string
	// prefixes are each application namespace's environment names, longest
	// first, for a workload named after its environment that the status does
	// not list — a deploy task's run, a scheduled process's Job.
	prefixes map[string][]string
	// instances maps a claim's provider-side name to its node. Providers put
	// what they run in namespaces of their own, so this is keyed on the name
	// alone; every such name carries the provider's `kitchen-` prefix and a
	// hash, so two of them do not collide.
	instances map[string]string
	// namespaces maps an application namespace to its project.
	namespaces map[string]string
}

func newTrafficIndex(in topologyInputs, scope projectScope) *trafficIndex {
	ix := &trafficIndex{
		scope:      scope,
		workloads:  map[string]string{},
		prefixes:   map[string][]string{},
		instances:  map[string]string{},
		namespaces: map[string]string{},
	}
	for i := range in.projects {
		ix.namespaces[controller.AppNamespace(in.projects[i].Name)] = in.projects[i].Name
	}
	for i := range in.environments {
		env := &in.environments[i]
		project := env.Spec.ProjectRef.Name
		namespace := controller.AppNamespace(project)
		ix.namespaces[namespace] = project
		node := ix.owned(project, environmentNodeID(env.Name))
		ix.workloads[namespace+"/"+env.Name] = node
		for _, process := range env.Status.Processes {
			workload := process.Workload
			if workload == "" {
				workload = controller.ProcessWorkloadName(env.Name, process.Name)
			}
			ix.workloads[namespace+"/"+workload] = node
		}
		ix.prefixes[namespace] = append(ix.prefixes[namespace], env.Name)
	}
	for namespace := range ix.prefixes {
		names := ix.prefixes[namespace]
		sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	}
	for i := range in.claims {
		claim := &in.claims[i]
		if claim.Spec.Type == kitchenv1alpha1.ClaimTypeService {
			continue
		}
		node := ix.owned(claim.Spec.ProjectRef.Name, resourceNodeID(claim.Name))
		for _, name := range []string{claim.Status.InstanceID, claim.Status.InstanceName} {
			if name != "" {
				ix.instances[name] = node
			}
		}
		for _, branch := range claim.Status.Branches {
			if branch.ID != "" {
				ix.instances[branch.ID] = node
			}
		}
	}
	return ix
}

// owned is a node of `project`'s when the caller may see that project, and
// the project as a whole when they may not.
func (ix *trafficIndex) owned(project, node string) string {
	if ix.scope.allows(project) {
		return node
	}
	return projectNodeID(project)
}

// resolve names the node one end of a flow belongs to, and the node itself
// when it is one the declared graph may not have.
func (ix *trafficIndex) resolve(name, namespace string) (string, *topologyNode) {
	if namespace == "" {
		if name == "" || name == worldEndpoint {
			node := internetNode()
			return node.ID, &node
		}
		node := topologyNode{ID: externalNodeID(name), Kind: topologyExternal, Name: name}
		return node.ID, &node
	}
	if id, ok := ix.workloads[namespace+"/"+name]; ok {
		return ix.answer(id)
	}
	if id, ok := ix.instances[name]; ok {
		return ix.answer(id)
	}
	for instance, id := range ix.instances {
		// A provider's own workloads are named after the instance they
		// serve — a database's pods, a cache's replicas.
		if strings.HasPrefix(name, instance+"-") {
			return ix.answer(id)
		}
	}
	if project, ok := ix.namespaces[namespace]; ok {
		for _, env := range ix.prefixes[namespace] {
			if strings.HasPrefix(name, env+"-") {
				return ix.answer(ix.owned(project, environmentNodeID(env)))
			}
		}
		return ix.answer(projectNodeID(project))
	}
	node := platformNode()
	return node.ID, &node
}

// answer is a resolved identifier, with the node spelled out when it is a
// whole project — the one kind the flows can reach that the declared graph
// may not have, because nothing of the caller's names it.
func (ix *trafficIndex) answer(id string) (string, *topologyNode) {
	project, ok := strings.CutPrefix(id, "project/")
	if !ok {
		return id, nil
	}
	node := topologyNode{ID: id, Kind: topologyProject, Name: project, Project: project,
		Foreign: !ix.scope.allows(project)}
	return id, &node
}

// declaredPaths answers which declared edges a flow from one node to another
// runs along: a direct edge, or two through the one node that sits between a
// caller and what it calls — a domain, or an offering.
type declaredPaths struct {
	out   map[string][]topologyEdge
	kinds map[string]string
}

func newDeclaredPaths(graph topologyGraph) declaredPaths {
	paths := declaredPaths{out: map[string][]topologyEdge{}, kinds: map[string]string{}}
	for _, node := range graph.Nodes {
		paths.kinds[node.ID] = node.Kind
	}
	for _, edge := range graph.Edges {
		paths.out[edge.From] = append(paths.out[edge.From], edge)
	}
	return paths
}

func (p declaredPaths) along(from, to string) []string {
	for _, edge := range p.out[from] {
		if edge.To == to {
			return []string{edge.ID}
		}
	}
	for _, first := range p.out[from] {
		kind := p.kinds[first.To]
		if kind != topologyOffering && kind != topologyDomain {
			continue
		}
		for _, second := range p.out[first.To] {
			if second.To == to {
				return []string{first.ID, second.ID}
			}
		}
	}
	return nil
}

// attributeTraffic lays the flow collector's edges onto the declared graph.
func attributeTraffic(
	edges []clickhouse.TrafficEdge,
	ix *trafficIndex,
	graph topologyGraph,
) topologyTraffic {
	paths := newDeclaredPaths(graph)
	extra := map[string]topologyNode{}
	merged := map[string]*observedEdge{}
	order := []string{}

	for _, edge := range edges {
		from, fromNode := ix.resolve(edge.Source, edge.SourceNamespace)
		to, toNode := ix.resolve(edge.Destination, edge.DestinationNamespace)
		if from == to {
			// An environment talking to itself — its worker to its web
			// process — is not an edge of this diagram.
			continue
		}
		for _, node := range []*topologyNode{fromNode, toNode} {
			if node == nil {
				continue
			}
			if _, declared := paths.kinds[node.ID]; !declared {
				extra[node.ID] = *node
			}
		}
		key := from + "->" + to
		observed, ok := merged[key]
		if !ok {
			observed = &observedEdge{From: from, To: to, Protocol: edge.Protocol}
			merged[key] = observed
			order = append(order, key)
		}
		if edge.Protocol == "HTTP" {
			observed.Protocol = "HTTP"
		}
		observed.Flows += edge.Flows
		observed.RPS += edge.RPS
		observed.Errors += edge.Errors
		observed.Drops += edge.Drops
		observed.P95Ms = max(observed.P95Ms, edge.P95Ms)
	}

	answer := topologyTraffic{Nodes: []topologyNode{}, Edges: make([]observedEdge, 0, len(order))}
	for _, key := range order {
		observed := merged[key]
		observed.Along = paths.along(observed.From, observed.To)
		observed.Status = observedStatus(observed, paths, extra)
		answer.Edges = append(answer.Edges, *observed)
	}
	for _, node := range extra {
		answer.Nodes = append(answer.Nodes, node)
	}
	sort.Slice(answer.Nodes, func(i, j int) bool { return answer.Nodes[i].ID < answer.Nodes[j].ID })
	sort.SliceStable(answer.Edges, func(i, j int) bool { return answer.Edges[i].Flows > answer.Edges[j].Flows })
	return answer
}

// observedStatus classifies one observed edge against the declared graph.
func observedStatus(edge *observedEdge, paths declaredPaths, extra map[string]topologyNode) string {
	if len(edge.Along) > 0 {
		return observedDeclared
	}
	kind := func(id string) string {
		if kind, ok := paths.kinds[id]; ok {
			return kind
		}
		return extra[id].Kind
	}
	from, to := kind(edge.From), kind(edge.To)
	switch {
	case from == topologyPlatform || to == topologyPlatform:
		return observedPlatform
	case from == topologyInternet || to == topologyInternet || from == topologyExternal || to == topologyExternal:
		return observedExternal
	default:
		return observedUndeclared
	}
}

// topologyTrafficRead serves the observed half for a window.
func (s *Server) topologyTrafficRead(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	since, err := timeParam(req, "since")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	until, err := timeParam(req, "until")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	project := strings.TrimSpace(req.URL.Query().Get("project"))
	if !s.visibleProject(w, req, project) {
		return
	}
	in, err := s.readTopologyInputs(ctx)
	if err != nil {
		s.writeError(w, err)
		return
	}

	// The store's own defaults, applied here so the answer can say which
	// window it is: the last hour, up to now.
	if until.IsZero() {
		until = time.Now()
	}
	if since.IsZero() {
		since = until.Add(-time.Hour)
	}
	query := clickhouse.TrafficQuery{Since: since, Until: until}
	if project != "" {
		query.Namespace = controller.AppNamespace(project)
	}
	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	edges, err := store.TrafficEdges(ctx, query)
	if err != nil {
		s.writeStoreError(w, err, "the traffic query")
		return
	}

	scope := scopeFrom(ctx)
	graph := buildTopology(in, scope)
	answer := attributeTraffic(visibleEdges(scope, edges), newTrafficIndex(in, scope), graph)
	if project != "" {
		completeFocus(&answer, graph, focusTopology(graph, project))
	}
	answer.Since, answer.Until = since.UTC(), until.UTC()
	writeJSON(w, http.StatusOK, answer)
}

// completeFocus adds the declared nodes a focused reading of the graph left
// out but the traffic reaches — an environment of the project two edges from
// the one asked about — so that every observed edge has both of its ends in
// the two answers together.
func completeFocus(answer *topologyTraffic, full, shown topologyGraph) {
	have := map[string]bool{}
	for _, node := range shown.Nodes {
		have[node.ID] = true
	}
	for _, node := range answer.Nodes {
		have[node.ID] = true
	}
	nodes := make(map[string]topologyNode, len(full.Nodes))
	for _, node := range full.Nodes {
		nodes[node.ID] = node
	}
	for _, edge := range answer.Edges {
		for _, id := range []string{edge.From, edge.To} {
			if node, ok := nodes[id]; ok && !have[id] {
				answer.Nodes = append(answer.Nodes, node)
				have[id] = true
			}
		}
	}
	sort.Slice(answer.Nodes, func(i, j int) bool { return answer.Nodes[i].ID < answer.Nodes[j].ID })
}

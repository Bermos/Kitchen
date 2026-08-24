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
)

// The criticality surface (issue #141): the designation on the way in, and
// the two questions it exists to answer on the way out.
//
// **Kitchen does not decide what is critical, and does not set the
// tolerances.** Those are the institution's, and this file refuses nothing on
// them: no route here blocks a deployment, and no designation is a gate. What
// the platform contributes is the part an institution assembling this by hand
// gets wrong — the *mapping*. Which environments, releases, resource claims,
// connections, domains and third parties stand behind a designated function,
// and which functions stand behind one third party. Both are traversals of a
// graph that is reconciled rather than maintained, which is why they are one
// request each and why neither has a cache: there is nothing to keep in step.
//
// Two read routes rather than an extension of GET /compliance/inventory, and
// deliberately. The inventory is a flat exportable table — one row per
// environment or claim, diffable between two exports — and both questions
// here are trees. Flattening the forward map into that table would throw away
// the mapping, which is the whole answer, and widening the table's row would
// change the shape of an export somebody else already reads.

// continuityChange is what a PATCH says about a designation: each field is nil
// when the request did not mention it, so one call can change any of the three
// without restating the rest.
//
// It is shared between the project's settings endpoint and the environment's
// owner-gated one because the vocabulary and the audit record must be the
// same on both. They are different writes with different guards; they are not
// two different meanings of the word "critical".
type continuityChange struct {
	criticality *kitchenv1alpha1.Criticality
	rto         *kitchenv1alpha1.Tolerance
	rpo         *kitchenv1alpha1.Tolerance
}

// touched reports whether the request said anything about the designation.
func (c continuityChange) touched() bool {
	return c.criticality != nil || c.rto != nil || c.rpo != nil
}

// continuityFromRequest validates the three fields as a set. A nil pointer in
// means a nil pointer out — untouched, not cleared.
func continuityFromRequest(criticality, rto, rpo *string) (continuityChange, error) {
	change := continuityChange{}
	if criticality != nil {
		designation, err := criticalityFromRequest(*criticality)
		if err != nil {
			return continuityChange{}, err
		}
		change.criticality = &designation
	}
	for _, field := range []struct {
		name  string
		value *string
		into  **kitchenv1alpha1.Tolerance
	}{{"rto", rto, &change.rto}, {"rpo", rpo, &change.rpo}} {
		if field.value == nil {
			continue
		}
		tolerance, err := toleranceFromRequest(field.name, *field.value)
		if err != nil {
			return continuityChange{}, err
		}
		*field.into = &tolerance
	}
	return change, nil
}

// recordInto writes the before and after into an audit record's details and
// marks it privileged — the same treatment a data class gets, for the same
// reason: the designation decides what alerts and what a policy may demand,
// so the trail has to show what it was before. A criticality is a label, not
// a secret.
func (c continuityChange) recordInto(
	details map[string]any, before kitchenv1alpha1.Continuity,
) {
	if !c.touched() {
		return
	}
	details["privileged"] = true
	if c.criticality != nil {
		details["previousCriticality"] = string(before.Criticality)
		details["criticality"] = string(*c.criticality)
	}
	if c.rto != nil {
		details["previousRTO"] = string(before.RTO)
		details["rto"] = string(*c.rto)
	}
	if c.rpo != nil {
		details["previousRPO"] = string(before.RPO)
		details["rpo"] = string(*c.rpo)
	}
}

// apply writes the change onto a spec's three fields.
func (c continuityChange) apply(
	criticality *kitchenv1alpha1.Criticality, rto, rpo *kitchenv1alpha1.Tolerance,
) {
	if c.criticality != nil {
		*criticality = *c.criticality
	}
	if c.rto != nil {
		*rto = *c.rto
	}
	if c.rpo != nil {
		*rpo = *c.rpo
	}
}

// changedContinuityFields names the designation fields a PATCH carried, for
// an audit record's field list.
func (c continuityChange) changedFields() []string {
	fields := []string{}
	for _, field := range []struct {
		name    string
		changed bool
	}{
		{"criticality", c.criticality != nil},
		{"rto", c.rto != nil},
		{"rpo", c.rpo != nil},
	} {
		if field.changed {
			fields = append(fields, field.name)
		}
	}
	return fields
}

// The word an absent designation is answered with. It is a word rather than an
// empty string for the reason the inventory's three absences are: a blank cell
// in something an auditor reads invites a generous reading, and "nobody has
// designated this" is a finding in its own right.
const criticalityUndesignated = "undesignated"

// mapDepth is the honest limit of both traversals, carried in the answer
// rather than only in the documentation, because the answer is the thing that
// gets exported and read six months later.
const mapDepth = "The graph the platform reconciles: a project's git source, its registry, its " +
	"resource claims and the Connections behind them, plus each environment's release and " +
	"custom domains. A third party an application calls at runtime — a payment gateway in its " +
	"own code — is not a Connection and is not visible here."

// criticalityFunctionView is one designated function: the project, what it is
// designated, and everything the platform can see standing behind it.
type criticalityFunctionView struct {
	Project     string `json:"project"`
	Criticality string `json:"criticality"`
	RTO         string `json:"rto,omitempty"`
	RPO         string `json:"rpo,omitempty"`

	Environments []criticalityEnvironmentView `json:"environments"`
	Claims       []criticalityClaimView       `json:"claims"`
	Connections  []criticalityConnectionView  `json:"connections"`
	// ThirdParties is the distinct set of providers behind those connections,
	// which is the list an operational-resilience register asks for.
	ThirdParties []string `json:"thirdParties"`
}

// criticalityEnvironmentView is one environment under a function, with the
// designation that actually applies to it.
type criticalityEnvironmentView struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Criticality string `json:"criticality"`
	RTO         string `json:"rto,omitempty"`
	RPO         string `json:"rpo,omitempty"`
	// Inherited names the fields that came from the project rather than from
	// the environment, so nothing here reads as a declaration nobody made.
	Inherited []string `json:"inherited,omitempty"`

	URL string `json:"url,omitempty"`
	// Release is what the environment is *observed* to be running where the
	// reconciler has said so, and what it has been pointed at otherwise. The
	// distinction matters here more than on most screens: a mapping that
	// named the release a rollout is still applying would name an artifact
	// that is not yet serving the function.
	Release string `json:"release,omitempty"`
	// Image is that release's deployable reference — the artifact every piece
	// of evidence about this function is keyed to.
	Image   string   `json:"image,omitempty"`
	Domains []string `json:"domains,omitempty"`
}

// criticalityClaimView is one provisioned resource under a function.
type criticalityClaimView struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Connection string `json:"connection,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Phase      string `json:"phase,omitempty"`
	DataClass  string `json:"dataClass"`
	Residency  string `json:"residency"`
}

// criticalityConnectionView is one third-party relationship the function
// depends on, and what it depends on it for.
type criticalityConnectionView struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	// UsedFor is source, registry, or the claim it provisions.
	UsedFor []string `json:"usedFor"`
}

// criticalityMapBody is the whole forward answer.
type criticalityMapBody struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// Minimum is the filter applied, absent when none was asked for.
	Minimum   string                    `json:"minimum,omitempty"`
	Functions []criticalityFunctionView `json:"functions"`
	// Undesignated is how many of the caller's visible projects carry no
	// designation anywhere — on themselves or on any environment. It is in
	// the answer because a map of three critical functions means one thing
	// when the estate is four projects and another when it is ninety.
	Undesignated int    `json:"undesignated"`
	Depth        string `json:"depth"`
}

// complianceCriticality answers GET /api/v1/compliance/criticality: the
// function-to-resource mapping, in one request.
func (s *Server) complianceCriticality(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	scope := scopeFrom(ctx)

	minimum, err := criticalityFromRequest(req.URL.Query().Get("criticality"))
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	only := strings.TrimSpace(req.URL.Query().Get("project"))

	graph, err := s.readGraph(ctx)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := criticalityMapBody{
		GeneratedAt: time.Now().UTC(),
		Minimum:     string(minimum),
		Functions:   []criticalityFunctionView{},
		Depth:       mapDepth,
	}
	for i := range graph.projects {
		project := &graph.projects[i]
		if !scope.allows(project.Name) || (only != "" && project.Name != only) {
			continue
		}
		function := graph.function(project)
		if !function.designated() {
			body.Undesignated++
			continue
		}
		if minimum.Designated() && !function.atLeast(minimum) {
			continue
		}
		body.Functions = append(body.Functions, function.view())
	}
	writeJSON(w, http.StatusOK, body)
}

// criticalityDependentView is one environment that would be affected, and how
// it reaches the subject.
type criticalityDependentView struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Type        string   `json:"type"`
	Criticality string   `json:"criticality"`
	RTO         string   `json:"rto,omitempty"`
	RPO         string   `json:"rpo,omitempty"`
	Inherited   []string `json:"inherited,omitempty"`
	// Through is how the dependency runs: source, registry, or the claim.
	Through []string `json:"through"`
}

// criticalitySubjectView names what was asked about.
type criticalitySubjectView struct {
	// Kind is connection or provider.
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	// Connections is which Connections a provider query resolved to. Empty
	// for a provider nothing is configured with, which is an answer worth
	// distinguishing from a provider nothing depends on.
	Connections []string `json:"connections,omitempty"`
}

// criticalityDependentsBody is the whole reverse answer.
type criticalityDependentsBody struct {
	GeneratedAt time.Time                  `json:"generatedAt"`
	Subject     criticalitySubjectView     `json:"subject"`
	Affected    []criticalityDependentView `json:"affected"`
	// Counts is the affected environments by designation, "undesignated"
	// included — the headline an incident call actually wants.
	Counts map[string]int `json:"counts"`
	// TightestRTO is the smallest recovery objective among the affected
	// environments: how long this third party may be gone before the first
	// tolerance is breached. Absent when none of them declares one.
	TightestRTO string `json:"tightestRTO,omitempty"`
	Depth       string `json:"depth"`
}

// complianceDependents answers GET /api/v1/compliance/dependents: what breaks
// if this connection, or everything from this provider, is unavailable.
func (s *Server) complianceDependents(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	scope := scopeFrom(ctx)

	query := req.URL.Query()
	connection := strings.TrimSpace(query.Get("connection"))
	provider := strings.TrimSpace(query.Get("provider"))
	switch {
	case connection == "" && provider == "":
		badRequest(w, "ask about something: ?connection=<name> for one Connection, "+
			"or ?provider=<name> for every Connection from one third party")
		return
	case connection != "" && provider != "":
		badRequest(w, "ask about one thing: connection and provider name different subjects, "+
			"and answering both would say which environments depend on either")
		return
	}

	graph, err := s.readGraph(ctx)
	if err != nil {
		s.writeError(w, err)
		return
	}

	subject := criticalitySubjectView{Kind: "connection", Name: connection}
	subjects := map[string]struct{}{}
	if connection != "" {
		if found, ok := graph.connections[connection]; ok {
			subject.Provider = found.Spec.Provider
		}
		subjects[connection] = struct{}{}
	} else {
		subject = criticalitySubjectView{Kind: "provider", Name: provider, Provider: provider}
		for name, conn := range graph.connections {
			if !strings.EqualFold(conn.Spec.Provider, provider) {
				continue
			}
			subjects[name] = struct{}{}
			subject.Connections = append(subject.Connections, name)
		}
		sort.Strings(subject.Connections)
	}

	body := criticalityDependentsBody{
		GeneratedAt: time.Now().UTC(),
		Subject:     subject,
		Affected:    []criticalityDependentView{},
		Counts:      map[string]int{},
		Depth:       mapDepth,
	}
	var tightest time.Duration
	for i := range graph.projects {
		project := &graph.projects[i]
		if !scope.allows(project.Name) {
			continue
		}
		through := graph.pathsTo(project, subjects)
		if len(through) == 0 {
			continue
		}
		for _, env := range graph.environmentsOf(project.Name) {
			continuity := kitchenv1alpha1.EffectiveContinuity(project, env)
			designation := orWord(string(continuity.Criticality), criticalityUndesignated)
			body.Counts[designation]++
			body.Affected = append(body.Affected, criticalityDependentView{
				Project:     project.Name,
				Environment: env.Name,
				Type:        string(env.Spec.Type),
				Criticality: designation,
				RTO:         string(continuity.RTO),
				RPO:         string(continuity.RPO),
				Inherited:   continuity.Inherited,
				Through:     through,
			})
			if objective, ok := continuity.RTO.Duration(); ok &&
				(tightest == 0 || objective < tightest) {
				tightest = objective
				body.TightestRTO = string(continuity.RTO)
			}
		}
	}

	// Worst first, so an incident call reads the answer top-down, then by
	// name so two reads of an unchanged estate are byte-identical.
	sort.SliceStable(body.Affected, func(i, j int) bool {
		left := kitchenv1alpha1.Criticality(body.Affected[i].Criticality).Rank()
		right := kitchenv1alpha1.Criticality(body.Affected[j].Criticality).Rank()
		if left != right {
			return left > right
		}
		if body.Affected[i].Project != body.Affected[j].Project {
			return body.Affected[i].Project < body.Affected[j].Project
		}
		return body.Affected[i].Environment < body.Affected[j].Environment
	})
	writeJSON(w, http.StatusOK, body)
}

// complianceGraph is the one read both routes make: everything the traversal
// walks, listed once.
//
// It is deliberately not a cache and not an index. Five list calls against the
// operator's own informer cache is what a reconciled inventory costs, and an
// answer assembled per request cannot go stale — which is the property the
// whole feature turns on, since a mapping maintained by hand is exactly what
// this exists to replace.
type complianceGraph struct {
	projects     []kitchenv1alpha1.Project
	environments []kitchenv1alpha1.Environment
	claims       []kitchenv1alpha1.ResourceClaim
	domains      []kitchenv1alpha1.Domain
	connections  map[string]*kitchenv1alpha1.Connection
	// images is release name to deployable reference, so an environment's row
	// can name the artifact rather than only the object pointing at it.
	images map[string]string
}

func (s *Server) readGraph(ctx context.Context) (*complianceGraph, error) {
	graph := &complianceGraph{
		connections: map[string]*kitchenv1alpha1.Connection{},
		images:      map[string]string{},
	}

	projects := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(ctx, projects, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	graph.projects = projects.Items
	sort.Slice(graph.projects, func(i, j int) bool {
		return graph.projects[i].Name < graph.projects[j].Name
	})

	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := s.Client.List(ctx, environments, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	graph.environments = environments.Items

	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := s.Client.List(ctx, claims, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	graph.claims = claims.Items

	domains := &kitchenv1alpha1.DomainList{}
	if err := s.Client.List(ctx, domains, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	graph.domains = domains.Items

	connections := &kitchenv1alpha1.ConnectionList{}
	if err := s.Client.List(ctx, connections, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	for i := range connections.Items {
		graph.connections[connections.Items[i].Name] = &connections.Items[i]
	}

	releases := &kitchenv1alpha1.ReleaseList{}
	if err := s.Client.List(ctx, releases, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	for i := range releases.Items {
		graph.images[releases.Items[i].Name] = releases.Items[i].Spec.Image
	}
	return graph, nil
}

// environmentsOf lists one project's environments, production first and then
// by name — the order a person reads them in.
func (g *complianceGraph) environmentsOf(project string) []*kitchenv1alpha1.Environment {
	found := []*kitchenv1alpha1.Environment{}
	for i := range g.environments {
		if g.environments[i].Spec.ProjectRef.Name == project {
			found = append(found, &g.environments[i])
		}
	}
	sort.Slice(found, func(i, j int) bool {
		left := found[i].Spec.Type == kitchenv1alpha1.EnvironmentProduction
		right := found[j].Spec.Type == kitchenv1alpha1.EnvironmentProduction
		if left != right {
			return left
		}
		return found[i].Name < found[j].Name
	})
	return found
}

// claimsOf lists one project's resource claims by name.
func (g *complianceGraph) claimsOf(project string) []*kitchenv1alpha1.ResourceClaim {
	found := []*kitchenv1alpha1.ResourceClaim{}
	for i := range g.claims {
		if g.claims[i].Spec.ProjectRef.Name == project {
			found = append(found, &g.claims[i])
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found
}

// domainsOf lists the custom hostnames pointed at one environment.
func (g *complianceGraph) domainsOf(environment string) []string {
	found := []string{}
	for i := range g.domains {
		if g.domains[i].Spec.EnvironmentRef.Name == environment {
			found = append(found, g.domains[i].Spec.Hostname)
		}
	}
	sort.Strings(found)
	return found
}

// connectionUses is which Connections a project depends on, and for what.
// It is the one place the three ways a project reaches a third party are
// enumerated, so the forward map and the reverse query cannot disagree about
// what "depends on" means.
func (g *complianceGraph) connectionUses(project *kitchenv1alpha1.Project) map[string][]string {
	uses := map[string][]string{}
	add := func(name, reason string) {
		if name == "" {
			return
		}
		uses[name] = append(uses[name], reason)
	}
	add(project.Spec.Source.ConnectionRef.Name, "source")
	add(project.Spec.Registry.ConnectionRef.Name, "registry")
	for _, claim := range g.claimsOf(project.Name) {
		if ref := claim.Spec.ConnectionRef; ref != nil {
			add(ref.Name, "claim "+claim.Name)
		}
	}
	for name := range uses {
		sort.Strings(uses[name])
	}
	return uses
}

// pathsTo is the reverse traversal for one project: how it reaches any of the
// subject Connections, empty when it does not.
func (g *complianceGraph) pathsTo(
	project *kitchenv1alpha1.Project, subjects map[string]struct{},
) []string {
	through := []string{}
	for name, reasons := range g.connectionUses(project) {
		if _, ok := subjects[name]; !ok {
			continue
		}
		through = append(through, reasons...)
	}
	sort.Strings(through)
	return through
}

// complianceFunction is one project resolved into the forward map's answer,
// built before it is filtered so the filter can read the environments'
// effective designations rather than only the project's.
type complianceFunction struct {
	project      *kitchenv1alpha1.Project
	environments []criticalityEnvironmentView
	claims       []criticalityClaimView
	connections  []criticalityConnectionView
	thirdParties []string
	// worst is the highest designation anywhere under the function: the
	// project's own or any environment's, which is what the ?criticality=
	// filter compares against. A project nobody designated with one
	// environment somebody did is a designated function.
	worst kitchenv1alpha1.Criticality
}

func (f complianceFunction) designated() bool {
	if f.worst.Designated() {
		return true
	}
	// A tolerance without a criticality is still a designation somebody
	// made, and dropping it would lose the RTO the alerting fires against.
	if f.project.Spec.RTO.Declared() || f.project.Spec.RPO.Declared() {
		return true
	}
	for _, env := range f.environments {
		if env.RTO != "" || env.RPO != "" {
			return true
		}
	}
	return false
}

func (f complianceFunction) atLeast(minimum kitchenv1alpha1.Criticality) bool {
	return f.worst.AtLeast(minimum)
}

func (f complianceFunction) view() criticalityFunctionView {
	return criticalityFunctionView{
		Project:      f.project.Name,
		Criticality:  orWord(string(f.project.Spec.Criticality), criticalityUndesignated),
		RTO:          string(f.project.Spec.RTO),
		RPO:          string(f.project.Spec.RPO),
		Environments: f.environments,
		Claims:       f.claims,
		Connections:  f.connections,
		ThirdParties: f.thirdParties,
	}
}

// function assembles everything standing behind one project.
func (g *complianceGraph) function(project *kitchenv1alpha1.Project) complianceFunction {
	assembled := complianceFunction{
		project:      project,
		environments: []criticalityEnvironmentView{},
		claims:       []criticalityClaimView{},
		connections:  []criticalityConnectionView{},
		thirdParties: []string{},
		worst:        project.Spec.Criticality,
	}

	for _, env := range g.environmentsOf(project.Name) {
		continuity := kitchenv1alpha1.EffectiveContinuity(project, env)
		if continuity.Criticality.AtLeast(assembled.worst) {
			assembled.worst = continuity.Criticality
		}
		release := env.Status.ObservedRelease
		if release == "" {
			release = env.Spec.ReleaseRef.Name
		}
		assembled.environments = append(assembled.environments, criticalityEnvironmentView{
			Name:        env.Name,
			Type:        string(env.Spec.Type),
			Criticality: orWord(string(continuity.Criticality), criticalityUndesignated),
			RTO:         string(continuity.RTO),
			RPO:         string(continuity.RPO),
			Inherited:   continuity.Inherited,
			URL:         env.Status.URL,
			Release:     release,
			Image:       g.images[release],
			Domains:     g.domainsOf(env.Name),
		})
	}

	providers := map[string]struct{}{}
	for _, claim := range g.claimsOf(project.Name) {
		view := criticalityClaimView{
			Name:      claim.Name,
			Type:      claim.Spec.Type,
			Phase:     string(claim.Status.Phase),
			DataClass: orWord(string(claim.Spec.DataClass), inventoryUnclassified),
			Residency: orWord(claim.Status.Residency, inventoryUnknown),
		}
		if ref := claim.Spec.ConnectionRef; ref != nil {
			view.Connection = ref.Name
			if conn, ok := g.connections[ref.Name]; ok {
				view.Provider = conn.Spec.Provider
			}
		} else {
			// The one claim type with no Connection: its third party is the
			// platform's own identity provider, and saying so is better than
			// a blank that reads as "nobody".
			view.Provider = "platform identity provider"
		}
		if view.Provider != "" {
			providers[view.Provider] = struct{}{}
		}
		assembled.claims = append(assembled.claims, view)
	}

	for name, reasons := range g.connectionUses(project) {
		view := criticalityConnectionView{Name: name, UsedFor: reasons}
		if conn, ok := g.connections[name]; ok {
			view.Provider = conn.Spec.Provider
			providers[conn.Spec.Provider] = struct{}{}
		}
		assembled.connections = append(assembled.connections, view)
	}
	sort.Slice(assembled.connections, func(i, j int) bool {
		return assembled.connections[i].Name < assembled.connections[j].Name
	})

	for provider := range providers {
		assembled.thirdParties = append(assembled.thirdParties, provider)
	}
	sort.Strings(assembled.thirdParties)
	return assembled
}

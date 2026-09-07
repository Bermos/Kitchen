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
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// What one project offers another, as the API writes it and as the platform
// lists it (#493).
//
// The write is a field of the project's own settings — `offers` on
// PATCH /projects/{name} — beside the workload list and the files, because
// that is what an offering is: a statement the project makes about itself,
// changed by the same admin who decides what the project runs. It is
// deliberately not a route of its own. A whole route is the unit of
// authorization here, and a route saying exactly what the settings route
// already says would be a second spelling of one rule.
//
// The read is a route of its own, and has to be: a consumer choosing an
// offering holds no role on the project that makes it, so nothing it can
// already read would tell it the offering exists.

// offeringRequest is one entry of `offers` on the settings route.
type offeringRequest struct {
	Name string `json:"name"`
	// Process is the workload that answers; empty is the web process.
	Process string `json:"process,omitempty"`
	// Protocol is `http` or `tcp`; empty is http.
	Protocol string `json:"protocol,omitempty"`
	// Auth is what a consumer has to do to be admitted by the application
	// itself. `none` is the only rung built; `gate` and `oidc` are #496.
	Auth string `json:"auth,omitempty"`
	// Visibility is who may bind: `request` (the default, and the closed
	// one) or `open`. It is the half of an offering a repository may not
	// declare — a grant anybody with push access could widen is not a grant
	// — so this route is the only way it is ever set.
	Visibility string `json:"visibility,omitempty"`
	// Environment is which of this project's environments the offering
	// serves; empty is the project's production environment.
	Environment string `json:"environment,omitempty"`
}

// offeringsFromRequest validates the whole list against the project the
// offerings are on, and answers it as the spec holds it.
//
// `processes` is the project's workloads *as this request leaves them*, so
// that one request may add a service workload and offer it — the same
// ordering the files take, and for the same reason.
func offeringsFromRequest(
	requests []offeringRequest,
	processes []kitchenv1alpha1.ProcessSpec,
) ([]kitchenv1alpha1.ServiceOffering, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	offers := make([]kitchenv1alpha1.ServiceOffering, 0, len(requests))
	seen := map[string]bool{}
	for _, request := range requests {
		name := strings.TrimSpace(request.Name)
		if name == "" {
			return nil, fmt.Errorf("every offering has a name: it is what a consumer's claim names, and one " +
				"of them has none")
		}
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 || len(name) > 40 {
			return nil, fmt.Errorf("offering %q must work as a DNS label of at most 40 characters — "+
				"lowercase letters, digits and '-', starting and ending alphanumeric: the name travels "+
				"into the consumer's environment variables", request.Name)
		}
		if seen[name] {
			return nil, fmt.Errorf("offering %q is declared twice: one name is one offering", name)
		}
		seen[name] = true

		offering := kitchenv1alpha1.ServiceOffering{
			Name:        name,
			Process:     strings.TrimSpace(request.Process),
			Environment: strings.TrimSpace(request.Environment),
		}
		if err := offeringProcess(&offering, processes); err != nil {
			return nil, err
		}
		switch protocol := kitchenv1alpha1.OfferingProtocol(strings.TrimSpace(request.Protocol)); protocol {
		case "":
		case kitchenv1alpha1.OfferingHTTP, kitchenv1alpha1.OfferingTCP:
			offering.Speaks = protocol
		default:
			return nil, fmt.Errorf("offering %q speaks either http, which is handed over as a URL as well as "+
				"a host and a port, or tcp, which is handed over as the host and the port alone (got %q)",
				name, request.Protocol)
		}
		switch auth := kitchenv1alpha1.OfferingAuth(strings.TrimSpace(request.Auth)); auth {
		case "", kitchenv1alpha1.OfferingAuthNone:
			offering.Authorization = auth
		default:
			return nil, fmt.Errorf("offering %q asks for auth %q, and the only rung built is %s: what a "+
				"consumer is admitted by today is the grant on the offering, and — once policy between "+
				"application namespaces lands — reachability. A forward-auth gate and per-consumer OIDC "+
				"identities are #496", name, request.Auth, kitchenv1alpha1.OfferingAuthNone)
		}
		switch visibility := kitchenv1alpha1.OfferingVisibility(strings.TrimSpace(request.Visibility)); visibility {
		case "":
		case kitchenv1alpha1.OfferingRequest, kitchenv1alpha1.OfferingOpen:
			offering.VisibleTo = visibility
		default:
			return nil, fmt.Errorf("offering %q must be visible to consumers by %s — approved one at a time, "+
				"which is the default — or %s, which admits every project on the platform (got %q)",
				name, kitchenv1alpha1.OfferingRequest, kitchenv1alpha1.OfferingOpen, request.Visibility)
		}
		offers = append(offers, offering)
	}
	return offers, nil
}

// offeringProcess holds the workload an offering names to the two things
// that have to be true of it: the project declares it, and something
// addresses it. A worker and a scheduled job are refused by name — nothing
// is in front of either, so there is no address to hand a consumer, and
// finding that out from a claim that will not bind is the wrong end of it.
func offeringProcess(
	offering *kitchenv1alpha1.ServiceOffering,
	processes []kitchenv1alpha1.ProcessSpec,
) error {
	name := offering.ProcessName()
	if name == kitchenv1alpha1.WebProcessName {
		return nil
	}
	names := []string{kitchenv1alpha1.WebProcessName}
	for _, process := range processes {
		names = append(names, process.Name)
		if process.Name != name {
			continue
		}
		if !process.Addressed() {
			return fmt.Errorf("offering %q names the %s workload %q, and nothing addresses one: only the "+
				"web workload and a service workload answer an address. Offer one of those",
				offering.Name, process.Type, name)
		}
		return nil
	}
	return fmt.Errorf("offering %q names the workload %q, which this project does not have: its workloads "+
		"are %s", offering.Name, name, strings.Join(names, ", "))
}

// offeringView is one offering as the catalogue answers it.
type offeringView struct {
	Project  string `json:"project"`
	Name     string `json:"name"`
	Process  string `json:"process"`
	Protocol string `json:"protocol"`
	Auth     string `json:"auth"`
	// Visibility is `open` — any project may bind — or `request`, which
	// admits a consumer the project has approved. Approving is #495; until
	// that lands a `request` offering is listed and binds nobody, which is
	// what the catalogue is for: an offering nobody can see is an offering
	// nobody asks for.
	Visibility string `json:"visibility"`
	// Environment is which environment of the providing project a binding
	// resolves to, with the default filled in, so a reader never has to know
	// how the platform names a project's production environment.
	Environment string `json:"environment"`
	// Mine says this offering is made by a project the caller holds a role
	// on. It is what lets the dashboard tell "something my team offers"
	// from "something another team offers" without a second read.
	Mine bool `json:"mine,omitempty"`
}

// listOfferings is the catalogue: every offering this caller may bind to,
// plus every offering of a project they can already see.
//
// **An `open` offering is listed to every account with a token, whatever
// projects they hold a role on**, and that is the grant doing exactly what
// it says. `open` means any project on the platform may bind, so the name of
// the offering, the workload behind it and the environment it serves are
// already knowable by anyone who could make a claim naming them — and an
// offering nobody can find is one nobody binds to, which would leave the
// claim's `service.offering` a name to be told over a desk. What is not
// listed is anything else about the project: no repository, no environments,
// no members, no address beyond the offering's own.
//
// A `request` offering is listed on the same terms, because being asked is
// what it is for. Nothing about either says whether the offering is
// *working*: that is the providing project's screen, and a consumer learns
// it from its own claim.
func (s *Server) listOfferings(w http.ResponseWriter, req *http.Request) {
	projects := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(req.Context(), projects, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	scope := scopeFrom(req.Context())
	filter := strings.TrimSpace(req.URL.Query().Get("project"))
	views := []offeringView{}
	for i := range projects.Items {
		project := &projects.Items[i]
		if filter != "" && project.Name != filter {
			continue
		}
		mine := scope.allows(project.Name)
		for _, offering := range project.Spec.Offers {
			views = append(views, offeringView{
				Project:     project.Name,
				Name:        offering.Name,
				Process:     offering.ProcessName(),
				Protocol:    string(offering.Protocol()),
				Auth:        string(offering.Auth()),
				Visibility:  string(offering.Visibility()),
				Environment: offeringEnvironmentName(project, offering),
				Mine:        mine,
			})
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Project != views[j].Project {
			return views[i].Project < views[j].Project
		}
		return views[i].Name < views[j].Name
	})
	writeList(w, views)
}

// offeringEnvironmentName is the environment an offering serves, with the
// project's production environment filled in where it names none. The
// derivation is the controller's, asked here rather than spelled again.
func offeringEnvironmentName(
	project *kitchenv1alpha1.Project,
	offering kitchenv1alpha1.ServiceOffering,
) string {
	if offering.Environment != "" {
		return offering.Environment
	}
	return controller.ProductionTargetEnvironmentName(project)
}

// offeringViews is a project's own offerings, for the project payload.
func offeringViews(project *kitchenv1alpha1.Project) []offeringView {
	if len(project.Spec.Offers) == 0 {
		return nil
	}
	views := make([]offeringView, 0, len(project.Spec.Offers))
	for _, offering := range project.Spec.Offers {
		views = append(views, offeringView{
			Project:     project.Name,
			Name:        offering.Name,
			Process:     offering.ProcessName(),
			Protocol:    string(offering.Protocol()),
			Auth:        string(offering.Auth()),
			Visibility:  string(offering.Visibility()),
			Environment: offeringEnvironmentName(project, offering),
			Mine:        true,
		})
	}
	return views
}

// consumersOf is every project that binds an offering of this one, by name
// and without repeats — what deleting a project has to be able to say, and
// what the Offerings pane shows beside each offering.
func consumersOf(claims []kitchenv1alpha1.ResourceClaim, provider string) []string {
	var consumers []string
	for i := range claims {
		claim := &claims[i]
		if claim.Spec.Type != kitchenv1alpha1.ClaimTypeService {
			continue
		}
		if claim.Service().Project != provider || claim.Spec.ProjectRef.Name == provider {
			continue
		}
		if !slices.Contains(consumers, claim.Spec.ProjectRef.Name) {
			consumers = append(consumers, claim.Spec.ProjectRef.Name)
		}
	}
	sort.Strings(consumers)
	return consumers
}

// serviceClaims is every service claim on the platform, which the two
// questions below are asked of.
func (s *Server) serviceClaims(ctx context.Context) ([]kitchenv1alpha1.ResourceClaim, error) {
	list := &kitchenv1alpha1.ResourceClaimList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	claims := make([]kitchenv1alpha1.ResourceClaim, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.Type == kitchenv1alpha1.ClaimTypeService {
			claims = append(claims, list.Items[i])
		}
	}
	return claims, nil
}

// refusedWorkloadNamedLikeABinding is the collision rule from the other
// side: a binding arrives in the pod as `KITCHEN_SERVICE_<NAME>`, so a
// workload added later under a binding's name would be a second address in
// the same variable. The claim's own creation refuses the collision the
// other way round; this is the half that would otherwise sneak past it.
func (s *Server) refusedWorkloadNamedLikeABinding(
	ctx context.Context,
	w http.ResponseWriter,
	project *kitchenv1alpha1.Project,
	processes []kitchenv1alpha1.ProcessSpec,
) bool {
	claims, err := s.serviceClaims(ctx)
	if err != nil {
		s.writeError(w, err)
		return true
	}
	for i := range claims {
		claim := &claims[i]
		if claim.Spec.ProjectRef.Name != project.Name {
			continue
		}
		for _, process := range processes {
			if process.Name != claim.Name {
				continue
			}
			badRequest(w, "workload %q would collide with this project's binding to %s/%s: the binding "+
				"reaches the application as %s, which is the same variable this workload's address would "+
				"arrive in. Rename the workload, or delete the binding",
				process.Name, claim.Service().Project, claim.Service().Offering,
				kitchenv1alpha1.ServiceEnvPrefix(process.Name))
			return true
		}
	}
	return false
}

// breakBindingsParam is the explicit confirmation that deletes a project
// other projects bind to.
const breakBindingsParam = "breakBindings"

// refusedProjectWithConsumers is the blast radius the project finalizer
// cannot take back: deleting a project takes its offerings with it, and
// every binding to them stops resolving.
//
// So it is refused, and the refusal names the projects on the other end —
// which is the list whoever is deleting has to talk to. `?breakBindings=true`
// says it anyway, and it is deliberately a thing that has to be typed: the
// dashboard already confirms a project deletion by typing its name, and this
// is the second sentence of that confirmation rather than a checkbox nobody
// reads.
//
// The consumers' claims are not deleted and nothing of theirs is touched.
// They go Failed on their next reconcile, saying that the project they bind
// to is gone — which is the true state and the one they can act on.
func (s *Server) refusedProjectWithConsumers(
	ctx context.Context,
	w http.ResponseWriter,
	req *http.Request,
	project *kitchenv1alpha1.Project,
) bool {
	if strings.EqualFold(strings.TrimSpace(req.URL.Query().Get(breakBindingsParam)), "true") {
		return false
	}
	claims, err := s.serviceClaims(ctx)
	if err != nil {
		s.writeError(w, err)
		return true
	}
	consumers := consumersOf(claims, project.Name)
	if len(consumers) == 0 {
		return false
	}
	writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
		"project %s offers services that %d other project(s) bind to — %s — and deleting it takes the "+
			"offerings with it, so every one of those bindings stops resolving. Have them drop the "+
			"bindings first, or repeat the request with ?%s=true to delete it anyway and leave their "+
			"claims failing",
		project.Name, len(consumers), strings.Join(consumers, ", "), breakBindingsParam)})
	return true
}

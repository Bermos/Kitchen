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
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The service half of the claim API: binding to something another project
// offers (#493).
//
// It is the one claim whose request names an object outside the project the
// claim is for, and the two refusals that follow from that are the whole of
// what this file adds to the shared door:
//
//   - **The offering has to exist and admit this consumer.** Both are
//     decidable here, from two objects that already exist, so a claim that
//     could not bind is refused rather than created and left Failed. The
//     reconciler makes the same two checks again, because an offering can be
//     closed or removed after a claim was written.
//   - **A binding may not take a name one of the consumer's own workloads
//     has.** The binding arrives in the pod as `KITCHEN_SERVICE_<NAME>`,
//     which is the prefix a sibling process is handed under, so two of them
//     under one name is an address silently winning over another. It is
//     refused where the name is chosen rather than disambiguated afterwards.

// serviceClaimShaper is the claimShaper for type service.
type serviceClaimShaper struct{}

func (serviceClaimShaper) fields() []claimField {
	return []claimField{
		{
			name:  "service",
			set:   func(body *createClaimRequest) bool { return body.Service != nil },
			lacks: "no offering to bind",
		},
	}
}

// config validates the shape of what the claim asks for. Whether the
// offering exists and admits this project is resolve's, below, which is the
// half that needs to read another object.
func (serviceClaimShaper) config(
	w http.ResponseWriter,
	body *createClaimRequest,
	project *kitchenv1alpha1.Project,
	_ string,
) (*runtime.RawExtension, bool) {
	if body.Service == nil {
		badRequest(w, "service is required on a service claim: the project that makes the offering, and the "+
			"name of the offering")
		return nil, false
	}
	cfg := kitchenv1alpha1.ServiceConfig{
		Project:  strings.TrimSpace(body.Service.Project),
		Offering: strings.TrimSpace(body.Service.Offering),
	}
	if cfg.Project == "" || cfg.Offering == "" {
		badRequest(w, "service.project and service.offering are both required: a binding names the project "+
			"that offers and the offering it binds to")
		return nil, false
	}
	// The name of the claim is the name of the variable, so it collides with
	// a workload of this project's own — see the file comment.
	if names := project.ProcessNames(); slices.Contains(names, body.Name) {
		badRequest(w, "claim %q would collide with project %s's own %s workload: a binding reaches the "+
			"application as %s, which is the same variable that workload's address arrives in. Name the "+
			"binding something else", body.Name, project.Name, body.Name,
			kitchenv1alpha1.ServiceEnvPrefix(body.Name))
		return nil, false
	}
	raw, err := json.Marshal(claimConfigBody{Service: &cfg})
	if err != nil {
		badRequest(w, "%s", err.Error())
		return nil, false
	}
	return &runtime.RawExtension{Raw: raw}, true
}

// resolve is the cross-project half: the offering has to exist, and it has
// to admit this consumer.
//
// It is a read of the *providing* project, which the caller may hold no role
// on at all — and that is correct rather than a leak. An offering is a
// statement a project makes about what it will answer to, and the answer to
// "may I bind to this" cannot be given without reading it. What the read
// admits is exactly the offering's own fields; nothing else about the
// project is returned, and nothing at all is returned for a project that
// makes no offerings.
func (serviceClaimShaper) resolve(
	ctx context.Context,
	s *Server,
	w http.ResponseWriter,
	body *createClaimRequest,
	consumer *kitchenv1alpha1.Project,
) bool {
	cfg := body.Service
	name := strings.TrimSpace(cfg.Offering)
	provider := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, strings.TrimSpace(cfg.Project), provider); err != nil {
		if apierrors.IsNotFound(err) {
			badRequest(w, "no project named %q offers anything on this platform", strings.TrimSpace(cfg.Project))
			return false
		}
		s.writeError(w, err)
		return false
	}
	offering, ok := provider.Offering(name)
	if !ok {
		has := "it makes none"
		if names := provider.OfferingNames(); len(names) > 0 {
			has = "its offerings are " + strings.Join(names, ", ")
		}
		badRequest(w, "project %s makes no offering named %q: %s", provider.Name, name, has)
		return false
	}
	if provider.Name != consumer.Name && offering.Visibility() != kitchenv1alpha1.OfferingOpen {
		badRequest(w, "offering %s/%s admits consumers by request (visibility %s), and asking for one is not "+
			"built yet (#495). Until it is, project %s's admins open the offering to every project on the "+
			"platform (visibility %s) or it binds nobody",
			provider.Name, offering.Name, kitchenv1alpha1.OfferingRequest, provider.Name,
			kitchenv1alpha1.OfferingOpen)
		return false
	}
	return true
}

// view answers what the claim binds, which is the pair it named plus the two
// things the reconciler resolved: which environment of the provider it
// reached, and the address it wrote into the binding. Neither is a
// credential — an address is the whole of what this claim provisions — so
// both are answered rather than withheld.
func (serviceClaimShaper) view(claim *kitchenv1alpha1.ResourceClaim, view *claimView) {
	cfg := claim.Service()
	if cfg.Project == "" && cfg.Offering == "" {
		return
	}
	service := &claimServiceView{Project: cfg.Project, Offering: cfg.Offering, Bindings: []serviceBindingView{}}
	if claim.Status.Service != nil {
		for _, binding := range claim.Status.Service.Bindings {
			service.Bindings = append(service.Bindings, serviceBindingView{
				Consumer:    string(binding.Consumer),
				Environment: binding.Environment,
				Host:        binding.Host,
				DataClass:   string(binding.DataClass),
				Reason:      binding.Reason,
			})
		}
	}
	view.Service = service
}

func (serviceClaimShaper) deletionOutcome(*kitchenv1alpha1.ResourceClaim) string {
	return "the binding is removed; the offering carries on being offered"
}

// claimServiceView is what a service claim binds, as a reader sees it: the
// pair it named, and what each class of this project's own environments
// reaches through it.
type claimServiceView struct {
	Project  string `json:"project"`
	Offering string `json:"offering"`
	// Bindings answers the question the screen is for — what does my
	// preview reach — with one row per class of environment, whether it
	// reaches anything or not. Empty on a claim that has not resolved yet,
	// and on one bound by an operator older than #494, where every class
	// reached one address.
	Bindings []serviceBindingView `json:"bindings"`
}

// serviceBindingView is one class of this project's environments and what it
// reaches: which environment of the providing project, at what address and
// under what rating — or the reason it reaches nothing, in the words that
// name what would permit it.
//
// The address is answered rather than withheld because an address is the
// whole of what this claim provisions: there is no credential in a service
// binding to read back.
type serviceBindingView struct {
	Consumer    string `json:"consumer"`
	Environment string `json:"environment,omitempty"`
	Host        string `json:"host,omitempty"`
	DataClass   string `json:"dataClass,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

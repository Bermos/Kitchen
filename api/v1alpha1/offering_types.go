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

package v1alpha1

// What one project offers another (#493).
//
// An offering is a list on the Project rather than a kind of its own, and
// that is the whole design: it has no independent lifecycle — it is a
// statement a project makes about itself, it dies with the project, and
// there is nothing to reconcile until somebody claims it. The consumer's
// half is an ordinary ResourceClaim of type `service`, so the edge between
// two projects costs no new CRD at either end.
//
// The split inside the offering is the one the platform draws everywhere
// else: the repository says what the application *is*, and the platform says
// who may. `process`, `protocol` and `auth` are facts about the code and may
// therefore be declared in kitchen.json; `visibility` is a grant, and a
// grant anybody who can open a pull request could widen is not a grant — so
// it is written through the API by a project admin and refused in the file.

// OfferingProtocol is what an offering speaks, which decides what the
// consumer's binding says about the address rather than what the platform
// puts in front of it.
//
// `http` is the overwhelming majority and gets a URL as well as the host and
// the port; `tcp` is everything that is not — a database wire protocol, a
// queue, a raw port — and gets the host and the port alone, because a URL
// for it would be the platform pretending to know a scheme it was never
// told. It is the same distinction a sibling process's three variables draw
// (docs/api/processes.md).
// +kubebuilder:validation:Enum=http;tcp
type OfferingProtocol string

const (
	// OfferingHTTP is an offering spoken to over HTTP.
	OfferingHTTP OfferingProtocol = "http"
	// OfferingTCP is an offering that speaks something else over a port.
	OfferingTCP OfferingProtocol = "tcp"
)

// Protocol reports the offering's protocol with the default applied.
func (o ServiceOffering) Protocol() OfferingProtocol {
	if o.Speaks == "" {
		return OfferingHTTP
	}
	return o.Speaks
}

// OfferingAuth is what a consumer has to do to be admitted by the offering
// itself, and it is one rung of a ladder with three (docs/spikes/
// service-topology-2026-09.md): `none` today, `gate` and `oidc` with #496.
//
// `none` is not the weak rung it reads as. What the consumer gets is the
// address and — once #497 lands default-deny between application namespaces
// — a policy edge that only a declared binding produces, so reachability
// becomes the authorization. That is the honest model for a container that
// was never going to check a bearer token, which is exactly the case this
// feature exists for.
//
// The enum has one value on purpose: a value with no reconcile path behind
// it is a setting that reads as though it applies and does not, and the two
// other rungs arrive with the machinery that implements them.
// +kubebuilder:validation:Enum=none
type OfferingAuth string

// OfferingAuthNone admits any consumer the grant admits, and asks the
// application for nothing.
const OfferingAuthNone OfferingAuth = "none"

// Auth reports the offering's authorization mode with the default applied.
func (o ServiceOffering) Auth() OfferingAuth {
	if o.Authorization == "" {
		return OfferingAuthNone
	}
	return o.Authorization
}

// OfferingVisibility is who may bind to an offering: anybody, or anybody the
// project has approved.
// +kubebuilder:validation:Enum=request;open
type OfferingVisibility string

const (
	// OfferingRequest is the default, and it is the safe one: the offering
	// is visible in the catalogue and a claim on it is refused until the
	// providing project has approved the consumer. The approval flow is
	// #495; until it lands a request-visibility offering binds nothing, and
	// the refusal says exactly that rather than binding on the strength of
	// an approval nothing recorded.
	OfferingRequest OfferingVisibility = "request"
	// OfferingOpen admits any project on the platform. It is a grant the
	// providing project makes once, deliberately, through the API.
	OfferingOpen OfferingVisibility = "open"
)

// Visibility reports the offering's visibility with the default applied.
// Absent is `request`, which is the closed answer: an offering nobody has
// opened admits nobody, so a project that adds one from its repository
// cannot thereby publish it.
func (o ServiceOffering) Visibility() OfferingVisibility {
	if o.VisibleTo == "" {
		return OfferingRequest
	}
	return o.VisibleTo
}

// OfferingVisibilities is the vocabulary, in the order a form should offer
// it: the closed default first.
func OfferingVisibilities() []OfferingVisibility {
	return []OfferingVisibility{OfferingRequest, OfferingOpen}
}

// ServiceOffering is one thing a Project offers to other projects: which of
// its workloads answers, what that workload speaks, what a consumer has to
// do to be admitted, and who may ask.
//
// The three fields whose JSON name differs from the Go one do so because the
// obvious Go names are already methods that apply the defaults — `Protocol`,
// `Auth`, `Visibility` — and a field and a method cannot share a name. The
// wire and the CRD carry the obvious spelling, which is what anybody writing
// one reads.
type ServiceOffering struct {
	// Name identifies the offering within the project, and is what a claim
	// names. It is a DNS label because it travels into object names and into
	// the consumer's environment variables.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	Name string `json:"name"`

	// Process is the workload that answers: `web`, or one of the project's
	// declared service processes. Empty means `web`, which is what an
	// application with one workload offers.
	//
	// A worker and a scheduled job cannot be offered, and the API says so:
	// nothing addresses them, so there is no address to hand a consumer.
	// +optional
	Process string `json:"process,omitempty"`

	// Speaks is `http` or `tcp`; empty is `http`. It is `protocol` on the
	// wire — see the type's comment for why the Go field is not.
	// +optional
	Speaks OfferingProtocol `json:"protocol,omitempty"`

	// Authorization is the rung of the ladder this offering is on; empty is
	// `none`. It is `auth` on the wire.
	// +optional
	Authorization OfferingAuth `json:"auth,omitempty"`

	// VisibleTo is `request` or `open`; empty is `request`. It is
	// `visibility` on the wire, and it is the one field of an offering a
	// repository may not declare.
	// +optional
	VisibleTo OfferingVisibility `json:"visibility,omitempty"`

	// Environment is which of the provider's environments a binding
	// resolves to. Empty is the project's production environment, which is
	// what an offering means when it says nothing.
	//
	// It is the *offering's* default and not the consumer's choice: two
	// consumers wanting two different environments of one offering is
	// #489's `serves.consumers` question, and is deliberately not answered
	// here.
	// +optional
	Environment string `json:"environment,omitempty"`
}

// ProcessName is the workload that answers this offering, with the default
// applied.
func (o ServiceOffering) ProcessName() string {
	if o.Process == "" {
		return WebProcessName
	}
	return o.Process
}

// Offering finds one of this project's offerings by name.
func (p *Project) Offering(name string) (ServiceOffering, bool) {
	for _, offering := range p.Spec.Offers {
		if offering.Name == name {
			return offering, true
		}
	}
	return ServiceOffering{}, false
}

// OfferingNames is every offering this project makes, in declaration order —
// what a refusal lists when the one that was asked for is not there.
func (p *Project) OfferingNames() []string {
	names := make([]string, 0, len(p.Spec.Offers))
	for _, offering := range p.Spec.Offers {
		names = append(names, offering.Name)
	}
	return names
}

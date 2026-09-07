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

// Package service is the contract of the `service` claim type: a binding to
// another Project's offering (#493).
//
// It is the second contract whose provider is not a Connection, and the
// first whose provider is another *project*. There is nothing to provision
// and no provisioner interface to implement — the offering is already
// running, under its own project's quota, as its own project's workload —
// so what the reconciler does is resolve the offering, check the grant and
// write the address down. What lives here is what every contract has to say
// about itself: who the provider is, what it declares, and the keys its
// binding is spelled with.
package service

import "github.com/Bermos/Kitchen/internal/provider/contract"

// ProviderName is who fulfils a service claim. It is not the name of a
// company or a piece of software, because it is not one: the provider is
// another project on this platform, and the offering it makes is the thing
// being bound.
const ProviderName = "project"

// Declaration is what the platform says about a binding to an offering.
//
// A preview gets a *running environment* of the provider rather than a
// resource of its own — `shared` — because that is what an offering is: there
// is nothing here to branch or to create fresh. Which environment it gets is
// no longer the same one production gets, though: the provider's environment
// owners say which classes of consumer they admit (`serves.consumers`, #494),
// so a preview reaches whatever they opened to previews — a staging
// environment, or nothing at all — and the claim's own status says which,
// per class.
//
// Sharing here is not the sharing a database claim asks to be named: a
// binding provisions no data, so there is nothing for a preview to write
// that production would read back. That is what `HoldsData: false` on the
// claim type says, and it is why this declaration is not one a claim has to
// opt into.
var Declaration = contract.Declaration{
	Preview: contract.PreviewShared,
	PreviewNote: "a preview calls a running environment of the provider rather than a resource of its " +
		"own; which environment that is, or whether it reaches one at all, is the provider environment " +
		"owners' declaration (serves.consumers)",
	IdleNote: "a binding is an address and runs nothing, so an idle preview parks nothing here; the " +
		"workload behind the address is the providing project's and idles on its own terms",
}

// The keys a service binding is written with, and read back by.
//
// They are the three facts a sibling process is already handed — the URL,
// the host and the port — because from inside the application another team's
// service and a sibling process are the same thing: an address it did not
// have to work out. The Environment reconciler turns them into
// `KITCHEN_SERVICE_<NAME>`, `_HOST` and `_PORT`, which is the same triple a
// sibling gets under the same prefix.
//
// URL is empty for an offering that speaks something other than HTTP. A URL
// for a database wire protocol would be the platform inventing a scheme it
// was never told, and the host and the port beside it are the same two facts
// without an opinion on top.
const (
	// BindingKeyURL is `http://<host>:<port>` for an http offering.
	BindingKeyURL = "url"
	// BindingKeyHost is the fully qualified name the offering answers on.
	BindingKeyHost = "host"
	// BindingKeyPort is the port, as a decimal string.
	BindingKeyPort = "port"
	// BindingKeyProject and BindingKeyOffering are what the binding is *of*.
	// They carry no authority and are here because a binding Secret that
	// says only "some address" is a binding nobody can trace back to the
	// offering it came from — which is exactly the question a consumer's
	// operator asks when the address stops answering.
	BindingKeyProject  = "project"
	BindingKeyOffering = "offering"
	// BindingKeyEnvironment is which environment of the provider this
	// resolved to, which the offering decides and the consumer cannot.
	BindingKeyEnvironment = "environment"
)

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

// Package oidcclient is the contract of the oidcClient claim type: an OAuth
// client at the platform's own identity provider, so that a deployed
// application signs its users in with the same accounts the dashboard uses.
//
// It is the one contract whose provider is the platform itself. There is no
// Connection and no provisioner interface to implement — the operator
// registers clients at the issuer named by the Kitchen object's spec.auth
// with the service credential it holds (internal/idp) — so what lives here
// is what every contract has to say about itself: who the provider is, and
// what it declares.
package oidcclient

import "github.com/Bermos/Kitchen/internal/provider/contract"

// ProviderName is who declares an oidcClient claim's data provenance and its
// preview mode: the platform, since it is its own identity provider's client
// registrar and there is no Connection whose provider could say instead.
const ProviderName = "kitchen"

// Declaration is what the platform says about the clients it registers.
//
// Every environment of the project signs in through the one client: the
// operator adds each preview's callback URL to the client's redirect list as
// the preview appears and removes it when the pull request closes. That is
// the production resource itself — shared — and it is the right answer here,
// because an OAuth client holds no data for a preview to read or write. It
// is the one shared declaration a claim does not have to opt into, for that
// reason, and HoldsData on the claim type is what says so.
var Declaration = contract.Declaration{
	Preview: contract.PreviewShared,
	PreviewNote: "every environment signs in through the project's one client; the operator keeps " +
		"its redirect list in step as previews come and go, and a client holds no data",
}

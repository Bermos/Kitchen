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

// Package platformhost names the hostnames the platform itself publishes
// under the base domain, and is the one place they are named.
//
// Every one of them is a single label in front of `spec.baseDomain`, on the
// same shared Gateway and under the same wildcard certificate as every
// application the platform runs — and a project's name goes straight into a
// hostname of exactly that shape. So the labels the platform keeps for itself
// and the refusal of a project that would claim one have to be the same list,
// or the refusal drifts behind the routes: the chart or the operator gains a
// hostname, nobody reserves it, and the next project named after it writes a
// second HTTPRoute for an address the platform already answers on (#423).
//
// The package is deliberately a leaf — no Kubernetes types, no configuration —
// so that the API, the operator and anything else that builds one of these
// URLs can all import it.
package platformhost

import (
	"fmt"
	"regexp"
	"strings"
)

// The labels the platform publishes under the base domain. Each is used both
// to build the platform's own hostname and to refuse a project that would
// claim it; adding a platform hostname means adding it here, and the tests in
// this package and in internal/controller fail until it is.
const (
	// API is the operator's public name: the REST API, the dashboard served
	// at its root and the git webhook receiver, split by path.
	API = "kitchen"
	// Auth is the identity provider — the OIDC issuer every sign-in, every
	// token exchange and every JWKS fetch goes through.
	Auth = "auth"
	// PreviewGate is the forward-auth gate a protected preview finishes its
	// login at, and the only redirect URI its OAuth client has.
	PreviewGate = "previews"
	// Registry is the bundled container registry, published so the node's
	// container runtime can pull from it.
	Registry = "registry"
)

// reservation is one reserved label and what the platform serves there. The
// purpose is not decoration: it is what the refusal tells whoever picked the
// name, so that "shop" being fine and "auth" not is an explanation rather
// than a rule from nowhere.
type reservation struct {
	label   string
	purpose string
}

var reservations = []reservation{
	{API, "the REST API and the dashboard"},
	{Auth, "the identity provider every sign-in goes through"},
	{PreviewGate, "the gate protected previews are signed in at"},
	{Registry, "the platform's own container registry"},
}

// Reserved lists the labels the platform keeps for itself, in the order they
// are declared above.
func Reserved() []string {
	labels := make([]string, 0, len(reservations))
	for _, r := range reservations {
		labels = append(labels, r.label)
	}
	return labels
}

// IsReserved reports whether label is one the platform publishes under the
// base domain. Comparison is case-insensitive, because hostnames are.
func IsReserved(label string) bool {
	_, found := lookup(label)
	return found
}

func lookup(label string) (reservation, bool) {
	label = strings.ToLower(strings.TrimSpace(label))
	for _, r := range reservations {
		if r.label == label {
			return r, true
		}
	}
	return reservation{}, false
}

// Host is the hostname a reserved label is published at. An empty base domain
// gives an empty host: an installation without one publishes nothing.
func Host(label, baseDomain string) string {
	if baseDomain == "" {
		return ""
	}
	return label + "." + baseDomain
}

// Hosts is every hostname the platform publishes under baseDomain.
func Hosts(baseDomain string) []string {
	if baseDomain == "" {
		return nil
	}
	hosts := make([]string, 0, len(reservations))
	for _, r := range reservations {
		hosts = append(hosts, Host(r.label, baseDomain))
	}
	return hosts
}

// IsReservedHost reports whether host is one of the platform's own names
// under baseDomain. A port is ignored, and the comparison is
// case-insensitive.
func IsReservedHost(host, baseDomain string) bool {
	if baseDomain == "" {
		return false
	}
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	label, found := strings.CutSuffix(strings.ToLower(strings.TrimSpace(host)),
		"."+strings.ToLower(baseDomain))
	if !found {
		return false
	}
	return IsReserved(label)
}

// previewShaped matches the one name shape that collides with a generated
// preview hostname without naming anything reserved: project `shop-pr-7`
// publishes `shop-pr-7.<base>`, which is also where project `shop` publishes
// pull request 7. Both hostnames are generated, so nothing downstream can
// tell them apart — the collision has to be refused at the name.
//
// The prefix has to be non-empty for there to be another project to collide
// with, and the number is what the platform substitutes, so it is digits.
var previewShaped = regexp.MustCompile(`^.+-pr-[0-9]+$`)

// IsPreviewShaped reports whether name has the shape of a generated preview
// hostname's label.
func IsPreviewShaped(name string) bool {
	return previewShaped.MatchString(strings.ToLower(strings.TrimSpace(name)))
}

// CheckProjectName refuses a project name whose generated hostnames are ones
// the platform already serves — its own, or another project's preview. It
// says which hostname is taken and why, the way refusing a custom domain
// under the base domain does.
//
// baseDomain may be empty, and then the message names the label rather than a
// hostname: the rule does not depend on the installation's domain, only the
// wording does.
func CheckProjectName(name, baseDomain string) error {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	if r, found := lookup(trimmed); found {
		return fmt.Errorf(
			"name %q is reserved: the platform publishes %s — %s. Choose another name",
			name, describe(r.label, baseDomain), r.purpose)
	}
	if IsPreviewShaped(trimmed) {
		return fmt.Errorf(
			"name %q cannot be used: %s is where the platform publishes a pull request preview of "+
				"another project, so a project of that name would claim a hostname twice. "+
				"Choose a name that does not end in -pr-<number>",
			name, describe(trimmed, baseDomain))
	}
	return nil
}

// describe names the hostname a label resolves to, falling back to the label
// itself where the installation has no base domain to build one from.
func describe(label, baseDomain string) string {
	if host := Host(label, baseDomain); host != "" {
		return host
	}
	return label + ".<the platform's base domain>"
}

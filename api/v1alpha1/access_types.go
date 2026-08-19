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

// AccessRole is the role a grant names on a Project. The three are ordered —
// admin contains developer contains viewer — but the ordering lives in
// internal/access, which is the only thing that decides anything from a role.
// Here they are the wire form and the set the API server admits, so a
// misspelled role is refused at admission rather than silently granting
// nothing.
// +kubebuilder:validation:Enum=admin;developer;viewer
type AccessRole string

const (
	// AccessRoleAdmin is everything a developer may do, plus membership, the
	// project's own settings (git source, registry, previews policy), and
	// deleting it.
	AccessRoleAdmin AccessRole = "admin"
	// AccessRoleDeveloper is the day job: builds, redeploys, rollbacks,
	// environment variables, domains, claims, logs, deleting an environment.
	AccessRoleDeveloper AccessRole = "developer"
	// AccessRoleViewer reads status, URLs, builds, releases and logs, and may
	// open a protected preview. No writes.
	AccessRoleViewer AccessRole = "viewer"
)

// AccessSubject names the account an access entry is about.
//
// The canonical subject is the issuer's `sub`, which is opaque: the dashboard
// resolves an address to one when it writes an entry, and Email is carried
// beside it only so that a list of opaque strings still reads. Hand-written
// YAML may name an address in Subject instead, and the rule that tells the two
// apart is deliberately blunt: **a subject containing `@` is read as an email
// address**, matched case-insensitively (addresses are), and honoured only for
// a token whose `email_verified` claim is true.
//
// That last condition is the whole point. An unverified address is something
// the token holder said about themselves, so an unverified-email grant is a
// grant to whoever can get the identity provider to let them type that
// address — it resolves to no role at all rather than to the one written down.
//
// The rule has one corollary worth knowing: an issuer whose `sub` is itself an
// address cannot be named by `sub` here, because that spelling is read as the
// email it looks like. That is the conservative direction — it asks for a
// verified address rather than trusting an opaque string that happens to
// contain an `@`.
type AccessSubject struct {
	// Subject is the issuer's `sub` for the account, or an email address. A
	// value containing `@` is treated as an address and resolves against the
	// token's `email` claim only when `email_verified` is true, because an
	// unverified-email grant is a grant to whoever can claim that address at
	// the identity provider.
	// +kubebuilder:validation:MinLength=1
	Subject string `json:"subject"`

	// Email is informational: it is what makes a list of opaque subjects
	// readable in `kubectl get -o yaml` and in a git diff. Nothing resolves
	// against it — an entry that means to name an address puts the address in
	// Subject, and accepts the verified-email condition that comes with it.
	// +optional
	Email string `json:"email,omitempty"`
}

// AccessGrant is one account's role on a Project.
type AccessGrant struct {
	AccessSubject `json:",inline"`

	// Role this subject holds on the Project.
	Role AccessRole `json:"role"`
}

// AccessSpec is the platform's own access list.
//
// It is a struct around a single list rather than the bare list itself
// because the platform role is unlikely to be the last platform-scoped
// access decision — a platform-wide read-only role is the most likely next
// one — and a field that has to grow a sibling later grows it here instead of
// forcing a rename of something operators have already written down.
type AccessSpec struct {
	// Operators own the platform: everything, everywhere, and project admin
	// on every project, present and future. Every other account is a member —
	// an ordinary account, with no platform surface at all, which sees what
	// project membership grants it and may create projects.
	//
	// The entries name accounts the same way a Project's grants do, minus the
	// role: there is exactly one platform role worth writing down, and being
	// on this list is it.
	//
	// The field carries no `omitempty`, unlike almost everything else in
	// these types, for the same reason spec.access carries no default: an
	// absent list and an empty one mean different things here. With
	// `omitempty` an empty list marshals away, so narrowing the operators to
	// nobody would be indistinguishable from never having named any — and
	// the reconciler would seed the list straight back from the accounts
	// that exist. Writers diff two marshalled objects (client.MergeFrom), so
	// a nil list on both sides is still no change to patch.
	// +optional
	// +listType=map
	// +listMapKey=subject
	Operators []AccessSubject `json:"operators"`
}

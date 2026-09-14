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

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

// PlatformScope is one class of operation a platform credential may perform.
//
// It is a scope rather than a fourth platform role, and that is the whole
// design. The platform has two roles and a project has three; "may read the
// retention policy" is not a hat anybody wears, it is one operation a
// scheduled job needs. A role to hold it would be a fourth role in a model
// that has three, which is the same objection internal/api/policy.go already
// records against inventing a reviewer role to hold an access recertification.
//
// What a scope may reach is decided by internal/api/policy.go and by nothing
// here: a route names the scope that satisfies it, and a route that names none
// is the operator's alone however this list grows. So the set below is small
// on purpose, and stays small — each value is an argument about what a
// credential pasted into a scheduled job or an agent is allowed to become.
// +kubebuilder:validation:Enum=platform.read;compliance.read;backup.run
type PlatformScope string

const (
	// PlatformScopeRead is the platform's own read-only surface: its nodes,
	// workloads, edge, storage, cluster events, ingest, signals and retention.
	// Everything it reaches is a fact about the cluster, and none of it is a
	// credential.
	PlatformScopeRead PlatformScope = "platform.read"

	// PlatformScopeComplianceRead is the evidence surface: who holds what, the
	// access recertifications, the compliance posture, the audit chain's
	// verification and a project's audit pack. It is separate from
	// PlatformScopeRead because it is a different question with a different
	// audience — a compliance job wants it and a capacity job does not — and
	// because it reads across every project rather than about the cluster.
	PlatformScopeComplianceRead PlatformScope = "compliance.read"

	// PlatformScopeBackupRun is taking a backup, and reading what one would
	// carry.
	//
	// It is a scope of its own for one reason, and it is the reason the scopes
	// are split at all: a platform backup is every custom resource, every
	// Secret in the platform namespace and the identity provider's database,
	// in the clear. A scope that lumped it in with the reads above would make
	// the cron job that watches retention into a credential that can exfiltrate
	// the installation.
	PlatformScopeBackupRun PlatformScope = "backup.run"
)

// PlatformCredential is one machine credential's reach on the platform itself.
//
// It is what a scheduled job or an agent holds: an API key at the identity
// provider, owned by an account of its own under the reserved platform domain,
// named here by that account's `sub` exactly as every other grant names an
// account. Nothing about it is stored on the key — the key is a credential and
// this is what the platform will honour it for, which is the same separation
// a CI key's project role already has (docs/AUTH.md, "Machine accounts").
//
// Three things bound it, and all three are here rather than in the code that
// reads it:
//
//   - **Scopes**, which are operations and never a role. A credential holds
//     no platform role at all: internal/access resolves it as a member, like
//     any other account nobody made an operator.
//   - **Expires**, which is not optional. A credential that outlives the job
//     it was pasted into is the failure mode this whole shape exists to make
//     survivable, so the expiry is enforced where the scopes are resolved —
//     an expired credential holds nothing, whether or not anything has got
//     round to deleting it.
//   - **Projects**, which narrows the handful of scoped routes that are about
//     one project rather than about the platform.
type PlatformCredential struct {
	AccessSubject `json:",inline"`

	// Scopes are the operations this credential may perform. A credential with
	// none holds nothing, which is why the list cannot be empty: an entry that
	// granted nothing would be a credential that authenticates and can do
	// nothing, reported as a working credential.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	Scopes []PlatformScope `json:"scopes"`

	// Expires is when this credential stops being honoured. It is required,
	// and it is read at every request rather than only by whatever sweeps the
	// list: a leaked credential dies on its own, at this instant, without
	// anything having to run.
	Expires metav1.Time `json:"expires"`

	// Projects narrows the scoped routes that are about one project — today
	// that is a project's audit pack — to the ones named here.
	//
	// An empty list is every project, because that is what a *platform* scope
	// means when nobody has narrowed it, and because the narrowing is the
	// operator's tool rather than a trap for whoever forgets it. The screen
	// that issues a credential offers the list; a credential that should only
	// ever see two projects says so here.
	// +optional
	// +listType=set
	Projects []string `json:"projects,omitempty"`
}

// AccessSpec is the platform's own access list.
//
// It is a struct around a single list rather than the bare list itself
// because the platform role is unlikely to be the last platform-scoped
// access decision — a platform-wide read-only role is the most likely next
// one — and a field that has to grow a sibling later grows it here instead of
// forcing a rename of something operators have already written down. That
// prediction held: Credentials below is the sibling.
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

	// Credentials are the platform credentials this installation has issued:
	// machine accounts that hold scopes on the platform rather than a role on
	// a project.
	//
	// It carries `omitempty`, unlike Operators above, because here an absent
	// list and an empty one mean the same thing — no credential has been
	// issued, or none is left — and there is nothing to seed. A platform that
	// has never issued one is a platform where nothing but a person may reach
	// the platform surface, which is where every installation starts.
	// +optional
	// +listType=map
	// +listMapKey=subject
	Credentials []PlatformCredential `json:"credentials,omitempty"`

	// PersonalKeys are the personal keys this installation has issued, and
	// what each one may do for the person it belongs to.
	//
	// It is `atomic` rather than a map because a map key has to be one field
	// and a personal key is named by two — whose it is, and what they called
	// it — and because the one route that writes the list writes all of it.
	// +optional
	// +listType=atomic
	PersonalKeys []PersonalKeyGrant `json:"personalKeys,omitempty"`
}

// Personal keys, and where what one may do is written down (#595).
//
// A personal key is a copy of a person: the token it is exchanged for carries
// their `sub`, so every grant they hold resolves from it. PersonalKeyGrant
// below is where that is *bounded*. An entry says which projects the key may
// touch and at most what role it holds inside them, and internal/access takes
// the lesser of that and what its owner actually holds — so a key can be
// narrower than the person and can never be wider, however the entry is
// written and whoever wrote it.
//
// Four things follow from the grant living here rather than at the identity
// provider, and all four are the reason it does:
//
//   - **Every personal key has an entry, or it holds nothing.** A key whose
//     entry is gone is not a key that fell back to being its owner; it is a
//     credential the platform no longer recognises, and it is refused. That is
//     what makes deleting an entry a revocation rather than a promotion.
//   - **It is visible where every other grant is.** `kubectl get kitchen -o
//     yaml` shows it, the access survey rows it beside the person it copies,
//     and a recertification cycle reviews it.
//   - **The narrowing is Kitchen's state, not the issuer's.** The issuer knows
//     which key was presented and nothing else — the same division that keeps
//     a CI key's role on the Project and a platform credential's scopes on
//     this singleton.
//   - **It expires with the key.** The date here is the one the key was issued
//     with; the issuer refuses a lapsed key outright, and this is applied
//     again where a scope is read, so a key outlives neither.
//
// The doc comments below are deliberately short. They are what
// `kubectl explain` prints and what the generated CRD carries into the chart,
// and the chart's CRDs are within a few kilobytes of the 1MiB a Helm release
// Secret may hold — so the reasoning lives here, where it is read by whoever
// changes the type, and the schema carries what somebody needs at a terminal.

// PersonalKeyGrant is what one personal key may do: the platform's half of a
// credential whose other half is an API key at the identity provider.
type PersonalKeyGrant struct {
	// AccessSubject is the owner's account: the person this key is a copy of.
	AccessSubject `json:",inline"`

	// Key is what the key is called at the identity provider, which a token
	// minted from it names in its `kitchen_key` claim. One name is one key
	// per account.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	// Unrestricted says this key is its owner, entire: every project role they
	// hold, and the operator role if they wear one. It is a field rather than
	// the absence of a narrowing so that a truncated entry cannot widen the
	// credential it was meant to bound.
	// +optional
	Unrestricted bool `json:"unrestricted,omitempty"`

	// Projects are the projects this key may act on. An empty list is every
	// project its owner can reach, which the role below still caps.
	// +optional
	// +listType=set
	Projects []string `json:"projects,omitempty"`

	// Role is the most this key may hold on the projects above, whatever its
	// owner holds there. Empty on a narrowed key reads as viewer.
	// +optional
	Role AccessRole `json:"role,omitempty"`

	// Scopes are the platform operations this key may perform beside its
	// project role, honoured only while its owner is an operator.
	// +optional
	// +listType=set
	Scopes []PlatformScope `json:"scopes,omitempty"`

	// Expires is when this key stops being honoured: the date it was issued
	// with, which the identity provider enforces on presentation.
	Expires metav1.Time `json:"expires"`

	// IssuedAt is when the key was created, for the survey and the screens.
	// +optional
	IssuedAt metav1.Time `json:"issuedAt,omitempty"`
}

// Narrowed reports whether this grant bounds its key at all. An unrestricted
// key is its owner; a narrowed one is the lesser of its owner and this entry.
func (g PersonalKeyGrant) Narrowed() bool { return !g.Unrestricted }

// Ceiling is the most a narrowed key may hold on a project it may reach: what
// the entry says, and `viewer` for an entry that says nothing.
//
// The default is the floor of the vocabulary rather than the middle of it
// because this is read for a credential somebody issued and may not have
// thought about: the role that reads and changes nothing is the one to give
// an entry that forgot to say.
func (g PersonalKeyGrant) Ceiling() AccessRole {
	if g.Role == "" {
		return AccessRoleViewer
	}
	return g.Role
}

// Reaches reports whether this key may act on the named project at all. An
// empty list is every project its owner can reach — see Projects.
func (g PersonalKeyGrant) Reaches(project string) bool {
	if len(g.Projects) == 0 {
		return true
	}
	return slices.Contains(g.Projects, project)
}

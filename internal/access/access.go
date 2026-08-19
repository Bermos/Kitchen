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

// Package access decides who may do what: it turns a caller and the objects
// membership is recorded on into a role, and nothing else.
//
// It is a package rather than a method on the REST API because the REST API
// is not the only thing that has to answer the question. The forward-auth gate
// in front of a protected preview admits anyone holding any role on that
// project, and it resolves that against its own cache — a preview that closes
// because the API is restarting is an outage in something the gate is
// perfectly able to decide for itself. Both read the same two places,
// spec.access on a Project and spec.access.operators on the Kitchen singleton,
// through this one implementation, so the membership rule has exactly one
// meaning.
//
// Nothing here enforces anything. It resolves roles; refusing a request, and
// naming the role the refusal wanted, belongs to whoever asked.
//
// The model this implements is written down in docs/AUTH.md, "Who may do
// what", and the trust boundary is stated there too: cluster access is
// operator access. What these roles protect against is accident, blast radius,
// and developers seeing each other's unreleased work — not somebody who
// already holds kubectl on the cluster.
package access

import (
	"encoding/json"
	"strconv"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Caller is who a question is about: the identity behind a token, reduced to
// the three claims an access decision reads.
//
// It is deliberately not internal/api's Caller. The REST API converts its own
// into this one and the preview gate converts its session into this one, so
// neither half of the platform's authorization has to have the other linked
// in — and this package stays out of the import cycle that reaching back into
// the API package would create.
type Caller struct {
	// Subject is the issuer's `sub`: the canonical, opaque identifier an
	// access entry names. An empty Subject matches nothing, so a caller
	// nobody authenticated resolves to no role rather than to whatever an
	// empty entry happens to say.
	Subject string

	// Email is the token's `email` claim, and EmailVerified its
	// `email_verified`. They are here because an entry may name an address
	// instead of a `sub` (see AccessSubject on the API types), and such an
	// entry is honoured only for a verified address: an unverified one is
	// something the token holder said about themselves, so honouring it
	// would hand the role to whoever can get the identity provider to let
	// them type that address.
	Email         string
	EmailVerified bool
}

// emailMarker is what tells an email subject from a `sub`. A `sub` is opaque
// and may contain anything, so this is a convention rather than a parse: an
// entry whose subject contains "@" is an email address. The rule is written
// on AccessSubject in the API types, in the CRD field description and in
// docs/CRDS.md, because a rule about identity that is documented in one place
// and implemented in another is how somebody ends up holding a role nobody
// granted them.
const emailMarker = "@"

// IsEmailSubject reports whether an access entry's subject names an email
// address rather than the issuer's `sub`.
func IsEmailSubject(subject string) bool {
	return strings.Contains(subject, emailMarker)
}

// matches reports whether an entry naming subject is an entry about caller.
//
// Addresses are matched case-insensitively, since addresses are: an entry
// written Anna@Example.com and a token claiming anna@example.com are the same
// person, and reading them as two would be a grant that silently does nothing.
func matches(subject string, caller Caller) bool {
	if subject == "" {
		return false
	}
	if IsEmailSubject(subject) {
		if !caller.EmailVerified || caller.Email == "" {
			return false
		}
		return strings.EqualFold(subject, caller.Email)
	}
	return caller.Subject != "" && subject == caller.Subject
}

// PlatformRoleFor is the hat this caller wears on the platform: operator when
// the Kitchen singleton's operator list names them, member otherwise.
//
// A platform with no singleton at all — the operator has not seen one yet, or
// there is none — makes everybody a member. That is the safe direction: the
// worst it does is refuse a platform operation to somebody who is entitled to
// it, where the alternative would hand the platform to whoever asked first.
func PlatformRoleFor(caller Caller, kitchen *kitchenv1alpha1.Kitchen) PlatformRole {
	if kitchen == nil {
		return PlatformMember
	}
	for _, operator := range kitchen.Spec.Access.Operators {
		if matches(operator.Subject, caller) {
			return PlatformOperator
		}
	}
	return PlatformMember
}

// ProjectRoleFor is what this caller may do with this Project, and
// ProjectRoleNone when the answer is nothing at all.
//
// This is the one place the operator ⇒ project admin rule is applied.
// Everything that asks about a project — the API, the preview gate, the
// project list — comes through here, so an operator cannot be locked out of a
// project by the list on it and no second implementation can disagree about
// whether they are.
//
// Where two entries match the same caller — one naming their `sub`, one
// naming their verified address — the higher role wins. Both are written
// down, and reading the pair as the lower of the two would quietly withdraw a
// role somebody granted on purpose.
func ProjectRoleFor(caller Caller, kitchen *kitchenv1alpha1.Kitchen, project *kitchenv1alpha1.Project) ProjectRole {
	if PlatformRoleFor(caller, kitchen).AtLeast(PlatformOperator) {
		return ProjectAdmin
	}
	if project == nil {
		return ProjectRoleNone
	}
	held := ProjectRoleNone
	for _, grant := range project.Spec.Access {
		if !matches(grant.Subject, caller) {
			continue
		}
		if role, ok := ParseProjectRole(string(grant.Role)); ok && role > held {
			held = role
		}
	}
	return held
}

// VisibleProjects is the subset of projects this caller holds any role on,
// in the order they came in. It is what a cross-project query — the log
// search, the traffic view, the activity feed — filters by, so that a
// developer's question about "everything" is answered about everything of
// theirs.
//
// An operator sees all of them, for the same reason they hold admin on each:
// the rule is ProjectRoleFor's, and this walks the same function rather than
// short-circuiting on the platform role, so the two can never drift.
//
// The result is a new slice; the input is left alone, since it is usually the
// cached list straight off an informer.
func VisibleProjects(caller Caller, kitchen *kitchenv1alpha1.Kitchen, projects []kitchenv1alpha1.Project) []kitchenv1alpha1.Project {
	visible := make([]kitchenv1alpha1.Project, 0, len(projects))
	for i := range projects {
		if ProjectRoleFor(caller, kitchen, &projects[i]).AtLeast(ProjectViewer) {
			visible = append(visible, projects[i])
		}
	}
	return visible
}

// VerifiedClaim reads an `email_verified` claim that may be a boolean or the
// string spelling of one, which is the shape several issuers send. Everything
// else — an absent claim, a number, an object — is false.
//
// It lives here because this is the package that decides what a verified
// address is worth: matches honours an entry naming an address only for a
// verified one, so the claim's two spellings and the rule that reads them
// belong together. Both halves of the platform's authorization call it. The
// REST API decodes the claim raw and passes it through here; the forward-auth
// gate still carries a copy of the same three lines (verifiedClaim in
// internal/previewgate/oidc.go) that wants collapsing onto this one.
//
// Reading it leniently is not laxity. The strict alternative is worse in both
// directions: decoded as a plain bool, a string "true" makes json.Unmarshal
// fail over the *whole* claim set, which the API turns into "the token's
// claims are unreadable" — a 401 on every route for every caller of an issuer
// that spells it that way. Anything unrecognised still reads as unverified,
// which is the safe direction: the claim only ever widens what its holder may
// reach.
func VerifiedClaim(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var flag bool
	if err := json.Unmarshal(raw, &flag); err == nil {
		return flag
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return false
	}
	verified, err := strconv.ParseBool(text)
	return err == nil && verified
}

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

package access

import (
	"sort"
	"strings"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What a platform credential may reach, resolved the way a role is.
//
// A scope is not a role and does not sit anywhere on PlatformRole's ladder: a
// credential holding `platform.read` is a *member* who may additionally
// perform the operations that scope names. That is deliberate. A fourth
// platform role would be a role in a model that has two, and it would have to
// be ordered against operator — which is a question with no honest answer,
// since a credential that may read the retention policy is not "less operator"
// than one that may take a backup, it is a different thing entirely.
//
// So the two answers compose rather than merge: PlatformRoleFor says what hat
// the caller wears, ScopesFor says what operations they were additionally
// handed, and internal/api/policy.go's guard admits a route when either
// suffices. An operator needs no scope and a credential holds no role.

// Scope is one operation class a platform credential may perform. It is the
// wire form the CRD writes and the name a refusal says out loud, so it is the
// string rather than an ordered number: scopes do not contain one another, and
// giving them an order would invite a comparison that means nothing.
type Scope string

const (
	// ScopePlatformRead is the platform's own read-only surface.
	ScopePlatformRead Scope = Scope(kitchenv1alpha1.PlatformScopeRead)
	// ScopeComplianceRead is the evidence surface: identities, access
	// recertifications, the compliance posture, the audit chain and a
	// project's audit pack.
	ScopeComplianceRead Scope = Scope(kitchenv1alpha1.PlatformScopeComplianceRead)
	// ScopeBackupRun is taking a platform backup, and reading what one would
	// carry.
	ScopeBackupRun Scope = Scope(kitchenv1alpha1.PlatformScopeBackupRun)
)

// ScopeNone is the zero value and not a scope. A route whose requirement
// carries it names no scope at all, which is what every operator-only route
// does and what makes reaching one with a credential impossible rather than
// merely unlikely.
const ScopeNone Scope = ""

// Scopes is every scope the platform defines, in the order a list of them
// reads best. It is the set a request body is validated against and the set a
// screen offers, so that adding one means adding it here and nowhere else.
func Scopes() []Scope {
	return []Scope{ScopePlatformRead, ScopeComplianceRead, ScopeBackupRun}
}

// ParseScope reads a scope back from its wire form. The second result is false
// for anything that is not one of them, including the empty string: a scope
// nobody recognised must never resolve to one that exists.
func ParseScope(s string) (Scope, bool) {
	candidate := Scope(strings.TrimSpace(s))
	for _, scope := range Scopes() {
		if candidate == scope {
			return scope, true
		}
	}
	return ScopeNone, false
}

func (s Scope) String() string { return string(s) }

// Grant is what one platform credential holds, once the expiry has been
// applied: the operations, and the projects the project-shaped ones are about.
//
// It is a value rather than a pointer to the entry it came from, because the
// entry is a list on a cached object and a caller of this package must not be
// able to edit the platform's access list by holding onto what it was told.
type Grant struct {
	// Subject is the credential's account, and Email its address at the
	// issuer — which, being under the reserved platform domain, is also where
	// its name comes from.
	Subject string
	Email   string

	// scopes is what it may do. It is a set rather than a slice so that Allows
	// is a lookup, and unexported so that nothing can widen a grant it holds.
	scopes map[Scope]struct{}

	// projects is the project allowlist. An empty map is every project — the
	// unnarrowed case, which is what a platform scope means when nobody said
	// otherwise (see PlatformCredential.Projects).
	projects map[string]struct{}
}

// Allows reports whether this grant carries a scope. A grant carries ScopeNone
// never, which is what makes an unscoped route unreachable by any credential:
// the guard asks Allows(requirement.Scope), and a requirement that named no
// scope is refused here rather than by remembering to check first.
func (g Grant) Allows(scope Scope) bool {
	if scope == ScopeNone {
		return false
	}
	_, ok := g.scopes[scope]
	return ok
}

// AllowsProject reports whether a scoped route about one project is about a
// project this grant covers. An unnarrowed grant covers all of them; a
// narrowed one covers exactly what it names.
//
// A route that is *not* about a project asks nothing of this: the guard only
// consults it for a requirement that carries a resolver.
func (g Grant) AllowsProject(project string) bool {
	if len(g.projects) == 0 {
		return true
	}
	if project == "" {
		// The route is about one project and the request named none. Nobody
		// narrowed a credential to a set of projects meaning to include the
		// requests that name none of them.
		return false
	}
	_, ok := g.projects[project]
	return ok
}

// Held is the scopes this grant carries, sorted, for a refusal that has to say
// what the caller does hold and for a response that reports a credential.
func (g Grant) Held() []Scope {
	held := make([]Scope, 0, len(g.scopes))
	for scope := range g.scopes {
		held = append(held, scope)
	}
	sort.Slice(held, func(i, j int) bool { return held[i] < held[j] })
	return held
}

// Projects is the allowlist this grant was narrowed to, sorted, and empty for
// an unnarrowed grant. It is what a refusal names when a scoped route is about
// a project the credential was narrowed away from.
func (g Grant) Projects() []string {
	names := make([]string, 0, len(g.projects))
	for project := range g.projects {
		names = append(names, project)
	}
	sort.Strings(names)
	return names
}

// Empty reports a caller holding no platform scope at all — every person, and
// every credential whose entry has expired or was never written.
func (g Grant) Empty() bool { return len(g.scopes) == 0 }

// ScopesFor is what this caller may do on the platform beyond their role, at
// this instant.
//
// **The expiry is applied here**, which is the point of resolving it rather
// than trusting a sweep: an entry whose Expires has passed contributes nothing,
// so a credential that leaked stops working the moment it lapses, on every
// replica, with nothing having had to run. The sweep in the operator then
// tidies the entry and the account behind it — hygiene, not enforcement.
//
// A platform with no singleton resolves to nothing, for the same reason
// PlatformRoleFor makes everybody a member there: the worst an absent
// singleton can do is refuse an operation to somebody entitled to it.
//
// Where two entries match the same caller — which the list's `subject` map key
// makes impossible at admission, and which hand-written YAML can still
// produce — the scopes are the union and the project allowlists are too. That
// is the same direction ProjectRoleFor takes for two grants naming one person:
// both were written down, and reading the pair as less than either would
// quietly withdraw something somebody granted on purpose.
func ScopesFor(caller Caller, kitchen *kitchenv1alpha1.Kitchen, now time.Time) Grant {
	grant := Grant{Subject: caller.Subject, Email: caller.Email}
	if kitchen == nil {
		return grant
	}
	projects := map[string]struct{}{}
	unnarrowed := false
	for _, credential := range kitchen.Spec.Access.Credentials {
		if !SubjectMatches(credential.Subject, caller) || Expired(credential, now) {
			continue
		}
		if grant.scopes == nil {
			grant.scopes = map[Scope]struct{}{}
		}
		for _, scope := range credential.Scopes {
			if parsed, ok := ParseScope(string(scope)); ok {
				grant.scopes[parsed] = struct{}{}
			}
		}
		if len(credential.Projects) == 0 {
			// This entry narrows nothing, and an entry beside it that does
			// cannot take that back — the union is the widest of the two, in
			// both dimensions.
			unnarrowed = true
			continue
		}
		for _, project := range credential.Projects {
			projects[project] = struct{}{}
		}
	}
	if !unnarrowed && len(projects) > 0 {
		grant.projects = projects
	}
	return grant
}

// Expired reports whether a credential's entry has lapsed at this instant.
//
// A zero expiry counts as expired rather than as "never". The field is
// required by the CRD, so a zero value here means either an object written
// before the field existed or one written past the API server's validation
// with kubectl — and in both cases the safe reading of "no expiry was ever
// recorded" is that the credential is not honoured, not that it is honoured
// forever.
func Expired(credential kitchenv1alpha1.PlatformCredential, now time.Time) bool {
	if credential.Expires.IsZero() {
		return true
	}
	return !now.Before(credential.Expires.Time)
}

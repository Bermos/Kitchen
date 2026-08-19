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
	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// PlatformRole is the hat an account wears on the platform as a whole:
// exactly one per account, and the answer to "may Anna change the base
// domain?".
//
// It is an ordered number rather than a string so that "is this caller at
// least an operator" is one comparison instead of a set to keep in step with
// the roles. The order matters more than the numbers: the zero value sits at
// the bottom of it, so a role nobody recognised — an unparsed string, a caller
// resolution never ran for — can never outrank one somebody granted.
type PlatformRole uint8

const (
	// PlatformRoleNone is the zero value, and not a role. Resolution never
	// returns it: an account that is not an operator is a member. It exists
	// so that an absent or unrecognised role reads as less than every real
	// one rather than accidentally being one.
	PlatformRoleNone PlatformRole = iota
	// PlatformMember is an ordinary account. No platform surface at all — it
	// sees what project membership grants it, and it may create projects.
	PlatformMember
	// PlatformOperator owns the platform: everything, everywhere. It implies
	// project admin on every project, present and future, which
	// ProjectRoleFor applies in the one place that rule exists.
	PlatformOperator
)

// ProjectRole is what an account may do with one Project. Ordered like
// PlatformRole and for the same reason — "at least developer" is a
// comparison — with viewer at the bottom of the three, so asking for any role
// at all is AtLeast(ProjectViewer).
type ProjectRole uint8

const (
	// ProjectRoleNone is the zero value, and not a role: it is what an
	// account with no grant on a Project holds, and it satisfies nothing.
	ProjectRoleNone ProjectRole = iota
	// ProjectViewer reads status, URLs, builds, releases and logs, and may
	// open a protected preview. No writes. It is also the role a preview
	// link gets pasted to.
	ProjectViewer
	// ProjectDeveloper is the day job: builds, redeploys, rollbacks,
	// environment variables, domains, claims, logs, deleting an environment.
	ProjectDeveloper
	// ProjectAdmin is everything a developer may do, plus membership, the
	// project's own settings, and deleting it.
	ProjectAdmin
)

// AtLeast reports whether r carries at least the authority of other.
//
// A caller holding no role satisfies nothing, including a requirement of
// PlatformRoleNone: the zero value is what an absent grant, an unparsed
// string and a caller nobody has heard of all read as, and none of those may
// ever come out of this as "yes".
func (r PlatformRole) AtLeast(other PlatformRole) bool {
	return r != PlatformRoleNone && r >= other
}

// String is the role's wire form — what the API and the dashboard call it —
// and the empty string for PlatformRoleNone and for anything unrecognised,
// because a role that is not one of the two has no name to report.
func (r PlatformRole) String() string {
	switch r {
	case PlatformMember:
		return platformRoleMember
	case PlatformOperator:
		return platformRoleOperator
	default:
		return ""
	}
}

// Wire forms of the platform roles. They are only strings at the platform's
// edges — a status payload, a refusal that names the role it wanted — since
// nothing stores a platform role by name: being on the Kitchen singleton's
// operator list is what makes an account an operator.
const (
	platformRoleMember   = "member"
	platformRoleOperator = "operator"
)

// ParsePlatformRole reads a role back from its wire form. The second result
// is false for anything that is not one of the two, PlatformRoleNone's empty
// string included: it has no wire form, because "no role" is not something
// anybody writes down.
func ParsePlatformRole(s string) (PlatformRole, bool) {
	switch s {
	case platformRoleMember:
		return PlatformMember, true
	case platformRoleOperator:
		return PlatformOperator, true
	default:
		return PlatformRoleNone, false
	}
}

// AtLeast reports whether r carries at least the authority of other. As with
// PlatformRole, a caller holding no role satisfies nothing at all — which is
// what makes the zero value safe to hand around.
func (r ProjectRole) AtLeast(other ProjectRole) bool {
	return r != ProjectRoleNone && r >= other
}

// String is the role's wire form, which is also the value written in a
// Project's spec.access — the enum on AccessRole is the same three words. It
// is the empty string for ProjectRoleNone and for anything unrecognised.
func (r ProjectRole) String() string {
	switch r {
	case ProjectViewer:
		return string(kitchenv1alpha1.AccessRoleViewer)
	case ProjectDeveloper:
		return string(kitchenv1alpha1.AccessRoleDeveloper)
	case ProjectAdmin:
		return string(kitchenv1alpha1.AccessRoleAdmin)
	default:
		return ""
	}
}

// ParseProjectRole reads a role back from the wire form a grant is written
// in. It is the one place the CRD's enum becomes an ordering, so a role the
// API server would refuse at admission — and one an older Kitchen wrote that
// this one has never heard of — resolves to ProjectRoleNone with false rather
// than to something plausible.
func ParseProjectRole(s string) (ProjectRole, bool) {
	switch kitchenv1alpha1.AccessRole(s) {
	case kitchenv1alpha1.AccessRoleViewer:
		return ProjectViewer, true
	case kitchenv1alpha1.AccessRoleDeveloper:
		return ProjectDeveloper, true
	case kitchenv1alpha1.AccessRoleAdmin:
		return ProjectAdmin, true
	default:
		return ProjectRoleNone, false
	}
}

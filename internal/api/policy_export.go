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
	"fmt"

	"github.com/Bermos/Kitchen/internal/access"
)

// The enforcement table, read out as data.
//
// This file exists for exactly one caller: hack/gen-ui-policy, which writes
// the dashboard's copy of the policy (ui/src/lib/policy.generated.ts) from
// what PolicyTable returns. The dashboard has to know the same rules the API
// enforces — a control it renders for someone the API will refuse, or hides
// from someone the API would admit, is a bug in a permission model that only
// looks like it has one owner — and a second hand-written copy is how the two
// come to disagree. So the copy is generated, and this is the seam it is
// generated through.
//
// Nothing in the running platform reads it. It is deliberately not a
// serialization of `route`: the handlers are not data, and the projectResolver
// on a row is a function that reads the cluster. What comes out is every part
// of a row that is a constant, which is the part the dashboard can act on.

// The wire names of what a route asks of its caller: requirementKind's six
// values, in the words the generated module uses.
//
// They are spelled out here rather than derived from the Go identifiers so
// that renaming an unexported constant cannot silently rename a value the
// dashboard switches on.
const (
	PolicyAuthenticated   = "authenticated"
	PolicyPerson          = "person"
	PolicyOperator        = "operator"
	PolicyProjectRole     = "projectRole"
	PolicyVisibleProjects = "visibleProjects"
	PolicyRoleShapedBody  = "roleShapedBody"
)

// PolicyRoute is one row of the table with the handler taken off.
type PolicyRoute struct {
	// Pattern is the row's own pattern, "PATCH /api/v1/environments/{name}",
	// and "/" for the catch-all, which has no method of its own.
	Pattern string
	// Kind is one of the six constants above.
	Kind string
	// Role is the project role Kind PolicyProjectRole wants, in the wire form
	// a grant is written in ("viewer", "developer", "admin"). Empty for every
	// other kind.
	Role string
	// Doing names the operation in the words a refusal uses — "redeploying",
	// "changing the platform's settings". Empty for the kinds that never
	// refuse a caller who holds a valid token.
	Doing string
}

// Policy is the whole of what the dashboard's generated copy is made of: the
// two role orderings, and every row of the table in the order the table lists
// them.
type Policy struct {
	// PlatformRoles and ProjectRoles are the roles' wire forms, weakest
	// first. The order is the one internal/access compares with, so the
	// dashboard's "at least developer" is the same comparison the API makes
	// rather than a second opinion about which role outranks which.
	PlatformRoles []string
	ProjectRoles  []string
	Routes        []PolicyRoute
}

// PolicyTable is the enforcement table as data, for the generator that gives
// the dashboard its copy of it.
//
// It fails rather than emitting something incomplete. A requirement kind
// nobody has taught it about, or a role ordering it cannot read, would
// otherwise become a route the dashboard quietly treats as "anyone may" — and
// a generated permission model that is silently missing a rule is worse than
// no generated one at all.
func PolicyTable() (Policy, error) {
	platform, err := orderedRoles("platform", func(value uint8) string { return access.PlatformRole(value).String() })
	if err != nil {
		return Policy{}, err
	}
	project, err := orderedRoles("project", func(value uint8) string { return access.ProjectRole(value).String() })
	if err != nil {
		return Policy{}, err
	}

	// The zero Server is enough: routes() only takes method values, and
	// nothing but the handler on a row depends on the server at all.
	var server Server
	table := server.routes()

	routes := make([]PolicyRoute, 0, len(table))
	seen := make(map[string]bool, len(table))
	for _, row := range table {
		if seen[row.Pattern] {
			return Policy{}, fmt.Errorf("%s is in the table twice", row.Pattern)
		}
		seen[row.Pattern] = true

		kind, ok := policyKind(row.Requires.Kind)
		if !ok {
			return Policy{}, fmt.Errorf(
				"%s asks for requirement kind %d, which this file has never been told a name for: "+
					"add it here and to the helper in ui/src/lib/policy.ts, or the dashboard will read the route "+
					"as one anybody may call", row.Pattern, row.Requires.Kind)
		}
		routes = append(routes, PolicyRoute{
			Pattern: row.Pattern,
			Kind:    kind,
			Role:    row.Requires.Role.String(),
			Doing:   row.Requires.Doing,
		})
	}

	return Policy{PlatformRoles: platform, ProjectRoles: project, Routes: routes}, nil
}

// policyKind names a requirement kind, and reports false for one nothing here
// knows about.
func policyKind(kind requirementKind) (string, bool) {
	switch kind {
	case requireAuthenticated:
		return PolicyAuthenticated, true
	case requirePerson:
		return PolicyPerson, true
	case requireOperator:
		return PolicyOperator, true
	case requireProjectRole:
		return PolicyProjectRole, true
	case requireVisibleProjects:
		return PolicyVisibleProjects, true
	case requireRoleShapedBody:
		return PolicyRoleShapedBody, true
	default:
		return "", false
	}
}

// maxRoleScan is far past the top of either ordering. It is only a stop for
// the walk below, not a limit on how many roles there may be.
const maxRoleScan = 64

// orderedRoles reads a role ordering out of the type that defines it, by
// walking it upward from the first real value and asking each for its wire
// form.
//
// Both role types are ordered numbers whose zero value is not a role
// (internal/access), so counting up and stopping where the names run out *is*
// the ordering — which means a role added to either iota block reaches the
// dashboard without anybody remembering to list it a second time here.
//
// A gap is refused rather than walked past. A role inserted into the middle of
// an ordering without a String case would otherwise end the walk early and
// take every role above it with it, and the dashboard would lose "operator"
// because somebody forgot a switch arm.
func orderedRoles(kind string, nameOf func(value uint8) string) ([]string, error) {
	roles := make([]string, 0, maxRoleScan)
	gap := 0
	for value := 1; value <= maxRoleScan; value++ {
		name := nameOf(uint8(value))
		if name == "" {
			if gap == 0 {
				gap = value
			}
			continue
		}
		if gap != 0 {
			return nil, fmt.Errorf(
				"%s role %d has no wire form but %d (%q) does: the ordering cannot be read, "+
					"and every role above the gap would be missing from the dashboard's copy",
				kind, gap, value, name)
		}
		roles = append(roles, name)
	}
	if len(roles) == 0 {
		return nil, fmt.Errorf("no %s role has a wire form at all", kind)
	}
	return roles, nil
}

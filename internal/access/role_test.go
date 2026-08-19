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
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

func TestProjectRoleAtLeast(t *testing.T) {
	cases := []struct {
		name     string
		held     ProjectRole
		required ProjectRole
		want     bool
	}{
		{name: "admin satisfies developer", held: ProjectAdmin, required: ProjectDeveloper, want: true},
		{name: "admin satisfies viewer", held: ProjectAdmin, required: ProjectViewer, want: true},
		{name: "developer satisfies viewer", held: ProjectDeveloper, required: ProjectViewer, want: true},
		{name: "developer satisfies developer", held: ProjectDeveloper, required: ProjectDeveloper, want: true},
		{name: "viewer does not satisfy developer", held: ProjectViewer, required: ProjectDeveloper},
		{name: "developer does not satisfy admin", held: ProjectDeveloper, required: ProjectAdmin},
		{name: "any role satisfies viewer, which is how the gate asks", held: ProjectViewer, required: ProjectViewer, want: true},
		// The zero value is what an absent grant and an unparsed role both
		// read as. It has to satisfy nothing at all, including itself:
		// "no role is at least no role" would be a true statement and an
		// open door.
		{name: "no role does not satisfy viewer", held: ProjectRoleNone, required: ProjectViewer},
		{name: "no role does not satisfy no role", held: ProjectRoleNone, required: ProjectRoleNone},
		{name: "a real role satisfies no role", held: ProjectViewer, required: ProjectRoleNone, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.held.AtLeast(tc.required); got != tc.want {
				t.Errorf("%v.AtLeast(%v) = %v, want %v", tc.held, tc.required, got, tc.want)
			}
		})
	}
}

func TestPlatformRoleAtLeast(t *testing.T) {
	cases := []struct {
		name     string
		held     PlatformRole
		required PlatformRole
		want     bool
	}{
		{name: "operator satisfies operator", held: PlatformOperator, required: PlatformOperator, want: true},
		{name: "operator satisfies member", held: PlatformOperator, required: PlatformMember, want: true},
		{name: "member does not satisfy operator", held: PlatformMember, required: PlatformOperator},
		{name: "member satisfies member", held: PlatformMember, required: PlatformMember, want: true},
		{name: "no role does not satisfy member", held: PlatformRoleNone, required: PlatformMember},
		{name: "no role does not satisfy no role", held: PlatformRoleNone, required: PlatformRoleNone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.held.AtLeast(tc.required); got != tc.want {
				t.Errorf("%v.AtLeast(%v) = %v, want %v", tc.held, tc.required, got, tc.want)
			}
		})
	}
}

func TestProjectRoleWireForm(t *testing.T) {
	cases := []struct {
		role ProjectRole
		wire string
	}{
		{role: ProjectViewer, wire: string(kitchenv1alpha1.AccessRoleViewer)},
		{role: ProjectDeveloper, wire: string(kitchenv1alpha1.AccessRoleDeveloper)},
		{role: ProjectAdmin, wire: string(kitchenv1alpha1.AccessRoleAdmin)},
	}

	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			if got := tc.role.String(); got != tc.wire {
				t.Errorf("String() = %q, want %q", got, tc.wire)
			}
			got, ok := ParseProjectRole(tc.wire)
			if !ok || got != tc.role {
				t.Errorf("ParseProjectRole(%q) = %v, %v, want %v, true", tc.wire, got, ok, tc.role)
			}
		})
	}

	// Anything the CRD's enum would refuse resolves to the zero value with
	// false, rather than to whichever role it looks most like.
	for _, wire := range []string{"", "none", "Admin", "owner", "ADMIN"} {
		t.Run("rejects "+wire, func(t *testing.T) {
			if got, ok := ParseProjectRole(wire); ok || got != ProjectRoleNone {
				t.Errorf("ParseProjectRole(%q) = %v, %v, want %v, false", wire, got, ok, ProjectRoleNone)
			}
		})
	}

	if got := ProjectRoleNone.String(); got != "" {
		t.Errorf("ProjectRoleNone.String() = %q, want %q", got, "")
	}
}

func TestPlatformRoleWireForm(t *testing.T) {
	cases := []struct {
		role PlatformRole
		wire string
	}{
		{role: PlatformMember, wire: "member"},
		{role: PlatformOperator, wire: "operator"},
	}

	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			if got := tc.role.String(); got != tc.wire {
				t.Errorf("String() = %q, want %q", got, tc.wire)
			}
			got, ok := ParsePlatformRole(tc.wire)
			if !ok || got != tc.role {
				t.Errorf("ParsePlatformRole(%q) = %v, %v, want %v, true", tc.wire, got, ok, tc.role)
			}
		})
	}

	for _, wire := range []string{"", "Operator", "admin"} {
		t.Run("rejects "+wire, func(t *testing.T) {
			if got, ok := ParsePlatformRole(wire); ok || got != PlatformRoleNone {
				t.Errorf("ParsePlatformRole(%q) = %v, %v, want %v, false", wire, got, ok, PlatformRoleNone)
			}
		})
	}

	if got := PlatformRoleNone.String(); got != "" {
		t.Errorf("PlatformRoleNone.String() = %q, want %q", got, "")
	}
}

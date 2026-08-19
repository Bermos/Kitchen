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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	annaSubject = "user_01H8XANNA"
	annaEmail   = "anna@example.com"
	benSubject  = "user_01H8XBEN"
	benEmail    = "ben@example.com"
)

// anna is the ordinary account most cases are about: a verified address, and
// nothing granted to her unless the fixture says so.
func anna() Caller {
	return Caller{Subject: annaSubject, Email: annaEmail, EmailVerified: true}
}

// kitchenWith builds the platform singleton with the given operator list.
// A nil list is the installation that has never been told who its operators
// are, which is a different object from one whose list is empty.
func kitchenWith(operators ...string) *kitchenv1alpha1.Kitchen {
	kitchen := &kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	for _, subject := range operators {
		kitchen.Spec.Access.Operators = append(
			kitchen.Spec.Access.Operators,
			kitchenv1alpha1.AccessSubject{Subject: subject},
		)
	}
	return kitchen
}

// projectWith builds a Project whose access list is the given grants.
func projectWith(name string, grants ...kitchenv1alpha1.AccessGrant) *kitchenv1alpha1.Project {
	return &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       kitchenv1alpha1.ProjectSpec{Access: grants},
	}
}

func grant(subject string, role kitchenv1alpha1.AccessRole) kitchenv1alpha1.AccessGrant {
	return kitchenv1alpha1.AccessGrant{
		AccessSubject: kitchenv1alpha1.AccessSubject{Subject: subject},
		Role:          role,
	}
}

func TestPlatformRoleFor(t *testing.T) {
	cases := []struct {
		name    string
		caller  Caller
		kitchen *kitchenv1alpha1.Kitchen
		want    PlatformRole
	}{
		{
			// The installation upgrading into enforcement: nobody has said
			// who the operators are. Absent is not "everyone" — the seeding
			// happens on upgrade, not here.
			name:    "absent operator list makes everybody a member",
			caller:  anna(),
			kitchen: kitchenWith(),
			want:    PlatformMember,
		},
		{
			// Narrowed to nobody on purpose. Same answer, different meaning,
			// and the difference is why the field carries no default.
			name:    "empty operator list makes everybody a member",
			caller:  anna(),
			kitchen: &kitchenv1alpha1.Kitchen{Spec: kitchenv1alpha1.KitchenSpec{Access: kitchenv1alpha1.AccessSpec{Operators: []kitchenv1alpha1.AccessSubject{}}}},
			want:    PlatformMember,
		},
		{
			name:    "listed subject is an operator",
			caller:  anna(),
			kitchen: kitchenWith(benSubject, annaSubject),
			want:    PlatformOperator,
		},
		{
			name:    "unlisted subject is a member",
			caller:  anna(),
			kitchen: kitchenWith(benSubject),
			want:    PlatformMember,
		},
		{
			name:    "verified email subject is an operator",
			caller:  anna(),
			kitchen: kitchenWith("ANNA@Example.COM"),
			want:    PlatformOperator,
		},
		{
			name:    "unverified email subject grants nothing",
			caller:  Caller{Subject: annaSubject, Email: annaEmail},
			kitchen: kitchenWith(annaEmail),
			want:    PlatformMember,
		},
		{
			// No singleton to read: refuse the platform rather than hand it
			// to whoever asked first.
			name:    "no kitchen makes everybody a member",
			caller:  anna(),
			kitchen: nil,
			want:    PlatformMember,
		},
		{
			// A caller with nothing to match on must not be caught by an
			// entry that is somehow empty.
			name:    "empty caller matches nothing",
			caller:  Caller{},
			kitchen: kitchenWith("", annaSubject),
			want:    PlatformMember,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlatformRoleFor(tc.caller, tc.kitchen); got != tc.want {
				t.Errorf("PlatformRoleFor() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProjectRoleFor(t *testing.T) {
	cases := []struct {
		name    string
		caller  Caller
		kitchen *kitchenv1alpha1.Kitchen
		project *kitchenv1alpha1.Project
		want    ProjectRole
	}{
		{
			name:    "grant by subject",
			caller:  anna(),
			kitchen: kitchenWith(),
			project: projectWith("shop", grant(annaSubject, kitchenv1alpha1.AccessRoleDeveloper)),
			want:    ProjectDeveloper,
		},
		{
			// The case the whole model turns on: no grant is no role, not a
			// quiet viewer.
			name:    "a member with no grant holds nothing",
			caller:  anna(),
			kitchen: kitchenWith(),
			project: projectWith("shop", grant(benSubject, kitchenv1alpha1.AccessRoleAdmin)),
			want:    ProjectRoleNone,
		},
		{
			name:    "an empty access list holds nothing",
			caller:  anna(),
			kitchen: kitchenWith(),
			project: projectWith("shop"),
			want:    ProjectRoleNone,
		},
		{
			// operator ⇒ admin, on a project whose list has never heard of
			// them.
			name:    "an operator is admin on a project they are not listed on",
			caller:  anna(),
			kitchen: kitchenWith(annaSubject),
			project: projectWith("shop", grant(benSubject, kitchenv1alpha1.AccessRoleViewer)),
			want:    ProjectAdmin,
		},
		{
			// A listed operator is not demoted by a lesser grant of their
			// own: the platform role contains the project one entirely.
			name:    "an operator listed as viewer is still admin",
			caller:  anna(),
			kitchen: kitchenWith(annaSubject),
			project: projectWith("shop", grant(annaSubject, kitchenv1alpha1.AccessRoleViewer)),
			want:    ProjectAdmin,
		},
		{
			name:    "an operator is admin with no project at all",
			caller:  anna(),
			kitchen: kitchenWith(annaSubject),
			project: nil,
			want:    ProjectAdmin,
		},
		{
			name:    "a member holds nothing with no project at all",
			caller:  anna(),
			kitchen: kitchenWith(),
			project: nil,
			want:    ProjectRoleNone,
		},
		{
			name:    "verified email grant resolves case-insensitively",
			caller:  anna(),
			kitchen: kitchenWith(),
			project: projectWith("shop", grant("Anna@EXAMPLE.com", kitchenv1alpha1.AccessRoleDeveloper)),
			want:    ProjectDeveloper,
		},
		{
			// An unverified address is a claim by the token holder, so the
			// grant is to nobody until the identity provider stands behind
			// it.
			name:    "email grant with an unverified address resolves to nothing",
			caller:  Caller{Subject: annaSubject, Email: annaEmail},
			kitchen: kitchenWith(),
			project: projectWith("shop", grant(annaEmail, kitchenv1alpha1.AccessRoleAdmin)),
			want:    ProjectRoleNone,
		},
		{
			// The informational Email field is exactly that: it is not what
			// an address-shaped grant matches against.
			name:    "a token with no email claim matches no email grant",
			caller:  Caller{Subject: annaSubject, EmailVerified: true},
			kitchen: kitchenWith(),
			project: projectWith("shop", grant(annaEmail, kitchenv1alpha1.AccessRoleAdmin)),
			want:    ProjectRoleNone,
		},
		{
			// Somebody else's address, however verified this caller's own is.
			name:    "another account's email grant resolves to nothing",
			caller:  anna(),
			kitchen: kitchenWith(),
			project: projectWith("shop", grant(benEmail, kitchenv1alpha1.AccessRoleAdmin)),
			want:    ProjectRoleNone,
		},
		{
			// Two entries, one by sub and one by verified address. Both were
			// written down; the higher is what was meant.
			name:    "two matching grants resolve to the higher",
			caller:  anna(),
			kitchen: kitchenWith(),
			project: projectWith("shop",
				grant(annaSubject, kitchenv1alpha1.AccessRoleViewer),
				grant(annaEmail, kitchenv1alpha1.AccessRoleAdmin),
			),
			want: ProjectAdmin,
		},
		{
			// A role this build has never heard of — an older or newer
			// Kitchen's — is nothing, not something plausible.
			name:    "an unrecognised role resolves to nothing",
			caller:  anna(),
			kitchen: kitchenWith(),
			project: projectWith("shop", grant(annaSubject, kitchenv1alpha1.AccessRole("owner"))),
			want:    ProjectRoleNone,
		},
		{
			name:    "an unrecognised role does not shadow a real one",
			caller:  anna(),
			kitchen: kitchenWith(),
			project: projectWith("shop",
				grant(annaSubject, kitchenv1alpha1.AccessRole("owner")),
				grant(annaEmail, kitchenv1alpha1.AccessRoleViewer),
			),
			want: ProjectViewer,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProjectRoleFor(tc.caller, tc.kitchen, tc.project); got != tc.want {
				t.Errorf("ProjectRoleFor() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVisibleProjects(t *testing.T) {
	shop := *projectWith("shop", grant(annaSubject, kitchenv1alpha1.AccessRoleViewer))
	billing := *projectWith("billing", grant(benSubject, kitchenv1alpha1.AccessRoleAdmin))
	invoices := *projectWith("invoices", grant(annaEmail, kitchenv1alpha1.AccessRoleDeveloper))
	all := []kitchenv1alpha1.Project{shop, billing, invoices}

	cases := []struct {
		name    string
		caller  Caller
		kitchen *kitchenv1alpha1.Kitchen
		want    []string
	}{
		{
			name:    "a member sees the projects they are granted",
			caller:  anna(),
			kitchen: kitchenWith(),
			want:    []string{"shop", "invoices"},
		},
		{
			name:    "an operator sees every project",
			caller:  anna(),
			kitchen: kitchenWith(annaSubject),
			want:    []string{"shop", "billing", "invoices"},
		},
		{
			name:    "a member granted nothing sees nothing",
			caller:  Caller{Subject: "user_01H8XNOBODY"},
			kitchen: kitchenWith(),
			want:    nil,
		},
		{
			// The email grant on invoices is not hers to use unverified, and
			// the sub grant on shop still is.
			name:    "an unverified address drops the projects granted by email",
			caller:  Caller{Subject: annaSubject, Email: annaEmail},
			kitchen: kitchenWith(),
			want:    []string{"shop"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := VisibleProjects(tc.caller, tc.kitchen, all)
			names := make([]string, 0, len(got))
			for _, project := range got {
				names = append(names, project.Name)
			}
			if len(names) != len(tc.want) {
				t.Fatalf("VisibleProjects() = %v, want %v", names, tc.want)
			}
			for i, want := range tc.want {
				if names[i] != want {
					t.Errorf("VisibleProjects()[%d] = %q, want %q", i, names[i], want)
				}
			}
		})
	}
}

// VisibleProjects is handed the informer's own slice often enough that
// filtering in place would be a live bug rather than a style point.
func TestVisibleProjectsDoesNotMutateItsInput(t *testing.T) {
	projects := []kitchenv1alpha1.Project{
		*projectWith("shop", grant(annaSubject, kitchenv1alpha1.AccessRoleViewer)),
		*projectWith("billing", grant(benSubject, kitchenv1alpha1.AccessRoleAdmin)),
	}

	if got := VisibleProjects(anna(), kitchenWith(), projects); len(got) != 1 {
		t.Fatalf("VisibleProjects() returned %d projects, want 1", len(got))
	}
	if projects[0].Name != "shop" || projects[1].Name != "billing" {
		t.Errorf("VisibleProjects() reordered its input: %q, %q", projects[0].Name, projects[1].Name)
	}
}

func TestIsEmailSubject(t *testing.T) {
	cases := map[string]bool{
		annaEmail:     true,
		annaSubject:   false,
		"":            false,
		"anna@":       true,
		"user@sub@ok": true,
	}

	for subject, want := range cases {
		t.Run(subject, func(t *testing.T) {
			if got := IsEmailSubject(subject); got != want {
				t.Errorf("IsEmailSubject(%q) = %v, want %v", subject, got, want)
			}
		})
	}
}

// The two spellings issuers actually send, and everything else reading as
// unverified — which is the direction that withholds rather than grants.
func TestVerifiedClaim(t *testing.T) {
	cases := map[string]bool{
		`true`:      true,
		`false`:     false,
		`"true"`:    true,
		`"false"`:   false,
		`"TRUE"`:    true,
		`"1"`:       true,
		`1`:         false,
		`null`:      false,
		``:          false,
		`"perhaps"`: false,
		`{}`:        false,
	}

	for claim, want := range cases {
		t.Run(claim, func(t *testing.T) {
			if got := VerifiedClaim([]byte(claim)); got != want {
				t.Errorf("VerifiedClaim(%s) = %v, want %v", claim, got, want)
			}
		})
	}
}

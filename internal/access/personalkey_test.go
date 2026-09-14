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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// A narrowed personal key is the lesser of itself and the person it belongs
// to (#595) — never the greater, whatever its entry says.

// annaHolding is the account access_test.go's `anna` is, presenting one of her
// personal keys — or, for the empty name, signed in and presenting none.
func annaHolding(key string) Caller {
	caller := anna()
	caller.PersonalKey = key
	return caller
}

// keyed is a Kitchen with one personal-key grant on it, and Anna an operator
// or not as the caller says.
func keyed(operator bool, grants ...kitchenv1alpha1.PersonalKeyGrant) *kitchenv1alpha1.Kitchen {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if operator {
		kitchen.Spec.Access.Operators = []kitchenv1alpha1.AccessSubject{{Subject: annaSubject}}
	}
	for i := range grants {
		if grants[i].Subject == "" {
			grants[i].Subject = annaSubject
		}
		if grants[i].Expires.IsZero() {
			grants[i].Expires = metav1.NewTime(time.Now().Add(24 * time.Hour))
		}
	}
	kitchen.Spec.Access.PersonalKeys = grants
	return kitchen
}

// annasProject is a project granting Anna one role, or none at all for the
// empty one — the two halves every case below takes the lesser of.
func annasProject(name string, role kitchenv1alpha1.AccessRole) *kitchenv1alpha1.Project {
	if role == "" {
		return projectWith(name)
	}
	return projectWith(name, kitchenv1alpha1.AccessGrant{
		AccessSubject: kitchenv1alpha1.AccessSubject{Subject: annaSubject},
		Role:          role,
	})
}

// The whole of the feature: a key that may deploy one project, held by
// somebody who may deploy several.
func TestANarrowedKeyReachesOnlyTheProjectsItNames(t *testing.T) {
	kitchen := keyed(false, kitchenv1alpha1.PersonalKeyGrant{
		Key:      "ci-shop",
		Projects: []string{shopProject},
		Role:     kitchenv1alpha1.AccessRoleDeveloper,
	})
	shop := annasProject(shopProject, kitchenv1alpha1.AccessRoleAdmin)
	billing := annasProject("billing", kitchenv1alpha1.AccessRoleAdmin)

	// Through the key: developer on shop, nothing on billing.
	if role := ProjectRoleFor(annaHolding("ci-shop"), kitchen, shop); role != ProjectDeveloper {
		t.Errorf("the key holds %s on the project it names, want developer", role)
	}
	if role := ProjectRoleFor(annaHolding("ci-shop"), kitchen, billing); role != ProjectRoleNone {
		t.Errorf("the key holds %s on a project it does not name, want nothing", role)
	}

	// The person, signed in, still holds everything they were granted.
	if role := ProjectRoleFor(annaHolding(""), kitchen, billing); role != ProjectAdmin {
		t.Errorf("narrowing a key narrowed the person: %s", role)
	}

	// And the narrowing follows into the cross-project reads, which is what
	// keeps a scoped key from listing projects it may not touch.
	visible := VisibleProjects(annaHolding("ci-shop"), kitchen, []kitchenv1alpha1.Project{*shop, *billing})
	if len(visible) != 1 || visible[0].Name != shopProject {
		t.Errorf("the key sees projects it cannot act on: %+v", visible)
	}
}

// The ceiling is a ceiling, not a grant: a key cannot hold more than its owner
// on a project, however its entry is written.
func TestAKeyIsNeverWiderThanThePersonHoldingIt(t *testing.T) {
	kitchen := keyed(false, kitchenv1alpha1.PersonalKeyGrant{
		Key:  "ambitious",
		Role: kitchenv1alpha1.AccessRoleAdmin,
	})

	for _, tc := range []struct {
		held kitchenv1alpha1.AccessRole
		want ProjectRole
	}{
		{kitchenv1alpha1.AccessRoleViewer, ProjectViewer},
		{kitchenv1alpha1.AccessRoleDeveloper, ProjectDeveloper},
		{kitchenv1alpha1.AccessRoleAdmin, ProjectAdmin},
		{"", ProjectRoleNone},
	} {
		project := annasProject(shopProject, tc.held)
		if role := ProjectRoleFor(annaHolding("ambitious"), kitchen, project); role != tc.want {
			t.Errorf("a key claiming admin where its owner holds %q resolved to %s, want %s",
				tc.held, role, tc.want)
		}
	}
}

// An operator's narrowed key is not an operator. That is the difference
// between a credential that can do one job and a copy of the platform.
func TestANarrowedKeyWearsNoOperatorHat(t *testing.T) {
	kitchen := keyed(true, kitchenv1alpha1.PersonalKeyGrant{
		Key:      "ci-shop",
		Projects: []string{shopProject},
		Role:     kitchenv1alpha1.AccessRoleDeveloper,
	})

	if role := PlatformRoleFor(annaHolding("ci-shop"), kitchen); role != PlatformMember {
		t.Errorf("a narrowed key wears the hat: %s", role)
	}
	// And the hat's consequence — admin on every project — goes with it. The
	// key holds developer on shop because its entry says so, and nothing at
	// all on a project the entry does not name, even though its owner is an
	// operator.
	if role := ProjectRoleFor(annaHolding("ci-shop"), kitchen, annasProject(shopProject, "")); role != ProjectDeveloper {
		t.Errorf("want developer on the named project, got %s", role)
	}
	if role := ProjectRoleFor(annaHolding("ci-shop"), kitchen, annasProject("billing", "")); role != ProjectRoleNone {
		t.Errorf("an operator's narrowed key reached a project it does not name: %s", role)
	}

	// The same person, signed in, is still an operator.
	if role := PlatformRoleFor(annaHolding(""), kitchen); role != PlatformOperator {
		t.Errorf("the person lost the hat: %s", role)
	}
}

// An unrestricted key is its owner, entire — which is what shipped before the
// narrowing existed, and what somebody asks for when they want exactly that.
func TestAnUnrestrictedKeyIsItsOwner(t *testing.T) {
	kitchen := keyed(true, kitchenv1alpha1.PersonalKeyGrant{Key: "laptop", Unrestricted: true})

	if role := PlatformRoleFor(annaHolding("laptop"), kitchen); role != PlatformOperator {
		t.Errorf("an unrestricted key of an operator is an operator, got %s", role)
	}
	if role := ProjectRoleFor(annaHolding("laptop"), kitchen, annasProject("billing", "")); role != ProjectAdmin {
		t.Errorf("an unrestricted key holds what its owner holds, got %s", role)
	}
}

// A key the platform has no entry for holds nothing at all. Not its owner:
// that reading would make deleting an entry a promotion, and would make a key
// revoked at one end and not the other the most powerful thing on the
// platform.
func TestAKeyWithNoGrantHoldsNothing(t *testing.T) {
	kitchen := keyed(true, kitchenv1alpha1.PersonalKeyGrant{Key: "laptop", Unrestricted: true})

	stranger := annaHolding("revoked")
	if role := PlatformRoleFor(stranger, kitchen); role != PlatformMember {
		t.Errorf("an unrecognised key wears a hat: %s", role)
	}
	if role := ProjectRoleFor(stranger, kitchen, annasProject(shopProject, kitchenv1alpha1.AccessRoleAdmin)); role != ProjectRoleNone {
		t.Errorf("an unrecognised key holds %s, want nothing", role)
	}
	if !HoldsUnknownKey(stranger, kitchen) {
		t.Error("the platform cannot say that it does not recognise the key")
	}
	if HoldsUnknownKey(annaHolding("laptop"), kitchen) || HoldsUnknownKey(annaHolding(""), kitchen) {
		t.Error("a key the platform does recognise reads as unknown")
	}

	// And somebody else's key of the same name is not this caller's.
	elsewhere := keyed(false, kitchenv1alpha1.PersonalKeyGrant{
		AccessSubject: kitchenv1alpha1.AccessSubject{Subject: "user_someone_else"},
		Key:           "laptop",
		Unrestricted:  true,
	})
	if role := ProjectRoleFor(annaHolding("laptop"), elsewhere, annasProject(shopProject, kitchenv1alpha1.AccessRoleAdmin)); role != ProjectRoleNone {
		t.Errorf("a grant naming somebody else was honoured: %s", role)
	}
}

// An entry that names no role is a viewer: the safe reading of a credential
// somebody issued without saying what it may do.
func TestAnEntryThatNamesNoRoleReads(t *testing.T) {
	kitchen := keyed(false, kitchenv1alpha1.PersonalKeyGrant{Key: "quiet", Projects: []string{shopProject}})
	project := annasProject(shopProject, kitchenv1alpha1.AccessRoleAdmin)

	if role := ProjectRoleFor(annaHolding("quiet"), kitchen, project); role != ProjectViewer {
		t.Errorf("a key that named no role holds %s, want viewer", role)
	}
}

// An empty project list is every project its owner can reach — the same
// reading a platform credential's allowlist has, and safe for the same reason:
// the role still caps it.
func TestAnEmptyProjectListIsEveryProjectTheOwnerCanReach(t *testing.T) {
	kitchen := keyed(false, kitchenv1alpha1.PersonalKeyGrant{
		Key:  "read-everything",
		Role: kitchenv1alpha1.AccessRoleViewer,
	})

	mine := annasProject(shopProject, kitchenv1alpha1.AccessRoleAdmin)
	theirs := annasProject("billing", "")

	if role := ProjectRoleFor(annaHolding("read-everything"), kitchen, mine); role != ProjectViewer {
		t.Errorf("want viewer on a project its owner administers, got %s", role)
	}
	if role := ProjectRoleFor(annaHolding("read-everything"), kitchen, theirs); role != ProjectRoleNone {
		t.Errorf("a key reached a project its owner cannot: %s", role)
	}
}

// A key's platform scopes are honoured only while its owner is an operator,
// and only while the key itself is honoured.
func TestAKeysScopesFollowItsOwnersHat(t *testing.T) {
	now := time.Now()
	grant := kitchenv1alpha1.PersonalKeyGrant{
		Key:      "backups",
		Projects: []string{shopProject},
		Role:     kitchenv1alpha1.AccessRoleViewer,
		Scopes:   []kitchenv1alpha1.PlatformScope{kitchenv1alpha1.PlatformScopeBackupRun},
	}

	held := ScopesFor(annaHolding("backups"), keyed(true, grant), now)
	if !held.Allows(ScopeBackupRun) {
		t.Error("an operator's key does not hold the scope it was issued with")
	}
	if projects := held.Projects(); len(projects) != 1 || projects[0] != shopProject {
		t.Errorf("the key's allowlist did not narrow its scoped routes: %v", projects)
	}

	// The owner is demoted: the credential goes with the authority that
	// issued it, without anything having to rewrite the entry.
	if ScopesFor(annaHolding("backups"), keyed(false, grant), now).Allows(ScopeBackupRun) {
		t.Error("a key kept its scopes after its owner stopped being an operator")
	}

	// And a lapsed entry holds nothing either.
	lapsed := grant
	lapsed.Expires = metav1.NewTime(now.Add(-time.Hour))
	if ScopesFor(annaHolding("backups"), keyed(true, lapsed), now).Allows(ScopeBackupRun) {
		t.Error("a lapsed key still holds its scopes")
	}

	// A key issued without scopes holds none, which is every key but the few
	// an operator deliberately widened.
	plain := kitchenv1alpha1.PersonalKeyGrant{Key: "ci-shop", Projects: []string{shopProject}}
	if len(ScopesFor(annaHolding("ci-shop"), keyed(true, plain), now).Held()) != 0 {
		t.Error("a key with no scopes holds some")
	}
}

// Somebody signed in is unaffected by any of this: no claim, no narrowing, and
// the same answers the platform gave before personal keys existed.
func TestSomebodySignedInIsResolvedExactlyAsBefore(t *testing.T) {
	kitchen := keyed(true, kitchenv1alpha1.PersonalKeyGrant{
		Key: "ci-shop", Projects: []string{shopProject}, Role: kitchenv1alpha1.AccessRoleViewer,
	})

	if role := PlatformRoleFor(annaHolding(""), kitchen); role != PlatformOperator {
		t.Errorf("a browser session lost the hat: %s", role)
	}
	if role := ProjectRoleFor(annaHolding(""), kitchen, annasProject("billing", "")); role != ProjectAdmin {
		t.Errorf("a browser session was narrowed: %s", role)
	}
}

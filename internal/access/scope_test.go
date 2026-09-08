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

// What scope resolution promises: the expiry is applied here rather than by
// whatever sweeps the list, a scope nobody recognises grants nothing, and two
// entries about one caller resolve to the wider of the two in both dimensions.

var now = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// shopProject is the project these tests narrow a credential to.
const shopProject = "shop"

// agent is the caller these tests are about.
var agent = Caller{Subject: "user_agent", Email: "nightly@platform.kitchen.local"}

// withCredentials is a singleton granting exactly these credentials.
func withCredentials(entries ...kitchenv1alpha1.PlatformCredential) *kitchenv1alpha1.Kitchen {
	return &kitchenv1alpha1.Kitchen{
		Spec: kitchenv1alpha1.KitchenSpec{
			Access: kitchenv1alpha1.AccessSpec{Credentials: entries},
		},
	}
}

// credential is one entry for the caller above, live for an hour.
func credential(scopes ...kitchenv1alpha1.PlatformScope) kitchenv1alpha1.PlatformCredential {
	return kitchenv1alpha1.PlatformCredential{
		AccessSubject: kitchenv1alpha1.AccessSubject{Subject: agent.Subject, Email: agent.Email},
		Scopes:        scopes,
		Expires:       metav1.NewTime(now.Add(time.Hour)),
	}
}

func TestAGrantCarriesTheScopesItNames(t *testing.T) {
	grant := ScopesFor(agent, withCredentials(
		credential(kitchenv1alpha1.PlatformScopeRead)), now)

	if !grant.Allows(ScopePlatformRead) {
		t.Fatal("the scope it was granted is not held")
	}
	if grant.Allows(ScopeBackupRun) {
		t.Fatal("a scope it was not granted is held")
	}
}

// The zero scope is what an unscoped route's requirement carries, and it must
// never be satisfied — which is the default-deny the whole design rests on.
func TestNoGrantAllowsTheZeroScope(t *testing.T) {
	grant := ScopesFor(agent, withCredentials(
		credential(kitchenv1alpha1.PlatformScopeRead, kitchenv1alpha1.PlatformScopeBackupRun)), now)

	if grant.Allows(ScopeNone) {
		t.Fatal("a credential holding every scope satisfied a route that names none")
	}
}

// The expiry is applied at resolution, so a lapsed credential holds nothing on
// every replica the moment it lapses.
func TestALapsedCredentialHoldsNothing(t *testing.T) {
	entry := credential(kitchenv1alpha1.PlatformScopeRead)
	entry.Expires = metav1.NewTime(now.Add(-time.Second))

	grant := ScopesFor(agent, withCredentials(entry), now)
	if !grant.Empty() {
		t.Fatalf("a lapsed credential resolved to %v", grant.Held())
	}
}

// An entry with no expiry at all reads as lapsed rather than as eternal. The
// CRD requires the field, so a zero value is an object written around the API
// server — and the safe reading of "nobody recorded an expiry" is that this is
// not honoured.
func TestACredentialWithNoExpiryHoldsNothing(t *testing.T) {
	entry := credential(kitchenv1alpha1.PlatformScopeRead)
	entry.Expires = metav1.Time{}

	if !Expired(entry, now) {
		t.Fatal("an entry with no expiry was honoured")
	}
	if grant := ScopesFor(agent, withCredentials(entry), now); !grant.Empty() {
		t.Fatalf("an entry with no expiry resolved to %v", grant.Held())
	}
}

// A scope nobody recognises is dropped rather than honoured — the direction
// every other resolution here takes for a value it cannot read.
func TestAnUnknownScopeGrantsNothing(t *testing.T) {
	grant := ScopesFor(agent, withCredentials(credential("platform.write")), now)

	if !grant.Empty() {
		t.Fatalf("an unrecognised scope was honoured as %v", grant.Held())
	}
}

// Nobody but the caller the entry names holds it.
func TestACredentialIsNobodyElses(t *testing.T) {
	kitchen := withCredentials(credential(kitchenv1alpha1.PlatformScopeRead))
	other := Caller{Subject: "user_someone", Email: "anna@example.com", EmailVerified: true}

	if grant := ScopesFor(other, kitchen, now); !grant.Empty() {
		t.Fatalf("somebody else holds this credential's scopes: %v", grant.Held())
	}
}

// A platform with no singleton grants nothing, for PlatformRoleFor's reason:
// the worst that can do is refuse an operation to somebody entitled to it.
func TestNoSingletonGrantsNothing(t *testing.T) {
	if grant := ScopesFor(agent, nil, now); !grant.Empty() {
		t.Fatalf("an absent singleton granted %v", grant.Held())
	}
}

// The project allowlist: unnarrowed is every project, narrowed is exactly what
// it names, and a request naming no project at all is not one a narrowing was
// ever meant to include.
func TestTheProjectAllowlistNarrowsAndDefaultsToAll(t *testing.T) {
	unnarrowed := ScopesFor(agent, withCredentials(
		credential(kitchenv1alpha1.PlatformScopeComplianceRead)), now)
	if !unnarrowed.AllowsProject(shopProject) || !unnarrowed.AllowsProject("") {
		t.Fatal("an unnarrowed credential does not cover every project")
	}

	entry := credential(kitchenv1alpha1.PlatformScopeComplianceRead)
	entry.Projects = []string{shopProject}
	narrowed := ScopesFor(agent, withCredentials(entry), now)
	if !narrowed.AllowsProject(shopProject) {
		t.Fatal("a narrowed credential does not cover the project it names")
	}
	if narrowed.AllowsProject("blog") || narrowed.AllowsProject("") {
		t.Fatal("a narrowed credential covers a project it does not name")
	}
	if got := narrowed.Projects(); len(got) != 1 || got[0] != shopProject {
		t.Fatalf("want the allowlist reported back, got %v", got)
	}
}

// Two entries about one caller — which the list's map key makes impossible at
// admission and hand-written YAML can still produce — resolve to the wider of
// the two, in both dimensions.
func TestTwoEntriesResolveToTheWiderOfThem(t *testing.T) {
	narrow := credential(kitchenv1alpha1.PlatformScopeComplianceRead)
	narrow.Projects = []string{shopProject}
	wide := credential(kitchenv1alpha1.PlatformScopeRead)

	// In both orders, because "the widest wins" must not depend on which entry
	// the list happens to carry first.
	for _, entries := range [][]kitchenv1alpha1.PlatformCredential{
		{narrow, wide}, {wide, narrow},
	} {
		grant := ScopesFor(agent, withCredentials(entries...), now)
		if !grant.Allows(ScopePlatformRead) || !grant.Allows(ScopeComplianceRead) {
			t.Fatalf("the union of two entries lost a scope: %v", grant.Held())
		}
		if !grant.AllowsProject("blog") {
			t.Fatal("an entry that narrows nothing was narrowed by the one beside it")
		}
	}
}

// The vocabulary reads back exactly, and nothing else does.
func TestParseScope(t *testing.T) {
	for _, scope := range Scopes() {
		if parsed, ok := ParseScope(scope.String()); !ok || parsed != scope {
			t.Fatalf("%s does not read back", scope)
		}
	}
	for _, bad := range []string{"", "  ", "operator", "platform.write", "PLATFORM.READ"} {
		if _, ok := ParseScope(bad); ok {
			t.Fatalf("%q read as a scope", bad)
		}
	}
}

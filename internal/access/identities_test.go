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

var surveyNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func surveyPlatform(operators ...kitchenv1alpha1.AccessSubject) *kitchenv1alpha1.Kitchen {
	return &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       kitchenv1alpha1.KitchenSpec{Access: kitchenv1alpha1.AccessSpec{Operators: operators}},
	}
}

func surveyProject(name string, grants ...kitchenv1alpha1.AccessGrant) kitchenv1alpha1.Project {
	return kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       kitchenv1alpha1.ProjectSpec{Access: grants},
	}
}

// The survey answers one row per grant, not one per account: an account
// holding admin on three projects is three decisions for a reviewer, and
// collapsing them would ask one question where three were owed.
func TestSurveyIsOneRowPerGrant(t *testing.T) {
	survey := Survey(SurveyInput{
		Kitchen: surveyPlatform(kitchenv1alpha1.AccessSubject{Subject: "sub-1", Email: "grace@example.com"}),
		Projects: []kitchenv1alpha1.Project{
			surveyProject("shop", kitchenv1alpha1.AccessGrant{
				AccessSubject: kitchenv1alpha1.AccessSubject{Subject: "sub-1", Email: "grace@example.com"},
				Role:          kitchenv1alpha1.AccessRoleAdmin,
			}),
			surveyProject("billing", kitchenv1alpha1.AccessGrant{
				AccessSubject: kitchenv1alpha1.AccessSubject{Subject: "sub-2"},
				Role:          kitchenv1alpha1.AccessRoleDeveloper,
			}),
		},
		At:             surveyNow,
		InactivityDays: 90,
	})

	if len(survey.Identities) != 3 {
		t.Fatalf("want three grants, got %d: %+v", len(survey.Identities), survey.Identities)
	}
	// The platform's own grants lead: they are what a reviewer should meet
	// first, and a stable order is what makes two snapshots diffable.
	if survey.Identities[0].Grant != PlatformGrant || survey.Identities[0].Role != "operator" {
		t.Fatalf("the operator grant must lead: %+v", survey.Identities[0])
	}
	if survey.Identities[1].Grant != "billing" || survey.Identities[2].Grant != "shop" {
		t.Fatalf("project grants must be in name order: %+v", survey.Identities)
	}
}

// Activity is looked up by both spellings. The audit log names an actor by
// whatever the caller was named by — the verified address where a token
// carries one — so a grant written as a `sub` and activity recorded against
// the address are the same person.
func TestSurveyMatchesActivityByEitherSpelling(t *testing.T) {
	survey := Survey(SurveyInput{
		Kitchen: surveyPlatform(kitchenv1alpha1.AccessSubject{Subject: "sub-1", Email: "Grace@Example.com"}),
		Activity: map[string]time.Time{
			"grace@example.com": surveyNow.Add(-time.Hour),
		},
		At:             surveyNow,
		InactivityDays: 90,
	})
	identity := survey.Identities[0]
	if identity.LastActive == nil {
		t.Fatal("a grant by sub whose address has been active must not read as dormant")
	}
	if identity.Inactive {
		t.Fatalf("an hour ago is not dormant against a 90-day window: %+v", identity)
	}
}

// The issue's own definition: an orphan is no recent activity AND no owner.
// Either half alone has an innocent reading, and a survey that called either
// an orphan would produce a list nobody acts on.
func TestAnOrphanIsDormantAndUnknownTogether(t *testing.T) {
	survey := Survey(SurveyInput{
		Kitchen: surveyPlatform(
			// Dormant, but the directory knows them: a quiet quarter.
			kitchenv1alpha1.AccessSubject{Subject: "quiet@example.com"},
			// Active, but unknown to the directory: a machine account, or an
			// entry written before the account existed.
			kitchenv1alpha1.AccessSubject{Subject: "busy@example.com"},
			// Neither. This is the orphan.
			kitchenv1alpha1.AccessSubject{Subject: "gone@example.com"},
		),
		Activity: map[string]time.Time{
			"quiet@example.com": surveyNow.Add(-400 * 24 * time.Hour),
			"busy@example.com":  surveyNow.Add(-time.Hour),
		},
		Accounts: map[string]struct{}{
			"quiet@example.com": {},
		},
		DirectoryConsulted: true,
		At:                 surveyNow,
		InactivityDays:     90,
	})

	byName := map[string]Identity{}
	for _, identity := range survey.Identities {
		byName[identity.Subject] = identity
	}
	if byName["quiet@example.com"].Orphaned {
		t.Error("dormant but known is not an orphan: it is somebody who had a quiet quarter")
	}
	if byName["busy@example.com"].Orphaned {
		t.Error("unknown but active is not an orphan: somebody is plainly behind it")
	}
	if !byName["gone@example.com"].Orphaned {
		t.Error("dormant and unknown together is the orphan, and was not reported as one")
	}
	if survey.Orphans != 1 {
		t.Errorf("want one orphan, got %d", survey.Orphans)
	}
}

// A federated issuer serves no account directory at all, and "we could not
// ask" must not become "nobody is behind it". No row claims to be unknown,
// and therefore nothing is orphaned.
func TestNoDirectoryClaimsNoOrphans(t *testing.T) {
	survey := Survey(SurveyInput{
		Kitchen: surveyPlatform(kitchenv1alpha1.AccessSubject{Subject: "gone@example.com"}),
		// No activity at all, so every row is dormant.
		DirectoryConsulted: false,
		At:                 surveyNow,
		InactivityDays:     90,
		Message:            "the account directory could not be read",
	})
	identity := survey.Identities[0]
	if !identity.Inactive {
		t.Error("an identity with nothing recorded is dormant by this measure")
	}
	if identity.Unknown || identity.Orphaned {
		t.Errorf("a survey that could not ask the directory must claim nothing about ownership: %+v", identity)
	}
	if survey.Orphans != 0 {
		t.Errorf("want no orphans claimed, got %d", survey.Orphans)
	}
	if survey.Message == "" {
		t.Error("a survey that could not read something must say so: the failure mode of evidence is silence")
	}
}

// The dormancy window is the configured one, and it is a boundary worth
// pinning: an identity active exactly at the edge is not yet dormant.
func TestDormancyIsJudgedAgainstTheConfiguredWindow(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		last     time.Duration
		inactive bool
	}{
		{"inside the window", -89 * 24 * time.Hour, false},
		{"on the edge", -90 * 24 * time.Hour, false},
		{"past it", -91 * 24 * time.Hour, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			survey := Survey(SurveyInput{
				Kitchen:        surveyPlatform(kitchenv1alpha1.AccessSubject{Subject: "sub-1"}),
				Activity:       map[string]time.Time{"sub-1": surveyNow.Add(testCase.last)},
				At:             surveyNow,
				InactivityDays: 90,
			})
			if got := survey.Identities[0].Inactive; got != testCase.inactive {
				t.Errorf("last active %v: inactive=%v, want %v", testCase.last, got, testCase.inactive)
			}
		})
	}
}

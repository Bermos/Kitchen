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

// Who holds what, materialized once.
//
// This is the input a recertification cycle freezes and the answer the live
// "who has access here" read serves — one function, two consumers, because a
// snapshot assembled by different code from the screen showing it is a
// snapshot nobody can check against the screen.
//
// It is materialized rather than fetched during evaluation, which is the
// suite's rule everywhere else: the grants, the directory's accounts and the
// activity are all read first and handed in, so this is a pure function of
// its arguments and a test needs no cluster and no store.

// PlatformGrant is the value Grant carries for a role held on the platform
// itself rather than on a project. It is a word rather than an empty string
// so a row that lost its project reads as one, instead of looking like a
// platform grant.
const PlatformGrant = "platform"

// Identity is one account's one role in one place, with what the platform
// knows about whether anybody is still behind it.
type Identity struct {
	// Subject is the entry's subject exactly as written — a `sub` or an
	// address — and Email whatever readable address travels with it.
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`

	// Grant is PlatformGrant or a project name, and Role what is held there.
	Grant string `json:"grant"`
	Role  string `json:"role"`

	// LastActive is the newest audit record attributed to this identity, or
	// nil for one that has never been recorded doing anything.
	LastActive *time.Time `json:"lastActive,omitempty"`

	// Inactive is LastActive older than the window, or absent.
	Inactive bool `json:"inactive,omitempty"`

	// Unknown is the identity provider holding no account for this subject.
	// It is only set when the directory answered — see IdentitySurvey.
	Unknown bool `json:"unknown,omitempty"`

	// Orphaned is Inactive and Unknown together: no recent activity and no
	// owner.
	Orphaned bool `json:"orphaned,omitempty"`
}

// IdentitySurvey is the whole answer: every grant, the window it was judged
// against, and — importantly — whether the directory could be consulted at
// all.
type IdentitySurvey struct {
	// At is the instant surveyed.
	At time.Time `json:"at"`

	// InactivityDays is the window Inactive was judged against.
	InactivityDays int32 `json:"inactivityDays"`

	// DirectoryConsulted says whether the identity provider answered with a
	// list of accounts. When it is false, no row claims to be Unknown and no
	// row is Orphaned, because "we could not ask" and "nobody is behind it"
	// are different sentences and only one of them is evidence. A federated
	// issuer serves no account directory at all, which is the ordinary case
	// for this being false.
	DirectoryConsulted bool `json:"directoryConsulted"`

	// Identities is every grant, sorted by grant then subject so two
	// snapshots of an unchanged platform are byte-identical.
	Identities []Identity `json:"identities"`

	// Orphans is how many of them are orphaned.
	Orphans int `json:"orphans"`

	// Message explains anything the survey could not read.
	Message string `json:"message,omitempty"`
}

// SurveyInput is everything the survey needs, read before it runs.
type SurveyInput struct {
	// Kitchen is the platform singleton, for its operator list. Nil surveys
	// projects alone.
	Kitchen *kitchenv1alpha1.Kitchen

	// Projects are the projects whose grants are in scope.
	Projects []kitchenv1alpha1.Project

	// Activity is subject-or-address to the newest audit record attributed
	// to it. The audit log writes an actor as whatever the caller was named
	// by, so a lookup tries the subject and the address both.
	Activity map[string]time.Time

	// Accounts are the subjects and addresses the identity provider holds,
	// and DirectoryConsulted whether it answered at all. An empty map with
	// DirectoryConsulted true is a platform with no accounts, which is a
	// finding; an empty map with it false is a platform that could not ask,
	// which is not.
	Accounts           map[string]struct{}
	DirectoryConsulted bool

	// InactivityDays is the dormancy window and At the instant to judge
	// against.
	InactivityDays int32
	At             time.Time

	// Message is anything the caller could not read, carried through to the
	// answer.
	Message string
}

// Survey materializes who holds what.
//
// The one judgement in it is the definition of an orphan, and it is the
// issue's own: **no recent activity and no owner**. Either half alone has an
// innocent reading — a quiet quarter for a perfectly real person, or an
// issuer that answers no directory — and a survey that called either an
// orphan would be a list nobody acts on. The pair does not have an innocent
// reading: an identity that has done nothing and belongs to no account is a
// grant pointing at nobody.
func Survey(in SurveyInput) IdentitySurvey {
	survey := IdentitySurvey{
		At:                 in.At.UTC(),
		InactivityDays:     in.InactivityDays,
		DirectoryConsulted: in.DirectoryConsulted,
		Identities:         []Identity{},
		Message:            in.Message,
	}

	window := time.Duration(in.InactivityDays) * 24 * time.Hour
	add := func(subject, email, grant, role string) {
		identity := Identity{Subject: subject, Email: email, Grant: grant, Role: role}
		if last, ok := lastActive(in.Activity, subject, email); ok {
			at := last.UTC()
			identity.LastActive = &at
			identity.Inactive = window > 0 && in.At.Sub(at) > window
		} else {
			// Never recorded doing anything. Dormant by this measure, with
			// the caveat the type comment states: reads leave no record.
			identity.Inactive = true
		}
		if in.DirectoryConsulted {
			identity.Unknown = !knownAccount(in.Accounts, subject, email)
		}
		identity.Orphaned = identity.Inactive && identity.Unknown
		if identity.Orphaned {
			survey.Orphans++
		}
		survey.Identities = append(survey.Identities, identity)
	}

	if in.Kitchen != nil {
		for _, operator := range in.Kitchen.Spec.Access.Operators {
			add(operator.Subject, operator.Email, PlatformGrant, PlatformOperator.String())
		}
	}
	for i := range in.Projects {
		project := &in.Projects[i]
		for _, grant := range project.Spec.Access {
			add(grant.Subject, grant.Email, project.Name, string(grant.Role))
		}
	}

	sort.Slice(survey.Identities, func(i, j int) bool {
		left, right := survey.Identities[i], survey.Identities[j]
		if left.Grant != right.Grant {
			// The platform's own grants lead: they are the ones that matter
			// most and the ones a reviewer should meet first.
			if left.Grant == PlatformGrant {
				return true
			}
			if right.Grant == PlatformGrant {
				return false
			}
			return left.Grant < right.Grant
		}
		return left.Subject < right.Subject
	})
	return survey
}

// lastActive looks an identity up in the activity map by both spellings.
//
// The audit log records an actor as whatever the caller was named by, which
// is the verified address where a token carries one and the `sub` otherwise —
// so a grant written as a `sub` and activity recorded against the address are
// the same person and must not read as a dormant grant plus a stranger.
// Addresses are compared case-insensitively, as everywhere else here.
func lastActive(activity map[string]time.Time, subject, email string) (time.Time, bool) {
	newest := time.Time{}
	found := false
	for _, candidate := range []string{subject, email} {
		if candidate == "" {
			continue
		}
		for actor, at := range activity {
			if !strings.EqualFold(actor, candidate) {
				continue
			}
			if at.After(newest) {
				newest, found = at, true
			}
		}
	}
	return newest, found
}

// knownAccount reports whether the directory holds an account matching this
// entry, by either spelling.
func knownAccount(accounts map[string]struct{}, subject, email string) bool {
	for _, candidate := range []string{subject, email} {
		if candidate == "" {
			continue
		}
		for account := range accounts {
			if strings.EqualFold(account, candidate) {
				return true
			}
		}
	}
	return false
}

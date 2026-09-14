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

// What a personal key may do, when it may do less than the person it belongs
// to (#595).
//
// A personal key is a copy of its owner: the token it is exchanged for carries
// their `sub`, so every grant they hold resolves from it. Narrowing one is
// therefore not a second access model — it is a **ceiling applied to the first
// one**, and it is applied here, in the package every membership question
// already goes through, so that no caller can ask the question in a way that
// skips it.
//
// Two rules are the whole of it, and both are enforced in this file:
//
//   - **A key is the lesser of itself and its owner.** Never the greater. An
//     entry naming `admin` on a project its owner may only view is a key that
//     may view it; an entry naming projects its owner cannot see is a key that
//     cannot see them either. So an entry somebody wrote carelessly, or edited
//     later without looking at the person, cannot hand out access the person
//     does not have.
//   - **A key the platform does not recognise holds nothing.** Not "falls back
//     to its owner" — that reading would make deleting an entry a promotion,
//     and would make a revoked key that survives at the issuer the most
//     powerful credential on the platform.
//
// **The expiry is not applied here**, and that is a decision rather than an
// omission. A personal key expires at the *identity provider*, which refuses a
// lapsed key on presentation and deletes it — so a lapsed key mints no token
// and never reaches this package at all. What can reach it is a token minted
// in the minutes before the expiry, and that is the bargain every credential
// on this platform already makes: revoking one stops the next exchange, not
// the token somebody is already holding. A platform credential's expiry *is*
// applied in ScopesFor because a platform credential has no issuer-side expiry
// to be refused by; this one does, and a second check without a clock in hand
// would be a second answer rather than a second lock.

// personalKeyOf is the grant for the key a caller presented, and false when
// the caller presented none.
//
// The second return distinguishes the two absences that matter. A caller with
// no key at all is a person, resolved exactly as before this feature existed;
// a caller *with* a key and no grant is a credential the platform does not
// recognise, which holds nothing. Everything below reads both.
func personalKeyOf(
	caller Caller,
	kitchen *kitchenv1alpha1.Kitchen,
) (kitchenv1alpha1.PersonalKeyGrant, bool) {
	if caller.PersonalKey == "" || kitchen == nil {
		return kitchenv1alpha1.PersonalKeyGrant{}, false
	}
	for _, grant := range kitchen.Spec.Access.PersonalKeys {
		if grant.Key != caller.PersonalKey || !SubjectMatches(grant.Subject, caller) {
			continue
		}
		return grant, true
	}
	return kitchenv1alpha1.PersonalKeyGrant{}, false
}

// PersonalKeyGrantFor is the grant behind the key a caller presented, for the
// surfaces that describe a credential to whoever is holding it — `GET /me`,
// `kitchen whoami`, the refusal that explains why a key is refused.
//
// It answers nil for a caller holding no key *and* for one whose key the
// platform does not recognise, which are different states; `HoldsUnknownKey`
// is what tells them apart, because only the second is worth a sentence.
func PersonalKeyGrantFor(
	caller Caller,
	kitchen *kitchenv1alpha1.Kitchen,
) *kitchenv1alpha1.PersonalKeyGrant {
	grant, ok := personalKeyOf(caller, kitchen)
	if !ok {
		return nil
	}
	return &grant
}

// HoldsUnknownKey reports that this caller presented a personal key the
// platform has no grant for — revoked at one end and not the other, lapsed, or
// written by an installation that has since had the entry removed.
//
// It exists so that the refusal can say so. Every role resolves to nothing for
// such a caller, and "you have no role on shop" is a true but unhelpful answer
// to a pipeline whose key was working yesterday.
func HoldsUnknownKey(caller Caller, kitchen *kitchenv1alpha1.Kitchen) bool {
	if caller.PersonalKey == "" {
		return false
	}
	_, ok := personalKeyOf(caller, kitchen)
	return !ok
}

// keyCeiling is how much of its owner's project role a key carries on one
// project: all of it for an unrestricted key, none of it for a key the
// platform does not recognise, and at most its declared role for a narrowed
// one — on the projects it names, and nothing elsewhere.
//
// `project` is empty for a question that is not about one project, where the
// answer is the ceiling itself: "could this key hold developer anywhere" is
// what VisibleProjects and the platform role need.
func keyCeiling(caller Caller, kitchen *kitchenv1alpha1.Kitchen, project string) ProjectRole {
	if caller.PersonalKey == "" {
		return ProjectAdmin
	}
	grant, ok := personalKeyOf(caller, kitchen)
	switch {
	case !ok:
		return ProjectRoleNone
	case grant.Unrestricted:
		return ProjectAdmin
	case project != "" && !grant.Reaches(project):
		return ProjectRoleNone
	}
	role, ok := ParseProjectRole(string(grant.Ceiling()))
	if !ok {
		// An entry naming a role this build has never heard of is an entry
		// nothing can honour. The CRD's enum makes it unwritable through the
		// API server, so reaching this means a downgrade read an object a
		// newer platform wrote — where the safe reading is the one that
		// grants nothing.
		return ProjectRoleNone
	}
	return role
}

// wearsTheHat reports whether a caller may wear the platform's operator hat
// through the key they are holding.
//
// A narrowed key never does, and that is deliberate rather than an omission:
// the operator role is everything, everywhere, so a credential that carries it
// is not narrower than its owner in any sense worth the word. Somebody who
// wants an operator's automation issues an unrestricted key and accepts what
// that means; somebody who wants less issues a narrowed one and gets scopes
// for the platform operations they actually need.
func wearsTheHat(caller Caller, kitchen *kitchenv1alpha1.Kitchen) bool {
	if caller.PersonalKey == "" {
		return true
	}
	grant, ok := personalKeyOf(caller, kitchen)
	return ok && grant.Unrestricted
}

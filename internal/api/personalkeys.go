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
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/idp"
)

// Personal keys (#593): the credential somebody signs their own automation
// with.
//
// It is the one credential on this platform that is *not* narrower than the
// person who holds it. A project key is a machine account with a role on one
// project; a platform credential is scopes on the platform and no role at all.
// A personal key is the person: a token minted from it carries their subject,
// so every grant they hold is resolved from it exactly as when they sign in —
// admin on the projects they administer, the operator hat if they wear one.
//
// The repository's existing answer to "may a credential be a copy of a person"
// is no, said twice and for good reasons (auth/src/keyscope.ts, and `admin` is
// refused to a project key). Nothing about those reasons has been found wrong.
// What was found wrong is the conclusion, and it was found wrong by watching
// what people did instead: the first rollout of a service with a volume cannot
// be finished by any credential the platform will issue, so somebody opens the
// browser's developer tools and copies the dashboard's access token into a
// script. That is the same copy of the same access, with no name on it, in no
// list, expiring when it feels like it and revocable by nothing. **A
// credential the platform issued, named, lists, bounds and can take back is
// strictly better than the one people were already using**, and refusing to
// issue it does not stop it existing — it only stops the platform knowing
// about it.
//
// So it exists, and four things bound it. Each is enforced somewhere here or
// in the two files this leans on:
//
//   - **Only a person can make one.** The route asks for a caller who *signed
//     in* — `requireInteractive` in policy.go, which admits a token an OAuth
//     client of the platform's issued and nothing else. A project key, a
//     platform credential and a personal key alike are exchanged at the issuer
//     for a token that names no client, so none of them can mint one. That is
//     the "no credential mints its own successors" rule, kept: the chain has
//     to start at somebody typing a password.
//   - **It expires.** Thirty days unless the request says otherwise, ninety at
//     the most, enforced at the *issuer* — the api-key plugin refuses a lapsed
//     key on presentation and deletes the row — so a forgotten one stops
//     working with nothing having had to run.
//   - **It is visible and revocable.** Listed by name, prefix, creation, last
//     use and expiry; revoked by name in one call; and both ends of its life
//     are in the audit log, classified as an access write beside the platform
//     credentials.
//   - **It is never read back.** The value exists in the creation response and
//     nowhere else, like every other credential this API issues.
//
// What is deliberately *not* here is a role knob. A personal key that could be
// narrowed would be a second, weaker way of writing the grants the platform
// already has, resolved in a second place — and the honest version of "I want
// a credential that may do less" is a project key or a platform credential,
// both of which exist and are narrower than this by construction.

const (
	// defaultPersonalKeyDays is how long a personal key lasts when the
	// request does not say. It is the platform credential's thirty days, for
	// the same reason: short enough that a forgotten credential is a
	// credential that stops working, long enough that rotating one is not
	// somebody's weekly chore.
	defaultPersonalKeyDays = 30

	// maxPersonalKeyDays is the ceiling, and it is the same ninety days a
	// platform credential gets — deliberately not longer for a credential
	// that carries more. The argument is the one made there: the person
	// issuing it cannot know, at the moment they issue it, which laptop or
	// pipeline it is about to be pasted into.
	maxPersonalKeyDays = 90
)

// personalKeyNamePattern is what a personal key may be called: the same
// DNS-label rule a project, a CI key and a platform credential follow. The
// name is a path segment here and the key's own name at the issuer, where it
// is unique per account — one person's `laptop` and another's are two
// credentials.
var personalKeyNamePattern = keyNamePattern

// personalKeyView is one personal key as its owner reads it. Never the key
// itself — there is no read anywhere that returns one.
type personalKeyView struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix,omitempty"`

	// Created is when it was issued, Expires when it stops being honoured,
	// and Expired whether that has already happened. The last is answered
	// rather than left to the reader to work out, for the reason a platform
	// credential's is: a list has to show a lapsed credential as lapsed.
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Expired bool      `json:"expired,omitempty"`

	// LastUsed is when it was last exchanged for a token, absent for one
	// nothing has used yet — which is a different statement from "used at the
	// zero time", and the one that answers "is this still the credential my
	// pipeline is holding".
	LastUsed *time.Time `json:"lastUsed,omitempty"`

	// Scope is what this key may do (#595): which projects it reaches, at
	// most what role inside them, and which platform operations it was
	// widened to. Absent means the key is its owner, entire.
	//
	// It is answered for a key the platform has a grant for, and the absence
	// of one is reported by Unknown rather than read as "unrestricted": a key
	// the platform does not recognise holds nothing, and a list that showed it
	// as unscoped would show the most alarming state as the most permissive
	// one.
	Scope *personalKeyScopeView `json:"scope,omitempty"`
	// Unknown is a key the identity provider still has and the platform has no
	// grant for. It authenticates and can do nothing; revoking it is the
	// answer.
	Unknown bool `json:"unknown,omitempty"`
}

// personalKeyScopeView is a narrowed key's reach, in the words the request
// that issued it used.
type personalKeyScopeView struct {
	// Projects is the allowlist, empty for a key that may reach every project
	// its owner can.
	Projects []string `json:"projects,omitempty"`
	// Role is the most this key holds inside them — the ceiling, not a grant:
	// what it actually holds on a project is the lesser of this and what its
	// owner holds there.
	Role string `json:"role"`
	// Scopes are the platform operations it was widened to, honoured while
	// its owner is an operator.
	Scopes []string `json:"scopes,omitempty"`
}

// issuedPersonalKeyView is the one response here that carries a credential:
// the key, once, at creation.
type issuedPersonalKeyView struct {
	personalKeyView
	Key string `json:"key"`
}

// createPersonalKeyRequest asks for a key. The name is how it is addressed and
// revoked; the life is optional and bounded.
type createPersonalKeyRequest struct {
	Name string `json:"name"`
	// ExpiresInDays is how long it should last. Absent takes
	// defaultPersonalKeyDays; anything over maxPersonalKeyDays is refused
	// rather than quietly clamped, because a caller that asked for a year and
	// was given ninety days would find out when the pipeline broke.
	ExpiresInDays int `json:"expiresInDays,omitempty"`

	// Role is what makes a key *fine-grained* (#595): the most it may hold on
	// the projects it reaches. Naming one is what narrows the key; leaving it
	// out asks for a key that is its owner, entire.
	//
	// It is a role rather than a grid of permissions because a role is what
	// this API enforces. A key that could be given "may deploy but not read
	// logs" would be a second vocabulary for saying what a developer is, and
	// the two would drift the first time a route was added — the same
	// argument that keeps `GET /projects` answering a role rather than a set
	// of capability booleans.
	Role string `json:"role,omitempty"`

	// Projects is the allowlist. Empty, with a role named, is every project
	// its owner can reach — which is still narrower than its owner, because
	// the role caps it and because a narrowed key never wears the operator
	// hat.
	Projects []string `json:"projects,omitempty"`

	// Scopes are platform operations to widen the key to, from the same
	// vocabulary a platform credential holds. Only an operator may ask for
	// any: a scope this platform grants is an operator's to hand out, and a
	// key cannot carry what its owner could not.
	Scopes []string `json:"scopes,omitempty"`
}

func newPersonalKeyView(
	key idp.PersonalKey,
	grant *kitchenv1alpha1.PersonalKeyGrant,
	now time.Time,
) personalKeyView {
	view := personalKeyView{
		Name:     key.Name,
		Prefix:   key.Prefix,
		Created:  key.Created,
		Expires:  key.Expires,
		Expired:  key.Expired(now),
		LastUsed: key.LastUsed,
	}
	switch {
	case grant == nil:
		view.Unknown = true
	case grant.Narrowed():
		view.Scope = &personalKeyScopeView{
			Projects: grant.Projects,
			Role:     string(grant.Ceiling()),
			Scopes:   scopeNamesOf(grant.Scopes),
		}
	}
	return view
}

// scopeNamesOf is a grant's platform scopes in the wire form a request writes
// them in, and nil for the overwhelming majority of keys, which hold none.
func scopeNamesOf(scopes []kitchenv1alpha1.PlatformScope) []string {
	if len(scopes) == 0 {
		return nil
	}
	names := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		names = append(names, string(scope))
	}
	return names
}

// personalKeyGrantOf is the platform's grant for one of an account's keys, and
// nil when it has none — which is a key that holds nothing, not a key that
// holds everything.
func personalKeyGrantOf(
	kitchen *kitchenv1alpha1.Kitchen,
	subject, name string,
) *kitchenv1alpha1.PersonalKeyGrant {
	if kitchen == nil {
		return nil
	}
	for i := range kitchen.Spec.Access.PersonalKeys {
		grant := &kitchen.Spec.Access.PersonalKeys[i]
		if grant.Key == name && grant.Subject == subject {
			return grant
		}
	}
	return nil
}

func (s *Server) listPersonalKeys(w http.ResponseWriter, req *http.Request) {
	caller, _ := CallerFrom(req.Context())
	keys, _, ok := s.personalKeysOf(w, req)
	if !ok {
		return
	}
	// The narrowing lives on the singleton, so the list is a join: the
	// identity provider says which credentials exist, and the platform says
	// what each may do. A key on one side and not the other is reported as
	// such rather than quietly dropped — see personalKeyView.Unknown.
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	now := s.now()
	views := make([]personalKeyView, 0, len(keys))
	for _, key := range keys {
		views = append(views, newPersonalKeyView(key, personalKeyGrantOf(kitchen, caller.Subject, key.Name), now))
	}
	writeList(w, views)
}

func (s *Server) createPersonalKey(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	caller, _ := CallerFrom(ctx)

	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := createPersonalKeyRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if !personalKeyNamePattern.MatchString(name) {
		badRequest(w, "name must be lowercase letters, digits and dashes, starting and ending with a "+
			"letter or digit, at most 32 characters (got %q): it is how the key is addressed and revoked, "+
			"so name it after where it is going — laptop, nightly, the pipeline it lives in", name)
		return
	}
	expires, ok := personalKeyExpiry(w, body.ExpiresInDays, s.now())
	if !ok {
		return
	}
	grant, ok := s.personalKeyGrantFrom(req.Context(), w, kitchen, caller, body, name, expires)
	if !ok {
		return
	}

	// The name is checked against the issuer's own list before anything is
	// recorded, for keys.go's reason: a name already taken is an outcome this
	// call has every day, and a tamper-evident log saying a key was issued
	// for a request that was answered 409 is a record of something that did
	// not happen.
	existing, directory, ok := s.personalKeysOf(w, req)
	if !ok {
		return
	}
	for _, key := range existing {
		if key.Name == name {
			writeJSON(w, http.StatusConflict, errorBody{Error: personalKeyNameTaken(name)})
			return
		}
	}

	if !s.recorded(w, req, audit.Transition{
		Object:     kitchen,
		Kind:       audit.KindPersonalKey,
		Operation:  clickhouse.AuditCreate,
		Privileged: audit.PrivilegeAccess,
		To:         name,
		Reason: fmt.Sprintf("%s issued the personal key %s, expiring %s",
			callerName(caller), name, expires.UTC().Format(time.RFC3339)),
		Details: map[string]any{
			"key": name,
			// Who the credential is. It is the whole of what makes this kind
			// of record worth keeping: a personal key is indistinguishable
			// from its owner at every later point, so the moment it was
			// issued is the only place the log can tie the two together.
			"subject": caller.Subject,
			"email":   caller.Email,
			"expires": expires.UTC().Format(time.RFC3339),
			"change":  "personal-key-issued",
			// And what it was issued *for*. A record that said only that a
			// key exists would leave the reviewer's question — how much of
			// this person is this credential — answerable only by reading the
			// singleton as it stands today.
			"unrestricted": grant.Unrestricted,
			"role":         string(grant.Role),
			"projects":     grant.Projects,
			"scopes":       scopeNamesOf(grant.Scopes),
		},
	}) {
		return
	}

	issued, err := directory.CreatePersonalKey(ctx, caller.Subject, name, expires)
	switch {
	case errors.Is(err, idp.ErrKeyExists):
		// Checked above, so reaching this is two requests for the same name
		// at once rather than the everyday case.
		writeJSON(w, http.StatusConflict, errorBody{Error: personalKeyNameTaken(name)})
		return
	case errors.Is(err, idp.ErrNoPersonalKeyDirectory):
		s.noPersonalKeyDirectory(w, err)
		return
	case errors.Is(err, idp.ErrNotAPerson):
		// The issuer's half of the rule this route's requirement enforces:
		// the account behind the token is a credential's. Reaching it means
		// something authenticated as a credential and still got here, so it
		// is refused in the words the rule is written in rather than as a
		// fault.
		badRequest(w, "a personal key belongs to somebody who signs in, and this token's account is a "+
			"credential's: a credential that could issue one would be minting a copy of a person. Sign in "+
			"and issue it there")
		return
	case err != nil:
		s.log().Error(err, "the identity provider would not issue a personal key", "key", name)
		writeJSON(w, http.StatusBadGateway, errorBody{Error: "the identity provider could not issue a " +
			"personal key; the operator's log has why"})
		return
	}

	// Both halves, or neither — the platform credential's rule, and here the
	// half that would be left behind is the dangerous one: a key at the
	// issuer that the platform has no grant for holds nothing, so a caller
	// would be handed a credential that authenticates and can do nothing, and
	// would find out from the first 404 rather than from this response.
	grant.Subject, grant.Email = issued.Subject, issued.Email
	patch := settingsAccessPatch(kitchen)
	kitchen.Spec.Access.PersonalKeys = append(kitchen.Spec.Access.PersonalKeys, grant)
	if err := s.Client.Patch(ctx, kitchen, patch); err != nil {
		s.revokePersonalKey(ctx, w, directory, caller.Subject, name, err)
		return
	}

	s.log().Info("personal key issued through the api",
		"key", name, "subject", issued.Subject, "expires", expires.UTC().Format(time.RFC3339),
		"unrestricted", grant.Unrestricted, "role", string(grant.Role), "projects", grant.Projects,
		"caller", callerName(caller))
	writeJSON(w, http.StatusCreated, issuedPersonalKeyView{
		personalKeyView: newPersonalKeyView(issued.PersonalKey, &grant, s.now()),
		Key:             issued.Secret,
	})
}

// revokePersonalKey takes a key back after the grant it was issued for could
// not be written, and answers the request either way. It is keys.go's `revoke`
// one credential over, and the two outcomes are worth different words for the
// same reason: one is a request that changed nothing, the other is a
// credential somebody has to go and remove by hand.
func (s *Server) revokePersonalKey(
	ctx context.Context,
	w http.ResponseWriter,
	directory accountDirectory,
	subject, name string,
	cause error,
) {
	if _, err := directory.DeletePersonalKey(ctx, subject, name); err != nil {
		s.log().Error(err, "a personal key was issued that nothing granted anything to, and could not be "+
			"taken back", "key", name, "cause", cause.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: fmt.Sprintf(
			"the personal key %s was created at the identity provider but the platform could not record "+
				"what it may do, and the key could not be taken back either: it authenticates and can do "+
				"nothing. Revoke it and try again", name)})
		return
	}
	s.log().Info("took back a personal key whose grant could not be written",
		"key", name, "cause", cause.Error())
	s.writeError(w, cause)
}

// personalKeyGrantFrom is what the request asked the key to be allowed to do,
// checked against what the caller may actually hand out.
//
// Three refusals, and each is a rule that would otherwise only be enforced at
// resolution — where it would be enforced silently, leaving somebody holding a
// credential that does less than the screen said it would:
//
//   - a project the caller cannot see is not a project they may scope a key
//     to, and is answered as not found for the reason every other unreadable
//     project is;
//   - a role above what the caller holds on a named project is refused rather
//     than clamped, because a key issued as `admin` that silently resolves to
//     `developer` is a key whose first failure is somebody else's outage;
//   - a platform scope asked for by anybody but an operator is refused
//     outright: scopes are what an operator hands to a machine.
func (s *Server) personalKeyGrantFrom(
	ctx context.Context,
	w http.ResponseWriter,
	kitchen *kitchenv1alpha1.Kitchen,
	caller Caller,
	body createPersonalKeyRequest,
	name string,
	expires time.Time,
) (kitchenv1alpha1.PersonalKeyGrant, bool) {
	grant := kitchenv1alpha1.PersonalKeyGrant{
		Key:      name,
		Expires:  metav1.NewTime(expires),
		IssuedAt: metav1.NewTime(s.now()),
	}

	role := strings.TrimSpace(body.Role)
	projects := trimmed(body.Projects)
	scopes := trimmed(body.Scopes)
	if role == "" {
		// Nothing narrows it: the key is its owner, entire. A request that
		// named projects or scopes without a role is a request that meant to
		// narrow and did not say how, which is refused rather than granted
		// everything.
		if len(projects) > 0 || len(scopes) > 0 {
			badRequest(w, "role is required to narrow a personal key: name the most this key may hold — "+
				"viewer, developer or admin — on the projects it reaches. A key with no role is the "+
				"whole of what you can do, which is what leaving projects and scopes out asks for")
			return grant, false
		}
		grant.Unrestricted = true
		return grant, true
	}

	parsed, ok := access.ParseProjectRole(role)
	if !ok {
		badRequest(w, "role must be viewer, developer or admin (got %q)", body.Role)
		return grant, false
	}
	grant.Role = kitchenv1alpha1.AccessRole(parsed.String())

	for _, name := range projects {
		project := &kitchenv1alpha1.Project{}
		if err := s.get(ctx, name, project); err != nil {
			s.writeError(w, err)
			return grant, false
		}
		held := access.ProjectRoleFor(caller.access(), kitchen, project)
		if !held.AtLeast(access.ProjectViewer) {
			// The caller cannot see it, so they are told what every other
			// caller who cannot see a project is told.
			s.writeError(w, apierrors.NewNotFound(
				schema.GroupResource{Group: kitchenv1alpha1.GroupVersion.Group, Resource: "projects"}, name))
			return grant, false
		}
		if !held.AtLeast(parsed) {
			badRequest(w, "you hold %s on %s, so a key of yours cannot hold %s there: a personal key is "+
				"the lesser of itself and the person it belongs to, and one issued above that would "+
				"quietly do less than it says", held, name, parsed)
			return grant, false
		}
	}
	grant.Projects = projects

	if len(scopes) > 0 {
		if !platformRoleFrom(ctx).AtLeast(access.PlatformOperator) {
			forbidden(w, "a platform scope on a personal key needs the operator role: scopes reach the "+
				"platform's own surface, and a key cannot carry what its owner could not")
			return grant, false
		}
		held, ok := parseScopes(w, scopes)
		if !ok {
			return grant, false
		}
		grant.Scopes = held
	}
	return grant, true
}

// trimmed is a request's list with the blanks taken out, which is what
// distinguishes "named nothing" from "named an empty string".
func trimmed(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// personalKeyNameTaken is the refusal a name already in use gets, from either
// the check that foresees it or the race that beats it.
func personalKeyNameTaken(name string) string {
	return fmt.Sprintf(
		"you already have a personal key called %s: revoke it and make a new one rather than reusing the "+
			"name, so that revoking either is unambiguous", name)
}

func (s *Server) deletePersonalKey(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	caller, _ := CallerFrom(ctx)

	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	name := strings.TrimSpace(req.PathValue("key"))

	// Read it first, so that a name the caller has no key for is the plain
	// not-found it ought to be rather than a record of a revocation that
	// revoked nothing.
	keys, directory, ok := s.personalKeysOf(w, req)
	if !ok {
		return
	}
	var found *idp.PersonalKey
	for i := range keys {
		if keys[i].Name == name {
			found = &keys[i]
			break
		}
	}
	// A grant whose key is already gone at the issuer is still this route's to
	// remove: it is the half that stays behind when a revocation was
	// interrupted, and leaving it would mean the only way to tidy it is
	// kubectl. It is the platform credential's reasoning, and the same shape.
	at := indexOfPersonalKey(kitchen, caller.Subject, name)
	if found == nil && at < 0 {
		s.writeError(w, apierrors.NewNotFound(
			schema.GroupResource{Group: kitchenv1alpha1.GroupVersion.Group, Resource: "personalkeys"}, name))
		return
	}

	patch := settingsAccessPatch(kitchen)
	if !s.recorded(w, req, audit.Transition{
		Object:     kitchen,
		Kind:       audit.KindPersonalKey,
		Operation:  clickhouse.AuditDelete,
		Privileged: audit.PrivilegeAccess,
		From:       name,
		Reason:     fmt.Sprintf("%s revoked the personal key %s", callerName(caller), name),
		Details: map[string]any{
			"key":     name,
			"subject": caller.Subject,
			"email":   caller.Email,
			"change":  "personal-key-revoked",
		},
	}) {
		return
	}

	if found == nil {
		// Only the grant is left. Nothing to revoke at the issuer, so the
		// write below is the whole of the revocation.
		if at >= 0 {
			kitchen.Spec.Access.PersonalKeys = append(
				kitchen.Spec.Access.PersonalKeys[:at], kitchen.Spec.Access.PersonalKeys[at+1:]...)
			if err := s.Client.Patch(ctx, kitchen, patch); err != nil {
				s.writeError(w, err)
				return
			}
		}
		s.log().Info("removed the grant of a personal key that was already gone",
			"key", name, "subject", caller.Subject)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch _, err := directory.DeletePersonalKey(ctx, caller.Subject, name); {
	case errors.Is(err, idp.ErrKeyNotFound):
		// Somebody revoked it between the read above and here — two browser
		// tabs, or the key being used to revoke itself twice. The end state
		// is the one that was asked for.
		s.log().Info("a personal key was already gone when it was revoked", "key", name)
	case errors.Is(err, idp.ErrNoPersonalKeyDirectory):
		s.noPersonalKeyDirectory(w, err)
		return
	case err != nil:
		s.log().Error(err, "the identity provider would not revoke a personal key", "key", name)
		writeJSON(w, http.StatusBadGateway, errorBody{Error: fmt.Sprintf(
			"the identity provider could not revoke the personal key %s; the operator's log has why", name)})
		return
	}

	// The credential went first, for keys.go's reason: of the two ways this
	// can end up half done, a grant naming a key that no longer exists is a
	// line to tidy up, and a key that still works is not.
	if at >= 0 {
		kitchen.Spec.Access.PersonalKeys = append(
			kitchen.Spec.Access.PersonalKeys[:at], kitchen.Spec.Access.PersonalKeys[at+1:]...)
		if err := s.Client.Patch(ctx, kitchen, patch); err != nil {
			s.log().Error(err, "a revoked personal key's grant is still on the platform",
				"key", name, "subject", caller.Subject)
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: fmt.Sprintf(
				"the personal key %s was revoked and no longer works, but what it was allowed to do is "+
					"still recorded on the platform: revoke it again to finish the job", name)})
			return
		}
	}

	s.log().Info("personal key revoked through the api",
		"key", name, "subject", caller.Subject, "caller", callerName(caller))
	w.WriteHeader(http.StatusNoContent)
}

// indexOfPersonalKey is where one account's named key sits in the platform's
// list of what its personal keys may do, or -1.
func indexOfPersonalKey(kitchen *kitchenv1alpha1.Kitchen, subject, name string) int {
	if kitchen == nil {
		return -1
	}
	for i, grant := range kitchen.Spec.Access.PersonalKeys {
		if grant.Key == name && grant.Subject == subject {
			return i
		}
	}
	return -1
}

// personalKeysOf reads the calling account's own keys at the identity
// provider, answering the request itself when it cannot. The directory comes
// back with them so that a handler which then writes uses the connection it
// read through.
//
// There is no subject parameter and there is not going to be one: these
// routes are about the caller and nobody else, which is what lets them ask for
// nothing but a valid token. Reading somebody else's credentials is the access
// survey's question, and it answers it without ever naming a key.
func (s *Server) personalKeysOf(
	w http.ResponseWriter,
	req *http.Request,
) ([]idp.PersonalKey, accountDirectory, bool) {
	ctx := req.Context()
	caller, _ := CallerFrom(ctx)

	directory, err := s.directory(ctx)
	if err != nil {
		s.noDirectory(w, err)
		return nil, nil, false
	}
	keys, err := directory.PersonalKeys(ctx, caller.Subject)
	switch {
	case errors.Is(err, idp.ErrNoPersonalKeyDirectory):
		s.noPersonalKeyDirectory(w, err)
		return nil, nil, false
	case errors.Is(err, idp.ErrNotAPerson):
		// A credential asking for its own personal keys holds none and never
		// will. It is an empty list rather than a refusal: the question was
		// answerable, and `kitchen keys list` on a CI key should say "none"
		// rather than fail.
		return nil, directory, true
	case err != nil:
		s.log().Error(err, "cannot list an account's personal keys at the identity provider")
		writeJSON(w, http.StatusBadGateway, errorBody{Error: "the identity provider could not be asked " +
			"about your personal keys; the operator's log has why"})
		return nil, nil, false
	}
	return keys, directory, true
}

// noPersonalKeyDirectory answers a request that needed the issuer's
// personal-key endpoints on an installation whose issuer has none. 503, for
// noKeyDirectory's reason: the request was well formed, and what the platform
// cannot do is ask the issuer.
func (s *Server) noPersonalKeyDirectory(w http.ResponseWriter, err error) {
	if !errors.Is(err, idp.ErrNoPersonalKeyDirectory) {
		s.log().Error(err, "cannot reach the identity provider's personal-key endpoints")
	}
	writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "this installation's identity provider " +
		"issues no personal keys, so one cannot be created or revoked here: it is federated to an issuer " +
		"of its own, where credentials are that issuer's to hand out"})
}

// personalKeyExpiry turns the requested life into a date, defaulting and
// bounding it, and answers the request itself when it will not.
func personalKeyExpiry(w http.ResponseWriter, days int, now time.Time) (time.Time, bool) {
	switch {
	case days == 0:
		days = defaultPersonalKeyDays
	case days < 0:
		badRequest(w, "expiresInDays must be a positive number of days (got %d)", days)
		return time.Time{}, false
	case days > maxPersonalKeyDays:
		badRequest(w, "expiresInDays may be at most %d (got %d): a personal key carries every role you "+
			"hold, and one that has to outlive a quarter is one to reissue — which is an action somebody "+
			"takes and the log records", maxPersonalKeyDays, days)
		return time.Time{}, false
	}
	return now.Add(time.Duration(days) * 24 * time.Hour), true
}

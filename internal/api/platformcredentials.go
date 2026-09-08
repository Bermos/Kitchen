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
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/idp"
)

// Platform credentials (issue #349): what a scheduled job or an agent holds to
// reach the platform's own surface without being made an operator.
//
// It is the project-key shape one level up, and deliberately so — a credential
// at the issuer, an account created to own it because a key has no subject of
// its own, and a grant on the object the access is about. What differs is the
// object and what the grant says: a CI key's is a *role* on one Project, and
// this is a set of *scopes* on the Kitchen singleton. That difference is the
// whole of the feature, because it is what keeps a widened credential from
// being a laptop-shaped copy of the operator role, which is the objection
// docs/API.md has always recorded against handing a CLI a broad token.
//
// Four properties are what make it worth having, and every one of them is
// enforced somewhere in this file or in the two it leans on:
//
//   - **It is narrower than the operator role, and cannot be widened into it.**
//     The scopes reach exactly the rows internal/api/policy.go marks with one,
//     and none of those rows is a write to the operator list, to a connection,
//     to the platform's version, or to a credential — including this one.
//   - **It expires, and the expiry is enforced where the scope is resolved**
//     (access.ScopesFor), so a leaked credential lapses on its own without
//     anything having had to run.
//   - **Reading and writing are separable**: a job that reads retention is not
//     a job that can take a backup, because those are two scopes.
//   - **It is visible.** The grant is on the singleton, so it appears in the
//     access survey and in a recertification cycle like every other identity,
//     with its last-active date beside it.
//
// The pair — the credential at the issuer, and the grant that makes it worth
// holding — is what these handlers keep, exactly as the project key handlers
// keep theirs: creating writes both and takes the credential back if the grant
// will not land, and deleting removes both.

const (
	// defaultCredentialDays is how long a credential lasts when the request
	// does not say. Thirty days is short enough that a forgotten credential is
	// a credential that stops working, and long enough that rotating one is
	// not somebody's weekly chore.
	defaultCredentialDays = 30

	// nothingHeld is how a refusal and an audit record spell "this credential
	// holds no scope at all" — a credential whose grant was removed, or one
	// narrowed to nothing a scope recognises. It is a constant because two
	// files say it and they must say the same word.
	nothingHeld = "none"

	// maxCredentialDays is the ceiling, and it is a ceiling rather than a
	// default because the argument for it is different: the reason to bound
	// the *longest* life is that the operator issuing one cannot know, at the
	// moment they issue it, which pipeline or laptop it is about to be pasted
	// into. A credential that has to outlive a quarter is one to reissue, which
	// is an action somebody takes and the log records.
	maxCredentialDays = 90
)

// credentialNamePattern is what a platform credential may be called: the same
// DNS-label rule a project and a CI key follow, because the name is the local
// part of the account's address at the issuer and a path segment here.
var credentialNamePattern = keyNamePattern

// credentialView is one platform credential as the dashboard reads it. Never
// the credential itself — there is no read anywhere that returns one.
type credentialView struct {
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`

	// Scopes is what the singleton grants this credential, read from
	// `spec.access.credentials` rather than from anything stored on the key.
	// It is empty for a credential the platform has no grant for, which is a
	// credential that can do nothing — the state this feature exists to make
	// impossible, reported rather than hidden.
	Scopes []string `json:"scopes"`
	// Projects is the allowlist the scoped project-shaped routes are narrowed
	// to, and empty for a credential that was not narrowed.
	Projects []string `json:"projects,omitempty"`

	// Expires is when it stops being honoured, and Expired whether that has
	// already happened. Both are answered because a list has to show a lapsed
	// credential as lapsed rather than leaving every reader to compare dates.
	Expires *time.Time `json:"expires,omitempty"`
	Expired bool       `json:"expired"`

	Prefix   string     `json:"prefix,omitempty"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"lastUsed,omitempty"`
}

// issuedCredentialView is the one response here that carries a credential: the
// value, once, at creation.
type issuedCredentialView struct {
	credentialView
	Key string `json:"key"`
}

// createCredentialRequest asks for a credential.
//
// Scopes is the only required field beyond the name, and it is required rather
// than defaulted: there is no scope that is obviously the one somebody meant,
// and defaulting to the narrowest would issue a credential that silently does
// not do what it was asked for while defaulting to all of them would be the
// opposite mistake, once.
type createCredentialRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// Projects narrows the project-shaped scoped routes. Absent or empty is
	// every project — see PlatformCredential.Projects.
	Projects []string `json:"projects,omitempty"`
	// ExpiresInDays is how long it lasts, defaulting to defaultCredentialDays
	// and capped at maxCredentialDays.
	ExpiresInDays int `json:"expiresInDays,omitempty"`
}

func (s *Server) listPlatformCredentials(w http.ResponseWriter, req *http.Request) {
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	credentials, _, ok := s.credentialsOf(w, req)
	if !ok {
		return
	}

	now := s.now()
	views := make([]credentialView, 0, len(credentials))
	for _, credential := range credentials {
		views = append(views, newCredentialView(credential, grantFor(kitchen, credential.Subject), now))
	}
	writeList(w, views)
}

func (s *Server) createPlatformCredential(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := createCredentialRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if !credentialNamePattern.MatchString(name) {
		badRequest(w, "name must be lowercase letters, digits and dashes, starting and ending with a "+
			"letter or digit, at most 32 characters (got %q): it is how the credential is addressed", name)
		return
	}
	scopes, ok := parseScopes(w, body.Scopes)
	if !ok {
		return
	}
	expires, ok := credentialExpiry(w, body.ExpiresInDays, s.now())
	if !ok {
		return
	}
	projects, ok := s.parseCredentialProjects(ctx, w, body.Projects)
	if !ok {
		return
	}

	// The name is checked against the issuer's own list before anything is
	// recorded, for keys.go's reason: a name already taken is an outcome this
	// call has every day, and a tamper-evident log saying a credential was
	// issued for a request answered 409 is a record of something that did not
	// happen.
	existing, directory, ok := s.credentialsOf(w, req)
	if !ok {
		return
	}
	for _, credential := range existing {
		if credential.Name == name {
			writeJSON(w, http.StatusConflict, errorBody{Error: credentialNameTaken(name)})
			return
		}
	}

	patch := settingsAccessPatch(kitchen)
	if !s.recorded(w, req, audit.Transition{
		Object:     kitchen,
		Kind:       audit.KindPlatformCredential,
		Operation:  clickhouse.AuditCreate,
		Privileged: audit.PrivilegeAccess,
		To:         scopeNames(scopes),
		Reason: fmt.Sprintf("the platform credential %s was issued, scoped %s, expiring %s",
			name, scopeNames(scopes), expires.UTC().Format(time.RFC3339)),
		Details: map[string]any{
			"credential": name,
			"scopes":     scopeStrings(scopes),
			"projects":   projects,
			"expires":    expires.UTC().Format(time.RFC3339),
			"change":     "credential-issued",
		},
	}) {
		return
	}

	issued, err := directory.CreatePlatformKey(ctx, name)
	switch {
	case errors.Is(err, idp.ErrKeyExists):
		// Checked above, so reaching this is two requests for the same name at
		// once rather than the everyday case.
		writeJSON(w, http.StatusConflict, errorBody{Error: credentialNameTaken(name)})
		return
	case errors.Is(err, idp.ErrNoPlatformKeyDirectory):
		s.noCredentialDirectory(w, err)
		return
	case err != nil:
		s.log().Error(err, "the identity provider would not issue a platform credential", "credential", name)
		writeJSON(w, http.StatusBadGateway, errorBody{Error: "the identity provider could not issue a " +
			"platform credential; the operator's log has why"})
		return
	}

	// Both halves, or neither — keys.go's rule, and here the half that would
	// be left behind is worse: a credential at the issuer that the singleton
	// grants nothing is a credential nothing in Kitchen lists.
	grant := kitchenv1alpha1.PlatformCredential{
		AccessSubject: kitchenv1alpha1.AccessSubject{Subject: issued.Subject, Email: issued.Email},
		Scopes:        scopes,
		Expires:       metav1.NewTime(expires),
		Projects:      projects,
	}
	kitchen.Spec.Access.Credentials = append(kitchen.Spec.Access.Credentials, grant)
	if err := s.Client.Patch(ctx, kitchen, patch); err != nil {
		s.revokeCredential(ctx, w, directory, name, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("platform credential issued through the api",
		"credential", name, "subject", issued.Subject, "scopes", scopeNames(scopes),
		"expires", expires.UTC().Format(time.RFC3339), "caller", callerName(caller))
	writeJSON(w, http.StatusCreated, issuedCredentialView{
		credentialView: newCredentialView(issued.PlatformKey, &grant, s.now()),
		Key:            issued.Secret,
	})
}

func (s *Server) deletePlatformCredential(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	name := strings.TrimSpace(req.PathValue("name"))

	credentials, directory, ok := s.credentialsOf(w, req)
	if !ok {
		return
	}
	var found *idp.PlatformKey
	for i := range credentials {
		if credentials[i].Name == name {
			found = &credentials[i]
			break
		}
	}
	// A grant whose credential is already gone at the issuer is still this
	// route's to remove: it is the half that stays behind when a revocation
	// was interrupted, and leaving it would mean the only way to tidy it is
	// kubectl. It is matched by the address the platform wrote, which is the
	// one thing about a credential that is derived from its name.
	at := indexOfCredential(kitchen, found, idp.PlatformAddress(name))
	if found == nil && at < 0 {
		s.writeError(w, apierrors.NewNotFound(
			schema.GroupResource{Group: kitchenv1alpha1.GroupVersion.Group, Resource: "platformcredentials"}, name))
		return
	}

	scopes := []kitchenv1alpha1.PlatformScope{}
	if at >= 0 {
		scopes = kitchen.Spec.Access.Credentials[at].Scopes
	}
	patch := settingsAccessPatch(kitchen)
	if !s.recorded(w, req, audit.Transition{
		Object:     kitchen,
		Kind:       audit.KindPlatformCredential,
		Operation:  clickhouse.AuditDelete,
		Privileged: audit.PrivilegeAccess,
		From:       scopeNames(scopes),
		Reason:     fmt.Sprintf("the platform credential %s was revoked", name),
		Details: map[string]any{
			"credential": name,
			"scopes":     scopeStrings(scopes),
			"change":     "credential-revoked",
		},
	}) {
		return
	}

	// The credential goes first. Of the two ways this can end up half done, a
	// grant naming an account that no longer exists is a line to tidy up and a
	// credential that still works is not.
	if found != nil {
		switch _, err := directory.DeletePlatformKey(ctx, name); {
		case errors.Is(err, idp.ErrKeyNotFound):
			s.log().Info("a platform credential was already gone when it was revoked", "credential", name)
		case errors.Is(err, idp.ErrNoPlatformKeyDirectory):
			s.noCredentialDirectory(w, err)
			return
		case err != nil:
			s.log().Error(err, "the identity provider would not revoke a platform credential", "credential", name)
			writeJSON(w, http.StatusBadGateway, errorBody{Error: fmt.Sprintf(
				"the identity provider could not revoke the platform credential %s; "+
					"the operator's log has why", name)})
			return
		}
	}

	if at >= 0 {
		kitchen.Spec.Access.Credentials = append(
			kitchen.Spec.Access.Credentials[:at], kitchen.Spec.Access.Credentials[at+1:]...)
		if err := s.Client.Patch(ctx, kitchen, patch); err != nil {
			s.log().Error(err, "a revoked platform credential's grant is still on the platform",
				"credential", name)
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: fmt.Sprintf(
				"the credential %s was revoked and no longer works, but its grant is still on the "+
					"platform: remove it from spec.access.credentials to finish the job", name)})
			return
		}
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("platform credential revoked through the api",
		"credential", name, "caller", callerName(caller))
	w.WriteHeader(http.StatusNoContent)
}

// credentialsOf reads the platform's credentials at the identity provider,
// answering the request itself when it cannot. The directory comes back with
// them so that a handler which then writes uses the connection it read through.
func (s *Server) credentialsOf(
	w http.ResponseWriter,
	req *http.Request,
) ([]idp.PlatformKey, accountDirectory, bool) {
	ctx := req.Context()
	directory, err := s.directory(ctx)
	if err != nil {
		s.noDirectory(w, err)
		return nil, nil, false
	}
	credentials, err := directory.PlatformKeys(ctx)
	switch {
	case errors.Is(err, idp.ErrNoPlatformKeyDirectory):
		s.noCredentialDirectory(w, err)
		return nil, nil, false
	case err != nil:
		s.log().Error(err, "cannot list the platform's credentials at the identity provider")
		writeJSON(w, http.StatusBadGateway, errorBody{Error: "the identity provider could not be asked " +
			"about the platform's credentials; the operator's log has why"})
		return nil, nil, false
	}
	return credentials, directory, true
}

// revokeCredential takes a credential back after the grant it was issued for
// could not be written, and answers the request either way.
func (s *Server) revokeCredential(
	ctx context.Context,
	w http.ResponseWriter,
	directory accountDirectory,
	name string,
	cause error,
) {
	if _, err := directory.DeletePlatformKey(ctx, name); err != nil {
		s.log().Error(err, "a platform credential was issued that nothing granted anything to, "+
			"and could not be taken back", "credential", name, "cause", cause.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: fmt.Sprintf(
			"the credential %s was created at the identity provider but the platform could not be given "+
				"the grant that makes it useful, and the credential could not be taken back either: it "+
				"authenticates and can do nothing. Delete it and try again", name)})
		return
	}
	s.log().Info("took back a platform credential whose grant could not be written",
		"credential", name, "cause", cause.Error())
	s.writeError(w, cause)
}

// noCredentialDirectory answers a request that needed the issuer's platform
// credential endpoints on an installation whose issuer has none. 503, for
// noDirectory's reason: the request was well formed, and what the platform
// cannot do is ask the issuer.
func (s *Server) noCredentialDirectory(w http.ResponseWriter, err error) {
	if !errors.Is(err, idp.ErrNoPlatformKeyDirectory) {
		s.log().Error(err, "cannot reach the identity provider's platform credential endpoints")
	}
	writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "this installation's identity provider " +
		"issues no platform credentials, so one cannot be created or revoked here: it is federated to an " +
		"issuer of its own, where credentials are that issuer's to hand out"})
}

// credentialNameTaken is the refusal a name already in use gets, from either
// the check that foresees it or the race that beats it.
func credentialNameTaken(name string) string {
	return fmt.Sprintf(
		"the platform already has a credential called %s: revoke it and issue a new one rather than "+
			"reusing the name, so that revoking either is unambiguous", name)
}

// parseScopes reads the scopes a request asks for, answering the request
// itself when they are not scopes or when there are none.
//
// An unknown scope is refused rather than dropped. Dropping it would issue a
// credential narrower than the one that was asked for and say nothing, which
// is the failure a caller only finds at three in the morning when the job that
// holds it starts getting 403s.
func parseScopes(w http.ResponseWriter, raw []string) ([]kitchenv1alpha1.PlatformScope, bool) {
	seen := map[access.Scope]struct{}{}
	scopes := []kitchenv1alpha1.PlatformScope{}
	for _, value := range raw {
		scope, ok := access.ParseScope(value)
		if !ok {
			badRequest(w, "%q is not a platform scope; the scopes are %s",
				strings.TrimSpace(value), knownScopes())
			return nil, false
		}
		if _, already := seen[scope]; already {
			continue
		}
		seen[scope] = struct{}{}
		scopes = append(scopes, kitchenv1alpha1.PlatformScope(scope))
	}
	if len(scopes) == 0 {
		badRequest(w, "a credential needs at least one scope, or it authenticates and can do nothing; "+
			"the scopes are %s", knownScopes())
		return nil, false
	}
	return scopes, true
}

// knownScopes is the vocabulary a refusal offers, so that a caller who
// mistyped one is told what the words are rather than sent to the docs.
func knownScopes() string {
	names := make([]string, 0, len(access.Scopes()))
	for _, scope := range access.Scopes() {
		names = append(names, scope.String())
	}
	return strings.Join(names, ", ")
}

// credentialExpiry turns the requested lifetime into an instant, applying the
// default and the ceiling. Both refusals name the number that was asked for,
// because a client that computed one is a client whose arithmetic is wrong
// rather than one that typed something silly.
func credentialExpiry(w http.ResponseWriter, days int, now time.Time) (time.Time, bool) {
	switch {
	case days == 0:
		days = defaultCredentialDays
	case days < 0:
		badRequest(w, "expiresInDays must be positive (got %d)", days)
		return time.Time{}, false
	case days > maxCredentialDays:
		badRequest(w, "expiresInDays is at most %d (got %d): a credential that has to outlive a quarter "+
			"is one to reissue, which is an action somebody takes and the log records",
			maxCredentialDays, days)
		return time.Time{}, false
	}
	return now.Add(time.Duration(days) * 24 * time.Hour).UTC(), true
}

// parseCredentialProjects checks the allowlist a request narrows a credential
// to, answering the request itself when a name is not a project.
//
// A project that does not exist is refused rather than kept. The whole point
// of the list is that it narrows, so a typo in it narrows to nothing and would
// otherwise be a credential that reads as scoped and answers 403 for every
// project it was meant to cover.
func (s *Server) parseCredentialProjects(
	ctx context.Context, w http.ResponseWriter, raw []string,
) ([]string, bool) {
	projects := []string{}
	seen := map[string]struct{}{}
	for _, value := range raw {
		name := strings.TrimSpace(value)
		if name == "" {
			continue
		}
		if _, already := seen[name]; already {
			continue
		}
		project := &kitchenv1alpha1.Project{}
		switch err := s.get(ctx, name, project); {
		case apierrors.IsNotFound(err):
			badRequest(w, "there is no project called %s to narrow this credential to", name)
			return nil, false
		case err != nil:
			s.writeError(w, err)
			return nil, false
		}
		seen[name] = struct{}{}
		projects = append(projects, name)
	}
	sort.Strings(projects)
	return projects, true
}

// settingsAccessPatch is how a credential write reaches the cluster.
//
// It carries the caller's resourceVersion, for settingsPatch's reason applied
// to the other list on the same object: the list is replaced wholesale, and
// the decision that a name was free was made against the list this handler
// read. Two credentials issued at once would otherwise land as one, with the
// issuer holding two credentials and the platform granting one of them.
func settingsAccessPatch(kitchen *kitchenv1alpha1.Kitchen) client.Patch {
	return client.MergeFromWithOptions(kitchen.DeepCopy(), client.MergeFromWithOptimisticLock{})
}

// grantFor is the entry the singleton holds for a credential's subject, and
// nil when it holds none — which is a credential that authenticates and can do
// nothing, reported rather than hidden.
func grantFor(kitchen *kitchenv1alpha1.Kitchen, subject string) *kitchenv1alpha1.PlatformCredential {
	if kitchen == nil || subject == "" {
		return nil
	}
	for i := range kitchen.Spec.Access.Credentials {
		if kitchen.Spec.Access.Credentials[i].Subject == subject {
			return &kitchen.Spec.Access.Credentials[i]
		}
	}
	return nil
}

// indexOfCredential finds the grant a revocation has to remove: by the
// credential's subject when the issuer still knows about it, and otherwise by
// the address the platform wrote when it issued it.
func indexOfCredential(kitchen *kitchenv1alpha1.Kitchen, found *idp.PlatformKey, address string) int {
	for i := range kitchen.Spec.Access.Credentials {
		entry := kitchen.Spec.Access.Credentials[i]
		if found != nil && entry.Subject == found.Subject {
			return i
		}
		if strings.EqualFold(entry.Email, address) {
			return i
		}
	}
	return -1
}

func newCredentialView(
	key idp.PlatformKey, grant *kitchenv1alpha1.PlatformCredential, now time.Time,
) credentialView {
	view := credentialView{
		Name:     key.Name,
		Subject:  key.Subject,
		Email:    key.Email,
		Scopes:   []string{},
		Prefix:   key.Prefix,
		Created:  key.Created,
		LastUsed: key.LastUsed,
		// A credential the platform grants nothing reads as expired, which is
		// what it effectively is: it holds no scope and never will until
		// somebody writes one.
		Expired: true,
	}
	if grant == nil {
		return view
	}
	view.Scopes = scopeStrings(grant.Scopes)
	view.Projects = grant.Projects
	if !grant.Expires.IsZero() {
		expires := grant.Expires.Time.UTC()
		view.Expires = &expires
	}
	view.Expired = access.Expired(*grant, now)
	return view
}

// scopeStrings is the wire form of a grant's scopes, always a list and never
// null, so that a client can render it without a nil check.
func scopeStrings(scopes []kitchenv1alpha1.PlatformScope) []string {
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		out = append(out, string(scope))
	}
	return out
}

// scopeNames is the same list as one readable string, for an audit record's
// from/to and for a log line.
func scopeNames(scopes []kitchenv1alpha1.PlatformScope) string {
	if len(scopes) == 0 {
		return nothingHeld
	}
	return strings.Join(scopeStrings(scopes), ", ")
}

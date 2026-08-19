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
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/idp"
)

// A project's CI keys: the credential a pipeline holds, and the membership
// that makes it worth holding.
//
// A key is a member of a project and nothing else. The identity provider's
// api-key plugin turns a key into a session for the account the key belongs
// to, so the `sub` in the token a key is exchanged for is its *owner's* — a
// key has no subject of its own. Every key therefore gets an owner of its
// own, a machine account created for it, and it is that account's subject the
// project grants a role to. Which is why the routes here look like the
// membership routes: a key's grant is a grant like any other, resolved by
// internal/access from `spec.access` with nothing about keys in it.
//
// Two properties fall out of that and are the reason it is built this way. A
// key cannot outrank the project it was made for, because the grant is on the
// project. And revocation stays in one place: deleting the key at the issuer
// is what stops it working, and nothing in the operator has state to
// invalidate.
//
// The pair is the whole of the feature, so the pair is what these handlers
// keep. A key nothing has granted anything to authenticates and can do
// nothing, which reads as a broken platform; a grant whose subject no longer
// exists is a line in `spec.access` nobody can act on. So creating writes both
// and undoes the key if the grant will not land, and deleting removes both.

// keyRole is what a CI key is granted when nothing says otherwise. It is
// docs/AUTH.md's starting point: the day job, which is what a pipeline does —
// build, promote, roll back.
const keyRole = access.ProjectDeveloper

// keyNamePattern is what a key may be called. It is the same DNS-label rule a
// project name follows, because the two together are the machine account's
// address at the issuer and a URL path segment here; the identity provider
// applies it again at its end, where it is what keeps that address parseable.
var keyNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// keyView is one key as the dashboard reads it: what it is called, who it is
// at the issuer, what it may do here, and enough of its value to tell it from
// the next one. Never the key itself — there is no read anywhere that returns
// one.
type keyView struct {
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	// Role is the key's grant on this project, read from `spec.access` rather
	// than from anything stored on the key. It is empty for a key the project
	// has no grant for, which is a credential that can do nothing — the state
	// this feature exists to make impossible, reported rather than hidden.
	Role     string     `json:"role,omitempty"`
	Prefix   string     `json:"prefix,omitempty"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"lastUsed,omitempty"`
}

// issuedKeyView is the one response in the platform that carries a
// credential: the key, once, at creation. Nothing stores it in a form
// anything can read back, so a lost key is deleted and reissued.
type issuedKeyView struct {
	keyView
	Key string `json:"key"`
}

// createKeyRequest asks for a key. Role is the one knob, and it is optional:
// left out it is `developer`, which is what a pipeline needs.
//
// It exists so that the narrower role docs/AUTH.md leaves open — a `deployer`
// that can build and promote and nothing else — is a value this already
// validates rather than a new request shape. `admin` is refused: it is the
// role that creates keys, and a credential pasted into a CI system that can
// mint its own successors is a key the platform can no longer account for.
type createKeyRequest struct {
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
}

func newKeyView(key idp.Key, role kitchenv1alpha1.AccessRole) keyView {
	return keyView{
		Name:     key.Name,
		Subject:  key.Subject,
		Email:    key.Email,
		Role:     string(role),
		Prefix:   key.Prefix,
		Created:  key.Created,
		LastUsed: key.LastUsed,
	}
}

func (s *Server) listKeys(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	keys, _, ok := s.keysOf(w, req, project)
	if !ok {
		return
	}

	views := make([]keyView, 0, len(keys))
	for _, key := range keys {
		views = append(views, newKeyView(key, roleOf(project, key.Subject)))
	}
	writeList(w, views)
}

func (s *Server) createKey(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	body := createKeyRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if !keyNamePattern.MatchString(name) {
		badRequest(w, "name must be lowercase letters, digits and dashes, starting and ending with a "+
			"letter or digit, at most 32 characters (got %q): it is how the key is addressed", name)
		return
	}
	role, ok := parseKeyRole(w, body.Role)
	if !ok {
		return
	}

	directory, err := s.directory(ctx)
	if err != nil {
		s.noDirectory(w, err)
		return
	}

	// The audit record comes first, as it does for every write this API makes:
	// a change the log cannot record is a change the platform does not make.
	patch := membershipPatch(project)
	if !s.recorded(w, req, audit.Transition{
		Object:    project,
		Kind:      audit.KindProject,
		Operation: clickhouse.AuditUpdate,
		To:        string(role),
		Project:   project.Name,
		Reason:    fmt.Sprintf("the key %s was issued for %s as %s", name, project.Name, role),
		Details: map[string]any{
			"key":    name,
			"role":   string(role),
			"change": "key-issued",
		},
	}) {
		return
	}

	issued, err := directory.CreateKey(ctx, project.Name, name)
	switch {
	case errors.Is(err, idp.ErrKeyExists):
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"%s already has a key called %s: delete it and make a new one rather than reusing the name, "+
				"so that revoking either is unambiguous", project.Name, name)})
		return
	case errors.Is(err, idp.ErrNoKeyDirectory):
		s.noKeyDirectory(w, err)
		return
	case err != nil:
		s.log().Error(err, "the identity provider would not issue a key", "project", project.Name, "key", name)
		writeJSON(w, http.StatusBadGateway, errorBody{Error: fmt.Sprintf(
			"the identity provider could not issue a key for %s; the operator's log has why", project.Name)})
		return
	}

	// Both halves, or neither. A key with no grant authenticates and can do
	// nothing, so if the grant will not land the key does not survive the
	// request — and if it cannot be taken back either, the refusal says so
	// rather than leaving a credential nobody knows about.
	grant := kitchenv1alpha1.AccessGrant{
		AccessSubject: kitchenv1alpha1.AccessSubject{Subject: issued.Subject, Email: issued.Email},
		Role:          role,
	}
	project.Spec.Access = append(project.Spec.Access, grant)
	if err := s.Client.Patch(ctx, project, patch); err != nil {
		s.revoke(ctx, w, directory, project.Name, name, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("ci key issued through the api",
		"project", project.Name, "key", name, "subject", issued.Subject,
		"role", role, "caller", callerName(caller))
	writeJSON(w, http.StatusCreated, issuedKeyView{
		keyView: newKeyView(issued.Key, role),
		Key:     issued.Secret,
	})
}

func (s *Server) deleteKey(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	name := strings.TrimSpace(req.PathValue("key"))

	// Read the key before anything is recorded or removed, so that a name the
	// project has no key for is the plain not-found it ought to be, and so
	// that the record names the subject the grant is about.
	keys, directory, ok := s.keysOf(w, req, project)
	if !ok {
		return
	}
	var found *idp.Key
	for i := range keys {
		if keys[i].Name == name {
			found = &keys[i]
			break
		}
	}
	if found == nil {
		s.writeError(w, apierrors.NewNotFound(
			schema.GroupResource{Group: kitchenv1alpha1.GroupVersion.Group, Resource: "projectkeys"}, name))
		return
	}
	role := roleOf(project, found.Subject)

	patch := membershipPatch(project)
	if !s.recorded(w, req, audit.Transition{
		Object:    project,
		Kind:      audit.KindProject,
		Operation: clickhouse.AuditUpdate,
		From:      string(role),
		Project:   project.Name,
		Reason:    fmt.Sprintf("the key %s was revoked on %s", name, project.Name),
		Details: map[string]any{
			"key":    name,
			"member": found.Subject,
			"email":  found.Email,
			"role":   string(role),
			"change": "key-revoked",
		},
	}) {
		return
	}

	// The credential goes first. Of the two ways this can end up half done,
	// a grant naming an account that no longer exists is a line to tidy up
	// and a key that still works is not.
	switch _, err := directory.DeleteKey(ctx, project.Name, name); {
	case errors.Is(err, idp.ErrKeyNotFound):
		// Somebody else revoked it between the read above and here. The end
		// state is the one that was asked for, so the grant still comes off.
		s.log().Info("a key was already gone when it was revoked", "project", project.Name, "key", name)
	case errors.Is(err, idp.ErrNoKeyDirectory):
		s.noKeyDirectory(w, err)
		return
	case err != nil:
		s.log().Error(err, "the identity provider would not revoke a key", "project", project.Name, "key", name)
		writeJSON(w, http.StatusBadGateway, errorBody{Error: fmt.Sprintf(
			"the identity provider could not revoke %s's key %s; the operator's log has why",
			project.Name, name)})
		return
	}

	if at := indexOfMember(project, found.Subject); at >= 0 {
		project.Spec.Access = append(project.Spec.Access[:at], project.Spec.Access[at+1:]...)
		if err := s.Client.Patch(ctx, project, patch); err != nil {
			s.log().Error(err, "a revoked key's grant is still on the project",
				"project", project.Name, "key", name, "subject", found.Subject)
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: fmt.Sprintf(
				"the key %s was revoked and no longer works, but its grant is still on %s: "+
					"remove the member %s to finish the job", name, project.Name, found.Subject)})
			return
		}
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("ci key revoked through the api",
		"project", project.Name, "key", name, "subject", found.Subject, "caller", callerName(caller))
	w.WriteHeader(http.StatusNoContent)
}

// keysOf reads a project's keys at the identity provider, answering the
// request itself when it cannot. The directory comes back with them so that a
// handler which then writes uses the same connection it read through.
func (s *Server) keysOf(
	w http.ResponseWriter,
	req *http.Request,
	project *kitchenv1alpha1.Project,
) ([]idp.Key, accountDirectory, bool) {
	ctx := req.Context()
	directory, err := s.directory(ctx)
	if err != nil {
		s.noDirectory(w, err)
		return nil, nil, false
	}
	keys, err := directory.Keys(ctx, project.Name)
	switch {
	case errors.Is(err, idp.ErrNoKeyDirectory):
		s.noKeyDirectory(w, err)
		return nil, nil, false
	case err != nil:
		s.log().Error(err, "cannot list a project's keys at the identity provider", "project", project.Name)
		writeJSON(w, http.StatusBadGateway, errorBody{Error: fmt.Sprintf(
			"the identity provider could not be asked about %s's keys; the operator's log has why",
			project.Name)})
		return nil, nil, false
	}
	return keys, directory, true
}

// revoke takes a key back after the grant it was issued for could not be
// written, and answers the request either way. The two outcomes are different
// enough to be worth different words: one is a request that changed nothing,
// and the other is a credential somebody has to go and remove by hand.
func (s *Server) revoke(
	ctx context.Context,
	w http.ResponseWriter,
	directory accountDirectory,
	project, name string,
	cause error,
) {
	if _, err := directory.DeleteKey(ctx, project, name); err != nil {
		s.log().Error(err, "a key was issued that nothing granted anything to, and could not be taken back",
			"project", project, "key", name, "cause", cause.Error())
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: fmt.Sprintf(
			"the key %s was created at the identity provider but %s could not be given the grant that "+
				"makes it useful, and the key could not be taken back either: it authenticates and can do "+
				"nothing. Delete it and try again", name, project)})
		return
	}
	s.log().Info("took back a key whose grant could not be written",
		"project", project, "key", name, "cause", cause.Error())
	s.writeError(w, cause)
}

// noKeyDirectory answers a request that needed the issuer's key endpoints on
// an installation whose issuer has none. 503, for the same reason
// noDirectory's is: the request was well formed, and what the platform cannot
// do is ask the issuer.
func (s *Server) noKeyDirectory(w http.ResponseWriter, err error) {
	if !errors.Is(err, idp.ErrNoKeyDirectory) {
		s.log().Error(err, "cannot reach the identity provider's key endpoints")
	}
	writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "this installation's identity provider " +
		"issues no CI keys, so a key cannot be created or revoked here: it is federated to an issuer of " +
		"its own, where keys are that issuer's to hand out"})
}

// parseKeyRole reads the role a key is asked for, defaulting to developer and
// refusing admin. It answers the request itself when it will not.
func parseKeyRole(w http.ResponseWriter, raw string) (kitchenv1alpha1.AccessRole, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return kitchenv1alpha1.AccessRole(keyRole.String()), true
	}
	role, ok := access.ParseProjectRole(raw)
	if !ok {
		badRequest(w, "role must be developer or viewer (got %q)", raw)
		return "", false
	}
	if role == access.ProjectAdmin {
		badRequest(w, "a key cannot be an admin: admin is the role that issues keys, and a credential "+
			"in a build pipeline that can issue more of them is one nobody can account for. "+
			"Use developer, or viewer for a key that only reads")
		return "", false
	}
	return kitchenv1alpha1.AccessRole(role.String()), true
}

// roleOf is the role a project grants a subject, straight off `spec.access`,
// and the empty role when it grants none.
func roleOf(project *kitchenv1alpha1.Project, subject string) kitchenv1alpha1.AccessRole {
	if at := indexOfMember(project, subject); at >= 0 {
		return project.Spec.Access[at].Role
	}
	return ""
}

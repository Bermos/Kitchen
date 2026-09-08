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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/idp"
)

// A project's membership: the read and the three writes over `spec.access`.
//
// This is what stops "add Bob to shop" from going through the platform's
// owner every time. Creating a project is self-service and its creator is its
// admin (see createProject), so the admin who can hand it to somebody else is
// already there the moment the project exists — these four routes are the
// rest of that sentence.
//
// Nothing here decides who holds which role. Whether the *caller* may be here
// at all is the enforcement table's (policy.go: project admin), and it
// resolves through internal/access like everything else. What these handlers
// own is the list itself: what may be written into it, and the one thing that
// may not be written out of it.

// memberView is one grant as the dashboard reads it — the readable form of an
// entry in `spec.access`.
//
// Subject is the canonical identifier and the one thing a write addresses a
// member by. Email is informational, exactly as it is on the object: it is
// what makes a list of opaque strings render as people. The two swap round
// for a grant hand-written against an address (AccessSubject: a subject
// containing `@` is read as one), where Subject carries the address and Email
// is usually empty — which is the truth about that entry and not a gap.
type memberView struct {
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	Role    string `json:"role"`
	// Kind is what holds this grant: "account" for a person, "key" for the
	// machine account behind a CI key (keys.go). Keys and people are one list
	// — a key is a member of the project like anybody else — and this is what
	// lets that list say which it is showing rather than rendering a robot as
	// somebody with an odd address.
	//
	// It is derived from the address, which is the machine account's own, and
	// it is a display rule and nothing more: no access decision anywhere reads
	// it. internal/access resolves a role from the subject alone.
	Kind string `json:"kind"`
	// Name is the key's name, for a grant held by one. A person's grant
	// carries no name — `spec.access` records a subject and an address — so
	// it is empty there rather than guessed at.
	Name string `json:"name,omitempty"`
}

// The two values Kind takes.
const (
	memberKindAccount = "account"
	memberKindKey     = "key"
)

func newMemberView(grant kitchenv1alpha1.AccessGrant) memberView {
	view := memberView{
		Subject: grant.Subject,
		Email:   grant.Email,
		Role:    string(grant.Role),
		Kind:    memberKindAccount,
	}
	if _, name, ok := idp.MachineIdentity(grant.Email); ok {
		view.Kind, view.Name = memberKindKey, name
	}
	return view
}

// addMemberRequest adds somebody to a project.
//
// It names them by address, which the platform resolves to the issuer's `sub`
// before anything is written: the address is what a person can type and the
// `sub` is what a token will actually carry, and resolving at write time is
// what keeps an entry that names a stranger from being stored as a grant that
// silently matches nobody.
//
// Subject is the other way in, for an identity that has no address to resolve
// — a machine account, or any account on an installation federated to an
// issuer that serves no directory. It is taken as given, which is the whole
// difference between the two fields, so it is the caller who is asserting the
// identifier is real. Exactly one of the two is required.
type addMemberRequest struct {
	Email   string `json:"email,omitempty"`
	Subject string `json:"subject,omitempty"`
	Role    string `json:"role"`
}

// changeMemberRequest moves an existing member to another role, and
// removeMemberRequest takes one off the project.
//
// Both address the member by `subject` in the body rather than in the path,
// and that is a deliberate choice about one thing: a `sub` is opaque. Every
// path segment this API addresses an object by is a Kubernetes name — a DNS
// label — and an issuer's subject is not: it may carry `/`, `%` or `#`, so a
// path form would add a percent-encoding rule that every client would have to
// remember and that only ever bites on the accounts whose identifiers happen
// to be ugly. The body has no such rule, and it is where the rest of this API
// already names things that are not path names (a domain names its
// environment, a claim names its project). A `DELETE` with a body is the
// price; it is a small one, and the alternative is a 404 that only some
// issuers can produce.
type changeMemberRequest struct {
	Subject string `json:"subject"`
	Role    string `json:"role"`
}

type removeMemberRequest struct {
	Subject string `json:"subject"`
}

// accountDirectory is the slice of the identity provider this package needs:
// an address resolved to the account that holds it, and the machine accounts
// a project's CI keys are (keys.go). It is an interface for the same reason
// logReader is — a test must be able to answer without an issuer to talk to.
//
// They are one interface because they are one connection, resolved off the
// platform's own identity-provider secret. Splitting them would mean two
// resolutions of the same thing, which is two ways for the platform to
// disagree with itself about which issuer it is talking to.
type accountDirectory interface {
	AccountByEmail(ctx context.Context, email string) (*idp.Account, error)
	// Accounts is every account the issuer holds. It is what the access
	// survey (access.go) asks whether a grant belongs to anybody at all, and
	// it is the one read here that a federated issuer answers with
	// idp.ErrNoDirectory — which the survey treats as "could not ask" rather
	// than as "nobody is behind it".
	Accounts(ctx context.Context) ([]idp.Account, error)
	Keys(ctx context.Context, project string) ([]idp.Key, error)
	CreateKey(ctx context.Context, project, name string) (*idp.IssuedKey, error)
	DeleteKey(ctx context.Context, project, name string) (*idp.Key, error)
}

// errNoAccountDirectory is what a resolution answers on an installation whose
// issuer the operator cannot read accounts from: no identity provider secret,
// or an issuer that is not the one the chart ships. It says what to do
// instead, because there is something to do instead.
var errNoAccountDirectory = errors.New(
	"this installation's identity provider serves no account directory, so an address cannot be resolved " +
		"to an account: add the member by their subject instead")

// directory resolves how a membership write reaches the account directory. It
// mirrors telemetryStore: read per request off the platform's own secret, so
// that pointing the platform at another issuer takes effect without a restart.
func (s *Server) directory(ctx context.Context) (accountDirectory, error) {
	if s.accounts != nil {
		return s.accounts(ctx)
	}
	kitchen := kitchenFrom(ctx)
	if kitchen == nil || kitchen.Spec.Auth.SecretRef == nil {
		return nil, errNoAccountDirectory
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: controller.PlatformNamespace, Name: kitchen.Spec.Auth.SecretRef.Name}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	cfg, err := idp.ConfigFromSecret(secret)
	if err != nil {
		return nil, err
	}
	return idp.New(cfg), nil
}

func (s *Server) listMembers(w http.ResponseWriter, req *http.Request) {
	project := &kitchenv1alpha1.Project{}
	if err := s.get(req.Context(), req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	// In the order they are written down, which is the order somebody editing
	// the object sees. Sorting them here would make the API's account of
	// `spec.access` a different document from `spec.access`.
	views := make([]memberView, 0, len(project.Spec.Access))
	for _, grant := range project.Spec.Access {
		views = append(views, newMemberView(grant))
	}
	writeList(w, views)
}

func (s *Server) addMember(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	body := addMemberRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Email = strings.TrimSpace(body.Email)
	body.Subject = strings.TrimSpace(body.Subject)

	role, ok := parseRole(w, body.Role)
	if !ok {
		return
	}

	grant, ok := s.resolveMember(ctx, w, body)
	if !ok {
		return
	}
	grant.Role = role

	if existing := indexOfMember(project, grant.Subject); existing >= 0 {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"%s is already a member of %s as %s: change the role with PATCH rather than adding a second grant",
			describeMember(project.Spec.Access[existing]), project.Name, project.Spec.Access[existing].Role)})
		return
	}

	patch := membershipPatch(project)
	project.Spec.Access = append(project.Spec.Access, grant)
	if !s.recorded(w, req, audit.Transition{
		Object:     project,
		Kind:       audit.KindProject,
		Operation:  clickhouse.AuditUpdate,
		Privileged: audit.PrivilegeAccess,
		To:         string(grant.Role),
		Project:    project.Name,
		Reason: fmt.Sprintf("%s was given %s on %s",
			describeMember(grant), grant.Role, project.Name),
		Details: map[string]any{
			"member": grant.Subject,
			"email":  grant.Email,
			"role":   string(grant.Role),
			"change": "added",
		},
	}) {
		return
	}
	if err := s.Client.Patch(ctx, project, patch); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("project membership granted through the api",
		"project", project.Name, "member", grant.Subject, "role", grant.Role, "caller", callerName(caller))
	writeJSON(w, http.StatusCreated, newMemberView(grant))
}

func (s *Server) changeMemberRole(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	body := changeMemberRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Subject = strings.TrimSpace(body.Subject)

	role, ok := parseRole(w, body.Role)
	if !ok {
		return
	}
	at, ok := s.member(w, project, body.Subject)
	if !ok {
		return
	}

	was := project.Spec.Access[at]
	if was.Role == role {
		// Nothing to write, and so nothing to record: an audit entry saying a
		// role changed from admin to admin is a false statement about a
		// change that did not happen.
		writeJSON(w, http.StatusOK, newMemberView(was))
		return
	}
	if role != kitchenv1alpha1.AccessRoleAdmin && isTheOnlyAdmin(project, was.Subject) {
		writeJSON(w, http.StatusConflict, errorBody{Error: lastAdminRefusal(project, was, "change this one")})
		return
	}

	patch := membershipPatch(project)
	project.Spec.Access[at].Role = role
	if !s.recorded(w, req, audit.Transition{
		Object:     project,
		Kind:       audit.KindProject,
		Operation:  clickhouse.AuditUpdate,
		Privileged: audit.PrivilegeAccess,
		From:       string(was.Role),
		To:         string(role),
		Project:    project.Name,
		Reason: fmt.Sprintf("%s went from %s to %s on %s",
			describeMember(was), was.Role, role, project.Name),
		Details: map[string]any{
			"member": was.Subject,
			"email":  was.Email,
			"role":   string(role),
			"change": "role",
		},
	}) {
		return
	}
	if err := s.Client.Patch(ctx, project, patch); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("project membership changed through the api",
		"project", project.Name, "member", was.Subject, "role", role, "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newMemberView(project.Spec.Access[at]))
}

func (s *Server) removeMember(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	body := removeMemberRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Subject = strings.TrimSpace(body.Subject)

	at, ok := s.member(w, project, body.Subject)
	if !ok {
		return
	}
	was := project.Spec.Access[at]
	if isTheOnlyAdmin(project, was.Subject) {
		writeJSON(w, http.StatusConflict, errorBody{Error: lastAdminRefusal(project, was, "remove this one")})
		return
	}

	patch := membershipPatch(project)
	project.Spec.Access = append(project.Spec.Access[:at], project.Spec.Access[at+1:]...)
	if !s.recorded(w, req, audit.Transition{
		Object:     project,
		Kind:       audit.KindProject,
		Operation:  clickhouse.AuditUpdate,
		Privileged: audit.PrivilegeAccess,
		From:       string(was.Role),
		Project:    project.Name,
		Reason:     fmt.Sprintf("%s no longer has a role on %s", describeMember(was), project.Name),
		Details: map[string]any{
			"member": was.Subject,
			"email":  was.Email,
			"role":   string(was.Role),
			"change": "removed",
		},
	}) {
		return
	}
	if err := s.Client.Patch(ctx, project, patch); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("project membership removed through the api",
		"project", project.Name, "member", was.Subject, "caller", callerName(caller))
	w.WriteHeader(http.StatusNoContent)
}

// membershipPatch is how every write here reaches the cluster: a merge patch
// carrying the caller's resourceVersion.
//
// The optimistic lock is the point, and it is stricter than what the other
// project writes need. Every decision above — whether this subject is already
// a member, whether it is the last admin — was made against the list this
// handler read, and a merge patch without the lock would land that decision on
// a list somebody else has meanwhile changed. Two admins removing each other
// at the same time is exactly the case the last-admin rule exists to prevent,
// and it is the case a lost update would let through. A conflict answers 409,
// which is the client's cue to re-read and try again.
func membershipPatch(project *kitchenv1alpha1.Project) client.Patch {
	return client.MergeFromWithOptions(project.DeepCopy(), client.MergeFromWithOptimisticLock{})
}

// parseRole reads the role a write asks for, answering the request itself when
// it is not one of the three. access.ParseProjectRole is the one place the
// CRD's enum becomes a role, here as everywhere else.
func parseRole(w http.ResponseWriter, raw string) (kitchenv1alpha1.AccessRole, bool) {
	role, ok := access.ParseProjectRole(strings.TrimSpace(raw))
	if !ok {
		badRequest(w, "role must be admin, developer or viewer (got %q)", strings.TrimSpace(raw))
		return "", false
	}
	return kitchenv1alpha1.AccessRole(role.String()), true
}

// resolveMember turns what a caller named into the entry that will be written:
// an address resolved at the identity provider, or a subject taken as given.
// It answers the request itself on every path that is not a grant.
func (s *Server) resolveMember(
	ctx context.Context,
	w http.ResponseWriter,
	body addMemberRequest,
) (kitchenv1alpha1.AccessGrant, bool) {
	switch {
	case body.Email == "" && body.Subject == "":
		badRequest(w, "name the member: `email` is the address they sign in with, "+
			"or `subject` is the issuer's identifier for an account that has no address")
		return kitchenv1alpha1.AccessGrant{}, false
	case body.Email != "" && body.Subject != "":
		badRequest(w, "name the member once: pass `email` to have the platform resolve it, "+
			"or `subject` to write an identifier you already have — not both")
		return kitchenv1alpha1.AccessGrant{}, false
	case body.Subject != "":
		if access.IsEmailSubject(body.Subject) {
			// An entry whose subject contains `@` is read as an address, and
			// is then honoured only for a verified one. That is the right rule
			// for hand-written YAML and the wrong one here, where the platform
			// can go and ask who holds the address.
			badRequest(w, "subject %q is an address: pass it as `email` instead, "+
				"so the platform resolves it to the account that holds it", body.Subject)
			return kitchenv1alpha1.AccessGrant{}, false
		}
		return kitchenv1alpha1.AccessGrant{
			AccessSubject: kitchenv1alpha1.AccessSubject{Subject: body.Subject},
		}, true
	}

	directory, err := s.directory(ctx)
	if err != nil {
		s.noDirectory(w, err)
		return kitchenv1alpha1.AccessGrant{}, false
	}
	account, err := directory.AccountByEmail(ctx, body.Email)
	switch {
	case errors.Is(err, idp.ErrAccountNotFound):
		// A 404 about the person, not about the project: the address is real
		// enough to type and there is nobody behind it, and the alternative —
		// writing the grant anyway — is an entry no token will ever match and
		// nobody will ever look at again.
		writeJSON(w, http.StatusNotFound, errorBody{Error: fmt.Sprintf(
			"the identity provider knows no account with the address %q: they have to sign in to Kitchen "+
				"once before they can be given a role on a project", body.Email)})
		return kitchenv1alpha1.AccessGrant{}, false
	case errors.Is(err, idp.ErrNoDirectory):
		s.noDirectory(w, errNoAccountDirectory)
		return kitchenv1alpha1.AccessGrant{}, false
	case err != nil:
		s.log().Error(err, "cannot resolve an address at the identity provider", "email", body.Email)
		writeJSON(w, http.StatusBadGateway, errorBody{Error: fmt.Sprintf(
			"the identity provider could not be asked who holds %q; the operator's log has why", body.Email)})
		return kitchenv1alpha1.AccessGrant{}, false
	}
	// The address is carried beside the subject so the list reads; nothing
	// resolves against it (AccessSubject). It is the directory's spelling of
	// the address rather than the caller's, so the object records what the
	// identity provider says the account is called.
	return kitchenv1alpha1.AccessGrant{AccessSubject: kitchenv1alpha1.AccessSubject{
		Subject: account.Subject,
		Email:   account.Email,
	}}, true
}

// noDirectory answers a write that needed the account directory and could not
// have it. 503 rather than 500: the request was well formed and the platform
// is willing — what it cannot do is ask the issuer, which is a condition that
// clears on its own, and there is a way through in the meantime.
func (s *Server) noDirectory(w http.ResponseWriter, err error) {
	if !errors.Is(err, errNoAccountDirectory) {
		s.log().Error(err, "cannot reach the identity provider's account directory")
	}
	writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: errNoAccountDirectory.Error()})
}

// member finds the grant a write addresses, answering a 404 when the project
// has no such member. The not-found names the members resource, so "there is
// no such grant" reads the same way every other missing object does.
func (s *Server) member(w http.ResponseWriter, project *kitchenv1alpha1.Project, subject string) (int, bool) {
	if subject == "" {
		badRequest(w, "subject is required: it is the `subject` GET /projects/%s/members answers with", project.Name)
		return 0, false
	}
	at := indexOfMember(project, subject)
	if at < 0 {
		s.writeError(w, apierrors.NewNotFound(
			schema.GroupResource{Group: kitchenv1alpha1.GroupVersion.Group, Resource: "projectmembers"}, subject))
		return 0, false
	}
	return at, true
}

// indexOfMember is where a subject is written down in the list, or -1.
func indexOfMember(project *kitchenv1alpha1.Project, subject string) int {
	for i, grant := range project.Spec.Access {
		if sameSubject(grant.Subject, subject) {
			return i
		}
	}
	return -1
}

// sameSubject reports whether two entries are written about the same account.
//
// It is a comparison of spellings, not a membership decision — who a subject
// resolves to is internal/access's and is not reimplemented here. The one
// thing it borrows is which spellings are addresses, because addresses are
// case-insensitive and an entry written Anna@Example.com must not be added
// twice as anna@example.com.
func sameSubject(a, b string) bool {
	if access.IsEmailSubject(a) || access.IsEmailSubject(b) {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// isTheOnlyAdmin reports whether this subject holds admin on the project and
// nobody else does — the state in which removing or demoting them leaves the
// project with no admin at all.
//
// The platform's operators are deliberately not counted. An operator holds
// admin on every project (access.ProjectRoleFor) and could indeed repair this,
// but a project whose only listed admin was removed is precisely the abandoned
// project the rule exists to prevent: the people who work on it would have to
// go and find an operator to get anything changed, which is the bottleneck
// self-service membership was built to remove.
func isTheOnlyAdmin(project *kitchenv1alpha1.Project, subject string) bool {
	holds, others := false, 0
	for _, grant := range project.Spec.Access {
		if grant.Role != kitchenv1alpha1.AccessRoleAdmin {
			continue
		}
		if sameSubject(grant.Subject, subject) {
			holds = true
			continue
		}
		others++
	}
	return holds && others == 0
}

// lastAdminRefusal says what is wrong and what would fix it, which is the rule
// every refusal in this platform follows. `instead` completes the sentence
// with the operation that was refused.
func lastAdminRefusal(project *kitchenv1alpha1.Project, grant kitchenv1alpha1.AccessGrant, instead string) string {
	return fmt.Sprintf(
		"%s is the only admin on %s, and a project with no admin has nobody left who can add one: "+
			"make somebody else an admin first, then %s",
		describeMember(grant), project.Name, instead)
}

// describeMember is the member as a person reading a refusal knows them: the
// address when the entry carries one, and the opaque subject when it does not,
// which is at least the string they would have typed. A machine account is
// named as what it is, since nobody knows it by its address.
func describeMember(grant kitchenv1alpha1.AccessGrant) string {
	if project, name, ok := idp.MachineIdentity(grant.Email); ok {
		return fmt.Sprintf("the key %s on %s", name, project)
	}
	if grant.Email != "" {
		return grant.Email
	}
	return grant.Subject
}

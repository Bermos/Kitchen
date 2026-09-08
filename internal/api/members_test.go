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
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/idp"
)

// What membership promises: a project's admin adds and removes people without
// an operator, an address nobody holds is refused rather than written down,
// and the last admin cannot be taken off the project by anybody — including
// themselves.

const (
	// anna is the person these tests add: an address the stub directory knows
	// and the `sub` it resolves to.
	annaEmail   = "anna@example.com"
	annaSubject = "user_9"
	// stranger is an address the identity provider has never heard of.
	stranger = "nobody@example.com"
	// membersPath is the one URL all four membership routes answer on.
	membersPath = "/api/v1/projects/" + feedProject + "/members"
)

// stubDirectory stands in for the identity provider's account directory: the
// one question a membership write asks it.
type stubDirectory struct {
	accounts map[string]idp.Account
	err      error
	asked    []string

	// The key half, which keys_test.go drives: one machine account per key,
	// stored per project the way the identity provider stores them, and an
	// error per operation for the paths where the issuer will not answer.
	keys      map[string][]idp.Key
	deleted   []string
	keysErr   error
	createErr error
	deleteErr error
}

func (d *stubDirectory) AccountByEmail(_ context.Context, email string) (*idp.Account, error) {
	d.asked = append(d.asked, email)
	if d.err != nil {
		return nil, d.err
	}
	account, ok := d.accounts[strings.ToLower(email)]
	if !ok {
		return nil, fmt.Errorf("resolving the account %q: %w", email, idp.ErrAccountNotFound)
	}
	return &account, nil
}

// Accounts is the whole directory, which is what the access survey asks so it
// can say whether a grant belongs to anybody at all.
func (d *stubDirectory) Accounts(_ context.Context) ([]idp.Account, error) {
	if d.err != nil {
		return nil, d.err
	}
	accounts := make([]idp.Account, 0, len(d.accounts))
	for _, account := range d.accounts {
		accounts = append(accounts, account)
	}
	return accounts, nil
}

// withDirectory points the harness at a directory holding one account: anna,
// who is who every test here adds.
func (h *harness) withDirectory() *stubDirectory {
	directory := &stubDirectory{accounts: map[string]idp.Account{
		annaEmail: {Subject: annaSubject, Email: annaEmail, Name: "Anna", EmailVerified: true},
	}}
	h.server.accounts = func(context.Context) (accountDirectory, error) { return directory, nil }
	return directory
}

// members is the project's grants as the API reports them.
func (h *harness) members(t *testing.T) []memberView {
	t.Helper()
	recorder := h.do(t, http.MethodGet, membersPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("reading the members: %d %s", recorder.Code, recorder.Body.String())
	}
	return decode[listBody[memberView]](t, recorder).Items
}

// The whole of what the feature is for: somebody who is not an operator adds
// somebody else to their own project, and the address they typed becomes the
// stable identifier a token will actually carry.
func TestAProjectAdminAddsAMemberWithoutAnOperator(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	directory := h.withDirectory()

	recorder := h.do(t, http.MethodPost, membersPath, `{"email": "`+annaEmail+`", "role": "developer"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	added := decode[memberView](t, recorder)
	if added.Subject != annaSubject || added.Email != annaEmail ||
		added.Role != string(kitchenv1alpha1.AccessRoleDeveloper) {
		t.Fatalf("want anna as a developer named by her subject, got %+v", added)
	}
	if len(directory.asked) != 1 || directory.asked[0] != annaEmail {
		t.Fatalf("the address was not resolved at the identity provider: %v", directory.asked)
	}

	// And it is on the object, as a `sub` — not as the address, which nothing
	// resolves against.
	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), feedProject, stored); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, grant := range stored.Spec.Access {
		if grant.Subject == annaSubject && grant.Email == annaEmail && grant.Role == kitchenv1alpha1.AccessRoleDeveloper {
			found = true
		}
	}
	if !found {
		t.Fatalf("spec.access does not carry the grant: %+v", stored.Spec.Access)
	}

	listed := h.members(t)
	if len(listed) != 2 {
		t.Fatalf("want the caller and anna, got %+v", listed)
	}
}

// Anna can then be moved and removed, which is the other half of "without an
// operator": the caller stays the project's only admin throughout, so neither
// write is the one the last-admin rule refuses.
func TestAProjectAdminChangesAndRemovesAMember(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	h.withDirectory()

	if recorder := h.do(t, http.MethodPost, membersPath,
		`{"email": "`+annaEmail+`", "role": "viewer"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("adding anna: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodPatch, membersPath, `{"subject": "`+annaSubject+`", "role": "developer"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if changed := decode[memberView](t, recorder); changed.Role != string(kitchenv1alpha1.AccessRoleDeveloper) {
		t.Fatalf("want developer, got %+v", changed)
	}

	if recorder := h.do(t, http.MethodDelete, membersPath,
		`{"subject": "`+annaSubject+`"}`); recorder.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if listed := h.members(t); len(listed) != 1 || listed[0].Subject != testSubject {
		t.Fatalf("want only the caller left, got %+v", listed)
	}
}

// An address nobody holds is a 404 about the person. The alternative — writing
// the grant anyway — is an entry no token will ever match, on a list somebody
// will later read as if it meant something.
func TestAddingAnAddressTheIssuerDoesNotKnow(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	h.withDirectory()

	recorder := h.do(t, http.MethodPost, membersPath, `{"email": "`+stranger+`", "role": "developer"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
	want := `the identity provider knows no account with the address "` + stranger + `": they have to sign in ` +
		`to Kitchen once before they can be given a role on a project`
	if got := errorOf(t, recorder.Body.String()); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if listed := h.members(t); len(listed) != 1 {
		t.Fatalf("a refused grant was written anyway: %+v", listed)
	}
}

// An installation whose issuer serves no directory can still name a subject,
// which is the way in for a machine account as well.
func TestAddingBySubjectNeedsNoDirectory(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	h.server.accounts = func(context.Context) (accountDirectory, error) {
		return nil, errNoAccountDirectory
	}

	recorder := h.do(t, http.MethodPost, membersPath, `{"subject": "svc_ci", "role": "developer"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	// The same request naming an address gets the way out in words, not a 500.
	refused := h.do(t, http.MethodPost, membersPath, `{"email": "`+annaEmail+`", "role": "developer"}`)
	if refused.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", refused.Code, refused.Body.String())
	}
	if got := errorOf(t, refused.Body.String()); got != errNoAccountDirectory.Error() {
		t.Fatalf("want the directory's own explanation, got %q", got)
	}
}

// An address in `subject` would be stored as an email grant — honoured only
// for a verified address — when the platform can simply go and ask who holds
// it. That is a different, weaker grant than the caller meant to write.
func TestAddingASubjectThatIsAnAddressIsRefused(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	h.withDirectory()

	recorder := h.do(t, http.MethodPost, membersPath, `{"subject": "`+annaEmail+`", "role": "developer"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "pass it as `email` instead") {
		t.Fatalf("the refusal does not say what would fix it: %q", got)
	}
}

// The rule that keeps a project from being abandoned. It applies to the
// removal and to the demotion alike, and an operator is not a substitute: a
// project whose only listed admin is gone is exactly what it exists to
// prevent, even though an operator could still repair it.
func TestTheLastAdminCannotBeRemovedOrDemoted(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	h.withDirectory()

	removed := h.do(t, http.MethodDelete, membersPath, `{"subject": "`+testSubject+`"}`)
	if removed.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", removed.Code, removed.Body.String())
	}
	want := testCaller + " is the only admin on " + feedProject + ", and a project with no admin has nobody " +
		"left who can add one: make somebody else an admin first, then remove this one"
	if got := errorOf(t, removed.Body.String()); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}

	demoted := h.do(t, http.MethodPatch, membersPath, `{"subject": "`+testSubject+`", "role": "developer"}`)
	if demoted.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", demoted.Code, demoted.Body.String())
	}
	want = testCaller + " is the only admin on " + feedProject + ", and a project with no admin has nobody " +
		"left who can add one: make somebody else an admin first, then change this one"
	if got := errorOf(t, demoted.Body.String()); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}

	// Nothing was written on either path.
	if listed := h.members(t); len(listed) != 1 || listed[0].Role != string(kitchenv1alpha1.AccessRoleAdmin) {
		t.Fatalf("the last admin was changed after all: %+v", listed)
	}

	// With a second admin on the project, the same two writes go through: the
	// rule is about the last one, not about admins.
	if recorder := h.do(t, http.MethodPost, membersPath,
		`{"email": "`+annaEmail+`", "role": "admin"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("adding a second admin: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodDelete, membersPath,
		`{"subject": "`+testSubject+`"}`); recorder.Code != http.StatusNoContent {
		t.Fatalf("want 204 once somebody else is an admin, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// A member who is not an admin is refused the *writes*, in the words every
// other refusal uses: what you have, what you were doing, and what it would
// have taken. The list itself is not one of them — see
// TestAViewerReadsWhoIsOnTheirProject.
func TestAMemberIsRefusedTheMembershipWrites(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	h.withDirectory()

	for _, attempt := range []struct{ method, body, doing string }{
		{http.MethodPost, `{"email": "` + annaEmail + `", "role": "viewer"}`, "adding somebody to a project"},
		{http.MethodPatch, `{"subject": "` + annaSubject + `", "role": "viewer"}`,
			"changing somebody's role on a project"},
		{http.MethodDelete, `{"subject": "` + annaSubject + `"}`, "removing somebody from a project"},
	} {
		t.Run(attempt.method, func(t *testing.T) {
			recorder := h.do(t, attempt.method, membersPath, attempt.body)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
			}
			want := "you have developer on " + feedProject + "; " + attempt.doing + " needs admin"
			if got := errorOf(t, recorder.Body.String()); got != want {
				t.Fatalf("want %q, got %q", want, got)
			}
		})
	}
}

// Somebody with no role on the project is told it does not exist, which is the
// answer everywhere else too: membership must not be the one surface that
// confirms `shop` is real.
func TestMembershipIsNotFoundForSomebodyWithNoRole(t *testing.T) {
	h := asMember(t, "")
	h.withDirectory()

	if recorder := h.do(t, http.MethodGet, membersPath, ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// The writes an admin can get wrong, each answered by saying which.
func TestMembershipRefusesMalformedWrites(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	h.withDirectory()

	for _, attempt := range []struct {
		name, method, body string
		status             int
		contains           string
	}{
		{"a role nobody has", http.MethodPost, `{"email": "` + annaEmail + `", "role": "owner"}`,
			http.StatusBadRequest, "role must be admin, developer or viewer"},
		{"nobody named", http.MethodPost, `{"role": "viewer"}`, http.StatusBadRequest, "name the member"},
		{"named twice", http.MethodPost,
			`{"email": "` + annaEmail + `", "subject": "user_9", "role": "viewer"}`,
			http.StatusBadRequest, "name the member once"},
		{"somebody who is not a member", http.MethodPatch, `{"subject": "user_404", "role": "viewer"}`,
			http.StatusNotFound, "user_404"},
		{"no subject at all", http.MethodDelete, `{}`, http.StatusBadRequest, "subject is required"},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			recorder := h.do(t, attempt.method, membersPath, attempt.body)
			if recorder.Code != attempt.status {
				t.Fatalf("want %d, got %d: %s", attempt.status, recorder.Code, recorder.Body.String())
			}
			if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, attempt.contains) {
				t.Fatalf("want a message about %q, got %q", attempt.contains, got)
			}
		})
	}

	// And the same person twice is a conflict rather than a second grant: two
	// entries about one account is a list nobody can reason about.
	if recorder := h.do(t, http.MethodPost, membersPath,
		`{"email": "`+annaEmail+`", "role": "viewer"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("adding anna: %d %s", recorder.Code, recorder.Body.String())
	}
	again := h.do(t, http.MethodPost, membersPath, `{"email": "`+annaEmail+`", "role": "admin"}`)
	if again.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", again.Code, again.Body.String())
	}
	if got := errorOf(t, again.Body.String()); !strings.Contains(got, "change the role with PATCH") {
		t.Fatalf("the refusal does not say what would fix it: %q", got)
	}
}

// Creating a project and then handing it to somebody else is one session, with
// nobody else involved: the creator is its admin from the create itself, so
// the membership write that follows is one they are already allowed to make.
func TestTheCreatorOfAProjectCanAdministerItImmediately(t *testing.T) {
	h := asMember(t, "")
	h.withDirectory()

	created := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name": "tools", "repo": "acme/tools", "connection": "gh", "registry": "registry"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", created.Code, created.Body.String())
	}

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/tools/members",
		`{"email": "`+annaEmail+`", "role": "developer"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("the creator must be able to add somebody: %d %s", recorder.Code, recorder.Body.String())
	}
}

// Project names are one flat namespace under the base domain, and the refusal
// says so rather than passing on the API server's account of a resource in a
// namespace.
func TestASecondProjectOfTheSameNameIsToldTheNameIsTaken(t *testing.T) {
	h := asMember(t, "")

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name": "`+feedProject+`", "repo": "acme/shop", "connection": "gh", "registry": "registry"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	got := errorOf(t, recorder.Body.String())
	for _, want := range []string{
		`the project name "` + feedProject + `" is taken`,
		"one flat namespace under the platform's base domain",
		"first-come-first-served",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("want a refusal mentioning %q, got %q", want, got)
		}
	}
	if strings.Contains(got, "kitchen.bermos.dev") {
		t.Fatalf("the refusal leaks the API server's own vocabulary: %q", got)
	}
}

// The connection picker. A member gets enough to fill a dropdown and nothing
// else — asserted on the bytes, because what matters is what goes over the
// wire and not what the struct happens to be called.
func TestAMemberPicksAConnectionWithoutSeeingOne(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)

	recorder := h.do(t, http.MethodGet, "/api/v1/connections", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[listBody[map[string]any]](t, recorder)
	if len(body.Items) != 2 {
		t.Fatalf("want both connections, got %+v", body.Items)
	}
	for _, item := range body.Items {
		keys := make([]string, 0, len(item))
		for key := range item {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "capabilities,name,ready" {
			t.Fatalf("a member was answered with %v, want name, capabilities and readiness alone", keys)
		}
	}
	if got := body.Items[0]["name"]; got != "gh" {
		t.Fatalf("want the connections by name, got %v", got)
	}
	if got := body.Items[0]["capabilities"]; fmt.Sprint(got) != "[gitSource]" {
		t.Fatalf("want the capability that decides whether it can be chosen, got %v", got)
	}

	// And the picker is all it is: nothing else under /connections/ answers
	// them, bar the repository listing that fills in the same form's next
	// field (TestListingWhatAConnectionCanSee).
	for _, refused := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/connections/gh", ""},
		{http.MethodPost, "/api/v1/connections", `{"name": "x", "provider": "github", "credential": {"token": "t"}}`},
		{http.MethodPost, "/api/v1/connections/test", `{"provider": "github", "credential": {"token": "t"}}`},
		{http.MethodPatch, "/api/v1/connections/gh", `{"config": {}}`},
		{http.MethodDelete, "/api/v1/connections/gh", ""},
	} {
		if recorder := h.do(t, refused.method, refused.path, refused.body); recorder.Code != http.StatusForbidden {
			t.Fatalf("%s %s: want 403, got %d: %s",
				refused.method, refused.path, recorder.Code, recorder.Body.String())
		}
	}
}

// The same route, the operator's own answer: the connections themselves.
func TestAnOperatorSeesTheWholeConnectionOnTheSameRoute(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/connections", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[listBody[map[string]any]](t, recorder)
	if len(body.Items) != 2 {
		t.Fatalf("want both connections, got %+v", body.Items)
	}
	if _, ok := body.Items[0]["provider"]; !ok {
		t.Fatalf("an operator was answered the picker's shape: %+v", body.Items[0])
	}
	if _, ok := body.Items[0]["createdAt"]; !ok {
		t.Fatalf("an operator was answered the picker's shape: %+v", body.Items[0])
	}

	// Neither shape ever carries a credential, which is the invariant the
	// filtering must not be allowed to look like it introduced.
	if raw, err := json.Marshal(body.Items); err != nil {
		t.Fatal(err)
	} else if strings.Contains(strings.ToLower(string(raw)), "credential") {
		t.Fatalf("a connection answer mentions a credential: %s", raw)
	}
}

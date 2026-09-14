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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/idp"
)

// Personal keys (#593): the credential somebody signs their own automation
// with, and the four things that bound it.

const (
	personalKeysPath = "/api/v1/me/keys"
	// feedProjectPath is the fixtures' project, which the scoped keys below
	// are issued for.
	feedProjectPath = "/api/v1/projects/" + feedProject
	// developerRole is the ceiling most of these keys are issued with: the
	// day job, which is what somebody automating their own work wants.
	developerRole = "developer"
	// scopedToFeed is the request body most of these cases issue: a key for
	// the fixtures' project, at the day job's role.
	scopedToFeed = `{"name": "ci-shop", "role": "developer", "projects": ["shop"]}`
)

// The personal-key half of the stub directory: one flat list per subject,
// because a personal key belongs to an account the platform did not create.
func (d *stubDirectory) PersonalKeys(_ context.Context, subject string) ([]idp.PersonalKey, error) {
	if d.personalKeysErr != nil {
		return nil, d.personalKeysErr
	}
	return append([]idp.PersonalKey(nil), d.personalKeys[subject]...), nil
}

func (d *stubDirectory) CreatePersonalKey(
	_ context.Context,
	subject, name string,
	expires time.Time,
) (*idp.IssuedPersonalKey, error) {
	if d.personalCreateErr != nil {
		return nil, d.personalCreateErr
	}
	for _, existing := range d.personalKeys[subject] {
		if existing.Name == name {
			return nil, idp.ErrKeyExists
		}
	}
	key := idp.PersonalKey{
		Name:    name,
		Subject: subject,
		Email:   testCaller,
		Prefix:  "abc123",
		Created: time.Unix(1, 0).UTC(),
		Expires: expires.UTC(),
	}
	if d.personalKeys == nil {
		d.personalKeys = map[string][]idp.PersonalKey{}
	}
	d.personalKeys[subject] = append(d.personalKeys[subject], key)
	return &idp.IssuedPersonalKey{PersonalKey: key, Secret: "the-personal-key-value"}, nil
}

func (d *stubDirectory) DeletePersonalKey(_ context.Context, subject, name string) (*idp.PersonalKey, error) {
	if d.personalDeleteErr != nil {
		return nil, d.personalDeleteErr
	}
	for i, existing := range d.personalKeys[subject] {
		if existing.Name == name {
			d.personalKeys[subject] = append(d.personalKeys[subject][:i], d.personalKeys[subject][i+1:]...)
			d.personalDeleted = append(d.personalDeleted, name)
			return &existing, nil
		}
	}
	return nil, idp.ErrKeyNotFound
}

// issueKey is a call carrying the token the dashboard holds: one issued to the
// platform's own OAuth client, which is what "somebody signed in" means here
// and the only thing `POST /me/keys` admits.
func (h *harness) issueKey(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(t, http.MethodPost, personalKeysPath, body, h.issuer.tokenFromClient(t, testDashboardClient))
}

// The whole of what the feature is for: somebody signs in, asks for a
// credential, and gets one they can paste into a script — carrying their own
// identity, with a name, an expiry and a way to take it back.
func TestSigningInIsWhatIssuesAPersonalKey(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	directory := h.withDirectory()

	recorder := h.issueKey(t, `{"name": "laptop"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	issued := decode[issuedPersonalKeyView](t, recorder)
	if issued.Key == "" {
		t.Fatal("the one response that carries a credential carried none")
	}
	if issued.Name != "laptop" || issued.Prefix == "" {
		t.Errorf("the answer does not describe the key: %+v", issued)
	}
	if issued.Expires.IsZero() || issued.Expires.Before(time.Now()) {
		t.Errorf("every personal key expires, and this one does not: %+v", issued.Expires)
	}

	// It belongs to the caller's own account, which is the point: the token it
	// is exchanged for carries this subject, and every role they hold is
	// resolved from it.
	stored := directory.personalKeys[testSubject]
	if len(stored) != 1 || stored[0].Subject != testSubject {
		t.Fatalf("the key was not issued for the caller: %+v", stored)
	}

	// And no read gives it back.
	listed := h.do(t, http.MethodGet, personalKeysPath, "")
	if listed.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", listed.Code, listed.Body.String())
	}
	if strings.Contains(listed.Body.String(), issued.Key) {
		t.Error("the value exists in the creation response and nowhere else")
	}
	keys := decode[listBody[personalKeyView]](t, listed).Items
	if len(keys) != 1 || keys[0].Name != "laptop" {
		t.Fatalf("the key is not in its owner's list: %+v", keys)
	}
}

// The rule the platform has always kept about credentials, kept: a credential
// does not mint its own successors. A personal key carries every role its
// holder has, so the chain has to start at somebody signing in — and a token
// exchanged from a key names no OAuth client, which is how that is told.
func TestNoCredentialCanIssueAPersonalKey(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withDirectory()

	// The default token the harness sends is the CI path: minted straight
	// from a session at the issuer, naming no client. That is what a project
	// key, a platform credential and a personal key are all exchanged for.
	recorder := h.do(t, http.MethodPost, personalKeysPath, `{"name": "successor"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "browser sign-in") {
		t.Errorf("the refusal does not say what is missing: %q", got)
	}

	// Reading and revoking are not a widening, so they are not refused: a
	// credential that can revoke a key the moment it leaks should be able to.
	if listed := h.do(t, http.MethodGet, personalKeysPath, ""); listed.Code != http.StatusOK {
		t.Fatalf("listing one's own keys wants a token and nothing else, got %d", listed.Code)
	}
}

// It expires, and how long for is bounded rather than the caller's to choose
// freely: a personal key carries every role its holder has.
func TestAPersonalKeysLifeIsBounded(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	directory := h.withDirectory()

	recorder := h.issueKey(t, `{"name": "nightly", "expiresInDays": 7}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	issued := decode[issuedPersonalKeyView](t, recorder)
	if days := time.Until(issued.Expires).Hours() / 24; days < 6.5 || days > 7.5 {
		t.Errorf("the key does not last the seven days it asked for: %v", issued.Expires)
	}

	// The default is the platform credential's thirty days.
	def := h.issueKey(t, `{"name": "laptop"}`)
	if def.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", def.Code, def.Body.String())
	}
	if days := time.Until(decode[issuedPersonalKeyView](t, def).Expires).Hours() / 24; days < 29 || days > 31 {
		t.Errorf("a key that named no life should last %d days, got %v", defaultPersonalKeyDays, days)
	}

	// And the ceiling is refused rather than quietly clamped: a caller that
	// asked for a year and was given ninety days would find out when the
	// pipeline broke.
	refused := h.issueKey(t, `{"name": "forever", "expiresInDays": 365}`)
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", refused.Code, refused.Body.String())
	}
	if got := errorOf(t, refused.Body.String()); !strings.Contains(got, "at most 90") {
		t.Errorf("the refusal does not name the ceiling: %q", got)
	}
	if len(directory.personalKeys[testSubject]) != 2 {
		t.Errorf("a refused request issued something: %+v", directory.personalKeys[testSubject])
	}
}

// One name is one key, so revoking one is unambiguous — and revoking it is a
// name and one call.
func TestAPersonalKeyIsNamedOnceAndRevokedByName(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	directory := h.withDirectory()

	if r := h.issueKey(t, `{"name": "laptop"}`); r.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", r.Code, r.Body.String())
	}
	again := h.issueKey(t, `{"name": "laptop"}`)
	if again.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", again.Code, again.Body.String())
	}

	revoked := h.do(t, http.MethodDelete, personalKeysPath+"/laptop", "")
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", revoked.Code, revoked.Body.String())
	}
	if len(directory.personalKeys[testSubject]) != 0 {
		t.Errorf("the key is still at the issuer: %+v", directory.personalKeys[testSubject])
	}
	if missing := h.do(t, http.MethodDelete, personalKeysPath+"/laptop", ""); missing.Code != http.StatusNotFound {
		t.Errorf("revoking a key that is not there should be a not-found, got %d", missing.Code)
	}

	// A name that could not be a key's is refused before anything is asked of
	// the issuer, because the name is a path segment and the key's address.
	bad := h.issueKey(t, `{"name": "My Laptop"}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", bad.Code, bad.Body.String())
	}
}

// A caller that is not a person holds no personal keys, and is told so as an
// empty list rather than as a fault: `kitchen keys list` on a CI key should
// answer the question it was asked.
func TestACredentialHoldsNoPersonalKeys(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	directory := h.withDirectory()
	directory.personalKeysErr = idp.ErrNotAPerson

	recorder := h.do(t, http.MethodGet, personalKeysPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if keys := decode[listBody[personalKeyView]](t, recorder).Items; len(keys) != 0 {
		t.Errorf("a credential was answered with keys: %+v", keys)
	}
}

// Fine-grained keys (#595): a key scoped to named projects, at most a named
// role inside them.

// asKey is a call carrying a token minted from one of the caller's personal
// keys — the credential, rather than the person at a browser.
func (h *harness) asKey(t *testing.T, key, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(t, method, path, body, h.issuer.tokenFromKey(t, key))
}

// The whole of the feature: somebody who administers two projects issues a key
// that may deploy one of them, and the platform holds it to that.
func TestAFineGrainedKeyIsScopedToItsProjectsAndRole(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withDirectory()
	h.grant(t, feedProject, kitchenv1alpha1.AccessRoleAdmin)

	issued := h.issueKey(t, scopedToFeed)
	if issued.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", issued.Code, issued.Body.String())
	}
	view := decode[issuedPersonalKeyView](t, issued)
	if view.Scope == nil || view.Scope.Role != developerRole {
		t.Fatalf("the answer does not say what the key may do: %+v", view.Scope)
	}
	if len(view.Scope.Projects) != 1 || view.Scope.Projects[0] != feedProject {
		t.Errorf("the answer does not say which projects: %+v", view.Scope.Projects)
	}

	// Through the key: it may read the project it names…
	if got := h.asKey(t, "ci-shop", http.MethodGet, feedProjectPath, ""); got.Code != http.StatusOK {
		t.Fatalf("the key cannot read the project it was issued for: %d %s", got.Code, got.Body.String())
	}
	// …and may not do what only an admin may, however much its owner may.
	refused := h.asKey(t, "ci-shop", http.MethodPatch, feedProjectPath, `{"replicas": 3}`)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", refused.Code, refused.Body.String())
	}
	if got := errorOf(t, refused.Body.String()); !strings.Contains(got, developerRole) {
		t.Errorf("the refusal does not name the role the key holds: %q", got)
	}

	// A project the key does not name is a project it cannot see at all,
	// which is the answer every caller who holds no role on one gets.
	if got := h.asKey(t, "ci-shop", http.MethodGet, "/api/v1/projects/blog", ""); got.Code != http.StatusNotFound {
		t.Errorf("a key reached a project it was not issued for: %d", got.Code)
	}

	// And the person is untouched: signed in, they are still an admin.
	if got := h.do(t, http.MethodPatch, feedProjectPath, `{"replicas": 3}`); got.Code != http.StatusOK {
		t.Fatalf("narrowing a key narrowed the person: %d %s", got.Code, got.Body.String())
	}
}

// A key cannot be issued above what its owner holds. Refused at the door
// rather than clamped at resolution: a key issued as admin that silently
// resolves to developer is a key whose first failure is somebody else's
// outage.
func TestAKeyCannotBeIssuedAboveWhatItsOwnerHolds(t *testing.T) {
	// A member rather than an operator: an operator holds admin on every
	// project, so there is nothing for a ceiling to be above.
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	h.withDirectory()

	refused := h.issueKey(t, `{"name": "too-much", "role": "admin", "projects": ["shop"]}`)
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", refused.Code, refused.Body.String())
	}
	if got := errorOf(t, refused.Body.String()); !strings.Contains(got, "you hold developer on shop") {
		t.Errorf("the refusal does not say what the caller holds: %q", got)
	}

	// And a project the caller cannot see is answered as one that is not
	// there, exactly as every other unreadable project is.
	missing := h.issueKey(t, `{"name": "elsewhere", "role": "viewer", "projects": ["blog"]}`)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", missing.Code, missing.Body.String())
	}
}

// Naming projects or scopes without a role is a request that meant to narrow
// and did not say how. It is refused rather than granted everything.
func TestNarrowingWithoutARoleIsRefused(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withDirectory()
	h.grant(t, feedProject, kitchenv1alpha1.AccessRoleAdmin)

	refused := h.issueKey(t, `{"name": "half-said", "projects": ["shop"]}`)
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", refused.Code, refused.Body.String())
	}
	if got := errorOf(t, refused.Body.String()); !strings.Contains(got, "role is required") {
		t.Errorf("the refusal does not name what is missing: %q", got)
	}
}

// Platform scopes on a key are an operator's to hand out: a key cannot carry
// what its owner could not.
func TestOnlyAnOperatorMayPutScopesOnAKey(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	h.withDirectory()

	refused := h.issueKey(t, `{"name": "backups", "role": "viewer", "scopes": ["backup.run"]}`)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", refused.Code, refused.Body.String())
	}
	if got := errorOf(t, refused.Body.String()); !strings.Contains(got, "operator role") {
		t.Errorf("the refusal does not say what is needed: %q", got)
	}
}

// A narrowed key may not create a project: its creator becomes the new
// project's admin, which is the one act that would widen a credential issued
// to reach two projects.
func TestANarrowedKeyMayNotCreateAProject(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withDirectory()
	h.grant(t, feedProject, kitchenv1alpha1.AccessRoleAdmin)

	if issued := h.issueKey(t, scopedToFeed); issued.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", issued.Code, issued.Body.String())
	}

	refused := h.asKey(t, "ci-shop", http.MethodPost, "/api/v1/projects",
		`{"name": "new", "repo": "acme/new", "connection": "gh", "registry": "`+testRegistry+`"}`)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", refused.Code, refused.Body.String())
	}
	if got := errorOf(t, refused.Body.String()); !strings.Contains(got, "narrowed personal key") {
		t.Errorf("the refusal does not say why: %q", got)
	}

	// An unrestricted key is its owner, so it may.
	if issued := h.issueKey(t, `{"name": "laptop"}`); issued.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", issued.Code, issued.Body.String())
	}
	allowed := h.asKey(t, "laptop", http.MethodPost, "/api/v1/projects",
		`{"name": "new", "repo": "acme/new", "connection": "gh", "registry": "`+testRegistry+`"}`)
	if allowed.Code == http.StatusForbidden {
		t.Errorf("an unrestricted key was refused what its owner may do: %s", allowed.Body.String())
	}
}

// A key the platform has no grant for holds nothing — and is told so, rather
// than being told it has no role on a project it administers.
func TestAKeyThePlatformDoesNotRecogniseIsToldSo(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withDirectory()
	h.grant(t, feedProject, kitchenv1alpha1.AccessRoleAdmin)

	refused := h.asKey(t, "revoked", http.MethodGet, feedProjectPath, "")
	if refused.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", refused.Code, refused.Body.String())
	}
	if got := errorOf(t, refused.Body.String()); !strings.Contains(got, "not one this platform recognises") {
		t.Errorf("the refusal does not say what is wrong: %q", got)
	}

	// And `/me` says the same thing, which is where `kitchen whoami` reads it.
	me := decode[meView](t, h.asKey(t, "revoked", http.MethodGet, "/api/v1/me", ""))
	if me.Key == nil || !me.Key.Unknown {
		t.Errorf("/me does not report the key as unrecognised: %+v", me.Key)
	}
}

// Revoking takes both halves: the credential at the issuer and what the
// platform allowed it to do.
func TestRevokingAKeyTakesItsGrantWithIt(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withDirectory()
	h.grant(t, feedProject, kitchenv1alpha1.AccessRoleAdmin)

	if issued := h.issueKey(t, scopedToFeed); issued.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", issued.Code, issued.Body.String())
	}
	if revoked := h.do(t, http.MethodDelete, personalKeysPath+"/ci-shop", ""); revoked.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", revoked.Code, revoked.Body.String())
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(t.Context(), types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if len(kitchen.Spec.Access.PersonalKeys) != 0 {
		t.Errorf("the grant outlived the key: %+v", kitchen.Spec.Access.PersonalKeys)
	}

	// A token minted before the revocation holds nothing now.
	if got := h.asKey(t, "ci-shop", http.MethodGet, feedProjectPath, ""); got.Code != http.StatusForbidden {
		t.Errorf("a revoked key still reaches the platform: %d", got.Code)
	}
}

// `/me` describes the credential in hand, which is what `kitchen whoami`
// prints — and the question "why can this token not do what I can" has no
// other answer.
func TestMeDescribesTheKeyInHand(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withDirectory()
	h.grant(t, feedProject, kitchenv1alpha1.AccessRoleAdmin)

	if issued := h.issueKey(t, scopedToFeed); issued.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", issued.Code, issued.Body.String())
	}

	me := decode[meView](t, h.asKey(t, "ci-shop", http.MethodGet, "/api/v1/me", ""))
	if me.Key == nil || me.Key.Name != "ci-shop" || me.Key.Role != developerRole {
		t.Fatalf("/me does not describe the key: %+v", me.Key)
	}
	if len(me.Key.Projects) != 1 || me.Key.Projects[0] != feedProject {
		t.Errorf("/me does not say which projects: %+v", me.Key)
	}

	// Signed in, the same account is holding no key at all.
	if signedIn := decode[meView](t, h.do(t, http.MethodGet, "/api/v1/me", "")); signedIn.Key != nil {
		t.Errorf("a browser session reports a key: %+v", signedIn.Key)
	}
}

// A lapsed key is reported as lapsed rather than left for the reader to work
// out from two dates. The issuer deletes the row when it is next presented, so
// this is the window in between.
func TestALapsedPersonalKeyReadsAsLapsed(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	directory := h.withDirectory()
	directory.personalKeys = map[string][]idp.PersonalKey{testSubject: {{
		Name:    "old",
		Subject: testSubject,
		Email:   testCaller,
		Prefix:  "abc123",
		Created: time.Now().Add(-200 * 24 * time.Hour),
		Expires: time.Now().Add(-24 * time.Hour),
	}}}

	recorder := h.do(t, http.MethodGet, personalKeysPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	keys := decode[listBody[personalKeyView]](t, recorder).Items
	if len(keys) != 1 || !keys[0].Expired {
		t.Fatalf("a lapsed key does not read as lapsed: %+v", keys)
	}
}

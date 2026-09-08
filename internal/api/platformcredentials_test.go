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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/idp"
)

// What a platform credential promises: it reaches the routes its scopes name
// and no others, it stops working when it lapses without anything having to
// run, it cannot mint another one, and revoking it removes both halves.

const (
	// agentSubject is the account a platform credential owns, and agentEmail
	// its address under the reserved platform domain.
	agentSubject = "user_agent"
	agentName    = "nightly"
	credsPath    = "/api/v1/platform/credentials"
)

var agentEmail = idp.PlatformAddress(agentName)

func (d *stubDirectory) PlatformKeys(_ context.Context) ([]idp.PlatformKey, error) {
	if d.platformKeysErr != nil {
		return nil, d.platformKeysErr
	}
	return append([]idp.PlatformKey(nil), d.platformKeys...), nil
}

func (d *stubDirectory) CreatePlatformKey(_ context.Context, name string) (*idp.IssuedPlatformKey, error) {
	if d.platformCreateErr != nil {
		return nil, d.platformCreateErr
	}
	for _, existing := range d.platformKeys {
		if existing.Name == name {
			return nil, idp.ErrKeyExists
		}
	}
	key := idp.PlatformKey{
		Name:    name,
		Subject: agentSubject,
		Email:   idp.PlatformAddress(name),
		Prefix:  "abc123",
		Created: time.Unix(1, 0).UTC(),
	}
	d.platformKeys = append(d.platformKeys, key)
	return &idp.IssuedPlatformKey{PlatformKey: key, Secret: "the-credential-value"}, nil
}

func (d *stubDirectory) DeletePlatformKey(_ context.Context, name string) (*idp.PlatformKey, error) {
	if d.platformDeleteErr != nil {
		return nil, d.platformDeleteErr
	}
	for i, existing := range d.platformKeys {
		if existing.Name == name {
			d.platformKeys = append(d.platformKeys[:i], d.platformKeys[i+1:]...)
			d.platformDeleted = append(d.platformDeleted, name)
			return &existing, nil
		}
	}
	return nil, idp.ErrKeyNotFound
}

// credential puts a live grant on the singleton and returns a token for it,
// which is how these tests say "the caller is a platform credential holding
// these scopes".
func (h *harness) credential(t *testing.T, scopes []kitchenv1alpha1.PlatformScope, options ...func(
	*kitchenv1alpha1.PlatformCredential),
) string {
	t.Helper()
	entry := kitchenv1alpha1.PlatformCredential{
		AccessSubject: kitchenv1alpha1.AccessSubject{Subject: agentSubject, Email: agentEmail},
		Scopes:        scopes,
		Expires:       metav1.NewTime(time.Now().Add(24 * time.Hour)),
	}
	for _, option := range options {
		option(&entry)
	}
	kitchen := &kitchenv1alpha1.Kitchen{}
	key := types.NamespacedName{Name: controller.KitchenSingletonName}
	if err := h.server.Client.Get(context.Background(), key, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Spec.Access.Credentials = append(kitchen.Spec.Access.Credentials, entry)
	if err := h.server.Client.Update(context.Background(), kitchen); err != nil {
		t.Fatal(err)
	}
	return h.issuer.tokenFor(t, agentSubject, agentEmail)
}

// storedCredentials is what the singleton grants, read back off the object.
func (h *harness) storedCredentials(t *testing.T) []kitchenv1alpha1.PlatformCredential {
	t.Helper()
	kitchen := &kitchenv1alpha1.Kitchen{}
	key := types.NamespacedName{Name: controller.KitchenSingletonName}
	if err := h.server.Client.Get(context.Background(), key, kitchen); err != nil {
		t.Fatal(err)
	}
	return kitchen.Spec.Access.Credentials
}

// The whole of what the feature is for: a credential that is not an operator
// reaches a platform route because its scopes name it.
func TestAPlatformCredentialReachesTheRoutesItsScopesName(t *testing.T) {
	h := asMember(t, "")
	token := h.credential(t, []kitchenv1alpha1.PlatformScope{kitchenv1alpha1.PlatformScopeRead})

	recorder := h.do(t, http.MethodGet, "/api/v1/platform/retention", "", token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want the retention read to be admitted, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// And the other half, which is the point: a scope is not the operator role.
// The credential above holds `platform.read` and the backup is somebody else's.
func TestAPlatformCredentialIsRefusedARouteOutsideItsScopes(t *testing.T) {
	h := asMember(t, "")
	token := h.credential(t, []kitchenv1alpha1.PlatformScope{kitchenv1alpha1.PlatformScopeRead})

	recorder := h.do(t, http.MethodPost, "/api/v1/platform/backup", "", token)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// The refusal names the scope that was wanted and the one that is held,
	// the way an operator-only refusal names the role.
	body := recorder.Body.String()
	if !strings.Contains(body, string(kitchenv1alpha1.PlatformScopeBackupRun)) ||
		!strings.Contains(body, string(kitchenv1alpha1.PlatformScopeRead)) {
		t.Fatalf("the refusal names neither the scope wanted nor the one held: %s", body)
	}
}

// A route no row marked with a scope is the operator's, whatever a credential
// holds. This is the default-deny the whole design rests on.
func TestNoScopeReachesAnUnscopedPlatformRoute(t *testing.T) {
	h := asMember(t, "")
	token := h.credential(t, []kitchenv1alpha1.PlatformScope{
		kitchenv1alpha1.PlatformScopeRead,
		kitchenv1alpha1.PlatformScopeComplianceRead,
		kitchenv1alpha1.PlatformScopeBackupRun,
	})

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/settings"},
		{http.MethodPatch, "/api/v1/settings"},
		{http.MethodGet, "/api/v1/updates"},
		{http.MethodGet, credsPath},
		{http.MethodPost, credsPath},
	} {
		recorder := h.do(t, route.method, route.path, "{}", token)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s %s: want 403 for a credential holding every scope, got %d: %s",
				route.method, route.path, recorder.Code, recorder.Body.String())
		}
	}
}

// The expiry is enforced where the scope is resolved, so a lapsed credential
// stops working with nothing having had to run.
func TestALapsedPlatformCredentialHoldsNothing(t *testing.T) {
	h := asMember(t, "")
	token := h.credential(t, []kitchenv1alpha1.PlatformScope{kitchenv1alpha1.PlatformScopeRead},
		func(entry *kitchenv1alpha1.PlatformCredential) {
			entry.Expires = metav1.NewTime(time.Now().Add(-time.Minute))
		})

	recorder := h.do(t, http.MethodGet, "/api/v1/platform/retention", "", token)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want a lapsed credential refused, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// It reads as somebody holding nothing rather than as somebody holding the
	// wrong scope, because that is what it is.
	if !strings.Contains(recorder.Body.String(), "needs the operator role") {
		t.Fatalf("want the plain operator refusal for a lapsed credential, got %s", recorder.Body.String())
	}
}

// The project allowlist narrows the one scoped route that is about a project.
func TestANarrowedCredentialReachesOnlyItsProjects(t *testing.T) {
	h := asMember(t, "", blogFixtures()...)
	token := h.credential(t, []kitchenv1alpha1.PlatformScope{kitchenv1alpha1.PlatformScopeComplianceRead},
		func(entry *kitchenv1alpha1.PlatformCredential) { entry.Projects = []string{feedProject} })

	if recorder := h.do(t, http.MethodGet,
		"/api/v1/projects/"+otherProject+"/audit-pack", "", token); recorder.Code != http.StatusForbidden {
		t.Fatalf("want the project it was narrowed away from refused, got %d: %s",
			recorder.Code, recorder.Body.String())
	}
	// And the one it was narrowed to is not refused. The pack itself needs a
	// telemetry store this harness has not got, so what is asserted is that
	// the guard let it through to the handler rather than what the handler
	// then made of it.
	if recorder := h.do(t, http.MethodGet,
		"/api/v1/projects/"+feedProject+"/audit-pack", "", token); recorder.Code == http.StatusForbidden {
		t.Fatalf("want the project it was narrowed to admitted, got %d: %s",
			recorder.Code, recorder.Body.String())
	}
}

// A credential is still a machine account, so the one thing no credential may
// ever do — create a project it would be admin of — stays refused.
func TestAPlatformCredentialCannotCreateAProject(t *testing.T) {
	h := asMember(t, "")
	token := h.credential(t, []kitchenv1alpha1.PlatformScope{kitchenv1alpha1.PlatformScopeRead})

	recorder := h.do(t, http.MethodPost, "/api/v1/projects", `{"name": "mine"}`, token)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// Issuing one writes both halves: the credential at the issuer, and the grant
// that makes it worth holding.
func TestIssuingAPlatformCredentialWritesBothHalves(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	directory := h.withDirectory()

	recorder := h.do(t, http.MethodPost, credsPath,
		`{"name": "`+agentName+`", "scopes": ["platform.read"], "expiresInDays": 7}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	issued := decode[issuedCredentialView](t, recorder)
	if issued.Key == "" {
		t.Fatal("the one response that carries the credential did not carry it")
	}
	if len(issued.Scopes) != 1 || issued.Scopes[0] != string(kitchenv1alpha1.PlatformScopeRead) {
		t.Fatalf("want the scope it was issued with, got %v", issued.Scopes)
	}
	if len(directory.platformKeys) != 1 {
		t.Fatalf("the credential was not created at the issuer: %v", directory.platformKeys)
	}
	stored := h.storedCredentials(t)
	if len(stored) != 1 || stored[0].Subject != agentSubject {
		t.Fatalf("the grant is not on the singleton: %v", stored)
	}
	if stored[0].Expires.IsZero() {
		t.Fatal("a credential was granted with no expiry")
	}
}

// And no read ever answers with the value again.
func TestNoReadAnswersWithACredential(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withDirectory()

	if recorder := h.do(t, http.MethodPost, credsPath,
		`{"name": "`+agentName+`", "scopes": ["platform.read"]}`); recorder.Code != http.StatusCreated {
		t.Fatalf("issuing: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder := h.do(t, http.MethodGet, credsPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "the-credential-value") {
		t.Fatalf("a read answered with the credential: %s", recorder.Body.String())
	}
}

// Revoking removes both halves too.
func TestRevokingAPlatformCredentialRemovesBothHalves(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	directory := h.withDirectory()

	if recorder := h.do(t, http.MethodPost, credsPath,
		`{"name": "`+agentName+`", "scopes": ["platform.read"]}`); recorder.Code != http.StatusCreated {
		t.Fatalf("issuing: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder := h.do(t, http.MethodDelete, credsPath+"/"+agentName, "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(directory.platformDeleted) != 1 {
		t.Fatalf("the credential was not revoked at the issuer: %v", directory.platformDeleted)
	}
	if stored := h.storedCredentials(t); len(stored) != 0 {
		t.Fatalf("the grant is still on the singleton: %v", stored)
	}
}

// A grant left behind by an interrupted revocation is this route's to remove,
// because the alternative is kubectl.
func TestRevokingRemovesAGrantWhoseCredentialIsAlreadyGone(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withDirectory()
	h.credential(t, []kitchenv1alpha1.PlatformScope{kitchenv1alpha1.PlatformScopeRead})

	recorder := h.do(t, http.MethodDelete, credsPath+"/"+agentName, "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if stored := h.storedCredentials(t); len(stored) != 0 {
		t.Fatalf("the orphaned grant is still there: %v", stored)
	}
}

// The refusals a bad request gets, each of which exists because the silent
// alternative is worse.
func TestIssuingAPlatformCredentialRefusesWhatItCannotHonour(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withDirectory()

	for _, refusal := range []struct{ name, body, says string }{
		{"no scopes", `{"name": "a", "scopes": []}`, "at least one scope"},
		{"an unknown scope", `{"name": "a", "scopes": ["platform.write"]}`, "not a platform scope"},
		{"too long a life", `{"name": "a", "scopes": ["platform.read"], "expiresInDays": 400}`, "at most"},
		{"a project that is not there", `{"name": "a", "scopes": ["compliance.read"], "projects": ["nope"]}`,
			"no project called"},
		{"a name that is not a label", `{"name": "Not A Label", "scopes": ["platform.read"]}`, "name must be"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, credsPath, refusal.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), refusal.says) {
				t.Fatalf("want a refusal saying %q, got %s", refusal.says, recorder.Body.String())
			}
		})
	}
}

// policyRows is the enforcement table as data, which is how the three rules
// below are asserted about the table itself rather than about a list of routes
// somebody has to remember to keep in step with it.
func policyRows(t *testing.T) []PolicyRoute {
	t.Helper()
	policy, err := PolicyTable()
	if err != nil {
		t.Fatal(err)
	}
	return policy.Routes
}

// The table's own promise, held here rather than in a comment: no scope
// reaches a route that would let a credential mint, widen or outlive itself.
//
// It is a rule about patterns rather than a list of them, so a route added
// later under any of these prefixes is caught without anybody remembering to
// add it to a fixture.
func TestNoScopeReachesCredentialIssuance(t *testing.T) {
	forbidden := []string{
		"/api/v1/platform/credentials",
		"/api/v1/projects/{name}/keys",
		"/api/v1/settings",
		"/api/v1/updates",
		"/api/v1/connections",
	}
	for _, row := range policyRows(t) {
		if row.Scope == "" {
			continue
		}
		_, path, _ := strings.Cut(row.Pattern, " ")
		for _, prefix := range forbidden {
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				t.Errorf("%s carries the scope %s: a credential that can reach it can mint, widen or "+
					"outlive itself", row.Pattern, row.Scope)
			}
		}
	}
}

// Every scope a row names is a scope that exists, and every scope that exists
// is reachable by something. The first half stops a typo becoming a route
// nobody can reach; the second stops a scope being offered by the dashboard
// and reaching nothing at all.
func TestEveryScopeIsRealAndReachable(t *testing.T) {
	reached := map[access.Scope]int{}
	for _, row := range policyRows(t) {
		if row.Scope == "" {
			continue
		}
		if _, ok := access.ParseScope(row.Scope); !ok {
			t.Errorf("%s names %q, which is not a platform scope", row.Pattern, row.Scope)
			continue
		}
		reached[access.Scope(row.Scope)]++
	}
	for _, scope := range access.Scopes() {
		if reached[scope] == 0 {
			t.Errorf("no route names the scope %s, so a credential holding it can do nothing", scope)
		}
	}
}

// A scoped row that is about a project carries the resolver its allowlist is
// checked against. Without one the narrowing would silently not apply, which
// is the failure that looks exactly like it working.
func TestEveryProjectShapedScopedRouteResolvesItsProject(t *testing.T) {
	for _, row := range policyRows(t) {
		if row.Scope == "" || !strings.Contains(row.Pattern, "/projects/{name}") {
			continue
		}
		if row.ResolvesProject {
			continue
		}
		t.Errorf("%s is scoped %s and about a project, but resolves none: a credential's project "+
			"allowlist would not apply to it", row.Pattern, row.Scope)
	}
}

// The Kitchen singleton a credential test writes to, read through the same
// client the guard reads it through — a sanity check that the fake client's
// list-map key does not collapse two entries into one.
func TestTwoCredentialsCoexistOnTheSingleton(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	kitchen := &kitchenv1alpha1.Kitchen{}
	key := types.NamespacedName{Name: controller.KitchenSingletonName}
	if err := h.server.Client.Get(context.Background(), key, kitchen); err != nil {
		t.Fatal(err)
	}
	base := kitchen.DeepCopy()
	kitchen.Spec.Access.Credentials = []kitchenv1alpha1.PlatformCredential{
		{
			AccessSubject: kitchenv1alpha1.AccessSubject{Subject: "one", Email: idp.PlatformAddress("one")},
			Scopes:        []kitchenv1alpha1.PlatformScope{kitchenv1alpha1.PlatformScopeRead},
			Expires:       metav1.NewTime(time.Now().Add(time.Hour)),
		},
		{
			AccessSubject: kitchenv1alpha1.AccessSubject{Subject: "two", Email: idp.PlatformAddress("two")},
			Scopes:        []kitchenv1alpha1.PlatformScope{kitchenv1alpha1.PlatformScopeBackupRun},
			Expires:       metav1.NewTime(time.Now().Add(time.Hour)),
		},
	}
	if err := h.server.Client.Patch(context.Background(), kitchen, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	if stored := h.storedCredentials(t); len(stored) != 2 {
		t.Fatalf("want two credentials, got %v", stored)
	}
}

// `GET /me` is how a credential finds out what it is and what it holds — it
// has no other way, because what marks one is a convention of the identity
// provider's that no client should be reimplementing.
func TestMeDescribesACredentialToItself(t *testing.T) {
	h := asMember(t, "")
	token := h.credential(t, []kitchenv1alpha1.PlatformScope{kitchenv1alpha1.PlatformScopeRead},
		func(entry *kitchenv1alpha1.PlatformCredential) { entry.Projects = []string{feedProject} })

	me := decode[meView](t, h.do(t, http.MethodGet, "/api/v1/me", "", token))
	if me.Kind != callerKindCredential {
		t.Fatalf("want kind %q, got %q", callerKindCredential, me.Kind)
	}
	if len(me.Scopes) != 1 || me.Scopes[0] != string(kitchenv1alpha1.PlatformScopeRead) {
		t.Fatalf("want the scope it holds, got %v", me.Scopes)
	}
	if len(me.Projects) != 1 || me.Projects[0] != feedProject {
		t.Fatalf("want the narrowing reported, got %v", me.Projects)
	}
	// And it is a member, not an operator: a credential holds no role at all.
	if me.PlatformRole != "member" {
		t.Fatalf("want a credential to hold no platform role, got %q", me.PlatformRole)
	}
}

// A lapsed credential is told it holds nothing, which is the answer somebody
// is looking for when a scheduled job starts getting 403s.
func TestMeTellsALapsedCredentialItHoldsNothing(t *testing.T) {
	h := asMember(t, "")
	token := h.credential(t, []kitchenv1alpha1.PlatformScope{kitchenv1alpha1.PlatformScopeRead},
		func(entry *kitchenv1alpha1.PlatformCredential) {
			entry.Expires = metav1.NewTime(time.Now().Add(-time.Minute))
		})

	me := decode[meView](t, h.do(t, http.MethodGet, "/api/v1/me", "", token))
	if me.Kind != callerKindCredential {
		t.Fatalf("want kind %q, got %q", callerKindCredential, me.Kind)
	}
	if len(me.Scopes) != 0 {
		t.Fatalf("want a lapsed credential to hold nothing, got %v", me.Scopes)
	}
}

// A person is a person, and holds no scopes to report.
func TestMeCallsAPersonAPerson(t *testing.T) {
	h := asMember(t, "")

	me := decode[meView](t, h.do(t, http.MethodGet, "/api/v1/me", ""))
	if me.Kind != callerKindPerson {
		t.Fatalf("want kind %q, got %q", callerKindPerson, me.Kind)
	}
	if len(me.Scopes) != 0 {
		t.Fatalf("a person holds a role, not scopes: %v", me.Scopes)
	}
}

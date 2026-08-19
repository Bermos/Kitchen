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
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// What the enforcement table promises, from the outside: each requirement kind
// refused and allowed, the refusal naming the role it wanted, and the two
// payloads that vary by who is asking.
//
// The harness's caller is an operator by default, which is what keeps every
// test written before enforcement meaningful. These tests take that away: they
// hand the operator list to somebody else and then grant the caller exactly
// the role the case is about.

// demoteCaller makes the harness's caller an ordinary member by naming a
// stranger as the platform's only operator.
func (h *harness) demoteCaller(t *testing.T) {
	t.Helper()
	h.updateKitchen(t, func(kitchen *kitchenv1alpha1.Kitchen) {
		kitchen.Spec.Access.Operators = []kitchenv1alpha1.AccessSubject{
			{Subject: "user_0", Email: "chef@example.com"},
		}
	})
}

// grant gives the harness's caller a role on a project.
func (h *harness) grant(t *testing.T, project string, role kitchenv1alpha1.AccessRole) {
	t.Helper()
	obj := &kitchenv1alpha1.Project{}
	key := types.NamespacedName{Namespace: testNamespace, Name: project}
	if err := h.server.Client.Get(context.Background(), key, obj); err != nil {
		t.Fatal(err)
	}
	obj.Spec.Access = append(obj.Spec.Access, kitchenv1alpha1.AccessGrant{
		AccessSubject: kitchenv1alpha1.AccessSubject{Subject: testSubject, Email: testCaller},
		Role:          role,
	})
	if err := h.server.Client.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) updateKitchen(t *testing.T, edit func(*kitchenv1alpha1.Kitchen)) {
	t.Helper()
	kitchen := &kitchenv1alpha1.Kitchen{}
	key := types.NamespacedName{Name: controller.KitchenSingletonName}
	if err := h.server.Client.Get(context.Background(), key, kitchen); err != nil {
		t.Fatal(err)
	}
	edit(kitchen)
	if err := h.server.Client.Update(context.Background(), kitchen); err != nil {
		t.Fatal(err)
	}
}

// asMember is the everyday setup for these tests: the fixtures, somebody else
// as the operator, and the caller holding `role` on `shop` — or nothing at
// all, for the empty role.
func asMember(t *testing.T, role kitchenv1alpha1.AccessRole, extra ...runtime.Object) *harness {
	t.Helper()
	h := newHarness(t, nil, append(fixtures(), extra...)...)
	h.demoteCaller(t)
	if role != "" {
		h.grant(t, feedProject, role)
	}
	return h
}

// blogFixtures are a second project's objects: the ones a member on `shop`
// must never be answered about.
func blogFixtures() []runtime.Object {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: otherProject, Namespace: testNamespace},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source: kitchenv1alpha1.GitSourceSpec{
				ConnectionRef:    kitchenv1alpha1.LocalObjectReference{Name: "gh"},
				Repo:             "acme/blog",
				ProductionBranch: defaultProductionBranch,
			},
			Registry: kitchenv1alpha1.RegistrySpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
			},
		},
	}
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "blog-bld-000000000000", Namespace: testNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: otherProject},
			Git:        kitchenv1alpha1.GitRevision{SHA: "000000000000", Branch: defaultProductionBranch},
		},
	}
	environment := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "blog-production", Namespace: testNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: otherProject},
			Type:       kitchenv1alpha1.EnvironmentProduction,
			ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: "blog-rel-0"},
		},
	}
	return []runtime.Object{project, build, environment}
}

// errorOf reads the message off a refusal.
func errorOf(t *testing.T, body string) string {
	t.Helper()
	answer := errorBody{}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("unreadable error body %q: %v", body, err)
	}
	return answer.Error
}

func TestThePlatformsOwnSurfaceIsTheOperators(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)

	// Admin on every project is still not the platform.
	for _, route := range []struct {
		method, path, doing string
	}{
		{http.MethodGet, "/api/v1/settings", "reading the platform's settings"},
		{http.MethodPatch, "/api/v1/settings", "changing the platform's settings"},
		// The connection *list* is the picker every project is created from,
		// so it answers a member a thinned body rather than a refusal (see
		// TestAMemberPicksAConnectionWithoutSeeingOne). Everything else under
		// /connections is still the operator's, credentials and all.
		{http.MethodGet, "/api/v1/connections/gh", "reading a connection"},
		{http.MethodPatch, "/api/v1/connections/gh", "changing a connection"},
		{http.MethodDelete, "/api/v1/connections/gh", "deleting a connection"},
		{http.MethodPost, "/api/v1/connections/test", "testing a connection"},
		{http.MethodGet, "/api/v1/updates", "reading the platform's updates"},
		{http.MethodGet, "/api/v1/platform/nodes", "reading the platform's nodes"},
		{http.MethodGet, "/api/v1/compliance", "reading the platform's compliance posture"},
		{http.MethodGet, "/api/v1/environments/" + testEnvironment + "/objects",
			"reading an environment's Kubernetes objects"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			recorder := h.do(t, route.method, route.path, "")
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
			}
			want := route.doing + " needs the operator role; you are a member"
			if got := errorOf(t, recorder.Body.String()); got != want {
				t.Fatalf("want %q, got %q", want, got)
			}
		})
	}
}

func TestTheOperatorReachesThePlatformsOwnSurface(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/settings", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAViewerReadsAndDoesNotWrite(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)

	if recorder := h.do(t, http.MethodGet, "/api/v1/environments/"+testEnvironment, ""); recorder.Code != http.StatusOK {
		t.Fatalf("a viewer must be able to read an environment: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodPatch, "/api/v1/environments/"+testEnvironment,
		`{"release":"`+testPreviousRelease+`"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// The refusal every other one is built the same way as: what you have,
	// what you were doing, and what it would have taken.
	want := "you have viewer on shop; redeploying needs developer"
	if got := errorOf(t, recorder.Body.String()); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestADeveloperDeploysAndDoesNotAdminister(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)

	if recorder := h.do(t, http.MethodPatch, "/api/v1/environments/"+testEnvironment,
		`{"release":"`+testPreviousRelease+`"}`); recorder.Code != http.StatusOK {
		t.Fatalf("a developer must be able to redeploy: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodPost, "/api/v1/projects/shop/builds", ""); recorder.Code != http.StatusCreated {
		t.Fatalf("a developer must be able to start a build: %d %s", recorder.Code, recorder.Body.String())
	}

	for _, route := range []struct {
		method, path, body, doing, needs string
	}{
		{http.MethodDelete, "/api/v1/projects/shop", "", "deleting a project", "admin"},
		{http.MethodPatch, "/api/v1/projects/shop", `{"previews":false}`, "changing a project's settings", "admin"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			recorder := h.do(t, route.method, route.path, route.body)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
			}
			want := "you have developer on shop; " + route.doing + " needs " + route.needs
			if got := errorOf(t, recorder.Body.String()); got != want {
				t.Fatalf("want %q, got %q", want, got)
			}
		})
	}
}

func TestAnAdminAdministersTheirOwnProject(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)

	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"previews":false}`); recorder.Code != http.StatusOK {
		t.Fatalf("an admin must be able to change their project: %d %s", recorder.Code, recorder.Body.String())
	}
}

// The rule the whole surface is consistent about: an object belonging to a
// project the caller holds no role on does not exist as far as they are
// concerned. Not 403 — that would say it is there.
func TestAnObjectTheCallerHoldsNoRoleOnIsNotFound(t *testing.T) {
	h := asMember(t, "")

	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/projects/shop", ""},
		{http.MethodDelete, "/api/v1/projects/shop", ""},
		{http.MethodGet, "/api/v1/projects/shop/builds", ""},
		{http.MethodGet, "/api/v1/builds/" + testBuild, ""},
		{http.MethodPost, "/api/v1/builds/" + testBuild + "/cancel", ""},
		{http.MethodGet, "/api/v1/releases/" + testRelease, ""},
		{http.MethodGet, "/api/v1/environments/" + testEnvironment, ""},
		{http.MethodDelete, "/api/v1/environments/" + testEnvironment, ""},
		{http.MethodGet, "/api/v1/environments/" + testEnvironment + "/logs", ""},
		{http.MethodGet, "/api/v1/domains/shop-com", ""},
		{http.MethodGet, "/api/v1/claims/shop-db", ""},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			recorder := h.do(t, route.method, route.path, route.body)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
			}
			// And in the same words a genuinely missing object gets, so the
			// two cannot be told apart.
			if message := errorOf(t, recorder.Body.String()); !strings.HasSuffix(message, "not found") {
				t.Fatalf("want the API server's own not-found, got %q", message)
			}
		})
	}
}

func TestAMissingObjectAndAnInvisibleOneAnswerAlike(t *testing.T) {
	h := asMember(t, "")

	invisible := h.do(t, http.MethodGet, "/api/v1/builds/"+testBuild, "")
	missing := h.do(t, http.MethodGet, "/api/v1/builds/shop-bld-ffffffffffff", "")
	if invisible.Code != missing.Code {
		t.Fatalf("want the same status, got %d and %d", invisible.Code, missing.Code)
	}
	// The names differ, of course; the shape of the sentence must not.
	if !strings.HasPrefix(errorOf(t, invisible.Body.String()), "builds.kitchen.bermos.dev") ||
		!strings.HasPrefix(errorOf(t, missing.Body.String()), "builds.kitchen.bermos.dev") {
		t.Fatalf("want the same not-found for both, got %q and %q",
			invisible.Body.String(), missing.Body.String())
	}
}

func TestCollectionsAreFilteredToTheProjectsTheCallerCanSee(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, blogFixtures()...)

	projects := decode[listBody[projectView]](t, h.do(t, http.MethodGet, "/api/v1/projects", ""))
	if len(projects.Items) != 1 || projects.Items[0].Name != feedProject {
		t.Fatalf("want only shop, got %+v", projects.Items)
	}

	builds := decode[listBody[buildView]](t, h.do(t, http.MethodGet, "/api/v1/builds", ""))
	for _, build := range builds.Items {
		if build.Project != feedProject {
			t.Fatalf("a build of %s was answered to a member of shop", build.Project)
		}
	}
	if len(builds.Items) == 0 {
		t.Fatal("want the caller's own builds")
	}

	releases := decode[listBody[releaseView]](t, h.do(t, http.MethodGet, "/api/v1/releases", ""))
	for _, release := range releases.Items {
		if release.Project != feedProject {
			t.Fatalf("a release of %s was answered to a member of shop", release.Project)
		}
	}

	environments := decode[listBody[environmentView]](t, h.do(t, http.MethodGet, "/api/v1/environments", ""))
	for _, env := range environments.Items {
		if env.Project != feedProject {
			t.Fatalf("an environment of %s was answered to a member of shop", env.Project)
		}
	}

	// A `?project=` naming something they cannot see is answered like one
	// that does not exist: nothing, rather than a refusal that confirms it.
	filtered := decode[listBody[buildView]](t, h.do(t, http.MethodGet, "/api/v1/builds?project="+otherProject, ""))
	if len(filtered.Items) != 0 {
		t.Fatalf("want nothing, got %+v", filtered.Items)
	}
}

func TestTheOperatorSeesEveryProject(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), blogFixtures()...)...)

	projects := decode[listBody[projectView]](t, h.do(t, http.MethodGet, "/api/v1/projects", ""))
	if len(projects.Items) != 2 {
		t.Fatalf("want both projects, got %+v", projects.Items)
	}
}

func TestALogSearchIsNarrowedToTheCallersProjects(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, blogFixtures()...)

	if recorder := h.do(t, http.MethodGet, "/api/v1/logs?q=level:error", ""); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if want := "project IN ('shop')"; !strings.Contains(h.logs.lastFilter.Where, want) {
		t.Fatalf("want the search narrowed with %q, got %q", want, h.logs.lastFilter.Where)
	}

	// An operator's search is not narrowed at all.
	operator := newHarness(t, nil, fixtures()...)
	operator.do(t, http.MethodGet, "/api/v1/logs?q=level:error", "")
	if operator.logs.lastFilter.Where != "" {
		t.Fatalf("want an unnarrowed search for an operator, got %q", operator.logs.lastFilter.Where)
	}
}

func TestACallerWithNoProjectsIsAnsweredNothingWithoutAskingTheStore(t *testing.T) {
	h := asMember(t, "")

	lines := decode[listBody[clickhouse.LogLine]](t, h.do(t, http.MethodGet, "/api/v1/logs", ""))
	if len(lines.Items) != 0 {
		t.Fatalf("want no lines, got %+v", lines.Items)
	}
	if h.logs.lastFilter.Where != "" || h.logs.lastFilter.Limit != 0 {
		t.Fatalf("the store should not have been asked at all, got %+v", h.logs.lastFilter)
	}
}

func TestTheActivityFeedIsFilteredAndPlatformEntriesAreTheOperators(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, blogFixtures()...)
	h.logs.events = []clickhouse.Event{
		{Type: "build.succeeded", Project: feedProject, Message: "mine"},
		{Type: "build.succeeded", Project: otherProject, Message: "somebody else's"},
		{Type: "platform.upgraded", Message: "the platform's own"},
	}

	events := decode[listBody[clickhouse.Event]](t, h.do(t, http.MethodGet, "/api/v1/events", ""))
	if len(events.Items) != 1 || events.Items[0].Project != feedProject {
		t.Fatalf("want only the caller's own entry, got %+v", events.Items)
	}
}

func TestSavedQueriesNamingAnInvisibleProjectAreNotLeaked(t *testing.T) {
	mine := &kitchenv1alpha1.SavedQuery{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-500s", Namespace: testNamespace},
		Spec:       kitchenv1alpha1.SavedQuerySpec{Title: "Checkout 500s", Query: "project:" + feedProject + " level:error"},
	}
	theirs := &kitchenv1alpha1.SavedQuery{
		ObjectMeta: metav1.ObjectMeta{Name: "blog-500s", Namespace: testNamespace},
		Spec:       kitchenv1alpha1.SavedQuerySpec{Title: "Blog errors", Query: "project:" + otherProject},
	}
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, append(blogFixtures(), mine, theirs)...)

	saved := decode[listBody[savedQueryView]](t, h.do(t, http.MethodGet, "/api/v1/logs/saved", ""))
	if len(saved.Items) != 1 || saved.Items[0].Name != "shop-500s" {
		t.Fatalf("want only the query the caller may know about, got %+v", saved.Items)
	}

	// And the delete must not confirm it either: the same answer as a name
	// nobody ever saved.
	hidden := h.do(t, http.MethodDelete, "/api/v1/logs/saved/blog-500s", "")
	absent := h.do(t, http.MethodDelete, "/api/v1/logs/saved/never-existed", "")
	if hidden.Code != http.StatusNotFound || absent.Code != hidden.Code {
		t.Fatalf("want the same 404 for both, got %d and %d", hidden.Code, absent.Code)
	}
	if hidden.Body.String() == "" || !strings.HasSuffix(errorOf(t, hidden.Body.String()), "not found") {
		t.Fatalf("want a not-found, got %q", hidden.Body.String())
	}
	// It is still there: a refusal that deleted it would be a worse leak.
	kept := &kitchenv1alpha1.SavedQuery{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "blog-500s"}, kept); err != nil {
		t.Fatalf("the hidden query should not have been deleted: %v", err)
	}
}

// GET /status is the one payload that varies by role: the build queue for
// everybody, the platform's own health for an operator — and withheld means
// absent, not zeroed.
func TestStatusKeepsTheBuildQueueForEveryoneAndWithholdsThePlatform(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)

	recorder := h.do(t, http.MethodGet, "/api/v1/status", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tunnel", "gateway", "components"} {
		if _, present := raw[key]; present {
			t.Fatalf("%q must be absent for a member, not zeroed: %s", key, recorder.Body.String())
		}
	}
	if _, present := raw["builds"]; !present {
		t.Fatalf("the build queue is everybody's: %s", recorder.Body.String())
	}

	status := decode[statusView](t, recorder)
	if status.Cluster.Name == "" {
		t.Fatalf("the platform's name is everybody's, got %+v", status.Cluster)
	}
	if status.Cluster.Nodes != nil || status.Cluster.ReadyNodes != nil {
		t.Fatalf("the node counts are the operator's, got %+v", status.Cluster)
	}
	if status.Builds.Capacity == 0 {
		t.Fatalf("want the build queue's capacity, got %+v", status.Builds)
	}
}

func TestStatusAnswersTheOperatorWhole(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	status := decode[statusView](t, h.do(t, http.MethodGet, "/api/v1/status", ""))
	if status.Tunnel == nil || status.Gateway == nil {
		t.Fatalf("want the platform's own halves for an operator, got %+v", status)
	}
	if status.Cluster.Nodes == nil {
		t.Fatalf("want the node counts for an operator, got %+v", status.Cluster)
	}
}

func TestMeDescribesTheCallerToThemselves(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	me := decode[meView](t, h.do(t, http.MethodGet, "/api/v1/me", ""))
	if me.Subject != testSubject || me.Email != testCaller {
		t.Fatalf("want the token's own account, got %+v", me)
	}
	if me.PlatformRole != "operator" {
		t.Fatalf("want operator, got %q", me.PlatformRole)
	}

	h.demoteCaller(t)
	me = decode[meView](t, h.do(t, http.MethodGet, "/api/v1/me", ""))
	if me.PlatformRole != "member" {
		t.Fatalf("want member, got %q", me.PlatformRole)
	}
}

func TestEveryProjectPayloadCarriesTheCallersRole(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)

	project := decode[projectView](t, h.do(t, http.MethodGet, "/api/v1/projects/shop", ""))
	if project.Role != "developer" {
		t.Fatalf("want developer, got %q", project.Role)
	}
	listed := decode[listBody[projectView]](t, h.do(t, http.MethodGet, "/api/v1/projects", ""))
	if len(listed.Items) != 1 || listed.Items[0].Role != "developer" {
		t.Fatalf("want the role on the list entry too, got %+v", listed.Items)
	}
}

// The operator's admin is not written on any project, and must not have to be.
func TestAnOperatorReadsAdminOnAProjectTheyAreNotListedOn(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	project := decode[projectView](t, h.do(t, http.MethodGet, "/api/v1/projects/shop", ""))
	if len(project.Env) != 0 && project.Role == "" {
		t.Fatal("unreadable project")
	}
	if project.Role != "admin" {
		t.Fatalf("want admin, got %q", project.Role)
	}

	// And the fixture really does not name them.
	obj := &kitchenv1alpha1.Project{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: feedProject}, obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Spec.Access) != 0 {
		t.Fatalf("the fixture was meant to grant nobody anything, got %+v", obj.Spec.Access)
	}
}

func TestAnyAccountMayCreateAProjectAndBecomesItsAdmin(t *testing.T) {
	h := asMember(t, "")

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"tools","repo":"acme/tools","connection":"gh","registry":"registry"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if created := decode[projectView](t, recorder); created.Role != "admin" {
		t.Fatalf("want the creator to be its admin, got %q", created.Role)
	}

	// And it is theirs from then on: the list they could not see a moment ago
	// now has exactly this one in it.
	listed := decode[listBody[projectView]](t, h.do(t, http.MethodGet, "/api/v1/projects", ""))
	if len(listed.Items) != 1 || listed.Items[0].Name != "tools" {
		t.Fatalf("want the project they just created, got %+v", listed.Items)
	}
}

// The table is what makes every route's requirement a fact rather than a
// habit. These are the invariants a row cannot be written without.
func TestEveryRouteCarriesAWellFormedRequirement(t *testing.T) {
	server := &Server{Namespace: testNamespace}

	seen := map[string]bool{}
	for _, route := range server.routes() {
		if route.Handler == nil {
			t.Fatalf("%s has no handler", route.Pattern)
		}
		if seen[route.Pattern] {
			t.Fatalf("%s is registered twice", route.Pattern)
		}
		seen[route.Pattern] = true

		switch route.Requires.Kind {
		case requireOperator:
			if route.Requires.Doing == "" {
				t.Fatalf("%s refuses without saying what was being attempted", route.Pattern)
			}
		case requireProjectRole:
			if route.Requires.Doing == "" {
				t.Fatalf("%s refuses without saying what was being attempted", route.Pattern)
			}
			if route.Requires.Role == 0 {
				t.Fatalf("%s wants a project role and names none", route.Pattern)
			}
			if route.Requires.Project.Resolve == nil || route.Requires.Project.Resource == "" {
				t.Fatalf("%s wants a project role and says nothing about which project", route.Pattern)
			}
		case requireAuthenticated, requireVisibleProjects, requireRoleShapedBody:
		}
	}
	if !seen["GET /api/v1/projects"] || !seen["/"] {
		t.Fatal("the table is not the one the server registers from")
	}
}

// The two writes that name their project in the body rather than in the path.
// They are the ones where the guard has to read the request and put it back,
// so the handler decodes exactly what the caller sent.
func TestAWriteThatNamesItsProjectInTheBody(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, neonConnection())

	if recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "orders-db", "project": "`+feedProject+`", "connection": "neon", "type": "postgres"}`,
	); recorder.Code != http.StatusCreated {
		t.Fatalf("a developer must be able to claim a resource: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodPost, "/api/v1/domains",
		`{"hostname": "store.example.net", "environment": "`+testEnvironment+`"}`,
	); recorder.Code != http.StatusCreated {
		t.Fatalf("a developer must be able to attach a domain: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAWriteNamingSomebodyElsesProjectIsNotFound(t *testing.T) {
	h := asMember(t, "", neonConnection())

	claim := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "orders-db", "project": "`+feedProject+`", "connection": "neon", "type": "postgres"}`)
	if claim.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", claim.Code, claim.Body.String())
	}
	domain := h.do(t, http.MethodPost, "/api/v1/domains",
		`{"hostname": "store.example.net", "environment": "`+testEnvironment+`"}`)
	if domain.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", domain.Code, domain.Body.String())
	}
}

// A body naming no project at all is the handler's to refuse, and it does so
// with the field it wanted rather than with a not-found for nothing.
func TestABodyNamingNoProjectIsRefusedByTheHandler(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims", `{"name": "orders-db", "type": "postgres"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(errorOf(t, recorder.Body.String()), "project is required") {
		t.Fatalf("want the missing field named, got %q", recorder.Body.String())
	}
}

func TestTheAuditLogIsFilteredAndPlatformRecordsAreTheOperators(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, blogFixtures()...)
	h.logs.auditRecords = []clickhouse.AuditRecord{
		{Sequence: 3, Kind: "Project", Name: feedProject, Project: feedProject, Operation: "update"},
		{Sequence: 2, Kind: "Project", Name: otherProject, Project: otherProject, Operation: "update"},
		{Sequence: 1, Kind: "Kitchen", Name: "kitchen", Operation: "update"},
	}

	records := decode[listBody[auditRecordBody]](t, h.do(t, http.MethodGet, "/api/v1/audit", ""))
	if len(records.Items) != 1 || records.Items[0].Project != feedProject {
		t.Fatalf("want only the caller's own project's records, got %+v", records.Items)
	}
}

func TestTheServiceMapIsFilteredToEdgesTouchingTheCallersProjects(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, blogFixtures()...)
	h.logs.edges = []clickhouse.TrafficEdge{
		{Source: "shop", SourceNamespace: "kitchen-shop", Destination: "postgres", DestinationNamespace: "data"},
		{Source: "blog", SourceNamespace: "kitchen-blog", Destination: "postgres", DestinationNamespace: "data"},
	}

	edges := decode[listBody[clickhouse.TrafficEdge]](t, h.do(t, http.MethodGet, "/api/v1/traffic", ""))
	if len(edges.Items) != 1 || edges.Items[0].Source != feedProject {
		t.Fatalf("want only the edge touching the caller's project, got %+v", edges.Items)
	}
}

// A `?project=` naming something the caller cannot see is answered exactly
// like one naming something that is not there.
func TestAQueryNamingAnInvisibleProjectIsNotFound(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, blogFixtures()...)

	for _, path := range []string{
		"/api/v1/metrics/overview?project=" + otherProject,
		"/api/v1/traffic?project=" + otherProject,
		"/api/v1/traces?project=" + otherProject,
		"/api/v1/events?project=" + otherProject,
	} {
		t.Run(path, func(t *testing.T) {
			invisible := h.do(t, http.MethodGet, path, "")
			missing := h.do(t, http.MethodGet, strings.Replace(path, otherProject, "nothing-like-this", 1), "")
			if invisible.Code != http.StatusNotFound || missing.Code != invisible.Code {
				t.Fatalf("want the same 404 for both, got %d and %d", invisible.Code, missing.Code)
			}
		})
	}
}

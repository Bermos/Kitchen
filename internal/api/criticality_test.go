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

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Issue #141's acceptance criteria, criterion by criterion: the
// function-to-resource mapping in one call, the reverse query for any
// provider, and — the one most easily faked — the tolerance being a threshold
// rather than a number on a screen, which internal/signals asserts.

const (
	criticalityCritical = "critical"
	shopProject         = "shop"
	// neonProvider is the third party behind the fixtures' database claim,
	// and so the subject of the reverse query.
	neonProvider = "neon"
)

// The shop fixtures already carry the graph this feature traverses: a git
// connection, a registry connection, a Postgres claim on `neon`, a custom
// domain and a release. All that is missing is somebody's designation.
func designatedFixtures() []runtime.Object {
	return append(blogFixtures(), neonConnection())
}

// designate writes a designation onto the shop project, the way the settings
// endpoint does.
func designate(t *testing.T, h *harness, criticality kitchenv1alpha1.Criticality, rto string) {
	t.Helper()
	project := &kitchenv1alpha1.Project{}
	key := types.NamespacedName{Name: shopProject, Namespace: testNamespace}
	if err := h.server.Client.Get(context.Background(), key, project); err != nil {
		t.Fatal(err)
	}
	project.Spec.Criticality = criticality
	project.Spec.RTO = kitchenv1alpha1.Tolerance(rto)
	if err := h.server.Client.Update(context.Background(), project); err != nil {
		t.Fatal(err)
	}
}

func TestTheFunctionToResourceMappingIsOneRequest(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), designatedFixtures()...)...)
	designate(t, h, kitchenv1alpha1.CriticalityCritical, "1h")

	recorder := h.do(t, http.MethodGet, "/api/v1/compliance/criticality", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[criticalityMapBody](t, recorder)
	if len(body.Functions) != 1 || body.Functions[0].Project != shopProject {
		t.Fatalf("one function is designated, got %+v", body.Functions)
	}
	function := body.Functions[0]
	if function.Criticality != criticalityCritical || function.RTO != "1h" {
		t.Fatalf("the designation must be answered verbatim, got %+v", function)
	}
	// The blog project is designated nothing, and the answer says how many
	// such projects there are rather than silently omitting them.
	if body.Undesignated != 1 {
		t.Fatalf("the estate's undesignated projects must be counted, got %d", body.Undesignated)
	}

	// The whole point of the endpoint: everything supporting the function, in
	// this one answer, without a second request to assemble by hand.
	if len(function.Environments) != 1 {
		t.Fatalf("the environments must be here, got %+v", function.Environments)
	}
	env := function.Environments[0]
	if env.Criticality != criticalityCritical || env.RTO != "1h" {
		t.Fatalf("production must resolve to its project's designation, got %+v", env)
	}
	if len(env.Inherited) == 0 {
		t.Fatalf("an inherited designation must say it was inherited, got %+v", env)
	}
	if env.Release != testRelease || env.Image != "registry.example.com/shop@sha256:1111" {
		t.Fatalf("the release and its artifact must be here, got %+v", env)
	}
	if len(env.Domains) != 1 || env.Domains[0] != "shop.example.com" {
		t.Fatalf("the custom domain must be here, got %+v", env)
	}
	if len(function.Claims) != 1 || function.Claims[0].Provider != neonProvider {
		t.Fatalf("the claim and the third party behind it must be here, got %+v", function.Claims)
	}
	if len(function.Connections) != 3 {
		t.Fatalf("source, registry and the claim's connection, got %+v", function.Connections)
	}
	for _, want := range []string{"github", neonProvider, "dockerRegistry"} {
		if !contains(function.ThirdParties, want) {
			t.Fatalf("%q is missing from the third parties: %+v", want, function.ThirdParties)
		}
	}
	if body.Depth == "" {
		t.Fatal("the answer must say how deep the traversal goes")
	}
}

func TestTheMappingFiltersToADesignationAndNarrowsToAProject(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), designatedFixtures()...)...)
	designate(t, h, kitchenv1alpha1.CriticalityImportant, "")

	// Asked for critical-and-worse, an important function is not an answer.
	recorder := h.do(t, http.MethodGet, "/api/v1/compliance/criticality?criticality=critical", "")
	body := decode[criticalityMapBody](t, recorder)
	if len(body.Functions) != 0 {
		t.Fatalf("an important function answered a critical filter: %+v", body.Functions)
	}
	if body.Minimum != criticalityCritical {
		t.Fatalf("the answer must say what it was filtered by, got %q", body.Minimum)
	}

	// Asked for important-and-worse, it is.
	recorder = h.do(t, http.MethodGet, "/api/v1/compliance/criticality?criticality=important", "")
	if len(decode[criticalityMapBody](t, recorder).Functions) != 1 {
		t.Fatalf("the important function must answer its own filter: %s", recorder.Body.String())
	}

	// A vocabulary nobody uses is refused with the vocabulary that exists.
	recorder = h.do(t, http.MethodGet, "/api/v1/compliance/criticality?criticality=tier-1", "")
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(errorOf(t, recorder.Body.String()), "nonCritical") {
		t.Fatalf("the refusal must name the vocabulary, got %d %s", recorder.Code, recorder.Body.String())
	}

	// And a project nobody designated is not in the answer at all.
	recorder = h.do(t, http.MethodGet, "/api/v1/compliance/criticality?project="+otherProject, "")
	body = decode[criticalityMapBody](t, recorder)
	if len(body.Functions) != 0 || body.Undesignated != 1 {
		t.Fatalf("the blog is undesignated, got %+v", body)
	}
}

func TestTheReverseQueryAnswersWhatBreaks(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), designatedFixtures()...)...)
	designate(t, h, kitchenv1alpha1.CriticalityCritical, "1h")

	// One connection: only the project that names it.
	recorder := h.do(t, http.MethodGet, "/api/v1/compliance/dependents?connection=neon", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[criticalityDependentsBody](t, recorder)
	if len(body.Affected) != 1 || body.Affected[0].Environment != testEnvironment {
		t.Fatalf("the shop's production environment depends on neon, got %+v", body.Affected)
	}
	if body.Affected[0].Criticality != criticalityCritical {
		t.Fatalf("the affected environment must carry its designation, got %+v", body.Affected[0])
	}
	if len(body.Affected[0].Through) != 1 || body.Affected[0].Through[0] != "claim shop-db" {
		t.Fatalf("the answer must say how the dependency runs, got %+v", body.Affected[0].Through)
	}
	if body.TightestRTO != "1h" {
		t.Fatalf("the tightest objective among the affected is the headline, got %q", body.TightestRTO)
	}
	if body.Counts[criticalityCritical] != 1 {
		t.Fatalf("the counts must be by designation, got %+v", body.Counts)
	}

	// A provider: every connection from it, and every environment behind
	// those — including the ones nobody designated, counted as undesignated
	// rather than dropped.
	recorder = h.do(t, http.MethodGet, "/api/v1/compliance/dependents?provider=github", "")
	body = decode[criticalityDependentsBody](t, recorder)
	if body.Subject.Kind != "provider" || len(body.Subject.Connections) != 1 {
		t.Fatalf("a provider query must resolve to its connections, got %+v", body.Subject)
	}
	if len(body.Affected) != 2 {
		t.Fatalf("both projects use the git connection, got %+v", body.Affected)
	}
	// Worst first: the critical environment before the undesignated one.
	if body.Affected[0].Criticality != criticalityCritical ||
		body.Affected[1].Criticality != criticalityUndesignated {
		t.Fatalf("the answer must read worst-first, got %+v", body.Affected)
	}
}

func TestTheReverseQueryInsistsOnOneSubject(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, query := range map[string]string{
		"nothing": "",
		"both":    "?connection=neon&provider=neon",
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodGet, "/api/v1/compliance/dependents"+query, "")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	// A connection nothing depends on is an empty answer, not a 404: "no
	// environment breaks" is the answer, and it is a different answer from
	// "there is no such connection", which the subject's empty provider says.
	recorder := h.do(t, http.MethodGet, "/api/v1/compliance/dependents?connection=nowhere", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(decode[criticalityDependentsBody](t, recorder).Affected) != 0 {
		t.Fatalf("nothing depends on it: %s", recorder.Body.String())
	}
}

func TestBothMapsAreFilteredToTheCallersProjects(t *testing.T) {
	// The same filtering every cross-project read applies: a member sees
	// their own function and nobody else's, which matters more here than
	// elsewhere — a criticality map is a map of what is worth attacking.
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper, blogFixtures()...)
	designate(t, h, kitchenv1alpha1.CriticalityCritical, "1h")

	recorder := h.do(t, http.MethodGet, "/api/v1/compliance/criticality", "")
	body := decode[criticalityMapBody](t, recorder)
	for _, function := range body.Functions {
		if function.Project != feedProject {
			t.Fatalf("a project the caller holds no role on is in the map: %+v", function)
		}
	}

	recorder = h.do(t, http.MethodGet, "/api/v1/compliance/dependents?provider=github", "")
	for _, affected := range decode[criticalityDependentsBody](t, recorder).Affected {
		if affected.Project != feedProject {
			t.Fatalf("a project the caller holds no role on is in the answer: %+v", affected)
		}
	}
}

func TestDesignatingAProjectIsAPrivilegedRecordCarryingThePreviousValue(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"criticality": "critical", "rto": "1h", "rpo": "5m"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if view.Criticality != criticalityCritical || view.RTO != "1h" || view.RPO != "5m" {
		t.Fatalf("the designation must round-trip verbatim, got %+v", view)
	}

	// The record: privileged, and carrying what it was before.
	before := &kitchenv1alpha1.Project{
		Spec: kitchenv1alpha1.ProjectSpec{Criticality: kitchenv1alpha1.CriticalityImportant, RTO: "4h"},
	}
	next := kitchenv1alpha1.Criticality(criticalityCritical)
	rto := kitchenv1alpha1.Tolerance("1h")
	details := projectSettingsDetails(before, patchProjectRequest{}, nil,
		continuityChange{criticality: &next, rto: &rto})
	if details["privileged"] != true ||
		details["previousCriticality"] != "important" || details["criticality"] != criticalityCritical {
		t.Fatalf("the record must carry the previous designation, privileged: %v", details)
	}
	if details["previousRTO"] != "4h" || details["rto"] != "1h" {
		t.Fatalf("the record must carry the previous tolerance: %v", details)
	}
	// A field the PATCH did not carry is not in the record at all.
	if _, present := details["rpo"]; present {
		t.Fatalf("an untouched field was recorded as changed: %v", details)
	}
}

func TestATolerancesUnitIsNeverGuessedAt(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, body := range map[string]string{
		"a bare number":  `{"rto": "4"}`,
		"english":        `{"rto": "4 hours"}`,
		"milliseconds":   `{"rpo": "250ms"}`,
		"a vocabulary":   `{"criticality": "tier-1"}`,
		"seconds":        `{"rto": "90s"}`,
		"a negative one": `{"rto": "-5m"}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	// And the spellings that are accepted come back exactly as written.
	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"rto": "1h30m", "rpo": "0m"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if view.RTO != "1h30m" || view.RPO != "0m" {
		t.Fatalf("a tolerance must round-trip exactly, got %+v", view)
	}
}

func TestAnEnvironmentIsDesignatedByItsOwners(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch,
		"/api/v1/environments/"+testEnvironment+"/requirements",
		`{"criticality": "nonCritical", "rto": "72h"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[environmentView](t, recorder)
	if view.Criticality != "nonCritical" || view.RTO != "72h" {
		t.Fatalf("the environment's own designation must round-trip, got %+v", view)
	}

	// It is not capped by its project's: a nonCritical designation on an
	// environment of a critical project stands, because the institution said
	// so about that environment.
	designate(t, h, kitchenv1alpha1.CriticalityCritical, "1h")
	recorder = h.do(t, http.MethodGet, "/api/v1/compliance/criticality", "")
	body := decode[criticalityMapBody](t, recorder)
	if len(body.Functions) != 1 {
		t.Fatalf("one function, got %+v", body.Functions)
	}
	env := body.Functions[0].Environments[0]
	if env.Criticality != "nonCritical" || env.RTO != "72h" || len(env.Inherited) != 0 {
		t.Fatalf("the environment's own designation must win uncapped, got %+v", env)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

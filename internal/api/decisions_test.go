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
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/policy"
)

// The decision register over HTTP. What is being tested is the part the
// guard table cannot do for rows that live in the store: visibility follows
// the decision's own project, replay is a developer's write, and a replay
// actually re-runs the engine over the stored bytes.

// decisionFixtures seeds the stub store with one decision per visibility
// class: the member's project, somebody else's, and the platform's own.
func decisionFixtures(h *harness) {
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	h.logs.decisions = []clickhouse.Decision{
		{
			ID: "d-shop", Timestamp: at, Kind: policy.KindPromotion,
			Project: feedProject, Environment: testEnvironment, Release: testRelease,
			BundleDigest: "sha256:" + strings.Repeat("b", 64),
			InputDigest:  "sha256:" + strings.Repeat("c", 64),
			Verdict:      policy.VerdictBlocked,
			RulesFired:   `[{"rule":"require-sbom","message":"no SBOM"}]`,
			Input:        `{"kind":"promotion"}`,
		},
		{
			ID: "d-blog", Timestamp: at, Kind: policy.KindPromotion,
			Project: "blog", Environment: "blog-production", Release: "blog-rel-1",
			BundleDigest: "sha256:" + strings.Repeat("b", 64),
			InputDigest:  "sha256:" + strings.Repeat("d", 64),
			Verdict:      policy.VerdictAllowed, RulesFired: "[]", Input: "{}",
		},
		{
			ID: "d-platform", Timestamp: at, Kind: policy.KindRescan,
			Project:      "",
			BundleDigest: "sha256:" + strings.Repeat("b", 64),
			InputDigest:  "sha256:" + strings.Repeat("e", 64),
			Verdict:      policy.VerdictAllowed, RulesFired: "[]", Input: "{}",
		},
	}
}

func TestDecisionsAreFilteredToWhatTheCallerMaySee(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)
	decisionFixtures(h)

	recorder := h.do(t, http.MethodGet, "/api/v1/decisions", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[listBody[decisionBody]](t, recorder)
	if len(body.Items) != 1 || body.Items[0].ID != "d-shop" {
		t.Fatalf("a member sees their projects' decisions and nothing else, got %+v", body.Items)
	}
	// The list summarizes; the whole input is the single read's answer.
	if body.Items[0].Input != nil {
		t.Fatalf("the list must not carry full inputs, got %s", body.Items[0].Input)
	}
	if string(body.Items[0].RulesFired) != `[{"rule":"require-sbom","message":"no SBOM"}]` {
		t.Fatalf("the fired rules pass through verbatim, got %s", body.Items[0].RulesFired)
	}
}

func TestAnOperatorSeesEveryDecisionIncludingThePlatforms(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	decisionFixtures(h)

	recorder := h.do(t, http.MethodGet, "/api/v1/decisions", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[listBody[decisionBody]](t, recorder)
	if len(body.Items) != 3 {
		t.Fatalf("an operator sees everything the store answered, got %d", len(body.Items))
	}

	// And the narrowing is the store's rather than the handler's: the filters
	// reach it, and what comes back is what it selected.
	recorder = h.do(t, http.MethodGet, "/api/v1/decisions?verdict=allowed&kind=rescan", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if h.logs.lastDecisions.Verdict != "allowed" || h.logs.lastDecisions.Kind != "rescan" {
		t.Fatalf("the filters must reach the store, got %+v", h.logs.lastDecisions)
	}
	body = decode[listBody[decisionBody]](t, recorder)
	if len(body.Items) != 1 || body.Items[0].ID != "d-platform" {
		t.Fatalf("an operator was not answered the store's own selection, got %+v", body.Items)
	}
}

func TestAnInvisibleDecisionReadsAsMissing(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)
	decisionFixtures(h)

	// Somebody else's decision and a genuinely absent one answer alike.
	for _, id := range []string{"d-blog", "d-platform", "d-gone"} {
		recorder := h.do(t, http.MethodGet, "/api/v1/decisions/"+id, "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("decision %s: want 404, got %d: %s", id, recorder.Code, recorder.Body.String())
		}
	}

	recorder := h.do(t, http.MethodGet, "/api/v1/decisions/d-shop", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[decisionBody](t, recorder)
	if body.ID != "d-shop" || string(body.Input) != `{"kind":"promotion"}` {
		t.Fatalf("the single read answers the decision whole, got %+v", body)
	}
}

// replayFixture stores a decision whose input and bundle are real: the
// built-in bundle, and an input that blocks on a missing SBOM.
func replayFixture(t *testing.T, h *harness) clickhouse.Decision {
	t.Helper()
	bundle := policy.DefaultBundle()
	bundleDigest := policy.Digest(bundle)
	input := policy.Input{
		Kind:        policy.KindPromotion,
		At:          time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Parameters:  map[string]string{"require-sbom": "true"},
		Project:     policy.ProjectFacts{Name: feedProject},
		Environment: policy.EnvironmentFacts{Name: testEnvironment, Type: "production"},
		Release:     policy.ReleaseFacts{Name: testRelease},
	}
	canonical, err := input.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	inputDigest, err := input.Digest()
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(map[string]string(bundle))
	if err != nil {
		t.Fatal(err)
	}
	decision := clickhouse.Decision{
		ID: "d-replayable", Timestamp: input.At, Kind: policy.KindPromotion,
		Project: feedProject, Environment: testEnvironment, Release: testRelease,
		BundleDigest: bundleDigest, InputDigest: inputDigest,
		Verdict:    policy.VerdictBlocked,
		RulesFired: `[{"rule":"require-sbom","message":"stored"}]`,
		Input:      string(canonical),
		DecidedBy:  "system:controller/policy",
	}
	h.logs.decisions = append(h.logs.decisions, decision)
	h.logs.bundles = map[string]string{bundleDigest: string(content)}
	return decision
}

func TestReplayReproducesTheVerdictAndStoresAReplayDecision(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	stored := replayFixture(t, h)

	recorder := h.do(t, http.MethodPost, "/api/v1/decisions/"+stored.ID+"/replay", "")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[replayBody](t, recorder)
	if body.Original.Verdict != policy.VerdictBlocked || body.Replay.Verdict != policy.VerdictBlocked {
		t.Fatalf("the replay must reproduce the verdict, got %+v", body)
	}
	if !body.Match {
		t.Fatal("identical inputs and bundle must match")
	}
	if !strings.Contains(string(body.Replay.Fired), "require-sbom") {
		t.Fatalf("the replay reports what fired, got %s", body.Replay.Fired)
	}

	// The check itself has a record: a decision of kind replay, citing the
	// same digests and carrying the same input, decided by the caller.
	if len(h.logs.insertedDecisions) != 1 {
		t.Fatalf("want one stored replay, got %d", len(h.logs.insertedDecisions))
	}
	replay := h.logs.insertedDecisions[0]
	if replay.Kind != policy.KindReplay || replay.ID == stored.ID {
		t.Fatalf("the replay is a decision of its own, got %+v", replay)
	}
	if replay.BundleDigest != stored.BundleDigest || replay.InputDigest != stored.InputDigest ||
		replay.Input != stored.Input {
		t.Fatalf("the replay cites what it re-ran, got %+v", replay)
	}
	if replay.DecidedBy != testCaller {
		t.Fatalf("a replay is the caller's act, got %q", replay.DecidedBy)
	}
}

func TestReplayNoticesAVerdictThatDoesNotReproduce(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	stored := replayFixture(t, h)
	// Tamper with the stored verdict: the store said allowed, the bytes say
	// blocked. Replay is the endpoint that notices.
	h.logs.decisions[len(h.logs.decisions)-1].Verdict = policy.VerdictAllowed

	recorder := h.do(t, http.MethodPost, "/api/v1/decisions/"+stored.ID+"/replay", "")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[replayBody](t, recorder)
	if body.Match {
		t.Fatalf("a tampered verdict must not match, got %+v", body)
	}
	if body.Original.Verdict != policy.VerdictAllowed || body.Replay.Verdict != policy.VerdictBlocked {
		t.Fatalf("both verdicts are reported side by side, got %+v", body)
	}
}

func TestReplayOfAnUngatedPromotionsDecisionReproducesTheTrivialAllow(t *testing.T) {
	// An environment that declares no requirements still gets its promotion
	// decision recorded — with an empty bundle, because nothing was
	// evaluated. Replaying that decision reproduces the same trivial allow
	// instead of refusing to evaluate nothing.
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)
	input := policy.Input{
		Kind:        policy.KindPromotion,
		At:          time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Project:     policy.ProjectFacts{Name: feedProject},
		Environment: policy.EnvironmentFacts{Name: testEnvironment, Type: "production"},
		Release:     policy.ReleaseFacts{Name: testRelease},
	}
	canonical, err := input.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	inputDigest, err := input.Digest()
	if err != nil {
		t.Fatal(err)
	}
	stored := clickhouse.Decision{
		ID: "d-ungated", Timestamp: input.At, Kind: policy.KindPromotion,
		Project: feedProject, Environment: testEnvironment, Release: testRelease,
		BundleDigest: policy.Digest(nil), InputDigest: inputDigest,
		Verdict:    policy.VerdictAllowed,
		RulesFired: `[]`,
		Input:      string(canonical),
		DecidedBy:  "system:controller/policy",
	}
	// Deliberately no bundle row: an empty bundle was never persisted, and
	// the replay must not go looking for one.
	h.logs.decisions = append(h.logs.decisions, stored)

	recorder := h.do(t, http.MethodPost, "/api/v1/decisions/"+stored.ID+"/replay", "")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[replayBody](t, recorder)
	if !body.Match || body.Original.Verdict != policy.VerdictAllowed ||
		body.Replay.Verdict != policy.VerdictAllowed {
		t.Fatalf("the trivial allow must reproduce, got %+v", body)
	}
	if len(h.logs.insertedDecisions) != 1 {
		t.Fatalf("the check itself has a record, got %d", len(h.logs.insertedDecisions))
	}
	replay := h.logs.insertedDecisions[0]
	if replay.Kind != policy.KindReplay || replay.BundleDigest != stored.BundleDigest {
		t.Fatalf("the replay cites the empty bundle it re-ran nothing from, got %+v", replay)
	}
}

func TestReplayIsADevelopersWrite(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)
	stored := replayFixture(t, h)

	recorder := h.do(t, http.MethodPost, "/api/v1/decisions/"+stored.ID+"/replay", "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("a viewer must not replay, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "developer") {
		t.Fatalf("the refusal names the role it wanted, got %q", got)
	}
	if len(h.logs.insertedDecisions) != 0 {
		t.Fatalf("a refused replay must store nothing, got %+v", h.logs.insertedDecisions)
	}
}

func TestPolicyBundlesListTheBuiltInBundle(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/policy/bundles", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[listBody[bundleBody]](t, recorder)
	if len(body.Items) != 1 || body.Items[0].Source != policy.SourceBuiltIn {
		t.Fatalf("want the built-in bundle, got %+v", body.Items)
	}
	if body.Items[0].Digest != policy.Digest(policy.DefaultBundle()) {
		t.Fatalf("the digest is the bundle's real one, got %q", body.Items[0].Digest)
	}
	found := false
	for _, rule := range body.Items[0].Rules {
		if rule == "require-provenance" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the listing names the bundle's rules, got %v", body.Items[0].Rules)
	}

	// The listing is governance surface, and it is the operator's.
	member := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	refused := member.do(t, http.MethodGet, "/api/v1/policy/bundles", "")
	if refused.Code != http.StatusForbidden {
		t.Fatalf("a member must be refused, got %d: %s", refused.Code, refused.Body.String())
	}
}

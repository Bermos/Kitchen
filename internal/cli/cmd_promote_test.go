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

package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// The promotion commands, argv → HTTP → stdout, like everything else here.

// promotedRelease is the release these tests move around.
const promotedRelease = "shop-rel-77"

func TestPromoteAsksAndAnswersThePendingPromotion(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject

	code := h.run("promote", promotedRelease, "--environment", "shop-staging",
		"--reason", "ship 1.4", "--json")
	if code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	accepted := promotion{}
	h.answer(&accepted)
	if accepted.Phase != "Pending" || accepted.Release != promotedRelease {
		t.Fatalf("unexpected answer: %+v", accepted)
	}

	posts := h.platform.sent("POST", "/promotions")
	if len(posts) != 1 {
		t.Fatalf("wanted one POST, got %d", len(posts))
	}
	sent := map[string]string{}
	if err := json.Unmarshal([]byte(posts[0].Body), &sent); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	if sent["environment"] != "shop-staging" || sent["release"] != promotedRelease || sent["reason"] != "ship 1.4" {
		t.Fatalf("unexpected body: %s", posts[0].Body)
	}
}

func TestPromoteWithoutAnEnvironmentIsAUsageFailureNamingTheFlag(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject

	if code := h.run("promote", promotedRelease, "--json"); code != exitUsage {
		t.Fatalf("exit %d, wanted %d", code, exitUsage)
	}
	if refusal := h.failure(); !strings.Contains(refusal.Hint, "--environment") {
		t.Fatalf("the hint does not name the flag: %+v", refusal)
	}
	if posts := h.platform.sent("POST", "/promotions"); len(posts) != 0 {
		t.Fatalf("nothing should have been asked, got %+v", posts)
	}
}

func TestPromotionsListsAndShowsBlockedRules(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.promotions = []promotion{{
		Name: "shop-promo-9zz", Project: testProject,
		Environment: "shop-production", Release: promotedRelease,
		RequestedBy: "system:controller/build", Trigger: "automatic",
		Phase: "Blocked", Verdict: "blocked",
		UnmetRules: []string{"require-sbom"}, DecisionID: "0d9a1f7e",
	}}

	if code := h.run("promotions", "--phase", "Blocked", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	answer := list[promotion]{}
	h.answer(&answer)
	if len(answer.Items) != 1 || answer.Items[0].UnmetRules[0] != "require-sbom" {
		t.Fatalf("unexpected answer: %+v", answer)
	}
	lists := h.platform.sent("GET", "/promotions")
	if len(lists) != 1 || !strings.Contains(lists[0].Query, "phase=Blocked") {
		t.Fatalf("the filter has to reach the platform: %+v", lists)
	}

	// One promotion whole, by name — the other endpoint of the same command.
	if code := h.run("promotions", "shop-promo-9zz", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	one := promotion{}
	h.answer(&one)
	if one.Name != "shop-promo-9zz" || one.DecisionID != "0d9a1f7e" {
		t.Fatalf("unexpected answer: %+v", one)
	}
}

// The rollback contract against a gated environment: the CLI does not
// pretend anything moved — it answers the promotion the move became.
func TestRollbackAgainstAGatedEnvironmentAnswersThePromotion(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, ProductionEnvironment: "shop-production"}
	h.platform.environments = []environment{{
		Name: "shop-production", Project: testProject, Release: "shop-rel-42", Phase: "Live",
		History: []releaseHistory{{Release: promotedRelease, Reason: "promoted"}},
	}}
	h.platform.moveToPromotion = &promotion{
		Name: "shop-promo-7ab", Project: testProject,
		Environment: "shop-production", RequestedBy: "anna@example.com",
		Trigger: "manual", Phase: "Pending",
	}

	if code := h.run("rollback", "--yes", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	accepted := promotion{}
	h.answer(&accepted)
	if accepted.Name != "shop-promo-7ab" || accepted.Phase != "Pending" || accepted.Release != promotedRelease {
		t.Fatalf("the answer must be the promotion, got %+v", accepted)
	}
}

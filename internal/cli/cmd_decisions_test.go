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
	"strings"
	"testing"
	"time"
)

// The decisions commands, run the way somebody would run them: argv in, HTTP
// out, one JSON document back.

// verdictBlocked is the verdict these fixtures decide on; the word is the
// API's, from internal/policy.
const verdictBlocked = "blocked"

func decisionsFixture(h *harness) {
	h.platform.decisions = []decision{{
		ID:           "0d9a1f7e-1111-2222-3333-444444444444",
		Timestamp:    time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Kind:         "promotion",
		Project:      "shop",
		Environment:  "shop-production",
		Release:      "shop-rel-7",
		BundleDigest: "sha256:" + strings.Repeat("b", 64),
		InputDigest:  "sha256:" + strings.Repeat("c", 64),
		Verdict:      verdictBlocked,
		RulesFired:   []firedRule{{Rule: "require-sbom", Message: "no SBOM"}},
		DecidedBy:    "system:controller/policy",
	}}
}

func TestDecisionsListSendsTheFiltersAndAnswersTheList(t *testing.T) {
	h := newHarness(t)
	decisionsFixture(h)

	if code := h.run("decisions", "list", "--json",
		"--project", "shop", "--verdict", verdictBlocked, "--kind", "promotion", "--limit", "5"); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, h.stderr.String())
	}
	answer := list[decision]{}
	h.answer(&answer)
	if len(answer.Items) != 1 || answer.Items[0].Verdict != verdictBlocked {
		t.Fatalf("answered %+v", answer.Items)
	}
	if len(answer.Items[0].RulesFired) != 1 || answer.Items[0].RulesFired[0].Rule != "require-sbom" {
		t.Fatalf("the fired rules must survive the trip, got %+v", answer.Items[0].RulesFired)
	}

	sent := h.platform.sent("GET", "/decisions")
	if len(sent) != 1 {
		t.Fatalf("want one list call, got %v", h.platform.requests)
	}
	for _, fragment := range []string{"project=shop", "verdict=blocked", "kind=promotion", "limit=5"} {
		if !strings.Contains(sent[0].Query, fragment) {
			t.Errorf("the query must carry %s, got %q", fragment, sent[0].Query)
		}
	}
}

func TestDecisionsShowAnswersOneDecisionWhole(t *testing.T) {
	h := newHarness(t)
	decisionsFixture(h)

	if code := h.run("decisions", "show", "0d9a1f7e-1111-2222-3333-444444444444", "--json"); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, h.stderr.String())
	}
	answer := decision{}
	h.answer(&answer)
	if answer.ID != "0d9a1f7e-1111-2222-3333-444444444444" || answer.BundleDigest == "" {
		t.Fatalf("answered %+v", answer)
	}

	// A decision that is not there is exit 5, like every not-found.
	if code := h.run("decisions", "show", "nope", "--json"); code != 5 {
		t.Fatalf("want exit 5 for a missing decision, got %d", code)
	}
}

func TestDecisionsReplayReportsTheComparison(t *testing.T) {
	h := newHarness(t)
	decisionsFixture(h)
	h.platform.replay = &decisionReplay{
		Original: replayVerdict{Verdict: verdictBlocked},
		Replay: replayOutcome{
			Verdict: verdictBlocked,
			Fired:   []firedRule{{Rule: "require-sbom", Message: "no SBOM"}},
		},
		Match:    true,
		Decision: "replay-1",
	}

	if code := h.run("decisions", "replay", "0d9a1f7e-1111-2222-3333-444444444444", "--json"); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, h.stderr.String())
	}
	answer := decisionReplay{}
	h.answer(&answer)
	if !answer.Match || answer.Original.Verdict != verdictBlocked || answer.Replay.Verdict != verdictBlocked {
		t.Fatalf("answered %+v", answer)
	}

	sent := h.platform.sent("POST", "/replay")
	if len(sent) != 1 {
		t.Fatalf("want one replay call, got %v", h.platform.requests)
	}
}

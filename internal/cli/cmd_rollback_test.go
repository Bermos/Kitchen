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
)

// What `kitchen rollback` says before it asks (#181).
//
// The dashboard's complaint applies to a terminal word for word: a
// confirmation that repeats the release name back is not a safety mechanism,
// because the release name is the one thing the person typing it already knew.
// So the same comparison the dashboard's second step renders is printed above
// the prompt — and it is the same property that makes it safe to print, since
// no value is in the answer to begin with.

// rollbackFixtures is an environment one release from home, with a diff to
// read: a literal that changed, one that goes away, one that does not move.
func rollbackFixtures(h *harness) {
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, ProductionEnvironment: "shop-production"}
	h.platform.environments = []environment{{
		Name: "shop-production", Project: testProject, Release: "shop-rel-42", Phase: "Live",
		History: []releaseHistory{{Release: "shop-rel-41", Reason: "promoted"}},
	}}
	h.platform.configDiff = &configDiff{
		Release: "shop-rel-41", Against: "shop-rel-42", Project: testProject,
		Variables: []variableChange{
			{Name: "NEXT_PUBLIC_CDN", Change: "changed", Source: "value", AgainstSource: "value"},
			{Name: "FEATURE_BULK_IMPORT", Change: "removed", AgainstSource: "value"},
			{Name: "SESSION_SECRET", Change: "changed", Source: "claim", AgainstSource: "secret"},
			{Name: "NODE_ENV", Change: "unchanged", Source: "value", AgainstSource: "value"},
		},
		Runtime: []fieldChange{
			{Field: "replicas", From: "3", To: "2", Changed: true},
			{Field: "port", From: "3000", To: "3000"},
		},
		Processes: []processChange{
			{Name: "nightly", Change: "changed", Type: "cron", Schedule: "0 2 * * *"},
		},
	}
}

func TestRollbackShowsWhatChangesBeforeItAsks(t *testing.T) {
	h := newHarness(t)
	rollbackFixtures(h)
	h.stdinTerminal = true
	h.stdin = strings.NewReader("y\n")

	if code := h.run("rollback"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	// The comparison is asked for in the direction of the write: where the
	// environment is going, against where it is now.
	asked := h.platform.sent("GET", "/releases/shop-rel-41/config-diff")
	if len(asked) != 1 || !strings.Contains(asked[0].Query, "against=shop-rel-42") {
		t.Fatalf("unexpected comparison: %+v", asked)
	}

	said := h.stderr.String()
	for _, want := range []string{
		"shop-rel-42 → shop-rel-41",
		"NEXT_PUBLIC_CDN",
		"the value differs",
		"FEATURE_BULK_IMPORT",
		"a value → unset",
		// A variable whose source moved is the change no comparison of values
		// would have found, so it is spelled out rather than called "changed".
		"a secret → a claim binding",
		"replicas",
		"3 → 2",
		"nightly",
		"0 2 * * *",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the diff does not mention %q:\n%s", want, said)
		}
	}
	// What did not move is not listed. It is reassurance in the dashboard,
	// where it is one row; here it would be the diff's own noise.
	if strings.Contains(said, "NODE_ENV") || strings.Contains(said, "port") {
		t.Errorf("the diff lists what did not change:\n%s", said)
	}
}

// Nothing about the diff is a gate. It is a viewer's read that can fail on its
// own — an older platform, a snapshot from before the field existed — and a
// rollback stopped for it would be the safety feature making the outage
// longer.
func TestRollbackStillMovesWhenTheDiffCannotBeRead(t *testing.T) {
	h := newHarness(t)
	rollbackFixtures(h)
	h.platform.configDiff = nil
	h.stdinTerminal = true
	h.stdin = strings.NewReader("y\n")

	if code := h.run("rollback"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "could not read what changes") {
		t.Errorf("the failure to compare went unsaid:\n%s", h.stderr.String())
	}
	if patches := h.platform.sent("PATCH", "/environments/shop-production"); len(patches) != 1 {
		t.Fatalf("the move did not happen: %+v", patches)
	}
}

// `--yes` and `--json` are both "nobody is being asked". A diff nobody reads
// is a round trip spent on a stream something else is parsing.
func TestRollbackDoesNotCompareWhenNobodyIsAsked(t *testing.T) {
	for _, args := range [][]string{{"rollback", "--yes"}, {"rollback", "--yes", "--json"}} {
		h := newHarness(t)
		rollbackFixtures(h)

		if code := h.run(args...); code != exitOK {
			t.Fatalf("%v: exit %d, stderr: %s", args, code, h.stderr.String())
		}
		if asked := h.platform.sent("GET", "/releases/shop-rel-41/config-diff"); len(asked) != 0 {
			t.Errorf("%v: compared anyway: %+v", args, asked)
		}
	}
}

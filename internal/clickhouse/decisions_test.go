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

package clickhouse

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A stored decision is a reproduction contract: the row must carry both
// digests and the whole input, and read them back under the same names, or
// replay has nothing to stand on.

func TestInsertDecisionWritesEveryColumn(t *testing.T) {
	store := newFakeLogStore(t)
	err := store.client(t).InsertDecision(context.Background(), Decision{
		ID:           "0d9a1f7e-1111-2222-3333-444444444444",
		Timestamp:    time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Kind:         "promotion",
		Project:      "shop",
		Environment:  "shop-production",
		Release:      "shop-rel-7",
		Artifact:     "registry.example.com/kitchen/shop@sha256:" + strings.Repeat("a", 64),
		BundleDigest: "sha256:" + strings.Repeat("b", 64),
		InputDigest:  "sha256:" + strings.Repeat("c", 64),
		DataSnapshot: "trivy-db-2026-08-20",
		Verdict:      "blocked",
		RulesFired:   `[{"rule":"require-sbom","message":"no SBOM"}]`,
		Input:        `{"kind":"promotion"}`,
		DecidedBy:    "system:controller/promotion",
	})
	if err != nil {
		t.Fatalf("InsertDecision: %v", err)
	}

	_, payload, found := strings.Cut(store.query, "FORMAT JSONEachRow\n")
	if !found {
		t.Fatalf("the insert is not a JSONEachRow statement: %s", store.query)
	}
	row := map[string]any{}
	if err := json.Unmarshal([]byte(payload), &row); err != nil {
		t.Fatalf("the inserted row is not JSON: %v (%s)", err, payload)
	}
	for column, want := range map[string]any{
		"id":            "0d9a1f7e-1111-2222-3333-444444444444",
		"kind":          "promotion",
		"project":       "shop",
		"environment":   "shop-production",
		"release":       "shop-rel-7",
		"artifact":      "registry.example.com/kitchen/shop@sha256:" + strings.Repeat("a", 64),
		"bundle_digest": "sha256:" + strings.Repeat("b", 64),
		"input_digest":  "sha256:" + strings.Repeat("c", 64),
		"data_snapshot": "trivy-db-2026-08-20",
		"verdict":       "blocked",
		"rules_fired":   `[{"rule":"require-sbom","message":"no SBOM"}]`,
		"input":         `{"kind":"promotion"}`,
		"decided_by":    "system:controller/promotion",
	} {
		if row[column] != want {
			t.Errorf("column %s wrote %v, want %v", column, row[column], want)
		}
	}
	if row["timestamp"] != "2026-08-20 12:00:00.000" {
		t.Errorf("timestamp wrote %v, want the millisecond form the column stores", row["timestamp"])
	}
}

func TestInsertDecisionIsInsertIfAbsent(t *testing.T) {
	// The promotion path derives a deterministic decision id, so a requeue
	// re-stores the same decision under the same id — and the insert, like
	// the bundle insert, recognises its own earlier row instead of keeping
	// two on a plain MergeTree.
	present := newFakeLogStore(t)
	present.rows = "1"
	if err := present.client(t).InsertDecision(context.Background(), Decision{
		ID: "0d9a1f7e-1111-2222-3333-444444444444", Kind: "promotion", Verdict: "allowed",
	}); err != nil {
		t.Fatalf("InsertDecision: %v", err)
	}
	if present.sawQuery("INSERT INTO") {
		t.Fatalf("a present decision must not be re-inserted:\n%s", present.transcript())
	}
	if !present.sawQuery("id = {id:String}") {
		t.Fatalf("the probe must ask by id:\n%s", present.transcript())
	}
}

func TestQueryDecisionsFiltersAndReadsBack(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"id":"d-1","ts":"2026-08-20T12:00:00.000Z","kind":"promotion",` +
		`"project":"shop","environment":"shop-production","release":"shop-rel-7",` +
		`"artifact":"reg/shop@sha256:aa","bundle_digest":"sha256:bb","input_digest":"sha256:cc",` +
		`"data_snapshot":"","verdict":"allowed","rules_fired":"[]","input":"{}","decided_by":"grace@example.com"}`

	decisions, err := store.client(t).QueryDecisions(context.Background(), DecisionQuery{
		Project:     "shop",
		Environment: "shop-production",
		Release:     "shop-rel-7",
		Verdict:     "allowed",
		Kind:        "promotion",
		Since:       time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("QueryDecisions: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("want one decision, got %d", len(decisions))
	}
	decision := decisions[0]
	if decision.ID != "d-1" || decision.Verdict != "allowed" || decision.BundleDigest != "sha256:bb" {
		t.Errorf("decision read back as %+v", decision)
	}
	if !decision.Timestamp.Equal(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("timestamp read back as %v", decision.Timestamp)
	}

	// Every filter is parameterized, never spliced, and newest first is the
	// read order — the screens ask "what happened lately to this pair".
	for _, fragment := range []string{
		"project = {project:String}",
		"environment = {environment:String}",
		"release = {release:String}",
		"verdict = {verdict:String}",
		"kind = {kind:String}",
		"timestamp >= parseDateTime64BestEffort({since:String}, 3, 'UTC')",
		"ORDER BY timestamp DESC",
	} {
		if !strings.Contains(store.query, fragment) {
			t.Errorf("the statement does not carry %s:\n%s", fragment, store.query)
		}
	}
	if store.params.Get("param_project") != "shop" || store.params.Get("param_verdict") != "allowed" {
		t.Errorf("the filters were not passed as parameters: %v", store.params)
	}
}

func TestDecisionByIDDistinguishesAbsentFromBroken(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = ""

	_, found, err := store.client(t).Decision(context.Background(), "d-404")
	if err != nil || found {
		t.Fatalf("an absent decision is an answer, not an error: found=%v err=%v", found, err)
	}
	if !strings.Contains(store.query, "id = {id:String}") {
		t.Errorf("the id must be a parameter:\n%s", store.query)
	}
}

func TestInsertPolicyBundleIsInsertIfAbsent(t *testing.T) {
	store := newFakeLogStore(t)

	// Absent: the existence probe answers nothing, so the insert follows.
	store.rows = ""
	if err := store.client(t).InsertPolicyBundle(context.Background(),
		"sha256:"+strings.Repeat("b", 64), `{"promotion.rego":"package kitchen.promotion"}`); err != nil {
		t.Fatalf("InsertPolicyBundle: %v", err)
	}
	if !store.sawQuery("INSERT INTO") || !store.sawQuery(PolicyBundlesTable) {
		t.Fatalf("an absent bundle must be inserted:\n%s", store.transcript())
	}

	// Present: the probe answers a row, and nothing is written again — the
	// same bytes have nothing new to say.
	present := newFakeLogStore(t)
	present.rows = "1"
	if err := present.client(t).InsertPolicyBundle(context.Background(),
		"sha256:"+strings.Repeat("b", 64), `{}`); err != nil {
		t.Fatalf("InsertPolicyBundle: %v", err)
	}
	if present.sawQuery("INSERT INTO") {
		t.Fatalf("a present bundle must not be re-inserted:\n%s", present.transcript())
	}
}

func TestPolicyBundleReadsContentBack(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"content":"{\"promotion.rego\":\"package kitchen.promotion\"}"}`

	content, found, err := store.client(t).PolicyBundle(context.Background(), "sha256:"+strings.Repeat("b", 64))
	if err != nil || !found {
		t.Fatalf("PolicyBundle: found=%v err=%v", found, err)
	}
	if content != `{"promotion.rego":"package kitchen.promotion"}` {
		t.Errorf("content read back as %q", content)
	}
}

// The policy tables answer to the compliance retention, not the telemetry
// one, exactly as the audit log does — turning collection down must not
// shorten the evidence.
func TestEnsurePolicySchemaIsNotPartOfTheTelemetrySchema(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = ""

	if err := store.client(t).EnsureTelemetrySchema(context.Background(), 30); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}
	if store.sawQuery(PromotionDecisionsTable) || store.sawQuery(PolicyBundlesTable) {
		t.Errorf("the telemetry schema touched the policy tables:\n%s", store.transcript())
	}
}

func TestEnsurePolicySchemaCreatesBothTablesAndOnlyOneTTL(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = ""

	if err := store.client(t).EnsurePolicySchema(context.Background(), 365); err != nil {
		t.Fatalf("EnsurePolicySchema: %v", err)
	}
	if !store.sawQuery("CREATE TABLE IF NOT EXISTS") || !store.sawQuery(PromotionDecisionsTable) {
		t.Fatalf("the decisions table was not created:\n%s", store.transcript())
	}
	if !store.sawQuery(PolicyBundlesTable) {
		t.Fatalf("the bundles table was not created:\n%s", store.transcript())
	}
	// The decisions table takes the retention; the bundles table must not —
	// a bundle has to outlive every decision that cites it.
	for _, query := range store.queries {
		if strings.Contains(query, PolicyBundlesTable) && strings.Contains(query, "TTL") {
			t.Errorf("the bundles table carries a TTL:\n%s", query)
		}
	}
}

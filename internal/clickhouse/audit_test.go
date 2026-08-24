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

// The audit row goes out and comes back through the same field names. The
// round trip is what these tests are about: a column the writer and the reader
// spell differently is a field that silently reads back empty, and an empty
// field in a hashed record is a chain that will not verify.
func TestInsertAuditRecordWritesEveryColumn(t *testing.T) {
	store := newFakeLogStore(t)
	err := store.client(t).InsertAuditRecord(context.Background(), AuditRecord{
		Sequence:    7,
		Timestamp:   time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC),
		Actor:       "grace@example.com",
		ActorKind:   ActorUser,
		Correlation: "abc123",
		Operation:   AuditCreate,
		Kind:        "Project",
		Namespace:   "kitchen-system",
		Name:        "shop",
		UID:         "11111111-2222-3333-4444-555555555555",
		Project:     "shop",
		FromState:   "",
		ToState:     "shop",
		Reason:      "project shop created from acme/shop",
		Details:     `{"repo":"acme/shop"}`,
		PrevHash:    strings.Repeat("0", 64),
		Hash:        strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatalf("InsertAuditRecord: %v", err)
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
		"sequence":    float64(7),
		"actor":       "grace@example.com",
		"actor_kind":  ActorUser,
		"correlation": "abc123",
		"operation":   AuditCreate,
		"kind":        "Project",
		"name":        "shop",
		"uid":         "11111111-2222-3333-4444-555555555555",
		"project":     "shop",
		"to_state":    "shop",
		"details":     `{"repo":"acme/shop"}`,
		"hash":        strings.Repeat("a", 64),
	} {
		if row[column] != want {
			t.Errorf("column %s wrote %v, want %v", column, row[column], want)
		}
	}
	// The timestamp is written at the precision the column holds, because the
	// hash is over that value and not over the one the caller had.
	if row["timestamp"] != "2026-03-01 09:30:00.000" {
		t.Errorf("timestamp wrote %v, want the millisecond form the column stores", row["timestamp"])
	}
}

func TestQueryAuditRecordsReadsARowBack(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"seq":"7","ts":"2026-03-01T09:30:00.000Z","actor":"grace@example.com",` +
		`"actor_kind":"user","correlation":"abc123","operation":"create","kind":"Project",` +
		`"namespace":"kitchen-system","name":"shop","uid":"u-1","project":"shop",` +
		`"from_state":"","to_state":"shop","reason":"created","details":"{}",` +
		`"prev_hash":"` + strings.Repeat("0", 64) + `","hash":"` + strings.Repeat("a", 64) + `"}`

	records, err := store.client(t).QueryAuditRecords(context.Background(), AuditQuery{
		Kind: "Project", Name: "shop", Actor: "grace@example.com",
	})
	if err != nil {
		t.Fatalf("QueryAuditRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("want one record, got %d", len(records))
	}
	record := records[0]
	if record.Sequence != 7 || record.Actor != "grace@example.com" || record.Kind != "Project" {
		t.Errorf("record read back as %+v", record)
	}
	if !record.Timestamp.Equal(time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("timestamp read back as %v", record.Timestamp)
	}
	if record.Hash != strings.Repeat("a", 64) || record.PrevHash != strings.Repeat("0", 64) {
		t.Errorf("the chain fields did not survive the round trip: %+v", record)
	}

	for _, filter := range []string{"kind = {kind:String}", "name = {name:String}", "actor = {actor:String}"} {
		if !strings.Contains(store.query, filter) {
			t.Errorf("the statement does not filter on %s:\n%s", filter, store.query)
		}
	}
	if !strings.Contains(store.query, "ORDER BY sequence DESC") {
		t.Errorf("a page of the log is not newest first:\n%s", store.query)
	}
}

// The verifier walks the chain forwards, and it can only do that if the scan
// is ordered by sequence rather than by time. Two records written in the same
// millisecond have an order, and it is not the timestamp's.
func TestScanAuditRecordsReadsInChainOrder(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = ""

	if _, err := store.client(t).ScanAuditRecords(context.Background(), 40, 10); err != nil {
		t.Fatalf("ScanAuditRecords: %v", err)
	}
	if !strings.Contains(store.query, "ORDER BY sequence ASC") {
		t.Errorf("the verifier's scan is not in chain order:\n%s", store.query)
	}
	if !strings.Contains(store.query, "sequence >= {from:UInt64}") {
		t.Errorf("the scan is not bounded by sequence:\n%s", store.query)
	}
	if got := store.params.Get("param_from"); got != "40" {
		t.Errorf("the scan started at %q, want 40", got)
	}
}

// A chain with nothing in it is a first append, not a fault.
func TestAuditHeadOfAnEmptyChain(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = ""

	head, err := store.client(t).AuditHead(context.Background())
	if err != nil {
		t.Fatalf("AuditHead: %v", err)
	}
	if head.Sequence != 0 || head.Hash != "" {
		t.Errorf("an empty chain has head %+v, want the zero record", head)
	}
}

func TestAuditTableIsOrderedForVerificationAndCarriesItsOwnRetention(t *testing.T) {
	ddl := createAuditTable("kitchen", 365)
	if !strings.Contains(ddl, "ORDER BY (sequence)") {
		t.Errorf("the audit table is not ordered by sequence, which is the only order it can be verified in:\n%s", ddl)
	}
	if !strings.Contains(ddl, "toIntervalDay(365)") {
		t.Errorf("the audit table did not take the retention it was given:\n%s", ddl)
	}
	// Every hashed field has to be a column, or a record read back would be
	// missing part of what was signed over.
	for _, column := range []string{
		"sequence", "timestamp", "actor", "actor_kind", "correlation", "operation",
		"kind", "namespace", "name", "uid", "project", "from_state", "to_state",
		"reason", "details", "prev_hash", "hash",
	} {
		if !strings.Contains(ddl, column) {
			t.Errorf("the audit table has no %s column:\n%s", column, ddl)
		}
	}
}

// Audit retention is not telemetry retention. They are set separately and
// applied separately, and a change to one must not move the other.
func TestEnsureAuditSchemaIsNotPartOfTheTelemetrySchema(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = ""

	if err := store.client(t).EnsureTelemetrySchema(context.Background(), 30); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}
	if store.sawQuery(AuditTable) {
		t.Errorf("the telemetry schema touched the audit table:\n%s", store.transcript())
	}
}

// The privileged filter is a predicate over the hashed details rather than a
// column, because a column would change the hash of every record ever
// written. What matters for the read is that the predicate is there and that
// a class implies the boolean.
func TestQueryAuditRecordsFiltersOnThePrivilegedMarking(t *testing.T) {
	store := newFakeLogStore(t)
	client := store.client(t)

	if _, err := client.QueryAuditRecords(context.Background(), AuditQuery{Privileged: true}); err != nil {
		t.Fatalf("QueryAuditRecords: %v", err)
	}
	if !strings.Contains(store.query, "JSONExtractBool(details, 'privileged') = 1") {
		t.Errorf("a privileged-only page does not filter on the marking:\n%s", store.query)
	}

	// A class narrows further and implies the marking: a record carrying a
	// class but not the boolean is not a shape the recorder can write, and a
	// query that trusted the class alone would be trusting a hand-edited row.
	if _, err := client.QueryAuditRecords(context.Background(),
		AuditQuery{PrivilegeClass: "break-glass"}); err != nil {
		t.Fatalf("QueryAuditRecords: %v", err)
	}
	if !strings.Contains(store.query, "JSONExtractBool(details, 'privileged') = 1") {
		t.Errorf("a class filter must still require the marking:\n%s", store.query)
	}
	if !strings.Contains(store.query, "JSONExtractString(details, 'privilegedClass') = {privilegedClass:String}") {
		t.Errorf("the class is not filtered on:\n%s", store.query)
	}

	// And an ordinary page asks for none of it.
	if _, err := client.QueryAuditRecords(context.Background(), AuditQuery{Kind: "Project"}); err != nil {
		t.Fatalf("QueryAuditRecords: %v", err)
	}
	if strings.Contains(store.query, "privileged") {
		t.Errorf("an unfiltered page must not narrow to privileged records:\n%s", store.query)
	}
}

// Orphan detection reads the log for who was last seen doing something. It
// asks about people only: a controller actor would sit at the top of every
// answer and is not an identity anybody holds.
func TestActorActivityAnswersTheNewestRecordPerPerson(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"actor":"grace@example.com","ts":"2026-03-01T09:30:00.000Z"}` + "\n" +
		`{"actor":"heidi@example.com","ts":"2025-01-04T11:00:00.000Z"}`

	activity, err := store.client(t).ActorActivity(context.Background())
	if err != nil {
		t.Fatalf("ActorActivity: %v", err)
	}
	if len(activity) != 2 {
		t.Fatalf("want two actors, got %d", len(activity))
	}
	if !activity["grace@example.com"].Equal(time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("grace's last activity read back as %v", activity["grace@example.com"])
	}
	if !strings.Contains(store.query, "actor_kind = 'user'") {
		t.Errorf("the survey must ask about people, not controllers:\n%s", store.query)
	}
	if !strings.Contains(store.query, "GROUP BY actor") {
		t.Errorf("the survey must be one row per identity:\n%s", store.query)
	}
}

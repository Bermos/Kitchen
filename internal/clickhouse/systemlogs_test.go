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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// dropStatement is the shape every drop here takes, kept once because goconst
// counts a literal repeated across a package's test files.
const dropStatement = "DROP TABLE IF EXISTS system."

// fakeSystemLogStore answers the sweep's read and records what it is asked to
// drop.
type fakeSystemLogStore struct {
	server *httptest.Server
	// rows is what `system.tables` answers with, as ClickHouse's default TSV.
	rows string
	// dropFailure, when set, is the error a DROP is refused with; refuseFrom
	// is the 1-based drop it starts refusing at, so a store can accept the
	// first and refuse the second.
	dropFailure string
	refuseFrom  int

	queries []string
	drops   []string
}

func newFakeSystemLogStore(t *testing.T, rows string) *fakeSystemLogStore {
	t.Helper()
	store := &fakeSystemLogStore{rows: rows}
	store.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		query := string(body)
		store.queries = append(store.queries, query)

		if strings.HasPrefix(query, dropStatement) {
			store.drops = append(store.drops, query)
			if store.dropFailure != "" && len(store.drops) >= store.refuseFrom {
				http.Error(w, store.dropFailure, http.StatusBadRequest)
			}
			return
		}
		_, _ = io.WriteString(w, store.rows)
	}))
	t.Cleanup(store.server.Close)
	return store
}

func (s *fakeSystemLogStore) client(t *testing.T) *Client {
	t.Helper()
	endpoint, err := url.Parse(s.server.URL)
	if err != nil {
		t.Fatalf("parsing the test server URL: %v", err)
	}
	return New(Config{
		Host:     endpoint.Hostname(),
		HTTPPort: endpoint.Port(),
		Database: testDatabase,
		Username: testUsername,
		Password: testPassword,
	})
}

// The whole of the upgrade path: the tables ClickHouse renamed away when the
// chart's TTLs arrived are dropped, and what they cost is answered back so an
// operator can read how much of the volume came back.
func TestOrphanedSystemLogTablesAreDroppedAndCounted(t *testing.T) {
	store := newFakeSystemLogStore(t, "text_log_0\t6485000000\ntrace_log_0\t4456000000\n")

	dropped, err := store.client(t).ReclaimOrphanedSystemLogs(context.Background())
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped %d tables, want 2: %+v", len(dropped), dropped)
	}
	if dropped[0].Name != "text_log_0" || dropped[0].Bytes != 6485000000 {
		t.Errorf("first table is %+v, want text_log_0 at 6485000000 bytes", dropped[0])
	}

	// SYNC, so the bytes are gone when this returns: without it the drop is
	// queued and the number reported afterwards describes a volume that has
	// not changed yet.
	want := []string{
		dropStatement + "`text_log_0` SYNC",
		dropStatement + "`trace_log_0` SYNC",
	}
	if len(store.drops) != len(want) {
		t.Fatalf("sent %d drops, want %d: %v", len(store.drops), len(want), store.drops)
	}
	for i, statement := range want {
		if store.drops[i] != statement {
			t.Errorf("drop %d is %q, want %q", i, store.drops[i], statement)
		}
	}
}

// The ordinary case, which is every reconcile after the first: nothing has
// been orphaned, so nothing is dropped. The sweep runs on every reconcile
// rather than once precisely because a store can orphan a table at any time —
// each log is renamed when it is next written, not when the server starts — so
// this being cheap and silent is what makes that affordable.
func TestAStoreWithNothingOrphanedIsNotWrittenTo(t *testing.T) {
	store := newFakeSystemLogStore(t, "")

	dropped, err := store.client(t).ReclaimOrphanedSystemLogs(context.Background())
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped %+v on a store with no orphans", dropped)
	}
	if len(store.drops) != 0 {
		t.Errorf("sent %v to a store with no orphans", store.drops)
	}
	if len(store.queries) != 1 {
		t.Errorf("asked %d questions, want the one read of system.tables: %v",
			len(store.queries), store.queries)
	}

	// And what that one read is scoped to, because it is what decides which
	// tables can ever reach a DROP. Without the database clause the same
	// pattern would match a `kitchen.text_log_0` an installation made itself;
	// without the engine clause it would match a View or a Dictionary named
	// like one, which cannot be dropped as a table at all.
	read := store.queries[0]
	for _, clause := range []string{"database = 'system'", "engine LIKE '%MergeTree'"} {
		if !strings.Contains(read, clause) {
			t.Errorf("the sweep's read does not carry %q, so it is not confined to "+
				"ClickHouse's own log tables:\n%s", clause, read)
		}
	}
}

// A store that refuses partway through has still reclaimed what it dropped
// before it refused, and the caller records that rather than losing it — which
// is the path that matters, because the status is what tells an operator how
// much of the volume actually came back. The fake accepts the first drop and
// refuses the second.
func TestARefusedDropStillReportsWhatWasAlreadyReclaimed(t *testing.T) {
	store := newFakeSystemLogStore(t, "text_log_0\t100\ntrace_log_0\t200\n")
	store.dropFailure = "Code: 497. DB::Exception: kitchen: Not enough privileges"
	store.refuseFrom = 2

	dropped, err := store.client(t).ReclaimOrphanedSystemLogs(context.Background())
	if err == nil {
		t.Fatal("a refused drop was reported as a successful sweep")
	}
	if !strings.Contains(err.Error(), "trace_log_0") {
		t.Errorf("the error does not name the table it failed on: %v", err)
	}
	if len(dropped) != 1 || dropped[0].Name != "text_log_0" || dropped[0].Bytes != 100 {
		t.Fatalf("reported %+v as dropped; the first drop succeeded and its 100 bytes are "+
			"reclaimed whatever happened to the second", dropped)
	}
	// Both were attempted: a refusal stops the sweep, and the one that
	// already went is not retried by this call.
	if len(store.drops) != 2 {
		t.Errorf("sent %v; the sweep should have tried both and stopped there", store.drops)
	}
}

// The first drop refused is a store that reclaimed nothing, and says so rather
// than claiming a table it never dropped.
func TestAStoreThatRefusesEveryDropReclaimsNothing(t *testing.T) {
	store := newFakeSystemLogStore(t, "text_log_0\t100\ntrace_log_0\t200\n")
	store.dropFailure = "Code: 497. DB::Exception: kitchen: Not enough privileges"
	store.refuseFrom = 1

	dropped, err := store.client(t).ReclaimOrphanedSystemLogs(context.Background())
	if err == nil {
		t.Fatal("a refused drop was reported as a successful sweep")
	}
	if len(dropped) != 0 {
		t.Errorf("reported %+v as dropped, and the store refused the first one", dropped)
	}
}

// The name goes into a DROP, and it arrives over a network from a server that
// was asked to match a pattern. It is checked again on the way in rather than
// trusted, and a row that does not match stops the sweep instead of being
// dropped anyway.
func TestARowThatIsNotASupersededLogTableIsRefusedRatherThanDropped(t *testing.T) {
	store := newFakeSystemLogStore(t, "parts\t9000000\n")

	if _, err := store.client(t).ReclaimOrphanedSystemLogs(context.Background()); err == nil {
		t.Fatal("system.parts was accepted as a superseded log table")
	}
	if len(store.drops) != 0 {
		t.Errorf("sent %v after refusing the row", store.drops)
	}
}

// What the pattern may and may not match. The live tables are the ones the
// platform's own diagnostics are read from, and a pattern that caught one of
// them would delete the log an operator is in the middle of reading; a table
// somebody else named is not this platform's to touch at all.
func TestOnlySupersededCopiesOfTheBoundedTablesMatch(t *testing.T) {
	for _, name := range []string{"text_log_0", "trace_log_12", "error_log_3"} {
		if !orphanPattern.MatchString(name) {
			t.Errorf("system.%s is a superseded log table and is not collected", name)
		}
	}
	for _, name := range []string{
		"text_log",   // the live table
		"query_log",  // the live table
		"my_log_0",   // never configured by this chart
		"text_log_x", // not a rename ClickHouse makes
		"parts",
	} {
		if orphanPattern.MatchString(name) {
			t.Errorf("system.%s matches the sweep, which would drop a table this platform "+
				"neither created nor superseded", name)
		}
	}
}

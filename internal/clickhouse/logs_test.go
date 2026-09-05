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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeLogStore answers log queries with rows, and records what it was asked.
//
// `query` is the last statement, which is all most readers send. `queries` is
// every one of them, for the readers that take more than one round trip — see
// Trace, which asks the lookup table when a trace happened before it asks for
// its spans.
type fakeLogStore struct {
	server  *httptest.Server
	query   string
	queries []string
	params  url.Values
	rows    string
	// answer overrides rows per statement, so a two-round-trip reader can be
	// given a different answer for each.
	answer func(query string) string
}

func newFakeLogStore(t *testing.T) *fakeLogStore {
	t.Helper()
	store := &fakeLogStore{}
	store.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		store.query = string(body)
		store.queries = append(store.queries, store.query)
		store.params = r.URL.Query()

		rows := store.rows
		if store.answer != nil {
			rows = store.answer(store.query)
		}
		_, _ = io.WriteString(w, rows)
	}))
	t.Cleanup(store.server.Close)
	return store
}

// sawQuery reports whether any statement carried the fragment.
func (s *fakeLogStore) sawQuery(fragment string) bool {
	for _, query := range s.queries {
		if strings.Contains(query, fragment) {
			return true
		}
	}
	return false
}

func (s *fakeLogStore) transcript() string {
	return strings.Join(s.queries, "\n---\n")
}

func (s *fakeLogStore) client(t *testing.T) *Client {
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

func TestSearchLogsReadsABuildsOutputForwards(t *testing.T) {
	store := newFakeLogStore(t)
	// ClickHouse is asked for the newest lines first.
	store.rows = strings.Join([]string{
		`{"ts":"2026-08-13T10:00:02.000Z","source":"build","build":"shop-bld-1","stream":"stdout","message":"done"}`,
		`{"ts":"2026-08-13T10:00:01.000Z","source":"build","build":"shop-bld-1","stream":"stdout","message":"building"}`,
	}, "\n")

	lines, err := store.client(t).SearchLogs(context.Background(), LogQuery{
		Source: SourceBuild,
		Build:  "shop-bld-1",
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("want two lines, got %d", len(lines))
	}
	if lines[0].Message != "building" || lines[1].Message != "done" {
		t.Fatalf("a log reads forwards: %q then %q", lines[0].Message, lines[1].Message)
	}
	if !lines[0].Timestamp.Equal(time.Date(2026, 8, 13, 10, 0, 1, 0, time.UTC)) {
		t.Fatalf("unexpected timestamp: %s", lines[0].Timestamp)
	}
}

func TestSearchLogsPassesValuesAsParametersNotAsQueryText(t *testing.T) {
	store := newFakeLogStore(t)

	since := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	_, err := store.client(t).SearchLogs(context.Background(), LogQuery{
		Source:      SourceRuntime,
		Project:     "shop",
		Environment: "shop-production",
		Search:      "'; DROP TABLE logs; --",
		Since:       since,
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}

	if strings.Contains(store.query, "DROP TABLE") {
		t.Fatalf("a search term reached the query text:\n%s", store.query)
	}
	if got := store.params.Get("param_search"); got != "'; DROP TABLE logs; --" {
		t.Fatalf("the search term should travel as a parameter, got %q", got)
	}
	for name, want := range map[string]string{
		"param_project":     "shop",
		"param_environment": "shop-production",
		"param_source":      SourceRuntime,
		"param_limit":       "10",
	} {
		if got := store.params.Get(name); got != want {
			t.Errorf("%s: want %q, got %q", name, want, got)
		}
	}
	if !strings.HasPrefix(store.params.Get("param_since"), "2026-08-13T09:00:00") {
		t.Errorf("the window should travel as a parameter, got %q", store.params.Get("param_since"))
	}
}

func TestSearchLogsBoundsTheLimit(t *testing.T) {
	store := newFakeLogStore(t)

	for _, testCase := range []struct{ asked, want string }{
		{"", "200"},
		{"beyond", "5000"},
	} {
		query := LogQuery{Build: "shop-bld-1"}
		if testCase.asked == "beyond" {
			query.Limit = MaxLogLimit * 10
		}
		if _, err := store.client(t).SearchLogs(context.Background(), query); err != nil {
			t.Fatalf("SearchLogs: %v", err)
		}
		if got := store.params.Get("param_limit"); got != testCase.want {
			t.Fatalf("limit %q: want %s, got %s", testCase.asked, testCase.want, got)
		}
	}
}

func TestSearchLogsRefusesAnUnscopedQuery(t *testing.T) {
	store := newFakeLogStore(t)

	// Without a scope this would read every line in the cluster.
	if _, err := store.client(t).SearchLogs(context.Background(), LogQuery{Limit: 10}); err == nil {
		t.Fatal("an unscoped log query should be refused")
	}
}

func TestFilterLogsCompilesTheSelectionAndRunsItReadOnly(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"ts":"2026-08-13T10:00:01.000Z","source":"runtime","project":"shop","stream":"stderr","message":"boom"}`

	since := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	lines, err := store.client(t).FilterLogs(context.Background(), LogFilter{
		LogSelection: LogSelection{
			Query: "level:error",
			Scope: LogScope{Projects: []string{testProject}},
			Since: since,
		},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("FilterLogs: %v", err)
	}
	if len(lines) != 1 || lines[0].Message != "boom" {
		t.Fatalf("unexpected lines: %+v", lines)
	}

	// Nothing the caller typed is in the statement: the scope and the level
	// both leave as parameters, and the text around them is this package's.
	if !strings.Contains(store.query, "(project IN ({scope0:String}))") {
		t.Fatalf("the scope should bound the statement structurally:\n%s", store.query)
	}
	if got := store.params.Get("param_scope0"); got != testProject {
		t.Fatalf("the scope's names should travel as parameters, got %q", got)
	}
	if strings.Contains(store.query, "error") {
		t.Fatalf("a value the caller typed should never reach the statement:\n%s", store.query)
	}
	if got := store.params.Get("readonly"); got != "2" {
		t.Fatalf("a log query must run read-only, got readonly=%q", got)
	}
	if store.params.Get("max_execution_time") == "" {
		t.Fatalf("a log query must carry an execution cap")
	}
	// The window and the limit still travel as parameters.
	if !strings.HasPrefix(store.params.Get("param_since"), "2026-08-13T09:00:00") {
		t.Errorf("the window should travel as a parameter, got %q", store.params.Get("param_since"))
	}
	if got := store.params.Get("param_limit"); got != "10" {
		t.Errorf("the limit should travel as a parameter, got %q", got)
	}
}

// The observability view's first query — an empty one over the last hour,
// before a single line exists — used to be refused by ClickHouse, because the
// formatted timestamp was selected as `timestamp` and shadowed the column the
// window compares against ("No operation greaterOrEquals between String and
// DateTime64"). Both readers select it under another name so that `timestamp`
// in a condition is always the column.
func TestTheFormattedTimestampDoesNotShadowTheColumn(t *testing.T) {
	store := newFakeLogStore(t)
	since := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)

	if _, err := store.client(t).FilterLogs(context.Background(), LogFilter{
		LogSelection: LogSelection{Since: since, Scope: LogScope{Platform: true}},
	}); err != nil {
		t.Fatalf("FilterLogs: %v", err)
	}
	assertNoShadow(t, store.query)

	if _, err := store.client(t).SearchLogs(context.Background(), LogQuery{Build: "shop-bld-1", Since: since}); err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	assertNoShadow(t, store.query)
}

func assertNoShadow(t *testing.T, query string) {
	t.Helper()
	if strings.Contains(query, "AS "+timeColumnLogs) {
		t.Fatalf("the formatted timestamp must not take the column's name:\n%s", query)
	}
	if !strings.Contains(query, "AS "+timestampAlias) {
		t.Fatalf("the formatted timestamp should be selected as %q:\n%s", timestampAlias, query)
	}
	if !strings.Contains(query, logSinceCondition) {
		t.Fatalf("the window should compare the column:\n%s", query)
	}
}

// An empty selection is a legitimate question — "everything in the window" —
// and the window and the limit are what bound it. It used to be refused, which
// is why the observability view opened with `1 = 1` in its query bar.
func TestFilterLogsAcceptsAnEmptySelection(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).FilterLogs(context.Background(), LogFilter{
		LogSelection: LogSelection{Query: "  ", Scope: LogScope{Platform: true}},
	}); err != nil {
		t.Fatalf("an empty selection should select everything: %v", err)
	}
	if strings.Contains(store.query, "WHERE") {
		t.Fatalf("nothing to filter on should mean no WHERE clause at all:\n%s", store.query)
	}
	if strings.Contains(store.query, "1 = 1") {
		t.Fatalf("everything should be the absence of a predicate, not a tautology:\n%s", store.query)
	}
}

func TestFilterLogsBoundsTheLimit(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).FilterLogs(context.Background(), LogFilter{
		LogSelection: LogSelection{Scope: LogScope{Platform: true}}, Limit: 999999,
	}); err != nil {
		t.Fatalf("FilterLogs: %v", err)
	}
	if got := store.params.Get("param_limit"); got != "5000" {
		t.Fatalf("the limit should be capped at %d, got %q", MaxLogLimit, got)
	}
}

func TestARefusedQueryIsTypedAsTheCallersError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Code: 62. DB::Exception: Syntax error")
	}))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := New(Config{
		Host: endpoint.Hostname(), HTTPPort: endpoint.Port(),
		Database: testDatabase, Username: testUsername,
	})

	_, err = client.FilterLogs(context.Background(), LogFilter{
		LogSelection: LogSelection{Query: `message:/(/`, Scope: LogScope{Platform: true}},
	})
	queryErr := &QueryError{}
	if !errors.As(err, &queryErr) {
		t.Fatalf("want a QueryError, got %v", err)
	}
	if !strings.Contains(queryErr.Message, "Syntax error") {
		t.Fatalf("the diagnostic should survive: %v", queryErr)
	}
}

// The API's vocabulary is not the table's. The lines come back with Kitchen's
// names because the projection renames them on the way out — there are no
// ALIAS columns in the DDL doing it quietly.
func TestTheProjectionTranslatesTheExportersColumns(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).FilterLogs(context.Background(), LogFilter{
		LogSelection: LogSelection{Scope: LogScope{Platform: true}},
	}); err != nil {
		t.Fatalf("FilterLogs: %v", err)
	}
	for _, want := range []string{
		"Body AS message",
		"TraceId AS traceId",
		"SpanId AS spanId",
		"LogAttributes AS fields",
		logLevelColumn + " AS level",
	} {
		if !strings.Contains(store.query, want) {
			t.Errorf("the projection is missing %q:\n%s", want, store.query)
		}
	}
	// The Kitchen columns are materialized under these names already, so
	// nothing renames them.
	for _, column := range []string{
		"source", "project", "environment", "build", "pod", "container", "stream",
	} {
		if strings.Contains(store.query, column+" AS "+column) {
			t.Errorf("%s is already its own name and should not be re-aliased:\n%s", column, store.query)
		}
	}
	if !strings.Contains(store.query, "FROM "+qualified(LogsTable)) {
		t.Errorf("the lines should come off the OTel log table:\n%s", store.query)
	}
}

// The level is folded to lower case on the way out. OTel leaves the spelling of
// SeverityText to whatever produced the line, and Kitchen's levels have always
// been lower case — the histogram counts `error`, the facet offers `error`, and
// clicking it has to produce a query that matches.
func TestTheLevelIsFoldedRatherThanPassedThrough(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"ts":"2026-08-13T10:00:01.000Z","level":"error","message":"boom"}`

	lines, err := store.client(t).FilterLogs(context.Background(), LogFilter{
		LogSelection: LogSelection{Query: "level:error", Scope: LogScope{Platform: true}},
	})
	if err != nil {
		t.Fatalf("FilterLogs: %v", err)
	}
	if len(lines) != 1 || lines[0].Level != levelError {
		t.Fatalf("unexpected lines: %+v", lines)
	}
	if !strings.Contains(store.query, logLevelColumn+" = {q0:String}") {
		t.Fatalf("the level query should compare the folded column:\n%s", store.query)
	}
}

// A line's timestamp survives at microsecond resolution. The column is
// DateTime64(9) and ClickHouse renders six fractional digits, which is finer
// than the millisecond the pre-collector table kept.
func TestALineKeepsItsMicroseconds(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"ts":"2026-08-13T10:00:01.123456Z","message":"boom"}`

	lines, err := store.client(t).FilterLogs(context.Background(),
		LogFilter{LogSelection: LogSelection{Scope: LogScope{Platform: true}}})
	if err != nil {
		t.Fatalf("FilterLogs: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("want one line, got %d", len(lines))
	}
	want := time.Date(2026, 8, 13, 10, 0, 1, 123456000, time.UTC)
	if !lines[0].Timestamp.Equal(want) {
		t.Fatalf("want %s, got %s", want, lines[0].Timestamp)
	}
}

// One firing of a scheduled job is one query, which is the whole reason the
// pipeline learned about processes and runs (#78). The run outlives the Job it
// names — the platform keeps a handful of finished Jobs and the lines for the
// whole container-log retention — so this is what makes a failure from three
// weeks ago readable at all.
func TestSearchLogsNarrowsToOneProcessAndOneRun(t *testing.T) {
	store := newFakeLogStore(t)

	_, err := store.client(t).SearchLogs(context.Background(), LogQuery{
		Source:      SourceRuntime,
		Project:     "shop",
		Environment: "shop-production",
		Process:     "nightly",
		Run:         "shop-production-nightly-29387520",
	})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}

	for _, condition := range []string{"`process` = {process:String}", "`run` = {run:String}"} {
		if !strings.Contains(store.query, condition) {
			t.Errorf("the query does not filter on %s:\n%s", condition, store.query)
		}
	}
	for name, want := range map[string]string{
		"param_process": "nightly",
		"param_run":     "shop-production-nightly-29387520",
	} {
		if got := store.params.Get(name); got != want {
			t.Errorf("%s: want %q, got %q", name, want, got)
		}
	}
	if !strings.Contains(store.query, "process, run") {
		t.Errorf("a line does not carry the process and run it came from:\n%s", store.query)
	}
}

// An environment's logs meant every line the environment wrote before either
// field existed, and they still do: the web process writes neither column, so
// an unfiltered read is not quietly "the web process only".
func TestSearchLogsWithoutAProcessIsStillEveryLine(t *testing.T) {
	store := newFakeLogStore(t)

	_, err := store.client(t).SearchLogs(context.Background(), LogQuery{
		Source:      SourceRuntime,
		Environment: "shop-production",
	})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if strings.Contains(store.query, "{process:String}") || strings.Contains(store.query, "{run:String}") {
		t.Fatalf("an unasked-for filter reached the query:\n%s", store.query)
	}
}

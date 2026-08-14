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
type fakeLogStore struct {
	server *httptest.Server
	query  string
	params url.Values
	rows   string
}

func newFakeLogStore(t *testing.T) *fakeLogStore {
	t.Helper()
	store := &fakeLogStore{}
	store.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		store.query = string(body)
		store.params = r.URL.Query()
		_, _ = io.WriteString(w, store.rows)
	}))
	t.Cleanup(store.server.Close)
	return store
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
		Database: "kitchen",
		Username: "kitchen",
		Password: "hunter2",
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

func TestFilterLogsRunsTheExpressionReadOnly(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"ts":"2026-08-13T10:00:01.000Z","source":"runtime","project":"shop","stream":"stderr","message":"boom"}`

	since := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	lines, err := store.client(t).FilterLogs(context.Background(), LogFilter{
		Where: "project = 'shop' AND stream = 'stderr'",
		Since: since,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("FilterLogs: %v", err)
	}
	if len(lines) != 1 || lines[0].Message != "boom" {
		t.Fatalf("unexpected lines: %+v", lines)
	}

	// The expression is the feature, so it appears in the query text as
	// written — which is exactly why the query must be pinned read-only and
	// time-capped.
	if !strings.Contains(store.query, "(project = 'shop' AND stream = 'stderr')") {
		t.Fatalf("the expression should reach the query text as written:\n%s", store.query)
	}
	if got := store.params.Get("readonly"); got != "2" {
		t.Fatalf("a caller-written query must run read-only, got readonly=%q", got)
	}
	if store.params.Get("max_execution_time") == "" {
		t.Fatalf("a caller-written query must carry an execution cap")
	}
	// The window and the limit still travel as parameters.
	if !strings.HasPrefix(store.params.Get("param_since"), "2026-08-13T09:00:00") {
		t.Errorf("the window should travel as a parameter, got %q", store.params.Get("param_since"))
	}
	if got := store.params.Get("param_limit"); got != "10" {
		t.Errorf("the limit should travel as a parameter, got %q", got)
	}
}

// The observability view's first query — `1 = 1` over the last hour, before a
// single line exists — used to be refused by ClickHouse, because the formatted
// timestamp was selected as `timestamp` and shadowed the column the window
// compares against ("No operation greaterOrEquals between String and
// DateTime64"). Both readers select it under another name so that `timestamp`
// in a condition is always the column.
func TestTheFormattedTimestampDoesNotShadowTheColumn(t *testing.T) {
	store := newFakeLogStore(t)
	since := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)

	if _, err := store.client(t).FilterLogs(context.Background(), LogFilter{Where: "1 = 1", Since: since}); err != nil {
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
	if strings.Contains(query, "AS timestamp") {
		t.Fatalf("the formatted timestamp must not take the column's name:\n%s", query)
	}
	if !strings.Contains(query, "AS "+timestampAlias) {
		t.Fatalf("the formatted timestamp should be selected as %q:\n%s", timestampAlias, query)
	}
	if !strings.Contains(query, "timestamp >= parseDateTime64BestEffort({since:String}, 3, 'UTC')") {
		t.Fatalf("the window should compare the column:\n%s", query)
	}
}

func TestFilterLogsRefusesAnEmptyExpression(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).FilterLogs(context.Background(), LogFilter{Where: "   "}); err == nil {
		t.Fatal("an empty expression should be refused before it reaches the store")
	}
}

func TestFilterLogsBoundsTheLimit(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).FilterLogs(context.Background(), LogFilter{Where: "1 = 1", Limit: 999999}); err != nil {
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
	client := New(Config{Host: endpoint.Hostname(), HTTPPort: endpoint.Port(), Database: "kitchen", Username: "kitchen"})

	_, err = client.FilterLogs(context.Background(), LogFilter{Where: "projct = 'shop'"})
	queryErr := &QueryError{}
	if !errors.As(err, &queryErr) {
		t.Fatalf("want a QueryError, got %v", err)
	}
	if !strings.Contains(queryErr.Message, "Syntax error") {
		t.Fatalf("the diagnostic should survive: %v", queryErr)
	}
}

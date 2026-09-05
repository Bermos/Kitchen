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
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

func TestQueryingLogsSelectsWithTheQueryLanguage(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.lines = []clickhouse.LogLine{{
		Timestamp: time.Now(),
		Source:    "runtime",
		Project:   "shop",
		Stream:    "stderr",
		Message:   "unhandled rejection",
	}}

	query := url.QueryEscape("project:shop stream:stderr")
	recorder := h.do(t, http.MethodGet, "/api/v1/logs?q="+query+"&limit=50", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "unhandled rejection") {
		t.Fatalf("want the line in the answer, got %s", recorder.Body.String())
	}
	if h.logs.lastFilter.Query != "project:shop stream:stderr" {
		t.Fatalf("the query did not reach the store: %+v", h.logs.lastFilter)
	}
	if h.logs.lastFilter.Limit != 50 {
		t.Fatalf("the limit did not reach the store: %+v", h.logs.lastFilter)
	}
}

// `where` was a ClickHouse expression evaluated as written, and the caller's
// projects were only appended to it — which bounds what the statement answers
// with and nothing about what it may read (issue #421). It is refused on every
// route that took it, including for an operator: the escape hatch is gone
// rather than reserved, because the query language is what the platform can
// compile and bind.
func TestTheRawExpressionIsRefusedOnEveryQueryRoute(t *testing.T) {
	oracle := url.QueryEscape(
		"(SELECT count() FROM otel_logs WHERE project='billing' AND position(Body,'AKIA')>0) > 0")
	for _, route := range []string{"/api/v1/logs", "/api/v1/logs/histogram",
		"/api/v1/logs/facets", "/api/v1/logs/patterns"} {
		// The harness's caller is an operator, and an operator is refused too.
		h := newHarness(t, nil)
		recorder := h.do(t, http.MethodGet, route+"?where="+oracle, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d: %s", route, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "`q`") {
			t.Fatalf("%s: the refusal should name the replacement: %s", route, recorder.Body.String())
		}
		if h.logs.lastFilter.Query != "" || h.logs.lastFilter.Limit != 0 {
			t.Fatalf("%s: the store should not have been asked: %+v", route, h.logs.lastFilter)
		}
	}
}

// The oracle the issue is about, asked the only way it can still be asked. It
// is not refused — it is a perfectly ordinary search for a word — and it
// reaches the store as a query over the caller's own scope, where the words
// `SELECT` and `otel_logs` are a message to look for.
func TestASubqueryTypedIntoTheQueryLanguageIsJustText(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, blogFixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/logs?q="+
		url.QueryEscape(`"(SELECT count() FROM otel_logs WHERE project='billing')"`), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	scope := h.logs.lastFilter.Scope
	if scope.Platform || len(scope.Projects) != 1 || scope.Projects[0] != feedProject {
		t.Fatalf("the read should be bounded by the caller's projects, got %+v", scope)
	}
}

// Asking for everything is a question, not an omission. `/logs` used to refuse
// an empty `where`, which is why the observability view opened with `1 = 1` in
// its query bar — a tautology the user had to delete to type their own query.
// The window and the limit are the bounds; the predicate never was.
func TestQueryingLogsWithoutAQuerySelectsEverything(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.lines = []clickhouse.LogLine{{Timestamp: time.Now(), Source: "runtime", Message: "hello"}}

	recorder := h.do(t, http.MethodGet, "/api/v1/logs?limit=50", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if h.logs.lastFilter.Query != "" {
		t.Fatalf("nothing asked for should reach the store as nothing: %+v", h.logs.lastFilter)
	}
}

func TestQueryingLogsWithTheQueryLanguage(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.lines = []clickhouse.LogLine{{Timestamp: time.Now(), Level: "error", Message: "boom"}}

	recorder := h.do(t, http.MethodGet, "/api/v1/logs?q="+url.QueryEscape("level:error service:shop"), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if h.logs.lastFilter.Query != "level:error service:shop" {
		t.Fatalf("the query did not reach the store: %+v", h.logs.lastFilter)
	}
}

// What the two surfaces used to be written as, in one query: the cluster's own
// pods scoped out and a substring of the message, which the language says in
// one line.
func TestOneQuerySaysWhatBothSurfacesUsedTo(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodGet, "/api/v1/logs?q="+
		url.QueryEscape(`-source:cluster timeout`), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if h.logs.lastFilter.Query != "-source:cluster timeout" {
		t.Fatalf("the query should reach the store: %+v", h.logs.lastFilter)
	}
}

// A query the parser refuses is the caller's to fix, so it answers 400 with the
// parser's own message rather than 500 with the platform's.
func TestAnUnparseableQueryIsTheCallersProblem(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.filterErr = &clickhouse.LogQueryError{Message: "a bracket is never closed"}

	recorder := h.do(t, http.MethodGet, "/api/v1/logs?q="+url.QueryEscape("(level:error"), "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "bracket") {
		t.Fatalf("the parser's diagnostic should reach the caller: %s", recorder.Body.String())
	}
}

func TestTheAnalyticsShareTheSelection(t *testing.T) {
	const query = "level:error"
	h := newHarness(t, nil)
	selection := "?q=" + url.QueryEscape(query) + "&since=2026-08-13T09:00:00Z"

	h.logs.histogram = clickhouse.LogHistogram{BucketSeconds: 60, Total: 3}
	recorder := h.do(t, http.MethodGet, "/api/v1/logs/histogram"+selection+"&buckets=30", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if h.logs.lastHistogram.Query != query || h.logs.lastHistogram.Buckets != 30 {
		t.Fatalf("the histogram did not get the selection: %+v", h.logs.lastHistogram)
	}
	if h.logs.lastHistogram.Since.IsZero() {
		t.Fatalf("the histogram should be bounded by the window: %+v", h.logs.lastHistogram)
	}

	h.logs.facets = []clickhouse.LogFacet{{Field: "level", Values: []clickhouse.LogFacetValue{{Value: "error", Count: 3}}}}
	recorder = h.do(t, http.MethodGet, "/api/v1/logs/facets"+selection+"&fields=level,http.status", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if h.logs.lastFacets.Query != query {
		t.Fatalf("the facets did not get the selection: %+v", h.logs.lastFacets)
	}
	if len(h.logs.lastFacets.Fields) != 2 || h.logs.lastFacets.Fields[1] != "http.status" {
		t.Fatalf("the facet fields did not survive: %+v", h.logs.lastFacets.Fields)
	}

	h.logs.patterns = []clickhouse.LogPattern{{Pattern: "GET /works?page=<n>", Count: 14021}}
	recorder = h.do(t, http.MethodGet, "/api/v1/logs/patterns"+selection, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if h.logs.lastPatterns.Query != query {
		t.Fatalf("the patterns did not get the selection: %+v", h.logs.lastPatterns)
	}
	if !strings.Contains(recorder.Body.String(), "14021") {
		t.Fatalf("the pattern's count should reach the caller: %s", recorder.Body.String())
	}
}

// ClickHouse can still refuse what the compiler produced — a regular
// expression it will not compile is the one that gets that far — and that
// diagnostic is the caller's to read.
func TestABadExpressionIsTheCallersProblem(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.filterErr = &clickhouse.QueryError{
		Status:  "400 Bad Request",
		Message: "Code: 62. DB::Exception: Syntax error near 'projct'",
	}

	recorder := h.do(t, http.MethodGet, "/api/v1/logs?q="+url.QueryEscape("message:/(/"), "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Syntax error") {
		t.Fatalf("the ClickHouse diagnostic should reach the caller: %s", recorder.Body.String())
	}
}

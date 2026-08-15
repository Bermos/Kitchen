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

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

func TestQueryingLogsWithAClickHouseExpression(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.lines = []clickhouse.LogLine{{
		Timestamp: time.Now(),
		Source:    "runtime",
		Project:   "shop",
		Stream:    "stderr",
		Message:   "unhandled rejection",
	}}

	where := url.QueryEscape("project = 'shop' AND stream = 'stderr'")
	recorder := h.do(t, http.MethodGet, "/api/v1/logs?where="+where+"&limit=50", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "unhandled rejection") {
		t.Fatalf("want the line in the answer, got %s", recorder.Body.String())
	}
	if h.logs.lastFilter.Where != "project = 'shop' AND stream = 'stderr'" {
		t.Fatalf("the expression did not reach the store as written: %+v", h.logs.lastFilter)
	}
	if h.logs.lastFilter.Limit != 50 {
		t.Fatalf("the limit did not reach the store: %+v", h.logs.lastFilter)
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
	if h.logs.lastFilter.Where != "" || h.logs.lastFilter.Query != "" {
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

// The two surfaces compose rather than exclude each other: the view scopes the
// cluster's own pods out with `q` while the operator writes ClickHouse in
// `where`.
func TestTheQueryAndTheEscapeHatchCompose(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodGet, "/api/v1/logs?q="+url.QueryEscape("-source:cluster")+
		"&where="+url.QueryEscape("message ILIKE '%timeout%'"), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if h.logs.lastFilter.Query != "-source:cluster" || h.logs.lastFilter.Where != "message ILIKE '%timeout%'" {
		t.Fatalf("both surfaces should reach the store: %+v", h.logs.lastFilter)
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

func TestABadExpressionIsTheCallersProblem(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.filterErr = &clickhouse.QueryError{
		Status:  "400 Bad Request",
		Message: "Code: 62. DB::Exception: Syntax error near 'projct'",
	}

	recorder := h.do(t, http.MethodGet, "/api/v1/logs?where="+url.QueryEscape("projct == 'shop'"), "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Syntax error") {
		t.Fatalf("the ClickHouse diagnostic should reach the caller: %s", recorder.Body.String())
	}
}

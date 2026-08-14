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

func TestQueryingLogsNeedsAnExpression(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodGet, "/api/v1/logs", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "where is required") {
		t.Fatalf("the error should say what is missing: %s", recorder.Body.String())
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

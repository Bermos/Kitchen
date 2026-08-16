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
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// testTrace is the id every trace fixture here carries.
const testTrace = "9d8d0f"

func TestEnvironmentMetricsAsksForThatEnvironment(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.series = clickhouse.ResourceSeries{
		BucketSeconds: 60,
		Points: []clickhouse.ResourcePoint{
			{Start: time.Now().Add(-time.Minute), CPUCores: 0.25, Replicas: 2},
		},
		CPULimitCores: 0.5,
	}

	res := h.do(t, http.MethodGet, "/api/v1/environments/"+testEnvironment+"/metrics?points=120", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET metrics = %d: %s", res.Code, res.Body.String())
	}
	body := decode[clickhouse.ResourceSeries](t, res)
	if len(body.Points) != 1 || body.Points[0].Replicas != 2 {
		t.Errorf("unexpected series %+v", body)
	}
	// The endpoint's whole job is scoping the read to one workload — the store
	// refuses an unscoped one, and the environment knows its own project.
	if h.logs.lastSeries.Environment != testEnvironment || h.logs.lastSeries.Project != feedProject {
		t.Errorf("the query did not name the workload: %+v", h.logs.lastSeries)
	}
	if h.logs.lastSeries.Buckets != 120 {
		t.Errorf("the requested resolution did not carry: %+v", h.logs.lastSeries)
	}
	// A window is what this endpoint is for, so it has one whether or not the
	// caller supplied it.
	if h.logs.lastSeries.Since.IsZero() {
		t.Error("an unbounded history would read the whole retention")
	}
}

// A typo in the name says "no such environment" rather than answering an empty
// chart for something that does not exist.
func TestEnvironmentMetricsNeedsTheEnvironment(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodGet, "/api/v1/environments/nope/metrics", "")
	if res.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", res.Code, res.Body.String())
	}
}

// The query is the operator's own, so a store that refuses it is reporting a
// fault in Kitchen: the caller gets the name of the read and ClickHouse's text
// stays in the operator's log.
func TestEnvironmentMetricsReportsAStoreFaultAsOne(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.seriesErr = errors.New("Code: 47. DB::Exception: Unknown expression identifier")

	res := h.do(t, http.MethodGet, "/api/v1/environments/"+testEnvironment+"/metrics", "")
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", res.Code, res.Body.String())
	}
	if body := res.Body.String(); !strings.Contains(body, "resource history query") || strings.Contains(body, "DB::Exception") {
		t.Errorf("the store's diagnostic should not reach the caller: %s", body)
	}
}

func TestListTracesCarriesItsFilters(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.traces = []clickhouse.Trace{{
		TraceID:    testTrace,
		Name:       "GET /checkout",
		Service:    feedProject,
		DurationMs: 420,
		Spans:      7,
		Errors:     1,
	}}

	res := h.do(t, http.MethodGet,
		"/api/v1/traces?project=shop&service=shop&errors=1&minDuration=250&limit=25", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /traces = %d: %s", res.Code, res.Body.String())
	}
	body := decode[struct {
		Items []clickhouse.Trace `json:"items"`
	}](t, res)
	if len(body.Items) != 1 || body.Items[0].TraceID != testTrace {
		t.Errorf("unexpected traces %+v", body.Items)
	}

	query := h.logs.lastTraces
	if query.Project != feedProject || query.Service != feedProject {
		t.Errorf("the scope did not carry: %+v", query)
	}
	if !query.OnlyErrors || query.MinDurationMs != 250 || query.Limit != 25 {
		t.Errorf("the filters did not carry: %+v", query)
	}
}

func TestListTracesNeedsTheProjectToExist(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodGet, "/api/v1/traces?project=nope", "")
	if res.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", res.Code, res.Body.String())
	}
}

func TestAMalformedDurationBoundIsTheCallersError(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodGet, "/api/v1/traces?minDuration=soon", "")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", res.Code, res.Body.String())
	}
}

func TestGetTraceReadsItById(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.spans = []clickhouse.Span{
		{TraceID: testTrace, SpanID: "s1", Name: "GET /checkout", DurationMs: 420},
		{TraceID: testTrace, SpanID: "s2", ParentSpanID: "s1", Name: "SELECT orders", DurationMs: 390},
	}

	res := h.do(t, http.MethodGet, "/api/v1/traces/"+testTrace, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /traces/9d8d0f = %d: %s", res.Code, res.Body.String())
	}
	body := decode[traceView](t, res)
	if body.TraceID != testTrace || len(body.Spans) != 2 {
		t.Errorf("unexpected trace %+v", body)
	}
	if h.logs.lastTraceID != testTrace {
		t.Errorf("the id did not carry: %q", h.logs.lastTraceID)
	}
}

// A trace nothing was kept for is a 404: the id was a name, and the platform
// does not have it. Retention is the usual reason, so the answer says so.
func TestATraceWithNoSpansIsNotFound(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodGet, "/api/v1/traces/"+testTrace, "")
	if res.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "retention") {
		t.Errorf("the answer should say why it is missing: %s", res.Body.String())
	}
}

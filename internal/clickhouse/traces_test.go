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
	"strings"
	"testing"
	"time"
)

func TestTracesReadsARowIntoAnEntry(t *testing.T) {
	store := newFakeLogStore(t)
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	store.rows = `{"traceId":"9d8d0f","traceStart":"` + micros(start) + `","traceDurationMs":420.5,` +
		`"rootName":"GET /checkout","rootService":"shop","traceProject":"shop","traceEnvironment":"production",` +
		`"spans":"7","errors":"1","services":"3","httpStatus":"500"}`

	traces, err := store.client(t).Traces(context.Background(), TraceQuery{})
	if err != nil {
		t.Fatalf("Traces: %v", err)
	}
	if len(traces) != 1 {
		t.Fatalf("want one trace, got %d", len(traces))
	}
	trace := traces[0]
	if !trace.Timestamp.Equal(start) {
		t.Fatalf("want %s, got %s", start, trace.Timestamp)
	}
	if trace.Name != "GET /checkout" || trace.Service != "shop" {
		t.Fatalf("the root span should name the trace: %+v", trace)
	}
	if trace.DurationMs != 420.5 || trace.Spans != 7 || trace.Errors != 1 || trace.Services != 3 {
		t.Fatalf("the aggregates did not land: %+v", trace)
	}
	if trace.HTTPStatus != 500 {
		t.Fatalf("want the failing status, got %d", trace.HTTPStatus)
	}
}

// A trace is slow or failed as a whole — one slow span makes a slow trace — so
// both filters are over the group, not over the rows the group is built from.
func TestTraceFiltersAreOverTheTraceNotItsSpans(t *testing.T) {
	store := newFakeLogStore(t)

	_, err := store.client(t).Traces(context.Background(), TraceQuery{
		OnlyErrors:    true,
		MinDurationMs: 250,
	})
	if err != nil {
		t.Fatalf("Traces: %v", err)
	}
	if !strings.Contains(store.query, "HAVING") {
		t.Fatalf("the filters belong in a HAVING:\n%s", store.query)
	}
	if strings.Contains(store.query, "WHERE statusCode") {
		t.Fatalf("filtering the spans would drop the healthy half of a failed trace:\n%s", store.query)
	}
	if got := store.params.Get("param_minDuration"); got != "250" {
		t.Fatalf("the duration bound should have travelled as a parameter, got %q", got)
	}
}

// The alias the HAVING and the ORDER BY read has to be one no column answers
// to: ClickHouse resolves a name against the SELECT aliases first, so a
// `durationMs` alias would quietly become "some span took this long".
func TestTheTraceDurationDoesNotShadowTheSpanColumn(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).Traces(context.Background(), TraceQuery{MinDurationMs: 1}); err != nil {
		t.Fatalf("Traces: %v", err)
	}
	if strings.Contains(store.query, "AS durationMs") {
		t.Fatalf("the trace duration must not be aliased over the span column:\n%s", store.query)
	}
	if !strings.Contains(store.query, "AS traceDurationMs") {
		t.Fatalf("the trace duration should carry its own name:\n%s", store.query)
	}
}

func TestTracesScopeTravelsAsParameters(t *testing.T) {
	store := newFakeLogStore(t)

	_, err := store.client(t).Traces(context.Background(), TraceQuery{
		Service:     "shop'; DROP TABLE traces --",
		Project:     "shop",
		Environment: "production",
	})
	if err != nil {
		t.Fatalf("Traces: %v", err)
	}
	if strings.Contains(store.query, "DROP TABLE") {
		t.Fatalf("a service name reached the statement text:\n%s", store.query)
	}
	for name, want := range map[string]string{
		"param_service":     "shop'; DROP TABLE traces --",
		"param_project":     "shop",
		"param_environment": "production",
	} {
		if got := store.params.Get(name); got != want {
			t.Fatalf("%s should be %q, got %q", name, want, got)
		}
	}
}

func TestATraceWindowMustEndAfterItStarts(t *testing.T) {
	store := newFakeLogStore(t)
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)

	_, err := store.client(t).Traces(context.Background(), TraceQuery{
		Since: start,
		Until: start.Add(-time.Minute),
	})
	if err == nil {
		t.Fatal("a backwards window should have been refused")
	}
}

// One trace is read by id and nothing else. A window would break the one link
// that makes traces worth collecting — a log line offering its trace.
func TestOneTraceIsReadByIdAlone(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = strings.Join([]string{
		`{"ts":"2026-08-16T10:00:00.000000Z","traceId":"9d8d0f","spanId":"s1","parentSpanId":"",` +
			`"name":"GET /checkout","kind":"SERVER","service":"shop","project":"shop","environment":"production",` +
			`"durationMs":420.5,"statusCode":"ERROR","statusMessage":"boom","httpStatus":"500",` +
			`"attributes":{"http.route":"/checkout"},"resource":{"service.name":"shop"}}`,
		`{"ts":"2026-08-16T10:00:00.010000Z","traceId":"9d8d0f","spanId":"s2","parentSpanId":"s1",` +
			`"name":"SELECT orders","kind":"CLIENT","service":"shop-db","project":"shop","environment":"production",` +
			`"durationMs":390,"statusCode":"UNSET","statusMessage":"","httpStatus":"0",` +
			`"attributes":{},"resource":{}}`,
	}, "\n")

	spans, err := store.client(t).Trace(context.Background(), "9d8d0f")
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("want two spans, got %d", len(spans))
	}
	if spans[0].SpanID != "s1" || spans[1].ParentSpanID != "s1" {
		t.Fatalf("the waterfall lost its shape: %+v", spans)
	}
	// Microseconds, because two nested spans can start in the same
	// millisecond and a waterfall that shows them level is wrong.
	if !spans[1].Timestamp.Equal(time.Date(2026, 8, 16, 10, 0, 0, 10_000_000, time.UTC)) {
		t.Fatalf("unexpected timestamp: %s", spans[1].Timestamp)
	}
	if spans[0].HTTPStatus != 500 || spans[0].StatusCode != StatusError {
		t.Fatalf("the failed span lost its status: %+v", spans[0])
	}
	// An empty map is nothing to show, and marshals to `null` rather than to
	// a row of empty chips.
	if spans[1].Attributes != nil || spans[1].Resource != nil {
		t.Fatalf("empty maps should be absent, got %+v", spans[1])
	}
	if got := store.params.Get("param_traceId"); got != "9d8d0f" {
		t.Fatalf("the id should have travelled as a parameter, got %q", got)
	}
	if strings.Contains(store.query, "timestamp >=") {
		t.Fatalf("reading one trace should not need a window:\n%s", store.query)
	}
}

func TestATraceNeedsAnId(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).Trace(context.Background(), "  "); err == nil {
		t.Fatal("an empty trace id should have been refused")
	}
}

func TestInsertingSpansWritesMicroseconds(t *testing.T) {
	store := newFakeLogStore(t)

	err := store.client(t).InsertSpans(context.Background(), []Span{{
		Timestamp:  time.Date(2026, 8, 16, 10, 0, 0, 123456000, time.UTC),
		TraceID:    "9d8d0f",
		SpanID:     "s1",
		Name:       "GET /checkout",
		Service:    "shop",
		DurationMs: 420.5,
		StatusCode: StatusError,
		HTTPStatus: 500,
	}})
	if err != nil {
		t.Fatalf("InsertSpans: %v", err)
	}
	for _, fragment := range []string{
		"INSERT INTO `kitchen`.`traces` FORMAT JSONEachRow",
		`"timestamp":"2026-08-16 10:00:00.123456"`,
		`"httpStatus":500`,
	} {
		if !strings.Contains(store.query, fragment) {
			t.Fatalf("the insert should have carried %s:\n%s", fragment, store.query)
		}
	}
}

func micros(at time.Time) string {
	return stamp(at) + "000000"
}

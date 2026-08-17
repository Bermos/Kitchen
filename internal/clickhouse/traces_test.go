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
	if strings.Contains(store.query, "WHERE "+spanStatusCode) {
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
	// `service` is the emitting process's name, which is the exporter's
	// column; the other two are Kitchen's own and are named the same on both
	// sides.
	if !strings.Contains(store.query, "ServiceName = {service:String}") {
		t.Fatalf("the service filter should read the exporter's column:\n%s", store.query)
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

// One trace is read by an id and nothing else — a window the caller had to
// know would break the one link that makes traces worth collecting, a log line
// offering its trace. The bound the query does carry is found, not demanded:
// see TestATraceIsBoundedByTheLookupTable.
func TestOneTraceIsReadByIdAlone(t *testing.T) {
	store := newFakeLogStore(t)
	// The lookup is asked first and has nothing to say, so the span read is
	// unbounded and gets the rows.
	store.answer = spanAnswer(strings.Join([]string{
		`{"ts":"2026-08-16T10:00:00.000000Z","traceId":"9d8d0f","spanId":"s1","parentSpanId":"",` +
			`"name":"GET /checkout","kind":"SERVER","service":"shop","project":"shop","environment":"production",` +
			`"durationMs":420.5,"statusCode":"ERROR","statusMessage":"boom","httpStatus":"500",` +
			`"attributes":{"http.route":"/checkout"},"resource":{"service.name":"shop"}}`,
		`{"ts":"2026-08-16T10:00:00.010000Z","traceId":"9d8d0f","spanId":"s2","parentSpanId":"s1",` +
			`"name":"SELECT orders","kind":"CLIENT","service":"shop-db","project":"shop","environment":"production",` +
			`"durationMs":390,"statusCode":"UNSET","statusMessage":"","httpStatus":"0",` +
			`"attributes":{},"resource":{}}`,
	}, "\n"))

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
}

func TestATraceNeedsAnId(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).Trace(context.Background(), "  "); err == nil {
		t.Fatal("an empty trace id should have been refused")
	}
}

func micros(at time.Time) string {
	return stamp(at) + "000000"
}

// The lookup table earns its place here. The span table is ordered by
// (project, environment, Timestamp), so an id lookup with no time bound reads
// the retention; the lookup answers "when did this trace happen" in one point
// read and the span query is bounded by it.
func TestATraceIsBoundedByTheLookupTable(t *testing.T) {
	store := newFakeLogStore(t)
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	store.answer = lookupAnswer(
		`{"first":"` + stamp(start) + `","last":"` + stamp(start.Add(time.Second)) + `","rows":"1"}`)

	if _, err := store.client(t).Trace(context.Background(), "9d8d0f"); err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if !store.sawQuery("FROM " + qualified(TracesIDLookupTable)) {
		t.Fatalf("the window should have been read out of the lookup table:\n%s", store.transcript())
	}
	if !store.sawQuery(spanSinceCondition) || !store.sawQuery(spanUntilCondition) {
		t.Fatalf("the span read should be bounded by what the lookup answered:\n%s", store.transcript())
	}
	// The lookup keeps whole seconds where the spans keep nanoseconds, so the
	// bounds are widened rather than compared as they are — a span 400ms into
	// the trace's last second sits after the End the view recorded.
	if got := store.params.Get("param_until"); !strings.HasPrefix(got, "2026-08-16T10:00:02") {
		t.Fatalf("the upper bound should be widened past the recorded end, got %q", got)
	}
	if got := store.params.Get("param_since"); !strings.HasPrefix(got, "2026-08-16T09:59:59") {
		t.Fatalf("the lower bound should be widened before the recorded start, got %q", got)
	}
}

// A trace the lookup has never heard of is still read, unbounded. That is the
// gap between a span being written and the materialized view's part becoming
// visible, and answering "not found" there would make a fresh trace unopenable
// for exactly the seconds that matter most.
func TestATraceTheLookupHasNotSeenYetIsStillRead(t *testing.T) {
	store := newFakeLogStore(t)
	store.answer = lookupAnswer(`{"first":"0","last":"0","rows":"0"}`)

	if _, err := store.client(t).Trace(context.Background(), "9d8d0f"); err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if store.sawQuery(spanSinceCondition) {
		t.Fatalf("with no window to be had the read should not invent one:\n%s", store.transcript())
	}
	if !store.sawQuery("TraceId = {traceId:String}") {
		t.Fatalf("the id is still the filter:\n%s", store.transcript())
	}
}

// The exporter's units are not the API's: nanoseconds become milliseconds and
// `Error` becomes ERROR. Both conversions happen in the projection, so every
// caller sees one spelling.
func TestSpansAreConvertedOutOfTheExportersUnits(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).Trace(context.Background(), "9d8d0f"); err != nil {
		t.Fatalf("Trace: %v", err)
	}
	for _, want := range []string{
		spanDurationMs + " AS durationMs",
		spanStatusCode + " AS statusCode",
		"SpanName AS name",
		"SpanKind AS kind",
		"ServiceName AS service",
	} {
		if !store.sawQuery(want) {
			t.Errorf("the projection is missing %q:\n%s", want, store.transcript())
		}
	}
}

// The HTTP status is an attribute rather than a column, and semconv renamed it
// — `http.status_code` was stable for years before `http.response.status_code`
// replaced it. Both are still emitted in the wild, so both are read.
func TestTheHTTPStatusReadsBothSemconvSpellings(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).Traces(context.Background(), TraceQuery{}); err != nil {
		t.Fatalf("Traces: %v", err)
	}
	for _, attribute := range []string{"http.response.status_code", "http.status_code"} {
		if !strings.Contains(store.query, "SpanAttributes['"+attribute+"']") {
			t.Errorf("the status should be read from %q:\n%s", attribute, store.query)
		}
	}
}

// A trace's duration is the envelope of its spans, computed in the nanoseconds
// the columns already hold and divided once — a span's Timestamp is its start,
// so its end is that plus Duration.
func TestTheTraceDurationIsTheEnvelopeInNanoseconds(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).Traces(context.Background(), TraceQuery{}); err != nil {
		t.Fatalf("Traces: %v", err)
	}
	want := "(max(toUnixTimestamp64Nano(Timestamp) + toInt64(Duration)) - min(toUnixTimestamp64Nano(Timestamp))) / 1e6"
	if !strings.Contains(store.query, want) {
		t.Fatalf("want the envelope %q:\n%s", want, store.query)
	}
}

// A Trace read is two round trips — the lookup for the window, then the spans —
// and the two answer in different shapes. These serve one and leave the other
// empty, so neither is handed the other's rows to parse.

// lookupAnswer serves the trace-id lookup its row and the span query nothing.
func lookupAnswer(row string) func(string) string {
	return func(query string) string {
		if strings.Contains(query, TracesIDLookupTable) {
			return row
		}
		return ""
	}
}

// spanAnswer serves the span query its rows and the lookup nothing, which is a
// trace the lookup has not caught up with.
func spanAnswer(rows string) func(string) string {
	return func(query string) string {
		if strings.Contains(query, TracesIDLookupTable) {
			return ""
		}
		return rows
	}
}

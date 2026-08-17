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
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spans, as instrumented applications send them to the collector over OTLP.
//
// Nothing here is derived from the flow data. Hubble sees that one workload
// called another and how long the call took; only the application knows that
// the call was "checkout" and that it spent its time in a database. Promoting
// L7 flows into trace-shaped rows would produce something that looked like
// tracing and answered none of the questions tracing exists for.
//
// The table is the exporter's, so three of its conventions have to be undone on
// the way out: `Duration` is nanoseconds where Kitchen's API is milliseconds,
// `StatusCode` is `Unset`/`Ok`/`Error` where Kitchen's is upper case, and the
// HTTP status is an attribute rather than a column.

// DefaultTraceLimit is how many traces a list request returns when it does not
// ask for a number; MaxTraceLimit is the ceiling. MaxTraceSpans bounds one
// trace: a runaway instrumentation should make a trace look truncated, not
// make the operator hold a million spans.
const (
	DefaultTraceLimit = 50
	MaxTraceLimit     = 500
	MaxTraceSpans     = 2000
)

// Status codes, as Kitchen's API spells them.
//
// The exporter writes `Unset`/`Ok`/`Error`, and an SDK that sets the status
// itself may write either casing. Every comparison and every projection folds
// through `upper()` for that reason, which is also what keeps these three
// constants the only spelling this package knows.
const (
	StatusUnset = "UNSET"
	StatusOK    = "OK"
	StatusError = "ERROR"
)

// spanStatusCode is StatusCode normalised to the spelling above.
const spanStatusCode = "upper(StatusCode)"

// spanDurationMs converts the span's nanoseconds into the milliseconds the API
// answers with.
const spanDurationMs = "Duration / 1e6"

// spanHTTPStatus lifts the response status out of the span's attributes.
//
// It is two names because semconv renamed it: `http.status_code` was stable for
// years and `http.response.status_code` replaced it in 1.21. Both are still in
// the wild — the name depends on the age of the SDK, not on anything Kitchen
// controls — so the new one wins and the old one is the fallback. A span that
// carries neither reads as 0, which is what `omitempty` drops.
const spanHTTPStatus = `toUInt16OrZero(if(SpanAttributes['http.response.status_code'] != '', ` +
	`SpanAttributes['http.response.status_code'], SpanAttributes['http.status_code']))`

// The span window's two bounds, matching the column's own scale. See
// logSinceCondition.
const (
	spanSinceCondition = "Timestamp >= parseDateTime64BestEffort({since:String}, 9, 'UTC')"
	spanUntilCondition = "Timestamp <= parseDateTime64BestEffort({until:String}, 9, 'UTC')"
)

// Span is one operation inside one trace.
type Span struct {
	Timestamp    time.Time `json:"timestamp"`
	TraceID      string    `json:"traceId"`
	SpanID       string    `json:"spanId"`
	ParentSpanID string    `json:"parentSpanId,omitempty"`
	Name         string    `json:"name"`
	// Kind is OTLP's, as the exporter spells it: Internal, Server, Client,
	// Producer, Consumer.
	Kind string `json:"kind,omitempty"`
	// Service is the emitting process's `service.name`; Project and
	// Environment are Kitchen's own, which the platform puts into every
	// application's resource attributes so a trace knows where it ran without
	// the application being told.
	Service     string `json:"service"`
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`

	DurationMs    float64 `json:"durationMs"`
	StatusCode    string  `json:"statusCode,omitempty"`
	StatusMessage string  `json:"statusMessage,omitempty"`
	// HTTPStatus is lifted out of the attributes, because "show me the 500s"
	// should read one expression rather than two map lookups spelled out at
	// every call site.
	HTTPStatus uint16 `json:"httpStatus,omitempty"`

	Attributes map[string]string `json:"attributes,omitempty"`
	Resource   map[string]string `json:"resource,omitempty"`
}

// TraceQuery selects traces. The zero value answers the last hour, whatever
// the platform traced.
type TraceQuery struct {
	// Service, Project and Environment narrow to one emitter.
	Service     string
	Project     string
	Environment string
	// Since and Until bound the window. A zero Until means now; a zero Since
	// means an hour before Until.
	Since time.Time
	Until time.Time
	// OnlyErrors keeps the traces that hold at least one failed span, which
	// is the reason most people open a trace list.
	OnlyErrors bool
	// MinDurationMs keeps the traces at least this slow, which is the other
	// reason.
	MinDurationMs float64
	Limit         int
}

// Trace is one trace as a list entry: what it was, how long it took, and
// whether anything in it failed.
type Trace struct {
	TraceID   string    `json:"traceId"`
	Timestamp time.Time `json:"timestamp"`
	// Name and Service come from the root span — the one nothing else
	// started. A trace whose root is outside the window (or was dropped)
	// falls back to its earliest span, so the entry is still readable.
	Name        string  `json:"name"`
	Service     string  `json:"service"`
	Project     string  `json:"project,omitempty"`
	Environment string  `json:"environment,omitempty"`
	DurationMs  float64 `json:"durationMs"`
	Spans       uint64  `json:"spans"`
	Errors      uint64  `json:"errors"`
	// Services is how many distinct services the trace touched: the number
	// that says whether this was one process or a conversation.
	Services   uint64 `json:"services"`
	HTTPStatus uint16 `json:"httpStatus,omitempty"`
}

// Traces lists the traces in a window, newest first.
//
// The list is one GROUP BY over the window: a trace is its spans, and what a
// list entry says about it — how long it took end to end, how many services it
// touched, whether anything failed — is an aggregate over them. The root span
// is preferred for the name and service, with the earliest span as the
// fallback, because a trace whose root arrived late (or not at all) should
// still be readable rather than nameless.
func (c *Client) Traces(ctx context.Context, query TraceQuery) ([]Trace, error) {
	until := query.Until
	if until.IsZero() {
		until = time.Now()
	}
	since := query.Since
	if since.IsZero() {
		since = until.Add(-time.Hour)
	}
	if !until.After(since) {
		return nil, fmt.Errorf("the trace window must end after it starts")
	}
	limit := query.Limit
	if limit < 1 {
		limit = DefaultTraceLimit
	}
	if limit > MaxTraceLimit {
		limit = MaxTraceLimit
	}

	conditions := []string{spanSinceCondition, spanUntilCondition}
	params := map[string]string{
		"since": since.UTC().Format(time.RFC3339Nano),
		"until": until.UTC().Format(time.RFC3339Nano),
		"limit": strconv.Itoa(limit),
	}
	// `service` is the emitting process's name and lives in a column of the
	// exporter's; the other two are Kitchen's materialized columns and are
	// named the same on both sides.
	for _, filter := range []struct{ column, name, value string }{
		{"ServiceName", "service", query.Service},
		{"project", "project", query.Project},
		{"environment", "environment", query.Environment},
	} {
		if filter.value == "" {
			continue
		}
		conditions = append(conditions, fmt.Sprintf("%s = {%s:String}", filter.column, filter.name))
		params[filter.name] = filter.value
	}

	// The window bounds the spans; these bound the traces, so they are a
	// HAVING over the group rather than a WHERE over the rows — one slow span
	// makes a slow trace, and one failed span makes a failed trace.
	having := []string{}
	if query.OnlyErrors {
		having = append(having, fmt.Sprintf("countIf(%s = %s) > 0", spanStatusCode, quoteLiteral(StatusError)))
	}
	if query.MinDurationMs > 0 {
		having = append(having, "traceDurationMs >= {minDuration:Float64}")
		params["minDuration"] = strconv.FormatFloat(query.MinDurationMs, 'f', -1, 64)
	}
	havingClause := ""
	if len(having) > 0 {
		havingClause = "\nHAVING " + strings.Join(having, " AND ")
	}

	// `traceDurationMs` and `traceStart` are deliberately not named after any
	// column: ClickHouse resolves a name in HAVING and ORDER BY against the
	// SELECT aliases first, so an alias called `durationMs` would silently
	// turn "the trace took 500ms" into "some span in it did".
	//
	// The envelope is computed in the nanoseconds the columns are already in
	// and divided once at the end. A span's Timestamp is its start, so its end
	// is that plus Duration, and the trace's span is the last end minus the
	// first start — not the sum of its spans, which double-counts every nested
	// call, and not the longest span, which misses a sequence of short ones.
	statement := fmt.Sprintf(`SELECT
    TraceId AS traceId,
    toString(toUnixTimestamp64Micro(min(Timestamp))) AS traceStart,
    (max(toUnixTimestamp64Nano(Timestamp) + toInt64(Duration)) - min(toUnixTimestamp64Nano(Timestamp))) / 1e6 AS traceDurationMs,
    if(countIf(ParentSpanId = '') > 0, anyIf(SpanName, ParentSpanId = ''), argMin(SpanName, Timestamp)) AS rootName,
    if(countIf(ParentSpanId = '') > 0, anyIf(ServiceName, ParentSpanId = ''), argMin(ServiceName, Timestamp)) AS rootService,
    argMin(project, Timestamp) AS traceProject,
    argMin(environment, Timestamp) AS traceEnvironment,
    toString(count()) AS spans,
    toString(countIf(%s = %s)) AS errors,
    toString(uniqExact(ServiceName)) AS services,
    toString(max(%s)) AS httpStatus
FROM %s.%s
WHERE %s
GROUP BY traceId%s
ORDER BY traceStart DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		spanStatusCode, quoteLiteral(StatusError), spanHTTPStatus,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(TracesTable),
		strings.Join(conditions, " AND "), havingClause)

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}

	traces := []Trace{}
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := traceRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable trace row: %w", err)
		}
		micros, _ := strconv.ParseInt(row.Start, 10, 64)
		status, _ := strconv.ParseUint(row.HTTPStatus, 10, 16)
		traces = append(traces, Trace{
			TraceID:     row.TraceID,
			Timestamp:   time.UnixMicro(micros).UTC(),
			Name:        row.Name,
			Service:     row.Service,
			Project:     row.Project,
			Environment: row.Environment,
			DurationMs:  row.DurationMs,
			Spans:       parseUint(row.Spans),
			Errors:      parseUint(row.Errors),
			Services:    parseUint(row.Services),
			HTTPStatus:  uint16(status),
		})
	}
	return traces, nil
}

// traceRow is the wire shape of one list entry. The duration is the one value
// ClickHouse renders as a JSON number rather than a string, because it is
// arithmetic over two aggregates and the HAVING that filters on it needs it to
// still be a number.
type traceRow struct {
	TraceID     string  `json:"traceId"`
	Start       string  `json:"traceStart"`
	DurationMs  float64 `json:"traceDurationMs"`
	Name        string  `json:"rootName"`
	Service     string  `json:"rootService"`
	Project     string  `json:"traceProject"`
	Environment string  `json:"traceEnvironment"`
	Spans       string  `json:"spans"`
	Errors      string  `json:"errors"`
	Services    string  `json:"services"`
	HTTPStatus  string  `json:"httpStatus"`
}

// spanRow is the wire shape of a span on the way back out. Everything numeric
// is read as a string for the same reason the analytics are: one decoding path
// and no surprises about how ClickHouse renders an integer into JSON.
type spanRow struct {
	Timestamp     string            `json:"ts"`
	TraceID       string            `json:"traceId"`
	SpanID        string            `json:"spanId"`
	ParentSpanID  string            `json:"parentSpanId"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	Service       string            `json:"service"`
	Project       string            `json:"project"`
	Environment   string            `json:"environment"`
	DurationMs    float64           `json:"durationMs"`
	StatusCode    string            `json:"statusCode"`
	StatusMessage string            `json:"statusMessage"`
	HTTPStatus    string            `json:"httpStatus"`
	Attributes    map[string]string `json:"attributes"`
	Resource      map[string]string `json:"resource"`
}

// Trace reads one trace's spans, oldest first — which is the order a waterfall
// is drawn in.
//
// The caller supplies nothing but an id, on purpose: a trace id arrives from a
// log line or from the list, and asking the caller to also know when it
// happened would make the one link that matters — line to trace — impossible to
// follow. The window is found rather than demanded, out of the trace-id lookup
// table, which is one point read on a table ordered by exactly that. Without it
// the span table's ordering key (project, environment, Timestamp) offers a
// lookup by id nothing at all, and the bloom filter would be asked to skip
// granules across the whole retention.
//
// A trace the lookup has never heard of is still read, unbounded. That is the
// window between a span being written and the materialized view's part being
// visible, and answering "not found" there would make a fresh trace unopenable
// for the few seconds that matter most.
func (c *Client) Trace(ctx context.Context, traceID string) ([]Span, error) {
	traceID = strings.TrimSpace(traceID)
	if traceID == "" {
		return nil, fmt.Errorf("a trace id is required")
	}

	params := map[string]string{
		"traceId": traceID,
		"limit":   strconv.Itoa(MaxTraceSpans),
	}
	conditions := []string{"TraceId = {traceId:String}"}
	since, until, err := c.traceWindow(ctx, traceID)
	if err != nil {
		return nil, err
	}
	if !since.IsZero() {
		conditions = append(conditions, spanSinceCondition, spanUntilCondition)
		params["since"] = since.Format(time.RFC3339Nano)
		params["until"] = until.Format(time.RFC3339Nano)
	}

	statement := fmt.Sprintf(`SELECT
    %s AS ts,
    TraceId AS traceId, SpanId AS spanId, ParentSpanId AS parentSpanId,
    SpanName AS name, SpanKind AS kind, ServiceName AS service, project, environment,
    %s AS durationMs, %s AS statusCode, StatusMessage AS statusMessage,
    toString(%s) AS httpStatus,
    SpanAttributes AS attributes, ResourceAttributes AS resource
FROM %s.%s
WHERE %s
ORDER BY Timestamp ASC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		logTimestampFormat, spanDurationMs, spanStatusCode, spanHTTPStatus,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(TracesTable),
		strings.Join(conditions, " AND "))

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}

	spans := []Span{}
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := spanRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable span row: %w", err)
		}
		timestamp, err := time.Parse(otelTimestampLayout, row.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("unreadable span timestamp %q: %w", row.Timestamp, err)
		}
		status, _ := strconv.ParseUint(row.HTTPStatus, 10, 16)
		if len(row.Attributes) == 0 {
			row.Attributes = nil
		}
		if len(row.Resource) == 0 {
			row.Resource = nil
		}
		spans = append(spans, Span{
			Timestamp:     timestamp,
			TraceID:       row.TraceID,
			SpanID:        row.SpanID,
			ParentSpanID:  row.ParentSpanID,
			Name:          row.Name,
			Kind:          row.Kind,
			Service:       row.Service,
			Project:       row.Project,
			Environment:   row.Environment,
			DurationMs:    row.DurationMs,
			StatusCode:    row.StatusCode,
			StatusMessage: row.StatusMessage,
			HTTPStatus:    uint16(status),
			Attributes:    row.Attributes,
			Resource:      row.Resource,
		})
	}
	return spans, nil
}

// traceWindow asks the trace-id lookup when a trace happened. It answers zero
// times for a trace the lookup has no row for, which the caller reads as "do
// not bound".
//
// The bounds are widened by a second at each end. The lookup keeps whole
// seconds where the spans keep nanoseconds, so a span 400ms into the trace's
// last second sits after the `End` the view recorded, and comparing them
// unwidened would drop it.
func (c *Client) traceWindow(ctx context.Context, traceID string) (time.Time, time.Time, error) {
	statement := fmt.Sprintf(`SELECT
    toString(toUnixTimestamp(min(Start))) AS first,
    toString(toUnixTimestamp(max(End))) AS last,
    toString(count()) AS rows
FROM %s.%s
WHERE TraceId = {traceId:String}
FORMAT JSONEachRow`,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(TracesIDLookupTable))

	rows, err := c.selectionRows(ctx, statement, map[string]string{"traceId": traceID})
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if len(rows) == 0 || parseUint(rows[0]["rows"]) == 0 {
		return time.Time{}, time.Time{}, nil
	}
	first, last := parseUint(rows[0]["first"]), parseUint(rows[0]["last"])
	if first == 0 || last < first {
		return time.Time{}, time.Time{}, nil
	}
	return time.Unix(int64(first), 0).UTC().Add(-time.Second),
		time.Unix(int64(last), 0).UTC().Add(time.Second), nil
}

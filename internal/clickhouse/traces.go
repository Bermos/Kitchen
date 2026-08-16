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

// Spans, as instrumented applications send them to the platform's OTLP
// receiver.
//
// Nothing here is derived from the flow data. Hubble sees that one workload
// called another and how long the call took; only the application knows that
// the call was "checkout" and that it spent its time in a database. Promoting
// L7 flows into trace-shaped rows would produce something that looked like
// tracing and answered none of the questions tracing exists for.

// DefaultTraceLimit is how many traces a list request returns when it does not
// ask for a number; MaxTraceLimit is the ceiling. MaxTraceSpans bounds one
// trace: a runaway instrumentation should make a trace look truncated, not
// make the operator hold a million spans.
const (
	DefaultTraceLimit = 50
	MaxTraceLimit     = 500
	MaxTraceSpans     = 2000
)

// Status codes, as OTLP spells them.
const (
	StatusUnset = "UNSET"
	StatusOK    = "OK"
	StatusError = "ERROR"
)

// Span is one operation inside one trace.
type Span struct {
	Timestamp    time.Time `json:"timestamp"`
	TraceID      string    `json:"traceId"`
	SpanID       string    `json:"spanId"`
	ParentSpanID string    `json:"parentSpanId,omitempty"`
	Name         string    `json:"name"`
	// Kind is OTLP's: SERVER, CLIENT, INTERNAL, PRODUCER, CONSUMER.
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
	// should not be a map lookup over every span in the window.
	HTTPStatus uint16 `json:"httpStatus,omitempty"`

	Attributes map[string]string `json:"attributes,omitempty"`
	Resource   map[string]string `json:"resource,omitempty"`
}

// InsertSpans writes a batch of spans.
func (c *Client) InsertSpans(ctx context.Context, spans []Span) error {
	if len(spans) == 0 {
		return nil
	}
	rows := make([]string, 0, len(spans))
	for _, span := range spans {
		row, err := json.Marshal(map[string]any{
			// Microseconds, because a span's whole point is that it is short:
			// milliseconds would round two nested spans onto the same instant.
			"timestamp":     span.Timestamp.UTC().Format("2006-01-02 15:04:05.000000"),
			"traceId":       span.TraceID,
			"spanId":        span.SpanID,
			"parentSpanId":  span.ParentSpanID,
			"name":          span.Name,
			"kind":          span.Kind,
			"service":       span.Service,
			"project":       span.Project,
			"environment":   span.Environment,
			"durationMs":    span.DurationMs,
			"statusCode":    span.StatusCode,
			"statusMessage": span.StatusMessage,
			"httpStatus":    span.HTTPStatus,
			"attributes":    span.Attributes,
			"resource":      span.Resource,
		})
		if err != nil {
			return err
		}
		rows = append(rows, string(row))
	}
	statement := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow\n%s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(TracesTable), strings.Join(rows, "\n"))
	return c.Exec(ctx, statement)
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

	conditions := []string{
		"timestamp >= parseDateTime64BestEffort({since:String}, 6, 'UTC')",
		"timestamp <= parseDateTime64BestEffort({until:String}, 6, 'UTC')",
	}
	params := map[string]string{
		"since": since.UTC().Format(time.RFC3339Nano),
		"until": until.UTC().Format(time.RFC3339Nano),
		"limit": strconv.Itoa(limit),
	}
	for _, filter := range []struct{ column, value string }{
		{"service", query.Service},
		{"project", query.Project},
		{"environment", query.Environment},
	} {
		if filter.value == "" {
			continue
		}
		conditions = append(conditions,
			fmt.Sprintf("%s = {%s:String}", quoteIdentifier(filter.column), filter.column))
		params[filter.column] = filter.value
	}

	// The window bounds the spans; these bound the traces, so they are a
	// HAVING over the group rather than a WHERE over the rows — one slow span
	// makes a slow trace, and one failed span makes a failed trace.
	having := []string{}
	if query.OnlyErrors {
		having = append(having, fmt.Sprintf("countIf(statusCode = %s) > 0", quoteLiteral(StatusError)))
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
	statement := fmt.Sprintf(`SELECT
    traceId,
    toString(toUnixTimestamp64Micro(min(timestamp))) AS traceStart,
    max(toUnixTimestamp64Micro(timestamp) / 1000 + durationMs) - min(toUnixTimestamp64Micro(timestamp) / 1000) AS traceDurationMs,
    if(countIf(parentSpanId = '') > 0, anyIf(name, parentSpanId = ''), argMin(name, timestamp)) AS rootName,
    if(countIf(parentSpanId = '') > 0, anyIf(service, parentSpanId = ''), argMin(service, timestamp)) AS rootService,
    argMin(project, timestamp) AS traceProject,
    argMin(environment, timestamp) AS traceEnvironment,
    toString(count()) AS spans,
    toString(countIf(statusCode = %s)) AS errors,
    toString(uniqExact(service)) AS services,
    toString(max(httpStatus)) AS httpStatus
FROM %s.%s
WHERE %s
GROUP BY traceId%s
ORDER BY traceStart DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		quoteLiteral(StatusError),
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
// It is unwindowed on purpose: a trace id arrives from a log line or from the
// list, and asking the caller to also know when it happened would make the one
// link that matters — line to trace — impossible to follow. The bloom filter
// on `traceId` is what keeps that from being a scan of the retention.
func (c *Client) Trace(ctx context.Context, traceID string) ([]Span, error) {
	traceID = strings.TrimSpace(traceID)
	if traceID == "" {
		return nil, fmt.Errorf("a trace id is required")
	}

	statement := fmt.Sprintf(`SELECT
    formatDateTime(timestamp, '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC') AS ts,
    traceId, spanId, parentSpanId, name, kind, service, project, environment,
    durationMs, statusCode, statusMessage,
    toString(httpStatus) AS httpStatus,
    attributes, resource
FROM %s.%s
WHERE traceId = {traceId:String}
ORDER BY timestamp ASC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(TracesTable))

	body, err := c.QueryWithParams(ctx, statement, map[string]string{
		"traceId": traceID,
		"limit":   strconv.Itoa(MaxTraceSpans),
	})
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
		timestamp, err := time.Parse("2006-01-02T15:04:05.999999Z", row.Timestamp)
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

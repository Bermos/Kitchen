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

// The requests an uninstrumented application never reports, observed where
// they all pass anyway: the shared Gateway's proxy, read through the same
// Hubble stream the service map already comes from.
//
// Raw rows are kept and not only rollups, because the failing request is
// itself the answer — "show me the 500s" is a row listing, and the crash
// report joins these rows to log lines by time. They are also the cheapest
// rows in the store, measured at around ten bytes compressed.
//
// Nothing here normalises a path. The follower templates it at ingest against
// a per-environment budget, which is what makes `route` a LowCardinality
// column and what keeps the store from ever seeing the unbounded set.

// RequestSourceGateway is the vantage point every row carries today: the
// shared Gateway's Envoy, which sees 100% of what enters the platform and
// nothing that stays inside it. A second source (§3.1's eBPF successor) would
// be a second value here rather than a second table.
const RequestSourceGateway = "gateway"

// DefaultRequestLimit is how many rows a request listing returns when it does
// not ask for a number, and MaxRequestLimit the ceiling: the answer is read
// whole, so an unbounded listing is a way to make the operator hold a day of
// traffic in memory.
const (
	DefaultRequestLimit = 200
	MaxRequestLimit     = 5000
)

// DefaultCorrelationWindow is how far either side of a moment CorrelatedRequests
// looks when the caller does not say. It matches the ±30s the correlated-logs
// view uses, so the two halves of a crash report cover the same span.
const DefaultCorrelationWindow = 30 * time.Second

// Request is one HTTP request the edge observed, both on the way into the
// store and on the way back out.
//
// Path is what was asked for and Route is what it was templated to; they are
// both kept because the template is what groups and the path is what makes a
// mis-templated route diagnosable. Project and Environment are the operator's
// attribution from the host, and are empty for a host the platform never
// published — the unrouted bucket, which is a signal rather than a defect.
type Request struct {
	Timestamp   time.Time `json:"timestamp"`
	Project     string    `json:"project,omitempty"`
	Environment string    `json:"environment,omitempty"`
	Host        string    `json:"host,omitempty"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Route       string    `json:"route,omitempty"`
	Status      uint16    `json:"status"`
	DurationMs  float64   `json:"durationMs"`
	// Protocol is the request's own — HTTP/1.1, HTTP/2 — not the transport's.
	Protocol string `json:"protocol,omitempty"`
	// Source names the vantage point that observed it; see RequestSourceGateway.
	Source string `json:"source,omitempty"`
	// TraceID links the request to the application's own spans. It is empty
	// until a source for it exists, which is why the column is reserved rather
	// than populated.
	TraceID string `json:"traceId,omitempty"`
}

// InsertRequests writes a batch of observed requests.
func (c *Client) InsertRequests(ctx context.Context, requests []Request) error {
	if len(requests) == 0 {
		return nil
	}
	rows := make([]string, 0, len(requests))
	for _, request := range requests {
		source := request.Source
		if source == "" {
			// A row that names no vantage point came from the only one there
			// is, and defaulting here keeps a follower that forgot to set it
			// from filling the source facet with an empty value.
			source = RequestSourceGateway
		}
		row, err := json.Marshal(map[string]any{
			"Timestamp":   request.Timestamp.UTC().Format("2006-01-02 15:04:05.000000000"),
			"project":     request.Project,
			"environment": request.Environment,
			"host":        request.Host,
			"method":      request.Method,
			"path":        request.Path,
			"route":       request.Route,
			"status":      request.Status,
			"duration_ms": request.DurationMs,
			"protocol":    request.Protocol,
			"source":      source,
			"trace_id":    request.TraceID,
		})
		if err != nil {
			return err
		}
		rows = append(rows, string(row))
	}
	statement := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow\n%s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(RequestsTable), strings.Join(rows, "\n"))
	return c.Exec(ctx, statement)
}

// The raw table's window, compared at the column's own scale so that neither
// bound is a widening cast on every row. See logSinceCondition, which is the
// same bound over a column of the same type and name.
const (
	requestSinceCondition = "Timestamp >= parseDateTime64BestEffort({since:String}, 9, 'UTC')"
	requestUntilCondition = "Timestamp <= parseDateTime64BestEffort({until:String}, 9, 'UTC')"
)

// requestColumns is the projection the listing selects, and the one place the
// table's column names are translated into the API's.
//
// The timestamp is rendered by logTimestampFormat and read back by
// otelTimestampLayout: the request table's time column is spelled, scaled and
// therefore formatted exactly like the log table's, and the correlated view
// puts rows from both on one screen.
const requestColumns = "project, environment, host, method, path, route, status, " +
	"duration_ms AS durationMs, protocol, source, trace_id AS traceId"

// requestRow is the wire shape ClickHouse returns. The timestamp arrives as a
// string under the name `ts`; see timestampAlias for why it is not called
// `timestamp`.
type requestRow struct {
	Timestamp   string  `json:"ts"`
	Project     string  `json:"project"`
	Environment string  `json:"environment"`
	Host        string  `json:"host"`
	Method      string  `json:"method"`
	Path        string  `json:"path"`
	Route       string  `json:"route"`
	Status      uint16  `json:"status"`
	DurationMs  float64 `json:"durationMs"`
	Protocol    string  `json:"protocol"`
	Source      string  `json:"source"`
	TraceID     string  `json:"traceId"`
}

// RequestListQuery selects raw request rows, newest first. Project is
// required: this is the environment page's list, and "every request the
// platform served" is asked as a breakdown rather than as rows.
type RequestListQuery struct {
	Project     string
	Environment string
	// Since and Until bound the window. A zero Until means now; a zero Since
	// means an hour before Until.
	Since time.Time
	Until time.Time
	// Route keeps one route template, which is what clicking a row of the
	// route table filters by.
	Route string
	// Method keeps one verb, as the follower canonicalised it.
	Method string
	// StatusClass keeps one class of answer, written as its leading digit: 5
	// is every 5xx. Zero keeps all of them.
	StatusClass int
	// OnlyErrors keeps what the golden signals count as an error, which is
	// status >= 500. It composes with StatusClass rather than replacing it.
	OnlyErrors bool
	// ExcludeHealth drops the platform's own health checks, so that the list
	// under a set of numbers is the same traffic those numbers are of. It is
	// ignored where Route names one, for the reason RequestQuery gives.
	ExcludeHealth []HealthRoute
	Limit         int
}

// QueryRequests reads the raw rows a listing matches, newest first — which is
// the order they are wanted in, unlike a log, which reads forwards.
func (c *Client) QueryRequests(ctx context.Context, query RequestListQuery) ([]Request, error) {
	if strings.TrimSpace(query.Project) == "" {
		return nil, fmt.Errorf("a request listing must name a project")
	}
	since, until, err := resolveWindow(query.Since, query.Until)
	if err != nil {
		return nil, err
	}

	limit := query.Limit
	if limit < 1 {
		limit = DefaultRequestLimit
	}
	if limit > MaxRequestLimit {
		limit = MaxRequestLimit
	}

	conditions := []string{"project = {project:String}", requestSinceCondition, requestUntilCondition}
	params := map[string]string{
		"project": query.Project,
		"since":   since.Format(time.RFC3339Nano),
		"until":   until.Format(time.RFC3339Nano),
		"limit":   strconv.Itoa(limit),
	}
	filter := func(condition, name, value string) {
		if value == "" {
			return
		}
		conditions = append(conditions, condition)
		params[name] = value
	}
	filter("environment = {environment:String}", "environment", query.Environment)
	filter("route = {route:String}", "route", query.Route)
	filter("method = {method:String}", "method", query.Method)
	if query.Route == "" {
		if condition := healthCondition(query.ExcludeHealth, "", params); condition != "" {
			conditions = append(conditions, condition)
		}
	}
	if query.StatusClass > 0 {
		conditions = append(conditions, "intDiv(status, 100) = {statusClass:UInt8}")
		params["statusClass"] = strconv.Itoa(query.StatusClass)
	}
	if query.OnlyErrors {
		conditions = append(conditions, requestErrorCondition)
	}

	statement := fmt.Sprintf(`SELECT
    %s AS %s,
    %s
FROM %s.%s
WHERE %s
ORDER BY Timestamp DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		logTimestampFormat, timestampAlias, requestColumns,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(RequestsTable),
		strings.Join(conditions, " AND "))

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}

	requests := make([]Request, 0, limit)
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := requestRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable request row: %w", err)
		}
		timestamp, err := time.Parse(otelTimestampLayout, row.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("unreadable request timestamp %q: %w", row.Timestamp, err)
		}
		requests = append(requests, Request{
			Timestamp:   timestamp,
			Project:     row.Project,
			Environment: row.Environment,
			Host:        row.Host,
			Method:      row.Method,
			Path:        row.Path,
			Route:       row.Route,
			Status:      row.Status,
			DurationMs:  row.DurationMs,
			Protocol:    row.Protocol,
			Source:      row.Source,
			TraceID:     row.TraceID,
		})
	}
	return requests, nil
}

// RequestCorrelationQuery asks what the edge saw around a moment: the
// termination instant of a crashed container, or the timestamp of a log line
// someone clicked.
type RequestCorrelationQuery struct {
	Project     string
	Environment string
	// At is the moment to look around; a zero value means now.
	At time.Time
	// Within is how far either side of it to look, defaulting to
	// DefaultCorrelationWindow.
	Within time.Duration
	// OnlyErrors keeps the failed answers, which is what a crash report wants
	// and what a healthy window will have none of.
	OnlyErrors bool
	Limit      int
}

// CorrelatedRequests is the request half of the crash report: everything the
// edge served for one environment either side of a moment.
//
// It is a listing rather than an aggregate on purpose — the question is "what
// was being asked for when it died", and a rate cannot answer that.
func (c *Client) CorrelatedRequests(ctx context.Context, query RequestCorrelationQuery) ([]Request, error) {
	at := query.At
	if at.IsZero() {
		at = time.Now()
	}
	within := query.Within
	if within <= 0 {
		within = DefaultCorrelationWindow
	}
	return c.QueryRequests(ctx, RequestListQuery{
		Project:     query.Project,
		Environment: query.Environment,
		Since:       at.Add(-within),
		Until:       at.Add(within),
		OnlyErrors:  query.OnlyErrors,
		Limit:       query.Limit,
	})
}

// resolveWindow fills in the bounds a read left open: an open end is now, an
// open start is an hour before the end. Both come back in UTC, which is what
// the parameters are formatted as.
func resolveWindow(since, until time.Time) (time.Time, time.Time, error) {
	if until.IsZero() {
		until = time.Now()
	}
	if since.IsZero() {
		since = until.Add(-time.Hour)
	}
	until, since = until.UTC(), since.UTC()
	if !until.After(since) {
		return time.Time{}, time.Time{}, fmt.Errorf("the window must end after it starts")
	}
	return since, until, nil
}

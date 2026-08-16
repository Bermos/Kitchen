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

// DefaultLogLimit is how many lines a log request returns when it does not ask
// for a number, and MaxLogLimit is the ceiling on what it may ask for: the
// operator reads the answer whole, so an unbounded query would be a way to
// make it hold a build's entire output in memory.
const (
	DefaultLogLimit = 200
	MaxLogLimit     = 5000
)

// timestampAlias is the name the formatted timestamp is selected under.
//
// It is deliberately not `timestamp`: ClickHouse resolves a name in WHERE and
// ORDER BY against the SELECT aliases before the table's columns, so aliasing
// the formatted value to `timestamp` hides the DateTime64 column behind the
// String this expression produces. The window then compares the two and the
// query fails with "No operation greaterOrEquals between String and
// DateTime64" — which is what the observability view showed on its very first
// query, before a single line had been collected. The same shadowing would
// catch any caller-written expression in FilterLogs that mentions the column.
const timestampAlias = "ts"

// LogQuery selects log lines. The zero value selects nothing: every caller
// scopes the read to one build or one environment, because "every line in the
// cluster" is not a question the API answers.
type LogQuery struct {
	// Source filters on how the line was produced (build or runtime).
	Source string
	// Build names the Build whose output is wanted.
	Build string
	// Project and Environment scope runtime logs.
	Project     string
	Environment string
	// Container narrows to one container of the pod.
	Container string
	// Since and Until bound the window. Zero values are open ends.
	Since time.Time
	Until time.Time
	// Search keeps only lines containing this substring.
	Search string
	// Limit caps the number of lines returned, most recent first on the way
	// out of ClickHouse and oldest first in the result.
	Limit int
}

// LogLine is one collected log line.
type LogLine struct {
	Timestamp   time.Time `json:"timestamp"`
	Source      string    `json:"source"`
	Project     string    `json:"project,omitempty"`
	Environment string    `json:"environment,omitempty"`
	Build       string    `json:"build,omitempty"`
	Pod         string    `json:"pod,omitempty"`
	Container   string    `json:"container,omitempty"`
	Stream      string    `json:"stream,omitempty"`
	Level       string    `json:"level,omitempty"`
	Message     string    `json:"message"`
	// TraceID and SpanID are the line's own, lifted out of its structured
	// fields by the collector. They are what makes a log line and a trace two
	// views of the same request rather than two searches.
	TraceID string `json:"traceId,omitempty"`
	SpanID  string `json:"spanId,omitempty"`
	// Fields are the line's own structured fields — what the collector
	// flattened a JSON line into. Empty for a line that was not JSON.
	Fields map[string]string `json:"fields,omitempty"`
}

// logRow is the wire shape ClickHouse returns. The timestamp arrives as a
// string because JSONEachRow renders DateTime64 that way, under the name
// `ts` — see timestampAlias for why it is not called `timestamp`.
type logRow struct {
	Timestamp   string            `json:"ts"`
	Source      string            `json:"source"`
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	Build       string            `json:"build"`
	Pod         string            `json:"pod"`
	Container   string            `json:"container"`
	Stream      string            `json:"stream"`
	Level       string            `json:"level"`
	TraceID     string            `json:"traceId"`
	SpanID      string            `json:"spanId"`
	Message     string            `json:"message"`
	Fields      map[string]string `json:"fields"`
}

// logColumns is the projection every line-returning query selects. It is one
// constant because the two of them have to agree: a column added to one and
// forgotten in the other is a field that is populated on a build's log page
// and empty on the observability view.
const logColumns = "source, project, environment, build, pod, container, stream, level, " +
	"traceId, spanId, message, fields"

// SearchLogs reads the lines matching the query, oldest first.
//
// The limit applies to the newest lines — asking for 200 lines of a build that
// produced 10,000 should show its ending, not its beginning — and the result is
// reversed afterwards so it reads in the order the lines were written.
func (c *Client) SearchLogs(ctx context.Context, query LogQuery) ([]LogLine, error) {
	limit := query.Limit
	if limit < 1 {
		limit = DefaultLogLimit
	}
	if limit > MaxLogLimit {
		limit = MaxLogLimit
	}

	conditions := []string{}
	params := map[string]string{"limit": strconv.Itoa(limit)}
	filter := func(column, kind, name, value string) {
		if value == "" {
			return
		}
		conditions = append(conditions, fmt.Sprintf("%s = {%s:%s}", quoteIdentifier(column), name, kind))
		params[name] = value
	}
	filter("source", "String", "source", query.Source)
	filter("build", "String", "build", query.Build)
	filter("project", "String", "project", query.Project)
	filter("environment", "String", "environment", query.Environment)
	filter("container", "String", "container", query.Container)

	if !query.Since.IsZero() {
		conditions = append(conditions, "timestamp >= parseDateTime64BestEffort({since:String}, 3, 'UTC')")
		params["since"] = query.Since.UTC().Format(time.RFC3339Nano)
	}
	if !query.Until.IsZero() {
		conditions = append(conditions, "timestamp <= parseDateTime64BestEffort({until:String}, 3, 'UTC')")
		params["until"] = query.Until.UTC().Format(time.RFC3339Nano)
	}
	if query.Search != "" {
		conditions = append(conditions, "positionCaseInsensitive(message, {search:String}) > 0")
		params["search"] = query.Search
	}
	if len(conditions) == 0 {
		return nil, fmt.Errorf("a log query must be scoped to at least one of build, project or environment")
	}

	statement := fmt.Sprintf(`SELECT
    formatDateTime(timestamp, '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC') AS %s,
    %s
FROM %s.%s
WHERE %s
ORDER BY timestamp DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		timestampAlias, logColumns,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable), strings.Join(conditions, " AND "))

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}
	return parseLogLines(body, limit)
}

// LogSelection is what a question about the logs is asked over: the query, and
// the window it is asked in. Every analytic over the store — the lines, the
// histogram, the facets, the patterns — takes one, because they are four views
// of the same selection and would be lying if they could disagree about it.
//
// Both query surfaces are optional and compose with AND. An empty selection is
// a legitimate question — "everything in the window" — and the window and the
// limit are what bound it. There is deliberately no sentinel to type for that.
type LogSelection struct {
	// Query is Kitchen's log query language: `level:error service:shop`.
	// See CompileLogQuery. This is the front door.
	Query string
	// Where is a ClickHouse boolean expression over the table's columns,
	// evaluated as written. It is the escape hatch, kept because it is
	// genuinely more powerful than the query language and always will be.
	Where string
	// Since and Until bound the window on top of the query.
	Since time.Time
	Until time.Time
}

// conditions compiles the selection into ClickHouse predicates and the
// parameters they read. An empty selection compiles to no conditions at all —
// not to a tautology — and the caller renders no WHERE clause.
func (s LogSelection) conditions() ([]string, map[string]string, error) {
	conditions := []string{}
	params := map[string]string{}

	if query := strings.TrimSpace(s.Query); query != "" {
		compiled, err := CompileLogQuery(query)
		if err != nil {
			return nil, nil, err
		}
		if compiled.Expression != "" {
			conditions = append(conditions, "("+compiled.Expression+")")
			for name, value := range compiled.Params {
				params[name] = value
			}
		}
	}
	if where := strings.TrimSpace(s.Where); where != "" {
		conditions = append(conditions, "("+where+")")
	}
	if !s.Since.IsZero() {
		conditions = append(conditions, "timestamp >= parseDateTime64BestEffort({since:String}, 3, 'UTC')")
		params["since"] = s.Since.UTC().Format(time.RFC3339Nano)
	}
	if !s.Until.IsZero() {
		conditions = append(conditions, "timestamp <= parseDateTime64BestEffort({until:String}, 3, 'UTC')")
		params["until"] = s.Until.UTC().Format(time.RFC3339Nano)
	}
	return conditions, params, nil
}

// whereClause is conditions() rendered — an empty string when the selection
// bounds nothing, so that "everything" is the absence of a predicate rather
// than a predicate that is always true.
func (s LogSelection) whereClause() (string, map[string]string, error) {
	conditions, params, err := s.conditions()
	if err != nil {
		return "", nil, err
	}
	if len(conditions) == 0 {
		return "", params, nil
	}
	return "WHERE " + strings.Join(conditions, " AND "), params, nil
}

// LogFilter reads lines matching a selection.
type LogFilter struct {
	LogSelection
	// Limit caps the lines returned, newest kept, oldest first on the way out.
	Limit int
}

// FilterLogs answers the lines a selection matches.
//
// The `where` half of a selection goes into the query text as written — that is
// the feature — so the query runs read-only (readonly=2: no writes, no DDL) and
// under an execution-time cap. What a caller can reach is what the operator's
// ClickHouse user can read; today every API caller is a trusted platform user
// (scopes and RBAC are an open item in AUTH.md), and the settings keep a typo
// from becoming a write or a runaway scan.
func (c *Client) FilterLogs(ctx context.Context, filter LogFilter) ([]LogLine, error) {
	limit := filter.Limit
	if limit < 1 {
		limit = DefaultLogLimit
	}
	if limit > MaxLogLimit {
		limit = MaxLogLimit
	}

	where, params, err := filter.whereClause()
	if err != nil {
		return nil, err
	}
	params["limit"] = strconv.Itoa(limit)

	statement := fmt.Sprintf(`SELECT
    formatDateTime(timestamp, '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC') AS %s,
    %s
FROM %s.%s
%s
ORDER BY timestamp DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		timestampAlias, logColumns,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable), where)

	body, err := c.queryWithSettings(ctx, statement, params, readonlySettings)
	if err != nil {
		return nil, err
	}
	return parseLogLines(body, limit)
}

// parseLogLines turns a JSONEachRow answer into lines, oldest first. The
// query asked for the newest lines, so the order is reversed on the way out —
// a log reads forwards.
func parseLogLines(body string, capacity int) ([]LogLine, error) {
	lines := make([]LogLine, 0, capacity)
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := logRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable log row: %w", err)
		}
		timestamp, err := time.Parse("2006-01-02T15:04:05.999Z", row.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("unreadable log timestamp %q: %w", row.Timestamp, err)
		}
		if len(row.Fields) == 0 {
			row.Fields = nil
		}
		lines = append(lines, LogLine{
			Timestamp:   timestamp,
			Source:      row.Source,
			Project:     row.Project,
			Environment: row.Environment,
			Build:       row.Build,
			Pod:         row.Pod,
			Container:   row.Container,
			Stream:      row.Stream,
			Level:       row.Level,
			TraceID:     row.TraceID,
			SpanID:      row.SpanID,
			Message:     row.Message,
			Fields:      row.Fields,
		})
	}

	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines, nil
}

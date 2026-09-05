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
// It is deliberately not the time column's own name: ClickHouse resolves a name
// in WHERE and ORDER BY against the SELECT aliases before the table's columns,
// so aliasing the formatted value over the column hides the DateTime64 behind
// the String this expression produces. The window then compares the two and the
// query fails with "No operation greaterOrEquals between String and
// DateTime64" — which is what the observability view showed on its very first
// query, before a single line had been collected. The same shadowing would
// catch any caller-written expression in FilterLogs that mentions the column.
const timestampAlias = "ts"

// logTimestampFormat renders the log time column into the shape
// otelTimestampLayout reads back, and otelTimestampLayout is how Go reads it.
//
// ClickHouse's `%f` gives microseconds whatever the column's scale, so a
// DateTime64(9) arrives with six fractional digits. Spans are read the same
// way: microseconds are what a waterfall needs, because two nested spans can
// start in the same millisecond and drawing them level is wrong.
const (
	logTimestampFormat  = `formatDateTime(Timestamp, '%Y-%m-%dT%H:%i:%S.%fZ', 'UTC')`
	otelTimestampLayout = "2006-01-02T15:04:05.999999Z"
)

// The window's two bounds. The column is DateTime64(9) and the value is parsed
// at the same scale, so the comparison is between two of a kind rather than
// through a widening cast on every row.
const (
	logSinceCondition = "Timestamp >= parseDateTime64BestEffort({since:String}, 9, 'UTC')"
	logUntilCondition = "Timestamp <= parseDateTime64BestEffort({until:String}, 9, 'UTC')"
)

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
	// Process narrows to one of the project's workers or scheduled jobs, and
	// Run to one firing of a scheduled one. An empty Process is not "the web
	// process": it is every process, because the web process is what the
	// environment's logs already meant before either field existed.
	Process string
	Run     string
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
	Process     string    `json:"process,omitempty"`
	Run         string    `json:"run,omitempty"`
	Pod         string    `json:"pod,omitempty"`
	Container   string    `json:"container,omitempty"`
	Stream      string    `json:"stream,omitempty"`
	Level       string    `json:"level,omitempty"`
	Message     string    `json:"message"`
	// TraceID and SpanID are the line's own, correlated by the collector. They
	// are what makes a log line and a trace two views of the same request
	// rather than two searches.
	TraceID string `json:"traceId,omitempty"`
	SpanID  string `json:"spanId,omitempty"`
	// Fields are the line's own attributes — what the collector parsed a JSON
	// line into, plus whatever the receiver added. Empty for a line that
	// carried nothing.
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
	Process     string            `json:"process"`
	Run         string            `json:"run"`
	Pod         string            `json:"pod"`
	Container   string            `json:"container"`
	Stream      string            `json:"stream"`
	Level       string            `json:"level"`
	TraceID     string            `json:"traceId"`
	SpanID      string            `json:"spanId"`
	Message     string            `json:"message"`
	Fields      map[string]string `json:"fields"`
}

// logColumns is the projection every line-returning query selects, and the one
// place the OTel table is translated back into the shape the API answers with.
// It is one constant because the two readers have to agree: a column added to
// one and forgotten in the other is a field that is populated on a build's log
// page and empty on the observability view.
//
// The Kitchen-named half needs no work — those columns are materialized under
// these names. The rest is renaming, except `level`, which is folded to lower
// case; see logLevelColumn for why that is the API's shape and not a
// convenience.
const logColumns = "source, project, environment, build, process, run, pod, container, stream, " +
	logLevelColumn + " AS level, " +
	"TraceId AS traceId, SpanId AS spanId, " +
	logMessageColumn + " AS message, LogAttributes AS fields"

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
	filter("process", "String", "process", query.Process)
	filter("run", "String", "run", query.Run)
	filter("container", "String", "container", query.Container)

	if !query.Since.IsZero() {
		conditions = append(conditions, logSinceCondition)
		params["since"] = query.Since.UTC().Format(time.RFC3339Nano)
	}
	if !query.Until.IsZero() {
		conditions = append(conditions, logUntilCondition)
		params["until"] = query.Until.UTC().Format(time.RFC3339Nano)
	}
	if query.Search != "" {
		conditions = append(conditions,
			fmt.Sprintf("positionCaseInsensitive(%s, {search:String}) > 0", logMessageColumn))
		params["search"] = query.Search
	}
	if len(conditions) == 0 {
		return nil, fmt.Errorf("a log query must be scoped to at least one of build, project or environment")
	}

	statement := fmt.Sprintf(`SELECT
    %s AS %s,
    %s
FROM %s.%s
WHERE %s
ORDER BY Timestamp DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		logTimestampFormat, timestampAlias, logColumns,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable), strings.Join(conditions, " AND "))

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}
	return parseLogLines(body, limit)
}

// LogSelection is what a question about the logs is asked over: the query, the
// scope it may read, and the window it is asked in. Every analytic over the
// store — the lines, the histogram, the facets, the patterns — takes one,
// because they are four views of the same selection and would be lying if they
// could disagree about it.
//
// The query is optional and an empty one is a legitimate question —
// "everything in the window" — where the window, the scope and the limit are
// what bound it. There is deliberately no sentinel to type for that.
//
// **The scope is not optional**, and that is the whole of issue #421. A
// selection used to carry a second surface beside the query: `Where`, a
// ClickHouse expression composed into the statement as written, with the
// caller's projects appended to it as one more conjunct. A conjunct bounds
// what a statement *returns*; it bounds nothing about what it may *read*, so a
// subquery inside the expression answered about the whole telemetry database
// one bit at a time — and `readonly=2`, which forbids writes and DDL, permits
// `url()` outright (it wants CREATE TEMPORARY TABLE, which readonly=2 grants),
// so the reach was not even confined to this store. There is now one filter
// surface, the query language, whose every value leaves as a bound parameter
// and whose statement text this package writes itself; the scope is applied
// structurally around it, and a selection that names none reads nothing.
type LogSelection struct {
	// Query is Kitchen's log query language: `level:error service:shop`.
	// See CompileLogQuery. It is the only filter surface there is.
	Query string
	// Scope is what this selection may read. It is the caller's projects on
	// the API's routes, one project on a read that is about one, and the
	// platform's whole store only where the reader is entitled to it.
	Scope LogScope
	// Since and Until bound the window on top of the query.
	Since time.Time
	Until time.Time
}

// LogScope is which projects a selection may read: the boundary, expressed as
// a structure rather than as text somebody could compose around.
//
// Platform and an empty Projects are deliberately two different things. A
// scope that names nothing reads nothing — a zero LogScope is the safe
// direction, not "everything" — and the whole store is asked for by saying so.
type LogScope struct {
	// Projects narrows the read to lines belonging to these projects, by name.
	Projects []string
	// Platform is every line in the store, including the ones belonging to no
	// project at all — Kitchen's own components and the rest of the cluster.
	// It is the operator's view, and the two reads inside the platform that
	// are about the platform (a self-update's job, a component's own output).
	Platform bool
}

// condition compiles the scope into the predicate that bounds the statement,
// binding every project name as a parameter. Names are DNS labels and could
// safely be quoted, but nothing caller-shaped is written into a statement here
// on purpose: the rule is that values travel as parameters, and a rule with an
// exception in it is a rule somebody extends.
func (s LogScope) condition(params map[string]string) (string, error) {
	if s.Platform {
		return "", nil
	}
	if len(s.Projects) == 0 {
		// Not a LogQueryError: there is nothing the caller could type to fix
		// it. It is a selection the platform built without saying what it may
		// read, which is a bug in the caller of this package, and it reads
		// nothing rather than everything.
		return "", fmt.Errorf("a log selection must say which projects it may read")
	}
	placeholders := make([]string, 0, len(s.Projects))
	for i, project := range s.Projects {
		name := "scope" + strconv.Itoa(i)
		params[name] = project
		placeholders = append(placeholders, fmt.Sprintf("{%s:String}", name))
	}
	return "project IN (" + strings.Join(placeholders, ", ") + ")", nil
}

// conditions compiles the selection into ClickHouse predicates and the
// parameters they read: the scope first, then the caller's own query, then the
// window. A selection with nothing but a scope compiles to the scope alone —
// not to a tautology — and one that asks for everything in an unbounded window
// is still bounded by it.
func (s LogSelection) conditions() ([]string, map[string]string, error) {
	conditions := []string{}
	params := map[string]string{}

	scope, err := s.Scope.condition(params)
	if err != nil {
		return nil, nil, err
	}
	if scope != "" {
		conditions = append(conditions, "("+scope+")")
	}

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
	if !s.Since.IsZero() {
		conditions = append(conditions, logSinceCondition)
		params["since"] = s.Since.UTC().Format(time.RFC3339Nano)
	}
	if !s.Until.IsZero() {
		conditions = append(conditions, logUntilCondition)
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
// Nothing a caller typed reaches the statement as text: the query language
// compiles to predicates this package wrote, over columns it chose, with every
// value bound as a parameter. The read-only settings (readonly=2: no writes, no
// DDL) and the execution cap stay on top of that as the second line rather than
// the first — they are what keeps a platform-side mistake from becoming a write
// or a runaway scan, and they were never a boundary between callers.
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
    %s AS %s,
    %s
FROM %s.%s
%s
ORDER BY Timestamp DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		logTimestampFormat, timestampAlias, logColumns,
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
		timestamp, err := time.Parse(otelTimestampLayout, row.Timestamp)
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
			Process:     row.Process,
			Run:         row.Run,
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

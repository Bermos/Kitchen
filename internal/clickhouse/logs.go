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
	Message     string    `json:"message"`
}

// logRow is the wire shape ClickHouse returns. The timestamp arrives as a
// string because JSONEachRow renders DateTime64 that way.
type logRow struct {
	Timestamp   string `json:"timestamp"`
	Source      string `json:"source"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Build       string `json:"build"`
	Pod         string `json:"pod"`
	Container   string `json:"container"`
	Stream      string `json:"stream"`
	Message     string `json:"message"`
}

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
    formatDateTime(timestamp, '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC') AS timestamp,
    source, project, environment, build, pod, container, stream, message
FROM %s.%s
WHERE %s
ORDER BY timestamp DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable), strings.Join(conditions, " AND "))

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}

	lines := make([]LogLine, 0, limit)
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
		lines = append(lines, LogLine{
			Timestamp:   timestamp,
			Source:      row.Source,
			Project:     row.Project,
			Environment: row.Environment,
			Build:       row.Build,
			Pod:         row.Pod,
			Container:   row.Container,
			Stream:      row.Stream,
			Message:     row.Message,
		})
	}

	// ClickHouse handed back the newest lines; a log reads forwards.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines, nil
}

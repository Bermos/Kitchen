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
	"regexp"
	"strings"
	"time"

	"github.com/Bermos/Kitchen/internal/retention"
)

// What retention actually did, measured rather than assumed.
//
// ClickHouse expires data on its own merge schedule and tells nobody: there is
// no callback, no return value and — unless part logging happens to be on —
// nothing durable to read afterwards. A record written by inferring what the
// store must have done would be a guess with a timestamp on it, which is worse
// than no record.
//
// So this does not claim to observe the store's deletions. It makes a **dated
// claim about what is left**: for each class, the horizon in force, the oldest
// row that survives it, how much the class holds and how much of that is still
// on the wrong side of the line. Two consecutive claims are the evidence that
// data expired between them, and it is the kind of evidence retention actually
// needs — not "these particular rows were deleted", which nothing here can
// substantiate, but "at this time, under this rule, this class held nothing
// older than this date".
//
// Where it *can* delete exactly, it does: a partition every row of which is
// past the horizon is dropped as metadata, and those rows are counted. That is
// the one number here that is exact rather than observed, and it is the reason
// the sweep is not merely a reader.

// retentionTarget is where one class lives: the table that answers for it, the
// column its age is measured from, and the filter that distinguishes it from
// another class in the same table.
//
// Whether the sweep may *delete* a class is deliberately not here — it is
// retention.Definition.Sweepable, so that the rule lives once beside the class
// rather than once beside each of the places that enforce it.
type retentionTarget struct {
	Class  retention.Class
	Table  string
	Column string
	Filter string
}

// sweepable answers whether this sweep may delete the class's expired data.
func (t retentionTarget) sweepable() bool {
	definition, ok := retention.DefinitionFor(t.Class)
	return ok && definition.Sweepable
}

// retentionTargets is the register: which table answers for which class.
//
// A class whose data spans several tables — metrics across five point types
// and a rollup, traces across the spans and their id lookup — names its
// *principal* table here. The TTL is applied to all of them (see
// EnsureMetricsSchema and EnsureTracesSchema); what this register decides is
// which one the survey measures, because "how far back do the metrics go" is
// answered by the table that holds the metrics rather than by six numbers.
var retentionTargets = []retentionTarget{
	{retention.ClassContainerLogs, LogsTable, timeColumnLogs, containerLogsCondition},
	{retention.ClassBuildLogs, LogsTable, timeColumnLogs, buildLogsCondition},
	{retention.ClassFlows, FlowsTable, timeColumnKitchen, ""},
	{retention.ClassMetrics, MetricsGaugeTable, timeColumnMetrics, ""},
	{retention.ClassTraces, TracesTable, timeColumnLogs, ""},
	// The minute rollup, not the raw table: the class's number is the one
	// this table carries exactly. The raw rows live a week or that window,
	// whichever is shorter, and the hour rollup twelve of them, so measuring
	// either against the class's horizon would be measuring the wrong rule.
	{retention.ClassRequests, RequestsMinuteTable, timeColumnRollup, ""},
	{retention.ClassClusterEvents, K8sEventsTable, timeColumnKitchen, ""},
	{retention.ClassActivity, EventsTable, timeColumnKitchen, ""},
	{retention.ClassAudit, AuditTable, timeColumnKitchen, ""},
}

// RetentionObservation is one class's answer for one sweep.
type RetentionObservation struct {
	Class retention.Class `json:"class"`
	Table string          `json:"table"`

	// Days and Horizon are the rule that was in force, carried into the
	// record so that a reader never has to go and find out what the
	// configuration was that day.
	Days    int32     `json:"days"`
	Horizon time.Time `json:"horizon"`

	// Rows the class holds, Oldest the oldest surviving row, and Expired how
	// many rows are still older than the horizon.
	Rows    int64      `json:"rows"`
	Oldest  *time.Time `json:"oldest,omitempty"`
	Expired int64      `json:"expired"`

	// Removed is what this sweep deleted itself, and Partitions how many
	// partitions it dropped to do it. Both are zero for a class the sweep may
	// not delete, and usually zero for one it may — the store's own TTL
	// normally gets there first, which is the intended division of labour.
	Removed    int64 `json:"removed"`
	Partitions int   `json:"partitions"`

	// Error is what went wrong measuring this class, where something did. A
	// class that could not be measured is reported as unmeasured rather than
	// as empty: "we hold nothing" and "we could not ask" are the two answers
	// that must never be confused here.
	Error string `json:"error,omitempty"`
}

// Measured is whether this observation is a measurement at all.
func (o RetentionObservation) Measured() bool { return o.Error == "" }

// SweepRetention measures every class against the model and drops what it can
// drop exactly, returning one observation per class in the model's own order.
//
// It never returns an error for a class it could not reach: one unreachable
// table must not cost the record of the other eight. A store that is entirely
// down produces nine observations that each say so, and the caller records
// that — a sweep that ran and found nothing readable is itself a fact worth
// keeping.
func (c *Client) SweepRetention(ctx context.Context, model retention.Model, now time.Time) []RetentionObservation {
	observations := make([]RetentionObservation, 0, len(retentionTargets))
	for _, target := range retentionTargets {
		days := model.Days(target.Class)
		if days < 1 {
			// A class with no retention is a class this platform has been
			// told nothing about. Skipping it is right: enforcing a zero
			// would be deleting everything on the strength of a missing
			// configuration value.
			continue
		}
		observations = append(observations, c.sweepClass(ctx, target, days, model.Horizon(target.Class, now)))
	}
	return observations
}

func (c *Client) sweepClass(
	ctx context.Context,
	target retentionTarget,
	days int32,
	horizon time.Time,
) RetentionObservation {
	observation := RetentionObservation{
		Class:   target.Class,
		Table:   target.Table,
		Days:    days,
		Horizon: horizon.UTC(),
	}

	if target.sweepable() {
		removed, partitions, err := c.dropExpiredPartitions(ctx, target.Table, horizon)
		if err != nil {
			// A drop that failed is not a measurement that failed: say so
			// and go on to measure, because the numbers below are the record
			// and the drop is the housekeeping.
			observation.Error = err.Error()
		}
		observation.Removed, observation.Partitions = removed, partitions
	}

	rows, oldest, expired, err := c.measureClass(ctx, target, horizon)
	if err != nil {
		observation.Error = err.Error()
		return observation
	}
	observation.Rows, observation.Expired = rows, expired
	if oldest != nil {
		observation.Oldest = oldest
	}
	return observation
}

// measureClass asks the three questions that make up the claim, in one query:
// how much is there, how far back does it go, and how much of it is past its
// date.
func (c *Client) measureClass(
	ctx context.Context,
	target retentionTarget,
	horizon time.Time,
) (rows int64, oldest *time.Time, expired int64, err error) {
	where := ""
	if target.Filter != "" {
		where = "\nWHERE " + target.Filter
	}
	// The horizon is a time this package computed, formatted here, and never
	// a value from a request: it goes into the statement as written, like
	// every other identifier in this file.
	query := fmt.Sprintf(`SELECT
    toInt64(count()) AS rows,
    toInt64(countIf(%s < toDateTime64(%s, 3, 'UTC'))) AS expired,
    if(count() = 0, '', formatDateTime(toDateTime(min(%s)), '%%Y-%%m-%%dT%%H:%%M:%%SZ', 'UTC')) AS oldest
FROM %s.%s%s
FORMAT JSONEachRow`,
		target.Column, quoteLiteral(horizonLiteral(horizon)), target.Column,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(target.Table), where)

	body, err := c.Query(ctx, query)
	if err != nil {
		return 0, nil, 0, err
	}
	line := strings.TrimSpace(body)
	if line == "" {
		return 0, nil, 0, nil
	}
	var answer struct {
		Rows    int64  `json:"rows"`
		Expired int64  `json:"expired"`
		Oldest  string `json:"oldest"`
	}
	if err := json.Unmarshal([]byte(line), &answer); err != nil {
		return 0, nil, 0, err
	}
	if answer.Oldest != "" {
		if parsed, err := time.Parse(time.RFC3339, answer.Oldest); err == nil {
			oldest = &parsed
		}
	}
	return answer.Rows, oldest, answer.Expired, nil
}

// dropExpiredPartitions removes the partitions whose every row is past the
// horizon, and counts what went.
//
// Every table this is called for partitions by day (`toDate(<column>)`), so a
// partition's name is an ISO date and a partition strictly before the
// horizon's date holds nothing that is still in date. That is why the
// comparison is on the partition name rather than on the parts' own min/max
// time columns: the name is the partition key this package wrote, and the
// comparison is exactly the rule it was written to make cheap.
//
// A day-partitioned table therefore keeps at most one partition's worth of
// expired rows — the one the horizon falls inside — which is the number
// reported as Expired above and the reason a small non-zero there is normal.
func (c *Client) dropExpiredPartitions(
	ctx context.Context,
	table string,
	horizon time.Time,
) (removed int64, partitions int, err error) {
	cutoff := horizon.UTC().Format("2006-01-02")
	query := fmt.Sprintf(`SELECT partition, toInt64(sum(rows)) AS rows
FROM system.parts
WHERE database = %s AND table = %s AND active AND partition < %s
GROUP BY partition
ORDER BY partition
FORMAT JSONEachRow`,
		quoteLiteral(c.cfg.Database), quoteLiteral(table), quoteLiteral(cutoff))

	body, err := c.Query(ctx, query)
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var part struct {
			Partition string `json:"partition"`
			Rows      int64  `json:"rows"`
		}
		if err := json.Unmarshal([]byte(line), &part); err != nil {
			return removed, partitions, err
		}
		if !partitionNamePattern.MatchString(part.Partition) {
			// The partition name reaches the DROP statement as a literal, so
			// anything that is not the ISO date this schema partitions by is
			// left alone rather than quoted and hoped for. A table someone
			// re-partitioned by hand is a table the sweep does not touch.
			continue
		}
		statement := fmt.Sprintf("ALTER TABLE %s.%s DROP PARTITION %s",
			quoteIdentifier(c.cfg.Database), quoteIdentifier(table), quoteLiteral(part.Partition))
		if err := c.Exec(ctx, statement); err != nil {
			return removed, partitions, err
		}
		removed += part.Rows
		partitions++
	}
	return removed, partitions, nil
}

// partitionNamePattern is the only partition name this sweep will act on: the
// ISO date every table it may sweep is partitioned by. It is a guard on a
// value that came back from the store rather than from configuration, and the
// value goes into a DROP — so anything else is left alone.
var partitionNamePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// horizonLiteral is the horizon as ClickHouse reads a DateTime64(3).
func horizonLiteral(horizon time.Time) string {
	return horizon.UTC().Format("2006-01-02 15:04:05.000")
}

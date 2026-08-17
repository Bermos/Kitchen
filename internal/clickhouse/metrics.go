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

// The dashboard's numbers are not a fourth pipeline: they are aggregations
// over what the store already holds. Deploys and build times come from the
// activity feed's events; log volume from the logs themselves; and the store's
// own size and ingest rate from ClickHouse's system tables and a count of the
// last five minutes of writes. Where a source is not collected, its numbers are
// zero and the UI says so, rather than the platform pretending to measure
// something it does not.
//
// The traffic numbers used to be here too, off the flows, and are not any
// more: an edge request is attributed by its Host header, which only the
// request pipeline carries. The fields remain on the answer and the API fills
// them from the rollups; see api's fillOverviewTraffic.

// MetricsQuery scopes the overview. The zero value is the whole platform.
type MetricsQuery struct {
	// Project narrows logs and events to one project. It is the only scope
	// left: the traffic half of this answer moved to the request pipeline,
	// which is keyed on project rather than on the destination namespace the
	// flows were attributed by.
	Project string
}

// MetricsOverview is the pre-aggregated answer the dashboard draws from.
// Sparkline series are fixed-size buckets, oldest first: 24 hours for the
// hourly ones, 7 days for the daily one, the last bucket being the current
// (partial) hour or day.
type MetricsOverview struct {
	// Deploys are environments moving to a release, promotion and rollback
	// alike, over the last 7 days.
	Deploys7d     uint64   `json:"deploys7d"`
	DeploysPerDay []uint64 `json:"deploysPerDay"`
	// MedianBuildSeconds is over the successful builds of the last 7 days;
	// 0 means no builds finished.
	MedianBuildSeconds float64 `json:"medianBuildSeconds"`

	// HTTP traffic over the last 24 hours. Nothing in this package fills
	// these — they are the request pipeline's, and the API writes them over
	// this answer — but they stay part of the shape because they are what the
	// endpoint serves under these names.
	Requests24h     uint64    `json:"requests24h"`
	ErrorRate24h    float64   `json:"errorRate24h"`
	P95Ms24h        float64   `json:"p95Ms24h"`
	RequestsPerHour []uint64  `json:"requestsPerHour"`
	ErrorsPerHour   []uint64  `json:"errorsPerHour"`
	P95MsPerHour    []float64 `json:"p95MsPerHour"`

	// Log volume over the last 24 hours.
	LogLines24h     uint64   `json:"logLines24h"`
	LogLinesPerHour []uint64 `json:"logLinesPerHour"`

	// The store itself.
	StoreBytes         uint64  `json:"storeBytes"`
	StoreRowsPerSecond float64 `json:"storeRowsPerSecond"`
}

// StoreStats is the telemetry store's own size and ingest rate: what its active
// parts occupy on disk, and how fast rows are arriving.
type StoreStats struct {
	// BytesOnDisk is what the database's active parts occupy.
	BytesOnDisk uint64 `json:"bytesOnDisk"`
	// RowsPerSecond is the recent ingest rate across the tables the collector
	// fills and the operator writes. Zero while pods are running is the store's
	// own stalled-ingest symptom.
	RowsPerSecond float64 `json:"rowsPerSecond"`
}

const (
	hourlyBuckets = 24
	dailyBuckets  = 7
)

// MetricsOverview aggregates the dashboard's numbers out of the telemetry
// tables. Each source is one bounded GROUP BY; a store that has never seen a
// build or a log line simply contributes zeroes.
//
// The traffic fields are deliberately not filled here. They used to be, from
// the flows, and they were wrong in the way api's fillOverviewTraffic
// documents: a flow is attributed by its *destination* endpoint, so a protected
// preview's requests were credited to the forward-auth gate and an idling
// environment's to the KEDA interceptor. The API overwrites all six from the
// request rollups, which are keyed on the Host header, so computing them again
// here would only be a query nobody reads.
func (c *Client) MetricsOverview(ctx context.Context, query MetricsQuery) (MetricsOverview, error) {
	overview := MetricsOverview{
		DeploysPerDay:   make([]uint64, dailyBuckets),
		RequestsPerHour: make([]uint64, hourlyBuckets),
		ErrorsPerHour:   make([]uint64, hourlyBuckets),
		P95MsPerHour:    make([]float64, hourlyBuckets),
		LogLinesPerHour: make([]uint64, hourlyBuckets),
	}

	now := time.Now().UTC()
	hourStart := now.Truncate(time.Hour).Add(-time.Duration(hourlyBuckets-1) * time.Hour)
	dayStart := now.Truncate(24 * time.Hour).Add(-time.Duration(dailyBuckets-1) * 24 * time.Hour)

	if err := c.deployMetrics(ctx, query, dayStart, &overview); err != nil {
		return overview, err
	}
	if err := c.logMetrics(ctx, query, hourStart, &overview); err != nil {
		return overview, err
	}
	stats, err := c.StoreStats(ctx)
	if err != nil {
		return overview, err
	}
	overview.StoreBytes, overview.StoreRowsPerSecond = stats.BytesOnDisk, stats.RowsPerSecond
	return overview, nil
}

func (c *Client) deployMetrics(ctx context.Context, query MetricsQuery, dayStart time.Time, overview *MetricsOverview) error {
	conditions, params := windowConditions(dayStart)
	if query.Project != "" {
		conditions = append(conditions, "project = {project:String}")
		params["project"] = query.Project
	}

	statement := fmt.Sprintf(`SELECT
    toString(toUnixTimestamp(toStartOfDay(timestamp))) AS bucket,
    toString(countIf(type IN ('%s', '%s'))) AS deploys
FROM %s.%s
WHERE %s
GROUP BY bucket
FORMAT JSONEachRow`,
		EventReleasePromoted, EventReleaseRolledBack,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(EventsTable), strings.Join(conditions, " AND "))

	rows, err := c.aggregateRows(ctx, statement, params)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if i, ok := bucketIndex(row.Bucket, dayStart, 24*time.Hour, dailyBuckets); ok {
			overview.DeploysPerDay[i] = row.uint("deploys")
			overview.Deploys7d += row.uint("deploys")
		}
	}

	// The median is over the whole window, not per day — days are only
	// buckets for the deploy sparkline, and a median of daily medians would
	// be a different (wrong) number. So the builds are asked once, whole.
	medianConditions := append([]string{fmt.Sprintf("type = '%s'", EventBuildSucceeded), "value > 0"}, conditions...)
	statement = fmt.Sprintf("SELECT quantile(0.5)(value) FROM %s.%s WHERE %s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(EventsTable), strings.Join(medianConditions, " AND "))
	answer, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return err
	}
	if median, err := strconv.ParseFloat(strings.TrimSpace(answer), 64); err == nil {
		overview.MedianBuildSeconds = median
	}
	return nil
}

func (c *Client) logMetrics(ctx context.Context, query MetricsQuery, hourStart time.Time, overview *MetricsOverview) error {
	conditions, params := windowConditionsOn(timeColumnLogs, hourStart)
	if query.Project != "" {
		conditions = append(conditions, "project = {project:String}")
		params["project"] = query.Project
	}

	statement := fmt.Sprintf(`SELECT
    toString(toUnixTimestamp(toStartOfHour(Timestamp))) AS bucket,
    toString(count()) AS lines
FROM %s.%s
WHERE %s
GROUP BY bucket
FORMAT JSONEachRow`,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable), strings.Join(conditions, " AND "))

	rows, err := c.aggregateRows(ctx, statement, params)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if i, ok := bucketIndex(row.Bucket, hourStart, time.Hour, hourlyBuckets); ok {
			overview.LogLinesPerHour[i] = row.uint("lines")
			overview.LogLines24h += row.uint("lines")
		}
	}
	return nil
}

// StoreStats reads the store's own size and ingest rate, and nothing else.
//
// It is its own read rather than a field of the overview because two callers
// want only these two numbers and want them often: the signals gatherer takes
// the store's health on every `/platform/signals` and every environment's
// diagnostics strip, and the Storage screen shows the same figures. Asking
// MetricsOverview for them made each of those pay for a day of GROUP BYs over
// the logs and the events as well — work whose answer was thrown away.
//
// MetricsOverview reads it too, so the size the dashboard shows and the size
// `store.disk` fires on are one query and can never disagree.
//
// The two statements are the store's two vantage points: system.parts knows
// what is on disk, and counting the last five minutes of writes knows whether
// anything is still arriving. The logs are counted on the exporter's column
// name and the two tables the operator writes on their own; see timeColumnLogs.
func (c *Client) StoreStats(ctx context.Context) (StoreStats, error) {
	var stats StoreStats
	answer, err := c.Query(ctx, fmt.Sprintf(
		"SELECT toString(sum(bytes_on_disk)) FROM system.parts WHERE database = %s AND active",
		quoteLiteral(c.cfg.Database)))
	if err != nil {
		return stats, err
	}
	stats.BytesOnDisk, _ = strconv.ParseUint(strings.TrimSpace(answer), 10, 64)

	db := quoteIdentifier(c.cfg.Database)
	answer, err = c.Query(ctx, fmt.Sprintf(`SELECT toString(
    (SELECT count() FROM %s.%s WHERE %s >= now() - INTERVAL 5 MINUTE)
  + (SELECT count() FROM %s.%s WHERE %s >= now() - INTERVAL 5 MINUTE)
  + (SELECT count() FROM %s.%s WHERE %s >= now() - INTERVAL 5 MINUTE))`,
		db, quoteIdentifier(LogsTable), timeColumnLogs,
		db, quoteIdentifier(FlowsTable), timeColumnKitchen,
		db, quoteIdentifier(EventsTable), timeColumnKitchen))
	if err != nil {
		return stats, err
	}
	rows, _ := strconv.ParseUint(strings.TrimSpace(answer), 10, 64)
	stats.RowsPerSecond = float64(rows) / storeIngestWindowSeconds
	return stats, nil
}

// storeIngestWindowSeconds is the five minutes of writes the ingest rate is
// averaged over, in the unit the rate is reported in.
const storeIngestWindowSeconds = 5 * 60

// aggregateRow is one JSONEachRow answer of an aggregation: a bucket key plus
// whatever numeric columns the query selected, kept as raw strings because
// ClickHouse renders UInt64 that way.
type aggregateRow struct {
	Bucket string
	fields map[string]any
}

func (r aggregateRow) string(name string) string {
	value, _ := r.fields[name].(string)
	return value
}

func (r aggregateRow) uint(name string) uint64 {
	value, _ := strconv.ParseUint(r.string(name), 10, 64)
	return value
}

func (c *Client) aggregateRows(ctx context.Context, statement string, params map[string]string) ([]aggregateRow, error) {
	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}
	rows := []aggregateRow{}
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		fields := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return nil, fmt.Errorf("unreadable metrics row: %w", err)
		}
		bucket, _ := fields["bucket"].(string)
		rows = append(rows, aggregateRow{Bucket: bucket, fields: fields})
	}
	return rows, nil
}

// windowConditions bounds a query to [since, now] over a table the operator
// writes itself.
func windowConditions(since time.Time) ([]string, map[string]string) {
	return windowConditionsOn(timeColumnKitchen, since)
}

// windowConditionsOn is windowConditions for a table whose time column the
// exporter named. The column is one this package chose, never one that came
// from a request, so it goes into the statement as written.
func windowConditionsOn(column string, since time.Time) ([]string, map[string]string) {
	return []string{fmt.Sprintf(
			"%s >= parseDateTime64BestEffort({metricsSince:String}, 3, 'UTC')", column)},
		map[string]string{"metricsSince": since.UTC().Format(time.RFC3339Nano)}
}

// bucketIndex places a unix-seconds bucket key into a fixed series starting at
// `start` with `width` buckets of `size`.
func bucketIndex(bucket string, start time.Time, size time.Duration, width int) (int, bool) {
	unix, err := strconv.ParseInt(bucket, 10, 64)
	if err != nil {
		return 0, false
	}
	index := int((unix - start.Unix()) / int64(size/time.Second))
	if index < 0 || index >= width {
		return 0, false
	}
	return index, true
}

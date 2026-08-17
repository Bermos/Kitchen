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
// over what the store already holds. Requests, error rates and p95 come from
// the flows the Hubble collector ships; deploys and build times from the
// activity feed's events; log volume from the logs themselves; and the store's
// own size from ClickHouse's system tables. Where a source is not collected —
// no flow collector configured, say — its numbers are zero and the UI says so,
// rather than the platform pretending to measure something it does not.

// MetricsQuery scopes the overview. The zero value is the whole platform.
type MetricsQuery struct {
	// Project narrows logs and events to one project.
	Project string
	// Namespace narrows flows to traffic into one namespace — the caller
	// maps a project to its app namespace, which this package does not know.
	Namespace string
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

	// HTTP traffic over the last 24 hours, from the flow pipeline. All zero
	// when no flow collector is configured.
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

	// Namespaces is inbound traffic per destination namespace, filled only
	// on the cluster-wide question — the caller joins namespaces back to
	// projects, which is its vocabulary rather than the store's.
	Namespaces []NamespaceTraffic `json:"namespaces,omitempty"`
}

// NamespaceTraffic is 24 hours of inbound HTTP traffic for one namespace.
type NamespaceTraffic struct {
	Namespace       string   `json:"namespace"`
	Requests24h     uint64   `json:"requests24h"`
	Errors5xx24h    uint64   `json:"errors5xx24h"`
	P95Ms           float64  `json:"p95Ms"`
	RequestsPerHour []uint64 `json:"requestsPerHour"`
}

const (
	hourlyBuckets = 24
	dailyBuckets  = 7
)

// MetricsOverview aggregates the dashboard's numbers out of the telemetry
// tables. Each source is one bounded GROUP BY; a store that has never seen a
// flow or an event simply contributes zeroes.
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
	if err := c.trafficMetrics(ctx, query, hourStart, &overview); err != nil {
		return overview, err
	}
	if err := c.logMetrics(ctx, query, hourStart, &overview); err != nil {
		return overview, err
	}
	if query.Project == "" && query.Namespace == "" {
		if err := c.namespaceMetrics(ctx, hourStart, &overview); err != nil {
			return overview, err
		}
	}
	return overview, c.storeMetrics(ctx, &overview)
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

func (c *Client) trafficMetrics(ctx context.Context, query MetricsQuery, hourStart time.Time, overview *MetricsOverview) error {
	conditions, params := windowConditions(hourStart)
	if query.Namespace != "" {
		conditions = append(conditions, "destinationNamespace = {namespace:String}")
		params["namespace"] = query.Namespace
	}

	statement := fmt.Sprintf(`SELECT
    toString(toUnixTimestamp(toStartOfHour(timestamp))) AS bucket,
    toString(countIf(protocol = 'HTTP')) AS requests,
    toString(countIf(httpStatus >= 500)) AS errors,
    quantileIf(0.95)(latencyMs, protocol = 'HTTP' AND latencyMs > 0) AS p95
FROM %s.%s
WHERE %s
GROUP BY bucket
FORMAT JSONEachRow`,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(FlowsTable), strings.Join(conditions, " AND "))

	rows, err := c.aggregateRows(ctx, statement, params)
	if err != nil {
		return err
	}

	var errors uint64
	var p95Sum float64
	var p95Hours int
	for _, row := range rows {
		if i, ok := bucketIndex(row.Bucket, hourStart, time.Hour, hourlyBuckets); ok {
			overview.RequestsPerHour[i] = row.uint("requests")
			overview.ErrorsPerHour[i] = row.uint("errors")
			overview.P95MsPerHour[i] = row.p95()
			overview.Requests24h += row.uint("requests")
			errors += row.uint("errors")
			if row.p95() > 0 {
				p95Sum += row.p95()
				p95Hours++
			}
		}
	}
	if overview.Requests24h > 0 {
		overview.ErrorRate24h = float64(errors) / float64(overview.Requests24h)
	}
	if p95Hours > 0 {
		// The mean of the hourly p95s, which is an approximation and cheap;
		// the per-hour series is right there for anyone who wants the shape.
		overview.P95Ms24h = p95Sum / float64(p95Hours)
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

func (c *Client) namespaceMetrics(ctx context.Context, hourStart time.Time, overview *MetricsOverview) error {
	conditions, params := windowConditions(hourStart)
	statement := fmt.Sprintf(`SELECT
    destinationNamespace AS bucket,
    toString(toUnixTimestamp(toStartOfHour(timestamp))) AS hour,
    toString(countIf(protocol = 'HTTP')) AS requests,
    toString(countIf(httpStatus >= 500)) AS errors,
    quantileIf(0.95)(latencyMs, protocol = 'HTTP' AND latencyMs > 0) AS p95
FROM %s.%s
WHERE %s AND destinationNamespace != ''
GROUP BY bucket, hour
FORMAT JSONEachRow`,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(FlowsTable), strings.Join(conditions, " AND "))

	rows, err := c.aggregateRows(ctx, statement, params)
	if err != nil {
		return err
	}

	byNamespace := map[string]*NamespaceTraffic{}
	p95Sum := map[string]float64{}
	p95Hours := map[string]int{}
	for _, row := range rows {
		entry := byNamespace[row.Bucket]
		if entry == nil {
			entry = &NamespaceTraffic{Namespace: row.Bucket, RequestsPerHour: make([]uint64, hourlyBuckets)}
			byNamespace[row.Bucket] = entry
		}
		entry.Requests24h += row.uint("requests")
		entry.Errors5xx24h += row.uint("errors")
		if i, ok := bucketIndex(row.string("hour"), hourStart, time.Hour, hourlyBuckets); ok {
			entry.RequestsPerHour[i] = row.uint("requests")
		}
		if row.p95() > 0 {
			p95Sum[row.Bucket] += row.p95()
			p95Hours[row.Bucket]++
		}
	}
	for namespace, entry := range byNamespace {
		if hours := p95Hours[namespace]; hours > 0 {
			entry.P95Ms = p95Sum[namespace] / float64(hours)
		}
		overview.Namespaces = append(overview.Namespaces, *entry)
	}
	return nil
}

func (c *Client) storeMetrics(ctx context.Context, overview *MetricsOverview) error {
	answer, err := c.Query(ctx, fmt.Sprintf(
		"SELECT toString(sum(bytes_on_disk)) FROM system.parts WHERE database = %s AND active",
		quoteLiteral(c.cfg.Database)))
	if err != nil {
		return err
	}
	overview.StoreBytes, _ = strconv.ParseUint(strings.TrimSpace(answer), 10, 64)

	// The logs are counted on the exporter's column name and the two tables
	// the operator writes on their own; see timeColumnLogs.
	db := quoteIdentifier(c.cfg.Database)
	answer, err = c.Query(ctx, fmt.Sprintf(`SELECT toString(
    (SELECT count() FROM %s.%s WHERE %s >= now() - INTERVAL 5 MINUTE)
  + (SELECT count() FROM %s.%s WHERE %s >= now() - INTERVAL 5 MINUTE)
  + (SELECT count() FROM %s.%s WHERE %s >= now() - INTERVAL 5 MINUTE))`,
		db, quoteIdentifier(LogsTable), timeColumnLogs,
		db, quoteIdentifier(FlowsTable), timeColumnKitchen,
		db, quoteIdentifier(EventsTable), timeColumnKitchen))
	if err != nil {
		return err
	}
	rows, _ := strconv.ParseUint(strings.TrimSpace(answer), 10, 64)
	overview.StoreRowsPerSecond = float64(rows) / (5 * 60)
	return nil
}

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

// p95 reads the one float column the aggregations select. ClickHouse renders
// a quantile over no rows as null, which reads back as 0 here.
func (r aggregateRow) p95() float64 {
	switch value := r.fields["p95"].(type) {
	case float64:
		return value
	case string:
		parsed, _ := strconv.ParseFloat(value, 64)
		return parsed
	}
	return 0
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

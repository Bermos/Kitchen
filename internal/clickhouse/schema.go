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
	"fmt"
	"regexp"
	"strconv"
)

// LogsTable holds every log line Kitchen collects: application containers,
// build jobs and the platform's own components. The collectors write it and
// the operator API reads it, so the name is a contract between the chart's
// Vector configuration and this package.
const LogsTable = "logs"

// EventsTable holds the platform's own activity: releases moving, builds
// finishing, previews coming and going. The reconcilers and the API write it;
// the dashboard's activity feed reads it.
const EventsTable = "events"

// FlowsTable holds network flow observations from Hubble, one row per flow the
// collector saw. The traffic view's service map is aggregated out of it.
const FlowsTable = "flows"

// MetricsTable holds resource telemetry: one row per container per sample, as
// the operator's usage collector reads it off the kubelet and off the pods
// themselves. It is what turns "what is this environment using" from an
// instant into a history.
//
// MetricsRollupTable is the same data pre-aggregated into five-minute buckets
// by MetricsRollupView, and MetricsRollupSeconds is that bucket width. A
// window wider than a few hours is answered from the rollup: the raw table
// stays for the resolutions a rollup cannot serve, and both live under the one
// retention.
const (
	MetricsTable         = "metrics"
	MetricsRollupTable   = "metrics_5m"
	MetricsRollupView    = "metrics_5m_mv"
	MetricsRollupSeconds = 300
)

// TracesTable holds spans, one row each, as instrumented applications send
// them to the platform's OTLP receiver. Hubble sees who talked to whom; only
// the application knows what it was doing, which is why nothing here is
// derived from the flow data.
const TracesTable = "traces"

// Log sources, as written into the `source` column by the collector's
// transform in charts/kitchen/templates/logs/configmap.yaml.
//
// The collector tails every container on the node, so the distinction that
// matters is whose the line is: SourcePlatform is Kitchen's own components,
// running in the platform namespace, and SourceCluster is everything else the
// cluster runs. They were one value until the platform facet turned out to be
// answering with Cilium and cert-manager.
const (
	SourceRuntime  = "runtime"
	SourceBuild    = "build"
	SourcePlatform = "platform"
	SourceCluster  = "cluster"
)

// ttlIntervalPattern pulls the retention out of a table's DDL. ClickHouse
// normalizes `INTERVAL 30 DAY` to `toIntervalDay(30)`, whichever form it was
// created with.
var ttlIntervalPattern = regexp.MustCompile(`toIntervalDay\((\d+)\)`)

// EnsureTelemetrySchema creates every telemetry table — logs, events, flows,
// metrics and traces — and keeps their TTLs in step with the retention
// configured on the Kitchen object. One retention knob covers all of them:
// they are facets of the same telemetry store, and a second knob would be a
// second thing to explain.
func (c *Client) EnsureTelemetrySchema(ctx context.Context, retentionDays int32) error {
	if err := c.EnsureLogsSchema(ctx, retentionDays); err != nil {
		return err
	}
	if err := c.EnsureEventsSchema(ctx, retentionDays); err != nil {
		return err
	}
	if err := c.EnsureFlowsSchema(ctx, retentionDays); err != nil {
		return err
	}
	if err := c.EnsureMetricsSchema(ctx, retentionDays); err != nil {
		return err
	}
	return c.EnsureTracesSchema(ctx, retentionDays)
}

// EnsureLogsSchema creates the log table if it is missing and keeps its TTL in
// step with the retention configured on the Kitchen object. It is safe to run
// on every reconcile: the DDL is idempotent and the TTL is only altered when
// it actually differs.
func (c *Client) EnsureLogsSchema(ctx context.Context, retentionDays int32) error {
	if err := c.ensureTable(ctx, LogsTable, createLogsTable(c.cfg.Database, retentionDays), retentionDays); err != nil {
		return err
	}
	// A logs table from before a column learned it here. ClickHouse makes
	// these idempotent, so they are safe to run on every reconcile — and they
	// have to be run on every reconcile, because a table created by an older
	// Kitchen is not recreated by the CREATE above.
	for _, column := range []string{
		"level LowCardinality(String) AFTER stream",
		"fields Map(String, String) AFTER labels",
		"traceId String AFTER level",
		"spanId String AFTER traceId",
	} {
		if err := c.Exec(ctx, fmt.Sprintf("ALTER TABLE %s.%s ADD COLUMN IF NOT EXISTS %s",
			quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable), column)); err != nil {
			return err
		}
	}
	return nil
}

// EnsureEventsSchema creates the platform activity table.
func (c *Client) EnsureEventsSchema(ctx context.Context, retentionDays int32) error {
	return c.ensureTable(ctx, EventsTable, createEventsTable(c.cfg.Database, retentionDays), retentionDays)
}

// EnsureFlowsSchema creates the network flow table.
func (c *Client) EnsureFlowsSchema(ctx context.Context, retentionDays int32) error {
	return c.ensureTable(ctx, FlowsTable, createFlowsTable(c.cfg.Database, retentionDays), retentionDays)
}

// EnsureMetricsSchema creates the resource telemetry table, its five-minute
// rollup, and the materialized view that fills the rollup.
//
// The view is created last and unconditionally: it only ever sees rows
// inserted after it exists, so an installation that had the raw table before
// the rollup has a gap in the rollup rather than a wrong answer — and the
// reader falls back to the raw table for any window the rollup cannot serve
// anyway.
func (c *Client) EnsureMetricsSchema(ctx context.Context, retentionDays int32) error {
	if err := c.ensureTable(ctx, MetricsTable,
		createMetricsTable(c.cfg.Database, retentionDays), retentionDays); err != nil {
		return err
	}
	if err := c.ensureTableTTL(ctx, MetricsRollupTable,
		createMetricsRollupTable(c.cfg.Database, retentionDays), "bucket", retentionDays); err != nil {
		return err
	}
	return c.Exec(ctx, createMetricsRollupView(c.cfg.Database))
}

// EnsureTracesSchema creates the span table.
func (c *Client) EnsureTracesSchema(ctx context.Context, retentionDays int32) error {
	return c.ensureTable(ctx, TracesTable, createTracesTable(c.cfg.Database, retentionDays), retentionDays)
}

// ensureTable runs the idempotent CREATE and then brings the TTL in line,
// altering only when the enforced retention actually differs.
func (c *Client) ensureTable(ctx context.Context, table, ddl string, retentionDays int32) error {
	return c.ensureTableTTL(ctx, table, ddl, "timestamp", retentionDays)
}

// ensureTableTTL is ensureTable for a table whose time column is not called
// `timestamp` — the rollup buckets by `bucket`, and a MODIFY TTL naming the
// wrong column is refused rather than ignored.
func (c *Client) ensureTableTTL(ctx context.Context, table, ddl, column string, retentionDays int32) error {
	if retentionDays < 1 {
		return fmt.Errorf("retentionDays must be at least 1, got %d", retentionDays)
	}

	db := quoteIdentifier(c.cfg.Database)
	if err := c.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+db); err != nil {
		return err
	}
	if err := c.Exec(ctx, ddl); err != nil {
		return err
	}

	current, err := c.tableRetentionDays(ctx, table)
	if err != nil {
		return err
	}
	if current == retentionDays {
		return nil
	}
	return c.Exec(ctx, fmt.Sprintf("ALTER TABLE %s.%s MODIFY TTL %s",
		db, quoteIdentifier(table), ttlExpressionOn(column, retentionDays)))
}

// tableRetentionDays reads back the retention ClickHouse is enforcing. A table
// without a TTL — one created by an older version of Kitchen, or edited by
// hand — reports 0, which never matches and so gets the TTL applied.
func (c *Client) tableRetentionDays(ctx context.Context, table string) (int32, error) {
	query := fmt.Sprintf(
		"SELECT engine_full FROM system.tables WHERE database = %s AND name = %s",
		quoteLiteral(c.cfg.Database), quoteLiteral(table))
	engine, err := c.Query(ctx, query)
	if err != nil {
		return 0, err
	}
	match := ttlIntervalPattern.FindStringSubmatch(engine)
	if match == nil {
		return 0, nil
	}
	days, err := strconv.ParseInt(match[1], 10, 32)
	if err != nil {
		return 0, nil
	}
	return int32(days), nil
}

func ttlExpression(retentionDays int32) string {
	return ttlExpressionOn("timestamp", retentionDays)
}

// ttlExpressionOn names the column the retention is measured from. It is a
// name this package chose, never one that came from configuration, so it goes
// into the statement as written.
func ttlExpressionOn(column string, retentionDays int32) string {
	return fmt.Sprintf("toDateTime(%s) + toIntervalDay(%d)", column, retentionDays)
}

// createLogsTable is the log schema, wide enough for the queries the UI and
// the operator API need: "this build's output" and "this project's runtime
// logs in this environment, over this window".
//
// The ordering key is what those queries filter on. `build` gets a set index
// rather than a place in the key: it is empty for every runtime line, so it
// would only dilute the primary index. `message` gets a token filter so that
// free-text search does not have to read every granule.
//
// `labels` are the pod's Kubernetes labels; `fields` are the line's own, from
// a JSON log the collector flattened. They are two maps rather than one
// because they answer different questions and a collision between a pod label
// and an application field would be silent. A missing key of either reads as
// the empty string, which is what makes `http.status:*` one test rather than
// two.
//
// `traceId` is lifted out of those fields into a column of its own, with a
// bloom filter over it, because it is the one field asked for by exact value
// from outside the log page — a span in the trace view offering its lines, a
// line offering its trace.
func createLogsTable(database string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    timestamp   DateTime64(3, 'UTC'),
    source      LowCardinality(String),
    project     LowCardinality(String),
    environment LowCardinality(String),
    build       LowCardinality(String),
    namespace   LowCardinality(String),
    pod         String,
    container   LowCardinality(String),
    node        LowCardinality(String),
    stream      LowCardinality(String),
    level       LowCardinality(String),
    traceId     String,
    spanId      String,
    message     String,
    labels      Map(LowCardinality(String), String),
    fields      Map(String, String),
    INDEX idx_build build TYPE set(0) GRANULARITY 4,
    INDEX idx_trace traceId TYPE bloom_filter GRANULARITY 4,
    INDEX idx_message message TYPE tokenbf_v1(32768, 3, 0) GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (project, environment, timestamp)
TTL %s`, quoteIdentifier(database), quoteIdentifier(LogsTable), ttlExpression(retentionDays))
}

// createEventsTable is the activity feed's schema: one row per thing that
// happened on the platform. The object columns (build, environment, release,
// claim) name what the entry is about, so a feed entry can link to it; `value`
// carries the one number some events have (a build's duration in seconds).
//
// The feed is read newest-first and per-project, which is what the ordering
// key serves. Volume is human-scale — deploys and builds, not requests — so
// no indexes beyond the key are worth their upkeep.
func createEventsTable(database string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    timestamp   DateTime64(3, 'UTC'),
    type        LowCardinality(String),
    project     LowCardinality(String),
    environment LowCardinality(String),
    build       LowCardinality(String),
    release     LowCardinality(String),
    claim       LowCardinality(String),
    message     String,
    actor       LowCardinality(String),
    value       Float64
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (timestamp)
TTL %s`, quoteIdentifier(database), quoteIdentifier(EventsTable), ttlExpression(retentionDays))
}

// createMetricsTable is the resource telemetry schema: one row per container
// per sample. The ordering key is what the environment page filters on, which
// is also what the rollup groups by.
//
// `restarts` is the container's lifetime count as Kubernetes reports it;
// `restarted` and `oomKilled` are what happened *since the previous sample*,
// which is the difference between a number that only ever climbs and a series
// with a spike on it. A cumulative counter cannot be bucketed without losing
// every transition that lands on a bucket boundary, so the collector — which
// knows what it saw last time — does the differencing and the store keeps
// events.
//
// The limits ride on every row rather than living in a table of their own:
// they are what makes a usage number mean anything, they change only when a
// release does, and LowCardinality columns of a repeated value cost close to
// nothing.
func createMetricsTable(database string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    timestamp        DateTime64(3, 'UTC'),
    project          LowCardinality(String),
    environment      LowCardinality(String),
    namespace        LowCardinality(String),
    pod              String,
    container        LowCardinality(String),
    node             LowCardinality(String),
    cpuCores         Float64,
    memoryBytes      UInt64,
    cpuLimitCores    Float64,
    memoryLimitBytes UInt64,
    restarts         UInt32,
    restarted        UInt16,
    oomKilled        UInt8
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (project, environment, timestamp)
TTL %s`, quoteIdentifier(database), quoteIdentifier(MetricsTable), ttlExpression(retentionDays))
}

// createMetricsRollupTable is the same data at five-minute resolution, kept as
// aggregate states so that merging two buckets is exact rather than an average
// of averages of different sample counts.
//
// It exists because the raw table is one row per container per sample: fine to
// scan for the last hour, and a week of it for a busy platform is the read
// that makes the environment page feel broken. The ordering key leads with the
// same columns as the raw table so the same filter hits the primary index.
func createMetricsRollupTable(database string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    bucket           DateTime('UTC'),
    project          LowCardinality(String),
    environment      LowCardinality(String),
    namespace        LowCardinality(String),
    pod              String,
    container        LowCardinality(String),
    cpuCores         AggregateFunction(avg, Float64),
    cpuPeakCores     AggregateFunction(max, Float64),
    memoryBytes      AggregateFunction(avg, UInt64),
    memoryPeakBytes  AggregateFunction(max, UInt64),
    cpuLimitCores    AggregateFunction(max, Float64),
    memoryLimitBytes AggregateFunction(max, UInt64),
    restarted        AggregateFunction(sum, UInt16),
    oomKills         AggregateFunction(sum, UInt8)
)
ENGINE = AggregatingMergeTree
PARTITION BY toDate(bucket)
ORDER BY (project, environment, bucket, namespace, pod, container)
TTL %s`, quoteIdentifier(database), quoteIdentifier(MetricsRollupTable),
		ttlExpressionOn("bucket", retentionDays))
}

// createMetricsRollupView fills the rollup as rows arrive.
//
// Every column is read through the source table's alias. ClickHouse resolves a
// bare name against the SELECT list's own aliases first, so `maxState(cpuCores)`
// after `avgState(cpuCores) AS cpuCores` takes the max of an aggregate state
// and the view is refused outright ("values of that data type are not
// comparable"). Qualifying every argument is what keeps the state columns
// nameable after the columns they aggregate. See timestampAlias in logs.go and
// protocolAlias in flows.go for the same trap where it merely returns the wrong
// answer.
func createMetricsRollupView(database string) string {
	return fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s.%s TO %s.%s AS
SELECT
    toDateTime(toStartOfInterval(m.timestamp, toIntervalSecond(%d)), 'UTC') AS bucket,
    m.project AS project,
    m.environment AS environment,
    m.namespace AS namespace,
    m.pod AS pod,
    m.container AS container,
    avgState(m.cpuCores) AS cpuCores,
    maxState(m.cpuCores) AS cpuPeakCores,
    avgState(m.memoryBytes) AS memoryBytes,
    maxState(m.memoryBytes) AS memoryPeakBytes,
    maxState(m.cpuLimitCores) AS cpuLimitCores,
    maxState(m.memoryLimitBytes) AS memoryLimitBytes,
    sumState(m.restarted) AS restarted,
    sumState(m.oomKilled) AS oomKills
FROM %s.%s AS m
GROUP BY bucket, project, environment, namespace, pod, container`,
		quoteIdentifier(database), quoteIdentifier(MetricsRollupView),
		quoteIdentifier(database), quoteIdentifier(MetricsRollupTable),
		MetricsRollupSeconds,
		quoteIdentifier(database), quoteIdentifier(MetricsTable))
}

// createTracesTable is the span schema: one row per span an instrumented
// application sent to the OTLP receiver.
//
// Traces are read two ways and the indexes are one for each: "what has this
// service been doing lately", which the ordering key serves, and "give me
// every span of this one trace", which arrives as an id out of a log line and
// would otherwise scan the window. `attributes` are the span's own and
// `resource` the emitting process's — two maps rather than one for the same
// reason the log table keeps `labels` and `fields` apart.
func createTracesTable(database string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    timestamp     DateTime64(6, 'UTC'),
    traceId       String,
    spanId        String,
    parentSpanId  String,
    name          LowCardinality(String),
    kind          LowCardinality(String),
    service       LowCardinality(String),
    project       LowCardinality(String),
    environment   LowCardinality(String),
    durationMs    Float64,
    statusCode    LowCardinality(String),
    statusMessage String,
    httpStatus    UInt16,
    attributes    Map(String, String),
    resource      Map(LowCardinality(String), String),
    INDEX idx_trace traceId TYPE bloom_filter GRANULARITY 4,
    INDEX idx_name name TYPE set(0) GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (service, timestamp)
TTL %s`, quoteIdentifier(database), quoteIdentifier(TracesTable), ttlExpression(retentionDays))
}

// createFlowsTable is the traffic schema: one row per flow observation the
// Hubble collector shipped. L7 rows (protocol = 'HTTP') carry a status and a
// latency; L3/4 rows carry only the verdict. The service map aggregates over
// (source, destination), which is what the ordering key serves.
func createFlowsTable(database string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    timestamp            DateTime64(3, 'UTC'),
    source               LowCardinality(String),
    sourceNamespace      LowCardinality(String),
    destination          LowCardinality(String),
    destinationNamespace LowCardinality(String),
    protocol             LowCardinality(String),
    verdict              LowCardinality(String),
    httpStatus           UInt16,
    latencyMs            Float64
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (sourceNamespace, source, destinationNamespace, destination, timestamp)
TTL %s`, quoteIdentifier(database), quoteIdentifier(FlowsTable), ttlExpression(retentionDays))
}

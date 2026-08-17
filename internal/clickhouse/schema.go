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
	"math"
	"regexp"
	"strconv"
)

// The telemetry schema, and who writes what into it.
//
// Logs, traces and metrics are written by the collector's stock
// `clickhouseexporter` and read by this package. The operator still owns the
// DDL — the exporter runs with `create_schema: false` — because every query
// Kitchen makes is scoped to a project and upstream's ordering keys are not.
// That trade has a price: **the base columns below are transcribed from a
// specific exporter version** (contrib `main`, post-v0.158.0), and an exporter
// whose INSERT column list grows a column this DDL lacks fails every insert.
// The collector's image tag is pinned in the chart for that reason; moving it
// means re-reading the exporter's templates.
//
// What Kitchen adds is only ever `MATERIALIZED`: computed at insert, absent
// from every INSERT column list, and therefore invisible to the exporter. That
// is what lets a stock exporter write a table with a Kitchen-shaped ordering
// key.
//
// Flows and events have no collector and no upstream shape; the operator
// writes them itself, and they are unchanged.

// LogsTable holds every log line Kitchen collects: application containers,
// build jobs and the platform's own components. The collector writes it and
// the operator API reads it.
const LogsTable = "otel_logs"

// EventsTable holds the platform's own activity: releases moving, builds
// finishing, previews coming and going. The reconcilers and the API write it;
// the dashboard's activity feed reads it.
const EventsTable = "events"

// FlowsTable holds network flow observations from Hubble, one row per flow the
// collector saw. The traffic view's service map is aggregated out of it.
const FlowsTable = "flows"

// RequestsTable holds one row per HTTP request the platform's edge observed,
// and the two rollups are what the golden-signal charts are actually read
// from: a request rate over a week is millions of raw rows and a few thousand
// buckets.
//
// Both views read the raw table. Chaining the hourly one off the minute
// rollup would look like the obvious saving and is the bug the design names:
// a view over aggregate states re-aggregates states, and the column names
// that hold them are the same names as the columns they aggregate — see
// createMetricsRollupGaugeView for what that shadowing costs.
//
// Requests are modelled apart from flows deliberately. Flows are edges of the
// service map, keyed on who talked to whom; a request is keyed on the host it
// asked for, which is the only attribution that survives a protected preview
// (where the destination is the gate) or an idling environment (where it is
// the interceptor).
const (
	RequestsTable       = "http_requests"
	RequestsMinuteTable = "http_requests_1m"
	RequestsHourTable   = "http_requests_1h"
	RequestsMinuteView  = "http_requests_1m_mv"
	RequestsHourView    = "http_requests_1h_mv"
)

// K8sEventsTable holds the cluster's Warning events, recorded by the operator
// from the same watch the component survey reads one at a time. It is what
// turns "what happened at 03:00" from an hour-lived mystery — a Kubernetes
// event outlives its object by an hour — into a question with an answer.
//
// It is not EventsTable: that is the platform's own story (releases, builds,
// previews), this is the cluster's, and a feed that mixed them would serve
// neither.
const K8sEventsTable = "k8s_events"

// TracesTable holds spans, one row each, as the collector receives them over
// OTLP from instrumented applications.
//
// TracesIDLookupTable is (TraceId, Start, End), filled by TracesIDLookupView,
// and it is not optional. The ordering key leads with the project, so looking a
// trace up by an id that arrived out of a log line — the one link that makes
// tracing worth collecting — would otherwise scan the whole retention. The
// lookup answers "when did this trace happen" in one point read, and the span
// query is bounded by it.
const (
	TracesTable         = "otel_traces"
	TracesIDLookupTable = "otel_traces_trace_id_ts"
	TracesIDLookupView  = "otel_traces_trace_id_ts_mv"
)

// The metric tables, one per OTLP point type, as the exporter names them.
//
// The exponential histogram is the one to be careful with: upstream's README
// calls it `otel_metrics_exp_histogram` and is stale — the code's default is
// the name below, and the chart sets it explicitly so the ambiguity cannot
// decide itself.
const (
	MetricsGaugeTable                = "otel_metrics_gauge"
	MetricsSumTable                  = "otel_metrics_sum"
	MetricsHistogramTable            = "otel_metrics_histogram"
	MetricsExponentialHistogramTable = "otel_metrics_exponential_histogram"
	MetricsSummaryTable              = "otel_metrics_summary"
)

// MetricsRollupTable is the environment page's usage pre-aggregated into
// five-minute buckets, and MetricsRollupSeconds is that bucket width. A window
// wider than a few hours is answered from it: the OTel tables stay for the
// resolutions a rollup cannot serve, and both live under the one retention.
//
// It is filled by two views rather than one because its inputs are two tables —
// usage and limits are gauges, restarts and OOM kills are delta sums — and a
// materialized view reads exactly one. Both write the same key, and
// AggregatingMergeTree merges the halves; each supplies only the columns it
// knows and the rest default to empty states, which merge as zero.
//
// The table's own shape is deliberately unchanged from the version that was
// fed by Kitchen's old `metrics` table: `CREATE TABLE IF NOT EXISTS` will not
// reshape an existing one, so an installation that upgrades keeps its history
// and simply starts being fed from the new source.
const (
	MetricsRollupTable     = "metrics_5m"
	MetricsRollupGaugeView = "metrics_5m_gauge_mv"
	MetricsRollupSumView   = "metrics_5m_sum_mv"
	MetricsRollupSeconds   = 300
)

// The metrics the read path asks for by name.
//
// The two usage metrics are `kubeletstats`'s own, at container resolution:
// `container.cpu.usage` is cores and `container.memory.working_set` is the
// bytes the old sampler meant by "memory", so the environment page's numbers
// keep meaning what they meant. The `kitchen.` six come from the operator's
// usage collector over OTLP — restarts, OOM kills, limits and replicas exist in
// no receiver, because they are API server state rather than anything the
// kubelet measures.
const (
	MetricContainerCPUUsage         = "container.cpu.usage"
	MetricContainerMemoryWorkingSet = "container.memory.working_set"

	MetricContainerRestarts      = "kitchen.container.restarts"
	MetricContainerRestartsDelta = "kitchen.container.restarts.delta"
	MetricContainerOOMKilled     = "kitchen.container.oom_killed"
	MetricContainerCPULimit      = "kitchen.container.cpu.limit"
	MetricContainerMemoryLimit   = "kitchen.container.memory.limit"
	MetricEnvironmentReplicas    = "kitchen.environment.replicas"
)

// Telemetry sources, as the collector sets `kitchen.source` and the `source`
// column below materializes it.
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

// The time column each table's retention is measured from. Upstream names
// them, not Kitchen: logs and traces stamp `Timestamp`, every metric table
// stamps `TimeUnix`, and the trace lookup keeps the trace's start.
const (
	timeColumnLogs    = "Timestamp"
	timeColumnMetrics = "TimeUnix"
	timeColumnLookup  = "Start"
	timeColumnRollup  = "bucket"
	// timeColumnKitchen is what the tables the operator writes itself use.
	timeColumnKitchen = "timestamp"
	// timeColumnRequests is the request table's, which the operator writes but
	// spells the way the OTel tables do: the requests screen reads it beside
	// the logs, and the design's verified DDL is this spelling.
	timeColumnRequests = "Timestamp"
)

// The retention every table but the raw requests one gets, and the two ratios
// §5 derives from that one knob.
//
// Raw request rows are the shortest-lived thing in the store because they are
// the densest: a week of them is what "show me the failing requests" needs,
// and the rollups carry the same window's shape for as long as anything else
// is retained. The hour rollup is kept a year's worth of retentions so a
// year-scale view has something to read. None of this is configurable — a
// second knob is a second thing to explain, and installations do not need to
// disagree about these ratios.
const (
	rawRequestRetentionDays = 7
	hourlyRequestRatio      = 12
)

// rawRequestRetention is the raw table's window: a week, or the whole
// retention where that is shorter, because retaining raw rows past everything
// else would be the one table that outlives the store's own knob.
func rawRequestRetention(retentionDays int32) int32 {
	return min(rawRequestRetentionDays, retentionDays)
}

// hourlyRequestRetention saturates rather than wrapping. The knob is an int32
// of days with no ceiling on it, and a retention that overflowed would come
// back negative — a TTL that expires every row the moment it is written.
func hourlyRequestRetention(retentionDays int32) int32 {
	if retentionDays > math.MaxInt32/hourlyRequestRatio {
		return math.MaxInt32
	}
	return retentionDays * hourlyRequestRatio
}

// ttlIntervalPattern pulls the retention out of a table's DDL. ClickHouse
// normalizes `INTERVAL 30 DAY` to `toIntervalDay(30)`, whichever form it was
// created with.
var ttlIntervalPattern = regexp.MustCompile(`toIntervalDay\((\d+)\)`)

// EnsureTelemetrySchema creates every telemetry table — logs, events, flows,
// requests, cluster events, metrics and traces — and keeps their TTLs in step
// with the retention configured on the Kitchen object. One retention knob
// covers all of them: they are facets of the same telemetry store, and a
// second knob would be a second thing to explain. The request tables are the
// one place that knob is scaled rather than applied, and the ratios are
// derived from it rather than configured beside it.
//
// Kitchen's pre-collector `logs`, `traces` and `metrics` tables are
// deliberately not dropped. They hold real history that this schema has no
// migration for, they age out on their own TTL, and an operator who wants the
// disk back before then can drop them by hand.
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
	if err := c.EnsureRequestsSchema(ctx, retentionDays); err != nil {
		return err
	}
	if err := c.EnsureK8sEventsSchema(ctx, retentionDays); err != nil {
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
	return c.ensureTableTTL(ctx, LogsTable,
		createLogsTable(c.cfg.Database, retentionDays), timeColumnLogs, retentionDays)
}

// EnsureEventsSchema creates the platform activity table.
func (c *Client) EnsureEventsSchema(ctx context.Context, retentionDays int32) error {
	return c.ensureTable(ctx, EventsTable, createEventsTable(c.cfg.Database, retentionDays), retentionDays)
}

// EnsureFlowsSchema creates the network flow table.
func (c *Client) EnsureFlowsSchema(ctx context.Context, retentionDays int32) error {
	return c.ensureTable(ctx, FlowsTable, createFlowsTable(c.cfg.Database, retentionDays), retentionDays)
}

// EnsureRequestsSchema creates the request table, its two rollups, and the
// views that fill them.
//
// The views are created last and unconditionally, for the reason
// EnsureMetricsSchema spells out: a view only ever sees rows inserted after it
// exists, so an installation that had the raw table first has a gap in the
// rollups rather than a wrong answer.
func (c *Client) EnsureRequestsSchema(ctx context.Context, retentionDays int32) error {
	raw := rawRequestRetention(retentionDays)
	if err := c.ensureTableTTL(ctx, RequestsTable,
		createRequestsTable(c.cfg.Database, raw), timeColumnRequests, raw); err != nil {
		return err
	}
	if err := c.ensureTableTTL(ctx, RequestsMinuteTable,
		createRequestsMinuteTable(c.cfg.Database, retentionDays), timeColumnRollup, retentionDays); err != nil {
		return err
	}
	hourly := hourlyRequestRetention(retentionDays)
	if err := c.ensureTableTTL(ctx, RequestsHourTable,
		createRequestsHourTable(c.cfg.Database, hourly), timeColumnRollup, hourly); err != nil {
		return err
	}
	if err := c.Exec(ctx, createRequestsMinuteView(c.cfg.Database)); err != nil {
		return err
	}
	return c.Exec(ctx, createRequestsHourView(c.cfg.Database))
}

// EnsureK8sEventsSchema creates the cluster's Warning-event history.
func (c *Client) EnsureK8sEventsSchema(ctx context.Context, retentionDays int32) error {
	return c.ensureTable(ctx, K8sEventsTable, createK8sEventsTable(c.cfg.Database, retentionDays), retentionDays)
}

// EnsureMetricsSchema creates the five OTel metric tables, the five-minute
// rollup, and the two materialized views that fill the rollup.
//
// The views are created last and unconditionally: they only ever see rows
// inserted after they exist, so an installation that had the metric tables
// before the rollup has a gap in the rollup rather than a wrong answer — and
// the reader falls back to the metric tables for any window the rollup cannot
// serve anyway.
func (c *Client) EnsureMetricsSchema(ctx context.Context, retentionDays int32) error {
	for table, ddl := range metricsTableDDL(c.cfg.Database, retentionDays) {
		if err := c.ensureTableTTL(ctx, table, ddl, timeColumnMetrics, retentionDays); err != nil {
			return err
		}
	}
	if err := c.ensureTableTTL(ctx, MetricsRollupTable,
		createMetricsRollupTable(c.cfg.Database, retentionDays), timeColumnRollup, retentionDays); err != nil {
		return err
	}
	if err := c.Exec(ctx, createMetricsRollupGaugeView(c.cfg.Database)); err != nil {
		return err
	}
	return c.Exec(ctx, createMetricsRollupSumView(c.cfg.Database))
}

// EnsureTracesSchema creates the span table, the trace-id lookup it is read by
// id through, and the view that fills the lookup.
func (c *Client) EnsureTracesSchema(ctx context.Context, retentionDays int32) error {
	if err := c.ensureTableTTL(ctx, TracesTable,
		createTracesTable(c.cfg.Database, retentionDays), timeColumnLogs, retentionDays); err != nil {
		return err
	}
	if err := c.ensureTableTTL(ctx, TracesIDLookupTable,
		createTracesIDLookupTable(c.cfg.Database, retentionDays), timeColumnLookup, retentionDays); err != nil {
		return err
	}
	return c.Exec(ctx, createTracesIDLookupView(c.cfg.Database))
}

// ensureTable runs the idempotent CREATE and then brings the TTL in line,
// altering only when the enforced retention actually differs.
func (c *Client) ensureTable(ctx context.Context, table, ddl string, retentionDays int32) error {
	return c.ensureTableTTL(ctx, table, ddl, timeColumnKitchen, retentionDays)
}

// ensureTableTTL is ensureTable for a table whose time column is not called
// `timestamp` — the OTel tables stamp `Timestamp` or `TimeUnix`, the rollup
// buckets by `bucket`, and a MODIFY TTL naming the wrong column is refused
// rather than ignored.
func (c *Client) ensureTableTTL(ctx context.Context, table, ddl, column string, retentionDays int32) error {
	if retentionDays < 1 {
		return fmt.Errorf("retentionDays must be at least 1, got %d", retentionDays)
	}

	db := quoteIdentifier(c.cfg.Database)
	// Deliberately not through Exec: see execOutsideDatabase for why the one
	// statement that creates the database cannot name it.
	if err := c.execOutsideDatabase(ctx, "CREATE DATABASE IF NOT EXISTS "+db); err != nil {
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
	return ttlExpressionOn(timeColumnKitchen, retentionDays)
}

// ttlExpressionOn names the column the retention is measured from. It is a
// name this package chose, never one that came from configuration, so it goes
// into the statement as written.
func ttlExpressionOn(column string, retentionDays int32) string {
	return fmt.Sprintf("toDateTime(%s) + toIntervalDay(%d)", column, retentionDays)
}

// kitchenColumns are what every OTel table gains, and the whole reason the
// operator owns this DDL rather than letting the exporter create its own.
//
// They are `MATERIALIZED`, which is load-bearing three times over: the value is
// computed at insert so the column can sit in the ordering key, it is not part
// of any INSERT column list so a stock exporter never learns they exist, and
// the resource attributes they read are set once by the collector for every
// signal it emits.
//
// `pod` is a plain String because a pod name is nearly unique per row and a
// LowCardinality dictionary of nearly-unique values costs more than it saves.
const kitchenColumns = `
    project     LowCardinality(String) MATERIALIZED ResourceAttributes['kitchen.project'] CODEC(ZSTD(1)),
    environment LowCardinality(String) MATERIALIZED ResourceAttributes['deployment.environment.name'] CODEC(ZSTD(1)),
    build       LowCardinality(String) MATERIALIZED ResourceAttributes['kitchen.build'] CODEC(ZSTD(1)),
    source      LowCardinality(String) MATERIALIZED ResourceAttributes['kitchen.source'] CODEC(ZSTD(1)),
    namespace   LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.namespace.name'] CODEC(ZSTD(1)),
    pod         String                 MATERIALIZED ResourceAttributes['k8s.pod.name'] CODEC(ZSTD(1)),
    container   LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.container.name'] CODEC(ZSTD(1)),
    node        LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.node.name'] CODEC(ZSTD(1)),`

// logStreamColumnDDL is the log table's own extra: stdout or stderr.
//
// It is separate from kitchenColumns because it reads the *record's*
// attributes rather than the resource's — the filelog receiver's container
// parser sets `log.iostream` per line, which is the only attribute it adds
// besides moving the message into the body. `MATERIALIZED` over LogAttributes
// behaves exactly as it does over ResourceAttributes: computed at insert, and
// absent from the exporter's INSERT column list.
//
// It is a column rather than a map lookup because it is part of Kitchen's query
// vocabulary (`stream:stderr`), one of the observability view's default facets,
// and the example in the raw-query box.
const logStreamColumnDDL = `
    stream      LowCardinality(String) MATERIALIZED LogAttributes['log.iostream'] CODEC(ZSTD(1)),`

// writeHeavySettings closes every OTel table and the request table.
// `ttl_only_drop_parts` is what makes expiry a part drop rather than a rewrite
// of every part that holds one stale row, which for tables written at these
// rates is the difference between retention costing nothing and costing a
// merge storm.
const writeHeavySettings = "SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1"

// createLogsTable is upstream's log schema plus kitchenColumns.
//
// The base columns, the codecs and the skipping indexes are transcribed from
// the exporter's own template and must stay that way — see the note at the top
// of this file. The `__otel_materialized_*` columns are upstream's, duplicating
// some of what kitchenColumns reads; they are kept because the exporter
// inspects the table at startup and the cost of a LowCardinality column holding
// one repeated value is close to nothing.
//
// Only three things are Kitchen's: the ordering key, which leads with the
// project because every query this package makes is project-scoped (upstream
// orders by `(toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp)`, which
// would make a project's logs a scan); the TTL; and kitchenColumns itself.
//
// `EventName` is optional upstream and detected once, by a `DESC TABLE` at
// collector startup. It is included, which means adding it later would need a
// collector restart rather than only a DDL change.
func createLogsTable(database string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    Timestamp DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TraceId String CODEC(ZSTD(1)),
    SpanId String CODEC(ZSTD(1)),
    TraceFlags UInt8,
    SeverityText LowCardinality(String) CODEC(ZSTD(1)),
    SeverityNumber UInt8,
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    Body String CODEC(ZSTD(1)),
    ResourceSchemaUrl LowCardinality(String) CODEC(ZSTD(1)),
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeSchemaUrl LowCardinality(String) CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion LowCardinality(String) CODEC(ZSTD(1)),
    ScopeAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    LogAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    EventName String CODEC(ZSTD(1)),
    "__otel_materialized_k8s.cluster.name" LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.cluster.name'] CODEC(ZSTD(1)),
    "__otel_materialized_k8s.container.name" LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.container.name'] CODEC(ZSTD(1)),
    "__otel_materialized_k8s.deployment.name" LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.deployment.name'] CODEC(ZSTD(1)),
    "__otel_materialized_k8s.namespace.name" LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.namespace.name'] CODEC(ZSTD(1)),
    "__otel_materialized_k8s.node.name" LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.node.name'] CODEC(ZSTD(1)),
    "__otel_materialized_k8s.pod.name" LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.pod.name'] CODEC(ZSTD(1)),
    "__otel_materialized_k8s.pod.uid" LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.pod.uid'] CODEC(ZSTD(1)),
    "__otel_materialized_deployment.environment.name" LowCardinality(String) MATERIALIZED ResourceAttributes['deployment.environment.name'] CODEC(ZSTD(1)),%s%s
    INDEX idx_trace_id TraceId TYPE bloom_filter(0.001) GRANULARITY 1,
    INDEX idx_res_attr_key mapKeys(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_res_attr_value mapValues(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_scope_attr_key mapKeys(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_scope_attr_value mapValues(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_log_attr_key mapKeys(LogAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_log_attr_value mapValues(LogAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_lower_body lower(Body) TYPE tokenbf_v1(32768, 3, 0) GRANULARITY 8
)
ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (project, environment, Timestamp)
TTL %s
%s`,
		quoteIdentifier(database), quoteIdentifier(LogsTable), kitchenColumns, logStreamColumnDDL,
		ttlExpressionOn(timeColumnLogs, retentionDays), writeHeavySettings)
}

// createTracesTable is upstream's span schema plus kitchenColumns, ordered the
// same way the logs are and for the same reason.
//
// `Duration` is nanoseconds and `StatusCode` is the string `Unset`/`Ok`/
// `Error`; a span's `Timestamp` is its *start*, not its end. The read path
// converts all three — see traces.go.
func createTracesTable(database string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    Timestamp DateTime64(9) CODEC(Delta, ZSTD(1)),
    TraceId String CODEC(ZSTD(1)),
    SpanId String CODEC(ZSTD(1)),
    ParentSpanId String CODEC(ZSTD(1)),
    TraceState String CODEC(ZSTD(1)),
    SpanName LowCardinality(String) CODEC(ZSTD(1)),
    SpanKind LowCardinality(String) CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion String CODEC(ZSTD(1)),
    SpanAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    Duration UInt64 CODEC(ZSTD(1)),
    StatusCode LowCardinality(String) CODEC(ZSTD(1)),
    StatusMessage String CODEC(ZSTD(1)),
    Events Nested (
        Timestamp DateTime64(9),
        Name LowCardinality(String),
        Attributes Map(LowCardinality(String), String)
    ) CODEC(ZSTD(1)),
    Links Nested (
        TraceId String,
        SpanId String,
        TraceState String,
        Attributes Map(LowCardinality(String), String)
    ) CODEC(ZSTD(1)),%s
    INDEX idx_trace_id TraceId TYPE bloom_filter(0.001) GRANULARITY 1,
    INDEX idx_res_attr_key mapKeys(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_res_attr_value mapValues(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_span_attr_key mapKeys(SpanAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_span_attr_value mapValues(SpanAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_duration Duration TYPE minmax GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (project, environment, Timestamp)
TTL %s
%s`,
		quoteIdentifier(database), quoteIdentifier(TracesTable), kitchenColumns,
		ttlExpressionOn(timeColumnLogs, retentionDays), writeHeavySettings)
}

// createTracesIDLookupTable is upstream's trace-id companion: one row per
// trace, saying when it started and when it ended.
//
// Nothing creates it when the exporter is not creating the schema, and without
// it a lookup by trace id has no time bound at all — see TracesIDLookupTable.
func createTracesIDLookupTable(database string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    TraceId String CODEC(ZSTD(1)),
    Start DateTime CODEC(Delta, ZSTD(1)),
    End DateTime CODEC(Delta, ZSTD(1)),
    INDEX idx_trace_id TraceId TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toDate(Start)
ORDER BY (TraceId, Start)
TTL %s
%s`,
		quoteIdentifier(database), quoteIdentifier(TracesIDLookupTable),
		ttlExpressionOn(timeColumnLookup, retentionDays), writeHeavySettings)
}

// createTracesIDLookupView fills the lookup as spans arrive.
func createTracesIDLookupView(database string) string {
	return fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s.%s TO %s.%s AS
SELECT
    TraceId,
    min(Timestamp) AS Start,
    max(Timestamp) AS End
FROM %s.%s
WHERE TraceId != ''
GROUP BY TraceId`,
		quoteIdentifier(database), quoteIdentifier(TracesIDLookupView),
		quoteIdentifier(database), quoteIdentifier(TracesIDLookupTable),
		quoteIdentifier(database), quoteIdentifier(TracesTable))
}

// metricsTableDDL is every metric table and the statement that creates it.
//
// They share a head, a tail and an ordering key and differ only in how a point
// of that type is shaped, so the differences are the only thing written out
// per table. The ordering key drops upstream's `cityHash64(Attributes)`:
// Kitchen reads whole series for an environment, never one attribute
// permutation, and the hash would only push `TimeUnix` further from the front.
func metricsTableDDL(database string, retentionDays int32) map[string]string {
	return map[string]string{
		MetricsGaugeTable: createOTelMetricsTable(database, MetricsGaugeTable, retentionDays,
			`    Value Float64 CODEC(ZSTD(1)),
    Flags UInt32 CODEC(ZSTD(1)),`+metricsExemplars),

		MetricsSumTable: createOTelMetricsTable(database, MetricsSumTable, retentionDays,
			`    Value Float64 CODEC(ZSTD(1)),
    Flags UInt32 CODEC(ZSTD(1)),`+metricsExemplars+`
    AggregationTemporality Int32 CODEC(ZSTD(1)),
    IsMonotonic Boolean CODEC(Delta, ZSTD(1)),`),

		MetricsHistogramTable: createOTelMetricsTable(database, MetricsHistogramTable, retentionDays,
			`    Count UInt64 CODEC(Delta, ZSTD(1)),
    Sum Float64 CODEC(ZSTD(1)),
    BucketCounts Array(UInt64) CODEC(ZSTD(1)),
    ExplicitBounds Array(Float64) CODEC(ZSTD(1)),`+metricsExemplars+`
    Flags UInt32 CODEC(ZSTD(1)),
    Min Float64 CODEC(ZSTD(1)),
    Max Float64 CODEC(ZSTD(1)),
    AggregationTemporality Int32 CODEC(ZSTD(1)),`),

		MetricsExponentialHistogramTable: createOTelMetricsTable(database, MetricsExponentialHistogramTable, retentionDays,
			`    Count UInt64 CODEC(Delta, ZSTD(1)),
    Sum Float64 CODEC(ZSTD(1)),
    Scale Int32 CODEC(ZSTD(1)),
    ZeroCount UInt64 CODEC(ZSTD(1)),
    PositiveOffset Int32 CODEC(ZSTD(1)),
    PositiveBucketCounts Array(UInt64) CODEC(ZSTD(1)),
    NegativeOffset Int32 CODEC(ZSTD(1)),
    NegativeBucketCounts Array(UInt64) CODEC(ZSTD(1)),`+metricsExemplars+`
    Flags UInt32 CODEC(ZSTD(1)),
    Min Float64 CODEC(ZSTD(1)),
    Max Float64 CODEC(ZSTD(1)),
    AggregationTemporality Int32 CODEC(ZSTD(1)),`),

		MetricsSummaryTable: createOTelMetricsTable(database, MetricsSummaryTable, retentionDays,
			`    Count UInt64 CODEC(Delta, ZSTD(1)),
    Sum Float64 CODEC(ZSTD(1)),
    ValueAtQuantiles Nested(
        Quantile Float64,
        Value Float64
    ) CODEC(ZSTD(1)),
    Flags UInt32 CODEC(ZSTD(1)),`),
	}
}

// metricsExemplars is the exemplar block every point type but the summary
// carries. It leads with a newline so a table body can concatenate onto it.
const metricsExemplars = `
    Exemplars Nested (
        FilteredAttributes Map(LowCardinality(String), String),
        TimeUnix DateTime,
        Value Float64,
        SpanId String,
        TraceId String
    ) CODEC(ZSTD(1)),`

// createOTelMetricsTable wraps one point type's columns in the head, the
// indexes and the engine every metric table shares.
func createOTelMetricsTable(database, table string, retentionDays int32, body string) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ResourceSchemaUrl String CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion String CODEC(ZSTD(1)),
    ScopeAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeDroppedAttrCount UInt32 CODEC(ZSTD(1)),
    ScopeSchemaUrl String CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    MetricName LowCardinality(String) CODEC(ZSTD(1)),
    MetricDescription String CODEC(ZSTD(1)),
    MetricUnit String CODEC(ZSTD(1)),
    Attributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    StartTimeUnix DateTime CODEC(Delta, ZSTD(1)),
    TimeUnix DateTime CODEC(Delta, ZSTD(1)),
%s%s
    INDEX idx_res_attr_key mapKeys(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_res_attr_value mapValues(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_scope_attr_key mapKeys(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_scope_attr_value mapValues(ScopeAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_attr_key mapKeys(Attributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_attr_value mapValues(Attributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_time_minmax TimeUnix TYPE minmax GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toDate(TimeUnix)
ORDER BY (project, environment, MetricName, TimeUnix)
TTL %s
%s`,
		quoteIdentifier(database), quoteIdentifier(table), body, kitchenColumns,
		ttlExpressionOn(timeColumnMetrics, retentionDays), writeHeavySettings)
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

// createMetricsRollupTable is the environment page's usage at five-minute
// resolution, kept as aggregate states so that merging two buckets is exact
// rather than an average of averages of different sample counts.
//
// It exists because the metric tables are one row per container per metric per
// scrape: fine to scan for the last hour, and a week of it for a busy platform
// is the read that makes the environment page feel broken. The ordering key
// leads with the same columns as they do so the same filter hits the primary
// index.
//
// The column types are what they have always been, deliberately — see
// MetricsRollupTable.
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
		ttlExpressionOn(timeColumnRollup, retentionDays))
}

// createMetricsRollupGaugeView fills the rollup's usage and limit columns as
// gauge points arrive.
//
// The `-If` combinator does the demultiplexing: a metric table is one row per
// metric per sample, so CPU and memory for the same container are different
// rows, and `avgStateIf(Value, MetricName = …)` is what turns them back into
// columns of one bucket. It composes to the plain state type —
// `avgStateIf(Float64, …)` is an `AggregateFunction(avg, Float64)`, not a
// state of `avgIf` — which is why these land in the columns unchanged.
//
// Every column is read through the source table's alias. ClickHouse resolves a
// bare name against the SELECT list's own aliases first, so `maxState(cpuCores)`
// after `avgState(cpuCores) AS cpuCores` takes the max of an aggregate state
// and the view is refused outright ("values of that data type are not
// comparable"). Qualifying every argument is what keeps the state columns
// nameable after the columns they aggregate. See timestampAlias in logs.go and
// protocolAlias in flows.go for the same trap where it merely returns the wrong
// answer.
func createMetricsRollupGaugeView(database string) string {
	return fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s.%s TO %s.%s AS
SELECT
    toDateTime(toStartOfInterval(g.TimeUnix, toIntervalSecond(%d)), 'UTC') AS bucket,
    g.project AS project,
    g.environment AS environment,
    g.namespace AS namespace,
    g.pod AS pod,
    g.container AS container,
    avgStateIf(g.Value, g.MetricName = %s) AS cpuCores,
    maxStateIf(g.Value, g.MetricName = %s) AS cpuPeakCores,
    avgStateIf(toUInt64(g.Value), g.MetricName = %s) AS memoryBytes,
    maxStateIf(toUInt64(g.Value), g.MetricName = %s) AS memoryPeakBytes,
    maxStateIf(g.Value, g.MetricName = %s) AS cpuLimitCores,
    maxStateIf(toUInt64(g.Value), g.MetricName = %s) AS memoryLimitBytes
FROM %s.%s AS g
WHERE g.MetricName IN (%s, %s, %s, %s)
GROUP BY bucket, project, environment, namespace, pod, container`,
		quoteIdentifier(database), quoteIdentifier(MetricsRollupGaugeView),
		quoteIdentifier(database), quoteIdentifier(MetricsRollupTable),
		MetricsRollupSeconds,
		quoteLiteral(MetricContainerCPUUsage),
		quoteLiteral(MetricContainerCPUUsage),
		quoteLiteral(MetricContainerMemoryWorkingSet),
		quoteLiteral(MetricContainerMemoryWorkingSet),
		quoteLiteral(MetricContainerCPULimit),
		quoteLiteral(MetricContainerMemoryLimit),
		quoteIdentifier(database), quoteIdentifier(MetricsGaugeTable),
		quoteLiteral(MetricContainerCPUUsage), quoteLiteral(MetricContainerMemoryWorkingSet),
		quoteLiteral(MetricContainerCPULimit), quoteLiteral(MetricContainerMemoryLimit))
}

// createMetricsRollupSumView fills the rollup's restart and OOM columns as
// delta sums arrive.
//
// It names only the columns it feeds. The rest take their column default,
// which for an AggregateFunction column is the empty state — it merges as a
// contribution of nothing, which is exactly right for a bucket where the only
// thing that happened was a restart.
func createMetricsRollupSumView(database string) string {
	return fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s.%s TO %s.%s AS
SELECT
    toDateTime(toStartOfInterval(s.TimeUnix, toIntervalSecond(%d)), 'UTC') AS bucket,
    s.project AS project,
    s.environment AS environment,
    s.namespace AS namespace,
    s.pod AS pod,
    s.container AS container,
    sumStateIf(toUInt16(s.Value), s.MetricName = %s) AS restarted,
    sumStateIf(toUInt8(s.Value), s.MetricName = %s) AS oomKills
FROM %s.%s AS s
WHERE s.MetricName IN (%s, %s)
GROUP BY bucket, project, environment, namespace, pod, container`,
		quoteIdentifier(database), quoteIdentifier(MetricsRollupSumView),
		quoteIdentifier(database), quoteIdentifier(MetricsRollupTable),
		MetricsRollupSeconds,
		quoteLiteral(MetricContainerRestartsDelta), quoteLiteral(MetricContainerOOMKilled),
		quoteIdentifier(database), quoteIdentifier(MetricsSumTable),
		quoteLiteral(MetricContainerRestartsDelta), quoteLiteral(MetricContainerOOMKilled))
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

// createRequestsTable is one row per request the edge observed.
//
// `route` is LowCardinality and `path` is not, and that asymmetry is the whole
// cardinality argument: the follower templates the path into a route out of a
// per-environment budget, so the route set is bounded and a dictionary pays
// for itself, while the raw path is unbounded by definition and is kept only
// because a mis-templated route has to stay diagnosable. Nothing here
// normalises anything — doing it in SQL would mean the store had already seen
// the unbounded set.
//
// `source` names the vantage point, which is `gateway` and nothing else today;
// it exists so a second one (§3.1's eBPF successor) lands without a migration,
// and `trace_id` exists for the same reason — whether Cilium can populate it
// from an incoming `traceparent` is open, and a reserved column is cheaper
// than finding out the answer is yes after the table is full.
func createRequestsTable(database string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    Timestamp     DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    project       LowCardinality(String),
    environment   LowCardinality(String),
    host          LowCardinality(String),
    method        LowCardinality(String),
    path          String CODEC(ZSTD(1)),
    route         LowCardinality(String),
    status        UInt16,
    duration_ms   Float64 CODEC(ZSTD(1)),
    protocol      LowCardinality(String),
    source        LowCardinality(String),
    trace_id      String CODEC(ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (project, environment, Timestamp)
TTL %s
%s`, quoteIdentifier(database), quoteIdentifier(RequestsTable),
		ttlExpressionOn(timeColumnRequests, retentionDays), writeHeavySettings)
}

// createRequestsMinuteTable is the golden signals at minute resolution.
//
// The counts are kept as aggregate states rather than numbers for the reason
// the metrics rollup keeps them: merging two buckets is then exact, and a
// percentile is only mergeable at all as a state — a mean of per-minute p95s
// is not a p95 of anything.
func createRequestsMinuteTable(database string, retentionDays int32) string {
	return createRequestsRollupTable(database, RequestsMinuteTable, "toDate(bucket)", retentionDays)
}

// createRequestsHourTable is the same shape an hour wide, partitioned by month
// because a year of daily partitions is 365 of them for a table holding a few
// thousand rows a day.
func createRequestsHourTable(database string, retentionDays int32) string {
	return createRequestsRollupTable(database, RequestsHourTable, "toYYYYMM(bucket)", retentionDays)
}

// createRequestsRollupTable is what the two rollups share, which is everything
// but their partitioning and their retention.
//
// The ordering key leads with the project like every other product table, then
// the bucket, so a window for one environment is a range read; host, route,
// method and status follow because they are the dimensions the screens group
// by and a key that stopped at the bucket would collapse them into one row.
func createRequestsRollupTable(database, table, partition string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    bucket        DateTime,
    project       LowCardinality(String),
    environment   LowCardinality(String),
    host          LowCardinality(String),
    route         LowCardinality(String),
    method        LowCardinality(String),
    status        UInt16,
    requests      AggregateFunction(count),
    duration      AggregateFunction(quantilesTDigest(0.5, 0.95, 0.99), Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY %s
ORDER BY (project, environment, bucket, host, route, method, status)
TTL %s`, quoteIdentifier(database), quoteIdentifier(table), partition,
		ttlExpressionOn(timeColumnRollup, retentionDays))
}

// createRequestsMinuteView and createRequestsHourView fill the rollups as
// requests arrive. Both read the raw table — see RequestsTable for why the
// hourly one is not chained off the minute one.
func createRequestsMinuteView(database string) string {
	return createRequestsRollupView(database, RequestsMinuteView, RequestsMinuteTable, "toStartOfMinute")
}

func createRequestsHourView(database string) string {
	return createRequestsRollupView(database, RequestsHourView, RequestsHourTable, "toStartOfHour")
}

// createRequestsRollupView is the aggregation both rollups are filled by, at
// whichever resolution it is given.
//
// Every column is read through the source table's alias, which is not a style
// choice: the state columns are named after what they aggregate, and
// ClickHouse resolves a bare name against the SELECT list's own aliases first.
// See createMetricsRollupGaugeView, where the same shadowing makes the view
// refuse to be created at all.
func createRequestsRollupView(database, view, table, bucket string) string {
	return fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s.%s TO %s.%s AS
SELECT
    %s(r.Timestamp) AS bucket,
    r.project AS project,
    r.environment AS environment,
    r.host AS host,
    r.route AS route,
    r.method AS method,
    r.status AS status,
    countState() AS requests,
    quantilesTDigestState(0.5, 0.95, 0.99)(r.duration_ms) AS duration
FROM %s.%s AS r
GROUP BY bucket, project, environment, host, route, method, status`,
		quoteIdentifier(database), quoteIdentifier(view),
		quoteIdentifier(database), quoteIdentifier(table), bucket,
		quoteIdentifier(database), quoteIdentifier(RequestsTable))
}

// createK8sEventsTable is the cluster's Warning events as a history.
//
// `project` is empty for platform and cluster objects, which is not a gap: the
// events that explain an install that never came up are exactly the ones no
// project owns, and the operator's Edge and Workloads screens read that bucket
// on purpose. The ordering key still leads with it, because the environment
// page's crash report is the other reader and it asks per project.
func createK8sEventsTable(database string, retentionDays int32) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    timestamp   DateTime64(3, 'UTC'),
    project     LowCardinality(String),
    environment LowCardinality(String),
    namespace   LowCardinality(String),
    kind        LowCardinality(String),
    name        String,
    reason      LowCardinality(String),
    message     String,
    count       UInt32,
    node        LowCardinality(String)
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (project, environment, timestamp)
TTL %s`, quoteIdentifier(database), quoteIdentifier(K8sEventsTable), ttlExpression(retentionDays))
}

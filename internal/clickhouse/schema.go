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

// Log sources, as written into the `source` column.
const (
	SourceRuntime  = "runtime"
	SourceBuild    = "build"
	SourcePlatform = "platform"
)

// ttlIntervalPattern pulls the retention out of a table's DDL. ClickHouse
// normalizes `INTERVAL 30 DAY` to `toIntervalDay(30)`, whichever form it was
// created with.
var ttlIntervalPattern = regexp.MustCompile(`toIntervalDay\((\d+)\)`)

// EnsureTelemetrySchema creates every telemetry table — logs, events, flows —
// and keeps their TTLs in step with the retention configured on the Kitchen
// object. One retention knob covers all three: they are facets of the same
// telemetry store, and a second knob would be a second thing to explain.
func (c *Client) EnsureTelemetrySchema(ctx context.Context, retentionDays int32) error {
	if err := c.EnsureLogsSchema(ctx, retentionDays); err != nil {
		return err
	}
	if err := c.EnsureEventsSchema(ctx, retentionDays); err != nil {
		return err
	}
	return c.EnsureFlowsSchema(ctx, retentionDays)
}

// EnsureLogsSchema creates the log table if it is missing and keeps its TTL in
// step with the retention configured on the Kitchen object. It is safe to run
// on every reconcile: the DDL is idempotent and the TTL is only altered when
// it actually differs.
func (c *Client) EnsureLogsSchema(ctx context.Context, retentionDays int32) error {
	if err := c.ensureTable(ctx, LogsTable, createLogsTable(c.cfg.Database, retentionDays), retentionDays); err != nil {
		return err
	}
	// A logs table from before the level column learned it here. ClickHouse
	// makes this idempotent, so it is safe to run on every reconcile.
	return c.Exec(ctx, fmt.Sprintf(
		"ALTER TABLE %s.%s ADD COLUMN IF NOT EXISTS level LowCardinality(String) AFTER stream",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable)))
}

// EnsureEventsSchema creates the platform activity table.
func (c *Client) EnsureEventsSchema(ctx context.Context, retentionDays int32) error {
	return c.ensureTable(ctx, EventsTable, createEventsTable(c.cfg.Database, retentionDays), retentionDays)
}

// EnsureFlowsSchema creates the network flow table.
func (c *Client) EnsureFlowsSchema(ctx context.Context, retentionDays int32) error {
	return c.ensureTable(ctx, FlowsTable, createFlowsTable(c.cfg.Database, retentionDays), retentionDays)
}

// ensureTable runs the idempotent CREATE and then brings the TTL in line,
// altering only when the enforced retention actually differs.
func (c *Client) ensureTable(ctx context.Context, table, ddl string, retentionDays int32) error {
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
		db, quoteIdentifier(table), ttlExpression(retentionDays)))
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
	return fmt.Sprintf("toDateTime(timestamp) + toIntervalDay(%d)", retentionDays)
}

// createLogsTable is the log schema, wide enough for the queries the UI and
// the operator API need: "this build's output" and "this project's runtime
// logs in this environment, over this window".
//
// The ordering key is what those queries filter on. `build` gets a set index
// rather than a place in the key: it is empty for every runtime line, so it
// would only dilute the primary index. `message` gets a token filter so that
// free-text search does not have to read every granule.
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
    message     String,
    labels      Map(LowCardinality(String), String),
    INDEX idx_build build TYPE set(0) GRANULARITY 4,
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

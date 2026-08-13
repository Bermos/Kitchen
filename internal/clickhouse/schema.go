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

// EnsureLogsSchema creates the log table if it is missing and keeps its TTL in
// step with the retention configured on the Kitchen object. It is safe to run
// on every reconcile: the DDL is idempotent and the TTL is only altered when
// it actually differs.
func (c *Client) EnsureLogsSchema(ctx context.Context, retentionDays int32) error {
	if retentionDays < 1 {
		return fmt.Errorf("retentionDays must be at least 1, got %d", retentionDays)
	}

	db := quoteIdentifier(c.cfg.Database)
	if err := c.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+db); err != nil {
		return err
	}
	if err := c.Exec(ctx, createLogsTable(c.cfg.Database, retentionDays)); err != nil {
		return err
	}

	current, err := c.logsRetentionDays(ctx)
	if err != nil {
		return err
	}
	if current == retentionDays {
		return nil
	}
	return c.Exec(ctx, fmt.Sprintf("ALTER TABLE %s.%s MODIFY TTL %s",
		db, quoteIdentifier(LogsTable), ttlExpression(retentionDays)))
}

// logsRetentionDays reads back the retention ClickHouse is enforcing. A table
// without a TTL — one created by an older version of Kitchen, or edited by
// hand — reports 0, which never matches and so gets the TTL applied.
func (c *Client) logsRetentionDays(ctx context.Context) (int32, error) {
	query := fmt.Sprintf(
		"SELECT engine_full FROM system.tables WHERE database = %s AND name = %s",
		quoteLiteral(c.cfg.Database), quoteLiteral(LogsTable))
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

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
	"strings"
)

// What the chart's TTLs leave behind, and why anything has to collect it.
//
// The chart puts one file in the telemetry store's config.d giving every
// `system.*_log` table a TTL (#530): without one they grow forever, and on the
// installation this was measured on they were 88% of a 20Gi volume. But a TTL
// is part of a system log table's *definition*, and ClickHouse does not alter
// a definition it disagrees with — it renames the table it already had to
// `<name>_<n>` and creates a fresh one beside it. The renamed table is never
// written to, never read, carries no TTL of its own and is never dropped. So
// on an installation that has been running a while, the release that fixes the
// growth reclaims nothing at all: every byte the fix is about is now sitting in
// a table nothing will ever touch again.
//
// Dropping those is the other half of the fix, and it belongs to the operator
// because the alternative is `kubectl exec` into the store — which is exactly
// what this platform says a running installation never needs (CLAUDE.md,
// "Nothing needs kubectl").

// SystemLogTables are the tables the chart bounds with a TTL, and so the only
// tables an orphan here can have come from. They are named rather than
// discovered so that this can never widen: it is a fixed list of ClickHouse's
// own diagnostic logs, each of which the server documents as safe to drop at
// any time, and nothing else in the `system` database is eligible however it
// is named.
//
// It is the same list, in the same order, as the file the chart ships —
// charts/kitchen/templates/clickhouse/system-logs-configmap.yaml. A table
// added there is added here, or its orphan is never collected.
var SystemLogTables = []string{
	"text_log",
	"trace_log",
	"metric_log",
	"asynchronous_metric_log",
	"query_metric_log",
	"background_schedule_pool_log",
	"processors_profile_log",
	"query_log",
	"query_views_log",
	"part_log",
	"asynchronous_insert_log",
	"error_log",
}

// orphanPattern is the name ClickHouse gives a system log table it has
// superseded: the table's own name and a counter. Verified against the image
// the chart pins (clickhouse/clickhouse-server:26.3.17.110-alpine), where
// adding the TTLs to a running server's config.d left `system.text_log_0`
// beside a fresh `system.text_log`.
//
// It is built from SystemLogTables rather than written as `.*_log_[0-9]+` so
// that the sweep is confined to the tables this chart configures: a
// `system.my_audit_log_1` somebody else created does not match. What it does
// match is any digits at all after one of the twelve names — `text_log_0` as
// the server writes it, and equally a `query_log_2024` somebody renamed by
// hand. That is deliberate rather than overlooked: the server picks the lowest
// free suffix, so pinning the pattern to a single digit would miss the second
// rename, and a table named `<one of the twelve>_<digits>` in the `system`
// database is a copy of a log ClickHouse documents as safe to drop whoever
// made it. A name that must survive is a name outside that shape.
var orphanPattern = regexp.MustCompile(
	`^(` + strings.Join(SystemLogTables, "|") + `)_[0-9]+$`)

// OrphanedSystemLog is one superseded system log table, and what it costs.
type OrphanedSystemLog struct {
	// Name is the table's name inside the `system` database.
	Name string

	// Bytes is what it occupied on disk, read before it was dropped. It is
	// the whole point of the exercise, so it is reported rather than
	// discarded: an operator who upgrades wants to know what came back.
	Bytes int64
}

// ReclaimOrphanedSystemLogs drops every system log table ClickHouse has
// renamed away, and answers with what it dropped.
//
// It is safe to call on every reconcile and does nothing on almost all of
// them: the sweep is one read of `system.tables`, and a store with no orphans
// answers it with no rows. That is deliberately not a one-shot recorded once
// and never repeated — the rename does not happen at startup but the first
// time each log is written after the definition changed, which for the quiet
// ones (`error_log`, `asynchronous_insert_log`) can be minutes or hours later.
// A single pass timed to the first reconcile after an upgrade would collect
// the loud tables and leave the quiet ones on the volume forever.
//
// The caller decides whether this store is one to sweep at all. Config.
// SystemLogsBounded is that decision: it is true only of a store this chart
// runs, whose config.d this release owns, and it is false for an external
// ClickHouse — whose renamed tables this platform did not cause and whose
// contents are not its to delete.
func (c *Client) ReclaimOrphanedSystemLogs(ctx context.Context) ([]OrphanedSystemLog, error) {
	orphans, err := c.orphanedSystemLogs(ctx)
	if err != nil {
		return nil, err
	}
	dropped := make([]OrphanedSystemLog, 0, len(orphans))
	for _, orphan := range orphans {
		// SYNC so the bytes are actually gone when this returns rather than
		// queued behind ClickHouse's asynchronous drop — the number reported
		// afterwards would otherwise be a claim about a volume that has not
		// changed yet.
		statement := fmt.Sprintf("DROP TABLE IF EXISTS system.%s SYNC", quoteIdentifier(orphan.Name))
		if err := c.Exec(ctx, statement); err != nil {
			// What has already gone is still worth recording: a store that
			// refuses halfway through has reclaimed everything before that.
			return dropped, fmt.Errorf("dropping the superseded system log table %s: %w",
				orphan.Name, err)
		}
		dropped = append(dropped, orphan)
	}
	return dropped, nil
}

// orphanedSystemLogs reads the superseded tables and their size.
//
// The match is done in ClickHouse rather than here so that a store with
// nothing to collect — every store, after the first sweep — answers with an
// empty body instead of a list of its system tables.
func (c *Client) orphanedSystemLogs(ctx context.Context) ([]OrphanedSystemLog, error) {
	statement := fmt.Sprintf(`
SELECT name, toString(ifNull(total_bytes, 0))
FROM system.tables
WHERE database = 'system'
  AND engine LIKE '%%MergeTree'
  AND match(name, %s)
ORDER BY name`, quoteLiteral(orphanPattern.String()))

	body, err := c.Query(ctx, statement)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(body, "\n")
	orphans := make([]OrphanedSystemLog, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, size, found := strings.Cut(line, "\t")
		if !found {
			return nil, fmt.Errorf("unreadable system.tables row %q", line)
		}
		// The server matched the pattern, but the name is about to be
		// interpolated into a DROP, so it is checked here too rather than
		// trusted across a network boundary.
		if !orphanPattern.MatchString(name) {
			return nil, fmt.Errorf("system.tables answered with %q, which is not a "+
				"superseded system log table", name)
		}
		bytes, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unreadable size %q for system.%s", size, name)
		}
		orphans = append(orphans, OrphanedSystemLog{Name: name, Bytes: bytes})
	}
	return orphans, nil
}

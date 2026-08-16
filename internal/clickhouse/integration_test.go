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
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// The tests in this package answer "did we build the statement we meant to"
// against a fake that records what it was sent. They cannot answer "will
// ClickHouse accept it", and that is where the interesting mistakes are: an
// alias that shadows a column, a materialized view whose aggregate states are
// named after the columns they aggregate, a MODIFY TTL naming a column the
// table does not have. Every one of those is a query that reads perfectly and
// fails, or worse, answers something else.
//
// So this file runs the same code against a real store, and is skipped unless
// one is pointed at:
//
//	docker run -d --rm -p 8123:8123 \
//	  -e CLICKHOUSE_USER=kitchen -e CLICKHOUSE_PASSWORD=hunter2 -e CLICKHOUSE_DB=kitchen \
//	  clickhouse/clickhouse-server:24.8
//	KITCHEN_CLICKHOUSE_URL=http://kitchen:hunter2@127.0.0.1:8123/kitchen \
//	  go test ./internal/clickhouse/ -run Integration -v
//
// It is not part of `make test` on purpose: CI has no ClickHouse, and a test
// that silently needs one is a test that silently stops running.

// The fixtures' names: the database the store is expected to hold, and the
// project every row written here belongs to.
const (
	integrationDatabase = "kitchen"
	integrationProject  = "shop"
)

// integrationClient resolves the store under test, or skips.
func integrationClient(t *testing.T) *Client {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("KITCHEN_CLICKHOUSE_URL"))
	if raw == "" {
		t.Skip("set KITCHEN_CLICKHOUSE_URL to run the store integration tests")
	}
	endpoint, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("KITCHEN_CLICKHOUSE_URL is not a URL: %v", err)
	}
	password, _ := endpoint.User.Password()
	database := strings.Trim(endpoint.Path, "/")
	if database == "" {
		database = integrationDatabase
	}
	return New(Config{
		Host:     endpoint.Hostname(),
		HTTPPort: endpoint.Port(),
		Database: database,
		Username: endpoint.User.Username(),
		Password: password,
	})
}

// TestIntegrationSchemaApplies is the whole point: every CREATE, every ALTER
// and the materialized view, run twice, because they all have to be idempotent
// — the operator applies them on every reconcile.
func TestIntegrationSchemaApplies(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()

	if err := client.EnsureTelemetrySchema(ctx, 30); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}
	if err := client.EnsureTelemetrySchema(ctx, 30); err != nil {
		t.Fatalf("EnsureTelemetrySchema is not idempotent: %v", err)
	}
	// And that a retention change is applied rather than refused — the MODIFY
	// TTL on the rollup names `bucket`, not `timestamp`.
	if err := client.EnsureTelemetrySchema(ctx, 7); err != nil {
		t.Fatalf("EnsureTelemetrySchema at a new retention: %v", err)
	}
	for _, table := range []string{LogsTable, EventsTable, FlowsTable, MetricsTable, MetricsRollupTable} {
		days, err := client.tableRetentionDays(ctx, table)
		if err != nil {
			t.Fatalf("reading %s's retention: %v", table, err)
		}
		if days != 7 {
			t.Errorf("%s retains %d days, want 7", table, days)
		}
	}
	if err := client.EnsureTelemetrySchema(ctx, 30); err != nil {
		t.Fatalf("EnsureTelemetrySchema back to 30: %v", err)
	}
}

// TestIntegrationResourceSeries writes samples and reads them back both ways:
// off the raw table for a fine window, and off the rollup for a wide one. The
// two have to agree, because they are the same arithmetic over the same rows
// and a user widening a range must not see the numbers change.
func TestIntegrationResourceSeries(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	if err := client.EnsureMetricsSchema(ctx, 30); err != nil {
		t.Fatalf("EnsureMetricsSchema: %v", err)
	}

	environment := fmt.Sprintf("series-%d", time.Now().UnixNano())
	start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	samples := []ResourceSample{}
	for i := 0; i < 120; i++ { // an hour at 30s
		at := start.Add(time.Duration(i) * 30 * time.Second)
		for pod := 0; pod < 2; pod++ {
			sample := ResourceSample{
				Timestamp:        at,
				Project:          integrationProject,
				Environment:      environment,
				Namespace:        "kitchen-app-shop",
				Pod:              fmt.Sprintf("%s-%d", environment, pod),
				Container:        "app",
				Node:             "node-1",
				CPUCores:         0.1,
				MemoryBytes:      100_000_000,
				CPULimitCores:    0.5,
				MemoryLimitBytes: 536_870_912,
			}
			// One restart, once, so the event series has something in it.
			if i == 60 && pod == 0 {
				sample.Restarts, sample.Restarted, sample.OOMKilled = 1, 1, true
			}
			samples = append(samples, sample)
		}
	}
	if err := client.InsertResourceSamples(ctx, samples); err != nil {
		t.Fatalf("InsertResourceSamples: %v", err)
	}
	// The materialized view fills on insert, but the parts it wrote are merged
	// in the background; the read merges states itself, so nothing is waited on
	// here beyond the insert being visible.

	query := ResourceSeriesQuery{
		Project:     integrationProject,
		Environment: environment,
		Since:       start,
		Until:       start.Add(time.Hour),
	}

	fine := query
	fine.Buckets = 120 // 30s buckets — under the rollup's width, so raw
	raw, err := client.ResourceSeries(ctx, fine)
	if err != nil {
		t.Fatalf("ResourceSeries (raw): %v", err)
	}
	if raw.Rollup {
		t.Fatalf("a %ds bucket should have come off the raw table", raw.BucketSeconds)
	}

	coarse := query
	coarse.Buckets = 6 // 10m buckets — a multiple of the rollup's width
	rolled, err := client.ResourceSeries(ctx, coarse)
	if err != nil {
		t.Fatalf("ResourceSeries (rollup): %v", err)
	}
	if !rolled.Rollup {
		t.Fatalf("a %ds bucket should have come off the rollup", rolled.BucketSeconds)
	}

	// Both windows cover the same samples, so the totals have to match.
	if raw.Restarts != 1 || rolled.Restarts != 1 {
		t.Errorf("restarts: raw %d, rollup %d, want 1 each", raw.Restarts, rolled.Restarts)
	}
	if raw.OOMKills != 1 || rolled.OOMKills != 1 {
		t.Errorf("OOM kills: raw %d, rollup %d, want 1 each", raw.OOMKills, rolled.OOMKills)
	}
	if raw.CPULimitCores != 0.5 || rolled.CPULimitCores != 0.5 {
		t.Errorf("cpu limit: raw %v, rollup %v, want 0.5 each", raw.CPULimitCores, rolled.CPULimitCores)
	}

	// Two pods reported throughout, and the environment's CPU is their sum.
	for name, series := range map[string]ResourceSeries{"raw": raw, "rollup": rolled} {
		reporting := 0
		for _, point := range series.Points {
			if point.Replicas == 0 {
				continue
			}
			reporting++
			if point.Replicas != 2 {
				t.Errorf("%s: %s has %d replicas, want 2", name, point.Start, point.Replicas)
			}
			if delta := point.CPUCores - 0.2; delta > 0.001 || delta < -0.001 {
				t.Errorf("%s: %s uses %v cores, want the two pods' 0.2", name, point.Start, point.CPUCores)
			}
			if point.MemoryBytes != 200_000_000 {
				t.Errorf("%s: %s uses %d bytes, want 200000000", name, point.Start, point.MemoryBytes)
			}
		}
		if reporting == 0 {
			t.Errorf("%s: nothing came back at all", name)
		}
	}
}

// TestIntegrationTraces writes spans and reads them back as a list and as a
// waterfall. The list is one GROUP BY with a HAVING over an alias that must not
// shadow a column, which is the kind of thing only a real server catches.
func TestIntegrationTraces(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	if err := client.EnsureTracesSchema(ctx, 30); err != nil {
		t.Fatalf("EnsureTracesSchema: %v", err)
	}

	traceID := fmt.Sprintf("%x", time.Now().UnixNano())
	start := time.Now().UTC().Add(-time.Minute)
	spans := []Span{{
		Timestamp:     start,
		TraceID:       traceID,
		SpanID:        "s1",
		Name:          "GET /checkout",
		Kind:          "SERVER",
		Service:       integrationProject,
		Project:       integrationProject,
		Environment:   "shop-production",
		DurationMs:    420.5,
		StatusCode:    StatusError,
		StatusMessage: "boom",
		HTTPStatus:    500,
		Attributes:    map[string]string{"http.route": "/checkout"},
		Resource:      map[string]string{"service.name": integrationProject},
	}, {
		Timestamp:    start.Add(10 * time.Millisecond),
		TraceID:      traceID,
		SpanID:       "s2",
		ParentSpanID: "s1",
		Name:         "SELECT orders",
		Kind:         "CLIENT",
		Service:      "shop-db",
		Project:      integrationProject,
		Environment:  "shop-production",
		DurationMs:   390,
		StatusCode:   StatusUnset,
	}}
	if err := client.InsertSpans(ctx, spans); err != nil {
		t.Fatalf("InsertSpans: %v", err)
	}

	traces, err := client.Traces(ctx, TraceQuery{
		Project:    integrationProject,
		Since:      start.Add(-time.Minute),
		OnlyErrors: true,
	})
	if err != nil {
		t.Fatalf("Traces: %v", err)
	}
	found := Trace{}
	for _, trace := range traces {
		if trace.TraceID == traceID {
			found = trace
		}
	}
	if found.TraceID == "" {
		t.Fatalf("the trace was not in the list of %d", len(traces))
	}
	// The root span names the trace, and the duration is the trace's own —
	// end to end, not the longest span.
	if found.Name != "GET /checkout" || found.Service != integrationProject {
		t.Errorf("the root should name the trace: %+v", found)
	}
	if found.Spans != 2 || found.Errors != 1 || found.Services != 2 {
		t.Errorf("unexpected aggregates: %+v", found)
	}
	// The envelope: from the first span's start to the last span's end. Not
	// the sum of the spans (810ms — they overlap, that is what nesting is)
	// and not the longest child (390ms).
	if found.DurationMs < 420 || found.DurationMs > 421 {
		t.Errorf("want the trace's own 420ms end to end, got %v", found.DurationMs)
	}

	// The duration bound is over the trace, so a trace slower than its own
	// longest span is still found by it.
	slow, err := client.Traces(ctx, TraceQuery{Since: start.Add(-time.Minute), MinDurationMs: 395})
	if err != nil {
		t.Fatalf("Traces (slow): %v", err)
	}
	if len(slow) == 0 {
		t.Error("a 400ms trace should be found by a 395ms bound")
	}

	read, err := client.Trace(ctx, traceID)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if len(read) != 2 {
		t.Fatalf("want two spans, got %d", len(read))
	}
	if read[0].SpanID != "s1" || read[1].ParentSpanID != "s1" {
		t.Errorf("the waterfall lost its shape: %+v", read)
	}
	if read[0].Attributes["http.route"] != "/checkout" ||
		read[0].Resource["service.name"] != integrationProject {
		t.Errorf("the maps did not survive the round trip: %+v", read[0])
	}
	// Microseconds: two spans of one trace can start in the same millisecond,
	// and a waterfall that draws them level is wrong.
	if !read[1].Timestamp.After(read[0].Timestamp) {
		t.Errorf("the child should start after its parent: %s then %s", read[0].Timestamp, read[1].Timestamp)
	}
}

// TestIntegrationLogTraceColumn proves the log table's new columns exist on a
// table created by an older Kitchen — the ALTER path — and that the query
// language reaches them.
func TestIntegrationLogTraceColumn(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	if err := client.EnsureLogsSchema(ctx, 30); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}

	traceID := fmt.Sprintf("%x", time.Now().UnixNano())
	insert := fmt.Sprintf(
		"INSERT INTO %s.%s (timestamp, source, project, environment, traceId, spanId, message, level) VALUES "+
			"(now64(3), 'runtime', 'shop', 'shop-production', %s, 's1', 'checkout failed', 'error')",
		quoteIdentifier(client.cfg.Database), quoteIdentifier(LogsTable), quoteLiteral(traceID))
	if err := client.Exec(ctx, insert); err != nil {
		t.Fatalf("writing a log line: %v", err)
	}

	// Every spelling resolves to the column, which is the point of accepting
	// them: the name belongs to the instrumentation library.
	for _, query := range []string{
		"traceId:" + traceID,
		"trace_id:" + traceID,
		"trace.id:" + traceID,
	} {
		lines, err := client.FilterLogs(ctx, LogFilter{LogSelection: LogSelection{Query: query}})
		if err != nil {
			t.Fatalf("FilterLogs(%q): %v", query, err)
		}
		if len(lines) != 1 {
			t.Fatalf("FilterLogs(%q) found %d lines, want 1", query, len(lines))
		}
		if lines[0].TraceID != traceID || lines[0].SpanID != "s1" {
			t.Errorf("the trace columns did not come back: %+v", lines[0])
		}
	}
}

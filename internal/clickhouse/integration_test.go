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
// There is a second thing only a real server can answer, and it is the one this
// change turns on: **the tables are written by a stock exporter, not by us.**
// Every INSERT below names exactly the columns the exporter's own template
// names — nothing more, nothing less — so a Kitchen column that stopped being
// MATERIALIZED, or a base column that drifted from upstream, fails here rather
// than in a cluster with a crash-looping collector.
//
// So this file runs the same code against a real store, and is skipped unless
// one is pointed at:
//
//	docker run -d --rm -p 8123:8123 \
//	  -e CLICKHOUSE_USER=kitchen -e CLICKHOUSE_PASSWORD=hunter2 -e CLICKHOUSE_DB=kitchen \
//	  clickhouse/clickhouse-server:26.3
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
	integrationRetained = 30
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

// every table the operator owns the TTL of.
func retainedTables() []string {
	return []string{
		LogsTable, EventsTable, FlowsTable, TracesTable, TracesIDLookupTable,
		MetricsGaugeTable, MetricsSumTable, MetricsHistogramTable,
		MetricsExponentialHistogramTable, MetricsSummaryTable, MetricsRollupTable,
	}
}

// TestIntegrationSchemaApplies is the whole point: every CREATE, every ALTER
// and every materialized view, run twice, because they all have to be
// idempotent — the operator applies them on every reconcile.
func TestIntegrationSchemaApplies(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()

	if err := client.EnsureTelemetrySchema(ctx, integrationRetained); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}
	if err := client.EnsureTelemetrySchema(ctx, integrationRetained); err != nil {
		t.Fatalf("EnsureTelemetrySchema is not idempotent: %v", err)
	}
	// And that a retention change is applied rather than refused — each MODIFY
	// TTL has to name that table's own time column, and there are five spellings
	// of it across these eleven tables.
	if err := client.EnsureTelemetrySchema(ctx, 7); err != nil {
		t.Fatalf("EnsureTelemetrySchema at a new retention: %v", err)
	}
	for _, table := range retainedTables() {
		days, err := client.tableRetentionDays(ctx, table)
		if err != nil {
			t.Fatalf("reading %s's retention: %v", table, err)
		}
		if days != 7 {
			t.Errorf("%s retains %d days, want 7", table, days)
		}
	}
	if err := client.EnsureTelemetrySchema(ctx, integrationRetained); err != nil {
		t.Fatalf("EnsureTelemetrySchema back to %d: %v", integrationRetained, err)
	}
}

// TestIntegrationTheExporterCanWriteEveryTable writes one row into each OTel
// table naming exactly the columns the exporter's template names.
//
// This is the contract the whole design rests on: Kitchen's columns are
// MATERIALIZED, so they are computed at insert and never appear in an INSERT
// column list. If one of them ever stopped being materialized, every insert the
// collector makes would fail with "no value for column" — in production, at
// install time, with nothing here to have caught it.
func TestIntegrationTheExporterCanWriteEveryTable(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	if err := client.EnsureTelemetrySchema(ctx, integrationRetained); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}

	environment := uniqueEnvironment("exporter")
	resource := resourceAttributes(environment, "pod-0", "app")

	for _, write := range []struct {
		table     string
		statement string
	}{
		{LogsTable, exporterLogInsert(client.cfg.Database, resource, time.Now())},
		{TracesTable, exporterSpanInsert(client.cfg.Database, resource, time.Now(), "t-"+environment, "s1", "")},
		{MetricsGaugeTable, exporterGaugeInsert(client.cfg.Database,
			[]string{gaugeValues(resource, MetricContainerCPUUsage, time.Now(), 0.25)})},
		{MetricsSumTable, exporterSumInsert(client.cfg.Database,
			[]string{sumValues(resource, MetricContainerRestartsDelta, time.Now(), 1)})},
		{MetricsHistogramTable, exporterHistogramInsert(client.cfg.Database, resource)},
		{MetricsExponentialHistogramTable, exporterExponentialHistogramInsert(client.cfg.Database, resource)},
		{MetricsSummaryTable, exporterSummaryInsert(client.cfg.Database, resource)},
	} {
		if err := client.Exec(ctx, write.statement); err != nil {
			t.Fatalf("the exporter's own column list was refused by %s: %v", write.table, err)
		}
		// And the columns it never mentioned computed anyway.
		answer, err := client.Query(ctx, fmt.Sprintf(
			"SELECT project, environment, pod, container FROM %s.%s WHERE environment = %s LIMIT 1",
			quoteIdentifier(client.cfg.Database), quoteIdentifier(write.table), quoteLiteral(environment)))
		if err != nil {
			t.Fatalf("reading %s back: %v", write.table, err)
		}
		want := strings.Join([]string{integrationProject, environment, "pod-0", "app"}, "\t")
		if strings.TrimSpace(answer) != want {
			t.Errorf("%s materialized %q, want %q", write.table, answer, want)
		}
	}
}

// TestIntegrationResourceSeries writes usage the way the collector does and
// reads it back both ways: off the metric tables for a fine window, and off the
// rollup for a wide one. The two have to agree, because they are the same
// arithmetic over the same rows and a user widening a range must not see the
// numbers change.
func TestIntegrationResourceSeries(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	if err := client.EnsureMetricsSchema(ctx, integrationRetained); err != nil {
		t.Fatalf("EnsureMetricsSchema: %v", err)
	}

	environment := uniqueEnvironment("series")
	start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)

	gauges := []string{}
	sums := []string{}
	for i := 0; i < 120; i++ { // an hour at 30s
		at := start.Add(time.Duration(i) * 30 * time.Second)
		for pod := 0; pod < 2; pod++ {
			resource := resourceAttributes(environment, fmt.Sprintf("%s-%d", environment, pod), "app")
			gauges = append(gauges,
				gaugeValues(resource, MetricContainerCPUUsage, at, 0.1),
				gaugeValues(resource, MetricContainerMemoryWorkingSet, at, 100_000_000),
				gaugeValues(resource, MetricContainerCPULimit, at, 0.5),
				gaugeValues(resource, MetricContainerMemoryLimit, at, 536_870_912),
			)
			// One restart, once, so the event series has something in it.
			if i == 60 && pod == 0 {
				sums = append(sums,
					sumValues(resource, MetricContainerRestartsDelta, at, 1),
					sumValues(resource, MetricContainerOOMKilled, at, 1),
				)
			}
		}
	}
	if err := client.Exec(ctx, exporterGaugeInsert(client.cfg.Database, gauges)); err != nil {
		t.Fatalf("writing gauge points: %v", err)
	}
	if err := client.Exec(ctx, exporterSumInsert(client.cfg.Database, sums)); err != nil {
		t.Fatalf("writing sum points: %v", err)
	}
	// The materialized views fill on insert, but the parts they wrote are merged
	// in the background; the read merges states itself, so nothing is waited on
	// here beyond the insert being visible.

	query := ResourceSeriesQuery{
		Project:     integrationProject,
		Environment: environment,
		Since:       start,
		Until:       start.Add(time.Hour),
	}

	fine := query
	fine.Buckets = 120 // 30s buckets — under the rollup's width, so the raw tables
	raw, err := client.ResourceSeries(ctx, fine)
	if err != nil {
		t.Fatalf("ResourceSeries (raw): %v", err)
	}
	if raw.Rollup {
		t.Fatalf("a %ds bucket should have come off the metric tables", raw.BucketSeconds)
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

	// Both windows cover the same points, so the totals have to match.
	if raw.Restarts != 1 || rolled.Restarts != 1 {
		t.Errorf("restarts: raw %d, rollup %d, want 1 each", raw.Restarts, rolled.Restarts)
	}
	if raw.OOMKills != 1 || rolled.OOMKills != 1 {
		t.Errorf("OOM kills: raw %d, rollup %d, want 1 each", raw.OOMKills, rolled.OOMKills)
	}
	if raw.CPULimitCores != 0.5 || rolled.CPULimitCores != 0.5 {
		t.Errorf("cpu limit: raw %v, rollup %v, want 0.5 each", raw.CPULimitCores, rolled.CPULimitCores)
	}
	if raw.MemoryLimitBytes != 536_870_912 || rolled.MemoryLimitBytes != 536_870_912 {
		t.Errorf("memory limit: raw %d, rollup %d", raw.MemoryLimitBytes, rolled.MemoryLimitBytes)
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
	if err := client.EnsureTracesSchema(ctx, integrationRetained); err != nil {
		t.Fatalf("EnsureTracesSchema: %v", err)
	}

	environment := uniqueEnvironment("traces")
	traceID := fmt.Sprintf("%x", time.Now().UnixNano())
	start := time.Now().UTC().Add(-time.Minute)

	root := resourceAttributes(environment, "shop-0", "app")
	child := resourceAttributes(environment, "shop-db-0", "db")
	if err := client.Exec(ctx, exporterSpanInsert(client.cfg.Database, root, start, traceID, "s1", "")); err != nil {
		t.Fatalf("writing the root span: %v", err)
	}
	if err := client.Exec(ctx, exporterChildSpanInsert(client.cfg.Database, child,
		start.Add(10*time.Millisecond), traceID, "s2", "s1")); err != nil {
		t.Fatalf("writing the child span: %v", err)
	}

	traces, err := client.Traces(ctx, TraceQuery{
		Project:     integrationProject,
		Environment: environment,
		Since:       start.Add(-time.Minute),
		OnlyErrors:  true,
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
	// The exporter writes `Error`; Kitchen's API says ERROR, and the HTTP status
	// is an attribute rather than a column.
	if found.HTTPStatus != 500 {
		t.Errorf("the status should have been lifted out of the attributes: %+v", found)
	}
	// The envelope: from the first span's start to the last span's end. Not
	// the sum of the spans (810ms — they overlap, that is what nesting is)
	// and not the longest child (390ms).
	if found.DurationMs < 420 || found.DurationMs > 421 {
		t.Errorf("want the trace's own 420ms end to end, got %v", found.DurationMs)
	}

	// The duration bound is over the trace, so a trace slower than its own
	// longest span is still found by it.
	slow, err := client.Traces(ctx, TraceQuery{
		Environment:   environment,
		Since:         start.Add(-time.Minute),
		MinDurationMs: 395,
	})
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
	if read[0].StatusCode != StatusError || read[1].StatusCode != StatusUnset {
		t.Errorf("the exporter's Error/Unset should read back as Kitchen's: %+v", read)
	}
	if read[0].DurationMs != 420.5 {
		t.Errorf("nanoseconds should have become milliseconds, got %v", read[0].DurationMs)
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

// TestIntegrationTraceIDLookup proves the companion table is filled and that
// the bound it produces does not exclude the very spans it is meant to find.
//
// The lookup keeps whole seconds where the spans keep nanoseconds, so a span
// part-way into the trace's last second sits after the End the view recorded.
// Reading that back unwidened returns nothing at all, which looks exactly like
// a trace that was never collected.
func TestIntegrationTraceIDLookup(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	if err := client.EnsureTracesSchema(ctx, integrationRetained); err != nil {
		t.Fatalf("EnsureTracesSchema: %v", err)
	}

	environment := uniqueEnvironment("lookup")
	traceID := fmt.Sprintf("%x", time.Now().UnixNano())
	// Deliberately part-way into a second, which is the case that breaks an
	// unwidened bound.
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Minute + 400*time.Millisecond)
	resource := resourceAttributes(environment, "shop-0", "app")
	if err := client.Exec(ctx, exporterSpanInsert(client.cfg.Database, resource, at, traceID, "s1", "")); err != nil {
		t.Fatalf("writing the span: %v", err)
	}

	since, until, err := client.traceWindow(ctx, traceID)
	if err != nil {
		t.Fatalf("traceWindow: %v", err)
	}
	if since.IsZero() {
		t.Fatal("the materialized view should have filled the lookup on insert")
	}
	if !since.Before(at) || !until.After(at) {
		t.Errorf("the span at %s is outside the window [%s, %s]", at, since, until)
	}

	spans, err := client.Trace(ctx, traceID)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("the bounded read lost the span it was bounding for: got %d", len(spans))
	}

	// An id nothing has ever written reads back unbounded and empty, rather
	// than being refused.
	none, err := client.Trace(ctx, "0000000000000000")
	if err != nil {
		t.Fatalf("Trace (unknown): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an unknown trace should be empty, got %d spans", len(none))
	}
}

// TestIntegrationLogQuery proves the query language reaches the OTel columns:
// the ones Kitchen materialized under its own names, the ones it renamed, and
// the attribute maps behind everything else.
func TestIntegrationLogQuery(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	if err := client.EnsureLogsSchema(ctx, integrationRetained); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}

	environment := uniqueEnvironment("logs")
	traceID := fmt.Sprintf("%x", time.Now().UnixNano())
	resource := resourceAttributes(environment, "shop-0", "app")
	if err := client.Exec(ctx, exporterLogInsertWithTrace(client.cfg.Database, resource, time.Now(), traceID)); err != nil {
		t.Fatalf("writing a log line: %v", err)
	}

	scope := "environment:" + environment
	for _, query := range []struct{ name, query string }{
		// Kitchen's own materialized columns, under the names they always had.
		{"project", scope + " project:" + integrationProject},
		{"source", scope + " source:" + SourceRuntime},
		{"pod", scope + " pod:shop-*"},
		{"container", scope + " container:app"},
		// stdout/stderr is a column again, materialized out of the record's
		// attributes rather than the resource's.
		{"stream", scope + " stream:stderr"},
		// The renamed ones.
		{"level", scope + " level:error"},
		{"message", scope + " checkout"},
		{"message phrase", scope + ` message:"checkout failed"`},
		// Every spelling of a trace id resolves to the column.
		{"traceId", scope + " traceId:" + traceID},
		{"trace_id", scope + " trace_id:" + traceID},
		{"trace.id", scope + " trace.id:" + traceID},
		// An unknown dotted name is a log attribute.
		{"attribute", scope + " http.status:500"},
		{"attribute comparison", scope + " http.status:>=500"},
		// A pod label rides in the resource attributes.
		{"label", scope + " labels.tier:web"},
	} {
		t.Run(query.name, func(t *testing.T) {
			lines, err := client.FilterLogs(ctx, LogFilter{LogSelection: LogSelection{Query: query.query}})
			if err != nil {
				t.Fatalf("FilterLogs(%q): %v", query.query, err)
			}
			if len(lines) != 1 {
				t.Fatalf("FilterLogs(%q) found %d lines, want 1", query.query, len(lines))
			}
			line := lines[0]
			if line.TraceID != traceID || line.SpanID != "s1" {
				t.Errorf("the trace columns did not come back: %+v", line)
			}
			if line.Level != levelError {
				t.Errorf("the level should be folded to lower case, got %q", line.Level)
			}
			if line.Stream != "stderr" {
				t.Errorf("the stream did not come back: %+v", line)
			}
			if line.Message != "checkout failed" {
				t.Errorf("the body should be the message, got %q", line.Message)
			}
			if line.Fields["http.status"] != "500" {
				t.Errorf("the attributes should be the fields, got %v", line.Fields)
			}
		})
	}

	// The analytics run over the same selection and must survive a real server:
	// the histogram buckets a DateTime64, the facets are a UNION ALL of
	// subqueries, and the patterns nest a regular expression per line.
	histogram, err := client.LogHistogram(ctx, LogHistogramQuery{LogSelection: LogSelection{Query: scope}})
	if err != nil {
		t.Fatalf("LogHistogram: %v", err)
	}
	if histogram.Total != 1 {
		t.Errorf("want the one line in the histogram, got %d", histogram.Total)
	}

	facets, err := client.LogFacets(ctx, LogFacetQuery{LogSelection: LogSelection{Query: scope}})
	if err != nil {
		t.Fatalf("LogFacets: %v", err)
	}
	byField := map[string]LogFacet{}
	for _, facet := range facets {
		byField[facet.Field] = facet
	}
	for field, want := range map[string]string{
		"level": levelError, "source": SourceRuntime, "project": integrationProject, "stream": "stderr",
	} {
		facet, ok := byField[field]
		if !ok || len(facet.Values) == 0 {
			t.Errorf("the %s facet came back empty: %+v", field, facet)
			continue
		}
		if facet.Values[0].Value != want {
			t.Errorf("the %s facet says %q, want %q", field, facet.Values[0].Value, want)
		}
	}

	patterns, err := client.LogPatterns(ctx, LogPatternQuery{LogSelection: LogSelection{Query: scope}})
	if err != nil {
		t.Fatalf("LogPatterns: %v", err)
	}
	if len(patterns) != 1 || patterns[0].Level != levelError {
		t.Errorf("unexpected patterns: %+v", patterns)
	}
}

// TestIntegrationMetricsOverview exercises the dashboard's aggregation, which
// is the one place a single statement counts rows in tables that do not agree
// about what their time column is called: the logs are the exporter's
// `Timestamp`, the flows and events the operator's own `timestamp`.
func TestIntegrationMetricsOverview(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	if err := client.EnsureTelemetrySchema(ctx, integrationRetained); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}

	environment := uniqueEnvironment("overview")
	resource := resourceAttributes(environment, "shop-0", "app")
	if err := client.Exec(ctx, exporterLogInsert(client.cfg.Database, resource, time.Now())); err != nil {
		t.Fatalf("writing a log line: %v", err)
	}

	overview, err := client.MetricsOverview(ctx, MetricsQuery{Project: integrationProject})
	if err != nil {
		t.Fatalf("MetricsOverview: %v", err)
	}
	if overview.LogLines24h == 0 {
		t.Error("the line just written should be in the last 24 hours")
	}
	if overview.StoreBytes == 0 {
		t.Error("a store with rows in it should report a size")
	}
	if overview.StoreRowsPerSecond == 0 {
		t.Error("the line just written should count towards the ingest rate")
	}
}

// uniqueEnvironment keeps one run's rows from being read by the next. The
// tables are not truncated between runs — they are a real store's, and a test
// that dropped them would be a test that could not run against a live one.
func uniqueEnvironment(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// resourceAttributes is the map the collector sets on every signal it emits.
// The materialized columns read exactly these keys, so this is the schema's
// other half written out.
func resourceAttributes(environment, pod, container string) string {
	return fmt.Sprintf(`map('kitchen.project', %s, 'deployment.environment.name', %s, `+
		`'kitchen.build', '', 'kitchen.source', %s, 'k8s.namespace.name', %s, `+
		`'k8s.pod.name', %s, 'k8s.container.name', %s, 'k8s.node.name', 'node-1', `+
		`'service.name', %s, 'tier', 'web')`,
		quoteLiteral(integrationProject), quoteLiteral(environment), quoteLiteral(SourceRuntime),
		quoteLiteral("kitchen-app-"+integrationProject), quoteLiteral(pod), quoteLiteral(container),
		quoteLiteral(integrationProject))
}

// stamped renders a time the way ClickHouse parses one at nanosecond scale.
func stamped(at time.Time) string {
	return fmt.Sprintf("parseDateTime64BestEffort(%s, 9, 'UTC')",
		quoteLiteral(at.UTC().Format(time.RFC3339Nano)))
}

// seconds renders a time for the metric tables, whose TimeUnix is a DateTime.
func seconds(at time.Time) string {
	return fmt.Sprintf("parseDateTimeBestEffort(%s, 'UTC')",
		quoteLiteral(at.UTC().Format(time.RFC3339)))
}

// The INSERT column lists below are the exporter's own, transcribed from its
// templates. They are deliberately spelled out rather than generated: the point
// of these statements is to be what the collector sends, and a helper that
// derived them from the schema would only ever agree with itself.

func exporterLogInsert(database, resource string, at time.Time) string {
	return exporterLogInsertWithTrace(database, resource, at, "")
}

func exporterLogInsertWithTrace(database, resource string, at time.Time, traceID string) string {
	return fmt.Sprintf(`INSERT INTO %s.%s (
    Timestamp, TraceId, SpanId, TraceFlags, SeverityText, SeverityNumber, ServiceName, Body,
    ResourceSchemaUrl, ResourceAttributes, ScopeSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes,
    LogAttributes, EventName
) VALUES (%s, %s, 's1', 1, 'ERROR', 17, %s, 'checkout failed', '', %s, '', 'kitchen', '1.0', {},
    map('log.iostream', 'stderr', 'http.status', '500'), '')`,
		quoteIdentifier(database), quoteIdentifier(LogsTable),
		stamped(at), quoteLiteral(traceID), quoteLiteral(integrationProject), resource)
}

func exporterSpanInsert(database, resource string, at time.Time, traceID, spanID, parent string) string {
	return exporterSpanRow(database, resource, at, traceID, spanID, parent,
		"GET /checkout", "Server", integrationProject, 420_500_000, "Error", "boom",
		`map('http.response.status_code', '500', 'http.route', '/checkout')`)
}

func exporterChildSpanInsert(database, resource string, at time.Time, traceID, spanID, parent string) string {
	return exporterSpanRow(database, resource, at, traceID, spanID, parent,
		"SELECT orders", "Client", integrationProject+"-db", 390_000_000, "Unset", "", "{}")
}

func exporterSpanRow(
	database, resource string, at time.Time,
	traceID, spanID, parent, name, kind, service string,
	duration uint64, status, message, attributes string,
) string {
	return fmt.Sprintf(`INSERT INTO %s.%s (
    Timestamp, TraceId, SpanId, ParentSpanId, TraceState, SpanName, SpanKind, ServiceName,
    ResourceAttributes, ScopeName, ScopeVersion, SpanAttributes, Duration, StatusCode, StatusMessage,
    Events.Timestamp, Events.Name, Events.Attributes,
    Links.TraceId, Links.SpanId, Links.TraceState, Links.Attributes
) VALUES (%s, %s, %s, %s, '', %s, %s, %s, %s, 'kitchen', '1.0', %s, %d, %s, %s,
    [], [], [], [], [], [], [])`,
		quoteIdentifier(database), quoteIdentifier(TracesTable),
		stamped(at), quoteLiteral(traceID), quoteLiteral(spanID), quoteLiteral(parent),
		quoteLiteral(name), quoteLiteral(kind), quoteLiteral(service), resource,
		attributes, duration, quoteLiteral(status), quoteLiteral(message))
}

func gaugeValues(resource, metric string, at time.Time, value float64) string {
	return fmt.Sprintf(`(%s, '', 'kitchen', '1.0', {}, 0, '', %s, %s, '', '', {}, %s, %s, %v, 0, [], [], [], [], [])`,
		resource, quoteLiteral(integrationProject), quoteLiteral(metric),
		seconds(at), seconds(at), value)
}

func exporterGaugeInsert(database string, rows []string) string {
	return fmt.Sprintf(`INSERT INTO %s.%s (
    ResourceAttributes, ResourceSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes, ScopeDroppedAttrCount,
    ScopeSchemaUrl, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix,
    TimeUnix, Value, Flags, Exemplars.FilteredAttributes, Exemplars.TimeUnix, Exemplars.Value,
    Exemplars.SpanId, Exemplars.TraceId
) VALUES %s`,
		quoteIdentifier(database), quoteIdentifier(MetricsGaugeTable), strings.Join(rows, ",\n"))
}

func sumValues(resource, metric string, at time.Time, value float64) string {
	return fmt.Sprintf(`(%s, '', 'kitchen', '1.0', {}, 0, '', %s, %s, '', '', {}, %s, %s, %v, 0, [], [], [], [], [], 1, true)`,
		resource, quoteLiteral(integrationProject), quoteLiteral(metric),
		seconds(at), seconds(at), value)
}

func exporterSumInsert(database string, rows []string) string {
	return fmt.Sprintf(`INSERT INTO %s.%s (
    ResourceAttributes, ResourceSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes, ScopeDroppedAttrCount,
    ScopeSchemaUrl, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix,
    TimeUnix, Value, Flags, Exemplars.FilteredAttributes, Exemplars.TimeUnix, Exemplars.Value,
    Exemplars.SpanId, Exemplars.TraceId, AggregationTemporality, IsMonotonic
) VALUES %s`,
		quoteIdentifier(database), quoteIdentifier(MetricsSumTable), strings.Join(rows, ",\n"))
}

func exporterHistogramInsert(database, resource string) string {
	return fmt.Sprintf(`INSERT INTO %s.%s (
    ResourceAttributes, ResourceSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes, ScopeDroppedAttrCount,
    ScopeSchemaUrl, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix,
    TimeUnix, Count, Sum, BucketCounts, ExplicitBounds, Exemplars.FilteredAttributes, Exemplars.TimeUnix,
    Exemplars.Value, Exemplars.SpanId, Exemplars.TraceId, Flags, Min, Max, AggregationTemporality
) VALUES (%s, '', 'kitchen', '1.0', {}, 0, '', %s, 'http.server.duration', '', 'ms', {}, now(), now(),
    3, 1.5, [1, 2], [0.5], [], [], [], [], [], 0, 0.1, 1.0, 2)`,
		quoteIdentifier(database), quoteIdentifier(MetricsHistogramTable),
		resource, quoteLiteral(integrationProject))
}

func exporterExponentialHistogramInsert(database, resource string) string {
	return fmt.Sprintf(`INSERT INTO %s.%s (
    ResourceAttributes, ResourceSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes, ScopeDroppedAttrCount,
    ScopeSchemaUrl, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix,
    TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset,
    NegativeBucketCounts, Exemplars.FilteredAttributes, Exemplars.TimeUnix, Exemplars.Value,
    Exemplars.SpanId, Exemplars.TraceId, Flags, Min, Max, AggregationTemporality
) VALUES (%s, '', 'kitchen', '1.0', {}, 0, '', %s, 'http.client.duration', '', 'ms', {}, now(), now(),
    3, 1.5, 2, 0, 0, [1, 2], 0, [], [], [], [], [], [], 0, 0.1, 1.0, 2)`,
		quoteIdentifier(database), quoteIdentifier(MetricsExponentialHistogramTable),
		resource, quoteLiteral(integrationProject))
}

func exporterSummaryInsert(database, resource string) string {
	return fmt.Sprintf(`INSERT INTO %s.%s (
    ResourceAttributes, ResourceSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes, ScopeDroppedAttrCount,
    ScopeSchemaUrl, ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, StartTimeUnix,
    TimeUnix, Count, Sum, ValueAtQuantiles.Quantile, ValueAtQuantiles.Value, Flags
) VALUES (%s, '', 'kitchen', '1.0', {}, 0, '', %s, 'rpc.duration', '', 'ms', {}, now(), now(),
    3, 1.5, [0.5, 0.95], [1.0, 2.0], 0)`,
		quoteIdentifier(database), quoteIdentifier(MetricsSummaryTable),
		resource, quoteLiteral(integrationProject))
}

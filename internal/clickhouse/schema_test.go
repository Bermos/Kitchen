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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The fixture connection every test in this package builds a client from.
const (
	testDatabase = "kitchen"
	testUsername = "kitchen"
	testPassword = "hunter2"
	// levelError is the severity the fixtures carry, asserted on in enough
	// places that goconst objects to the literal.
	levelError = "error"
)

// fakeStore records the statements it is sent and answers the one query the
// schema code reads back.
type fakeStore struct {
	server   *httptest.Server
	queries  []string
	engine   string
	failWith string
}

func newFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	store := &fakeStore{}
	store.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		query := string(body)
		store.queries = append(store.queries, query)

		if store.failWith != "" {
			http.Error(w, store.failWith, http.StatusBadRequest)
			return
		}
		if strings.Contains(query, "FROM system.tables") {
			_, _ = io.WriteString(w, store.engine+"\n")
			return
		}
	}))
	t.Cleanup(store.server.Close)
	return store
}

func (s *fakeStore) client(t *testing.T) *Client {
	t.Helper()
	endpoint, err := url.Parse(s.server.URL)
	if err != nil {
		t.Fatalf("parsing the test server URL: %v", err)
	}
	return New(Config{
		Host:     endpoint.Hostname(),
		HTTPPort: endpoint.Port(),
		Database: testDatabase,
		Username: testUsername,
		Password: testPassword,
	})
}

func (s *fakeStore) sent(fragment string) bool {
	for _, query := range s.queries {
		if strings.Contains(query, fragment) {
			return true
		}
	}
	return false
}

func (s *fakeStore) transcript() string {
	return strings.Join(s.queries, "\n---\n")
}

// qualified renders a table the way the DDL names it.
func qualified(table string) string {
	return fmt.Sprintf("%s.%s", quoteIdentifier(testDatabase), quoteIdentifier(table))
}

func TestEnsureLogsSchemaCreatesTheTable(t *testing.T) {
	store := newFakeStore(t)
	// A table that does not exist yet reports no engine at all.
	store.engine = ""

	if err := store.client(t).EnsureLogsSchema(context.Background(), 14); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}

	for _, want := range []string{
		"CREATE DATABASE IF NOT EXISTS " + quoteIdentifier(testDatabase),
		"CREATE TABLE IF NOT EXISTS " + qualified(LogsTable),
		// Project-scoped, which is the whole reason the operator owns this DDL
		// rather than letting the exporter create its own.
		"ORDER BY (project, environment, Timestamp)",
		"TTL toDateTime(Timestamp) + toIntervalDay(14)",
		"ttl_only_drop_parts = 1",
	} {
		if !store.sent(want) {
			t.Errorf("expected a statement containing %q, got:\n%s", want, store.transcript())
		}
	}
}

// Kitchen's columns have to be MATERIALIZED, and that is not a style choice:
// a materialized value is computed at insert, which is what lets it sit in the
// ordering key, and it is absent from every INSERT column list, which is what
// lets a stock exporter write this table without knowing Kitchen exists.
func TestKitchenColumnsAreMaterializedAndOrderable(t *testing.T) {
	ddl := createLogsTable(testDatabase, 30)

	for _, want := range []string{
		"project     LowCardinality(String) MATERIALIZED ResourceAttributes['kitchen.project']",
		"environment LowCardinality(String) MATERIALIZED ResourceAttributes['deployment.environment.name']",
		"build       LowCardinality(String) MATERIALIZED ResourceAttributes['kitchen.build']",
		"source      LowCardinality(String) MATERIALIZED ResourceAttributes['kitchen.source']",
		"namespace   LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.namespace.name']",
		"pod         String                 MATERIALIZED ResourceAttributes['k8s.pod.name']",
		"container   LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.container.name']",
		"node        LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.node.name']",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("the log table is missing %q:\n%s", want, ddl)
		}
	}

	// Every OTel table carries the same set, because every one of them is read
	// per project.
	for name, table := range map[string]string{
		LogsTable:         createLogsTable(testDatabase, 30),
		TracesTable:       createTracesTable(testDatabase, 30),
		MetricsGaugeTable: metricsTableDDL(testDatabase, 30)[MetricsGaugeTable],
	} {
		if !strings.Contains(table, kitchenColumns) {
			t.Errorf("%s does not carry the Kitchen columns:\n%s", name, table)
		}
	}
}

// The base columns are the exporter's and are transcribed from its templates.
// A column missing here is every insert failing at runtime, which is invisible
// until a collector is actually running.
func TestTheLogTableCarriesTheExportersColumns(t *testing.T) {
	ddl := createLogsTable(testDatabase, 30)
	for _, column := range []string{
		"Timestamp DateTime64(9)", "TraceId String", "SpanId String", "TraceFlags UInt8",
		"SeverityText LowCardinality(String)", "SeverityNumber UInt8",
		"ServiceName LowCardinality(String)", "Body String",
		"ResourceSchemaUrl LowCardinality(String)",
		"ResourceAttributes Map(LowCardinality(String), String)",
		"ScopeSchemaUrl LowCardinality(String)", "ScopeName String",
		"ScopeVersion LowCardinality(String)",
		"ScopeAttributes Map(LowCardinality(String), String)",
		"LogAttributes Map(LowCardinality(String), String)",
		// Optional upstream, and detected once by a DESC TABLE at collector
		// startup — so it is included rather than added later.
		"EventName String",
	} {
		if !strings.Contains(ddl, column) {
			t.Errorf("the log table is missing the exporter's %q:\n%s", column, ddl)
		}
	}
}

// The schema no longer patches columns into a table an older Kitchen created:
// the old `logs` table is not this one, and an ALTER against it would be a
// statement about a table nothing writes any more.
func TestTheSchemaNoLongerPatchesColumnsInPlace(t *testing.T) {
	store := newFakeStore(t)
	store.engine = "MergeTree TTL toDateTime(Timestamp) + toIntervalDay(30)"

	if err := store.client(t).EnsureTelemetrySchema(context.Background(), 30); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}
	if store.sent("ADD COLUMN") {
		t.Errorf("no column is migrated in place any more, got:\n%s", store.transcript())
	}
	// And the tables it replaced are left to their own TTL rather than dropped
	// out from under whatever is still reading them.
	if store.sent("DROP TABLE") {
		t.Errorf("the pre-collector tables age out, they are not dropped, got:\n%s", store.transcript())
	}
}

func TestEnsureLogsSchemaAltersTTLWhenRetentionChanges(t *testing.T) {
	store := newFakeStore(t)
	// The table exists, retaining 30 days.
	store.engine = "MergeTree PARTITION BY toDate(Timestamp) ORDER BY (project, environment, Timestamp) " +
		"TTL toDateTime(Timestamp) + toIntervalDay(30)"

	if err := store.client(t).EnsureLogsSchema(context.Background(), 7); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}

	want := "ALTER TABLE " + qualified(LogsTable) + " MODIFY TTL toDateTime(Timestamp) + toIntervalDay(7)"
	if !store.sent(want) {
		t.Errorf("expected %q, got:\n%s", want, store.transcript())
	}
}

func TestEnsureLogsSchemaLeavesAMatchingTTLAlone(t *testing.T) {
	store := newFakeStore(t)
	store.engine = "MergeTree TTL toDateTime(Timestamp) + toIntervalDay(30)"

	if err := store.client(t).EnsureLogsSchema(context.Background(), 30); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}

	if store.sent("MODIFY TTL") {
		t.Errorf("the TTL already matched, no TTL change expected, got:\n%s", store.transcript())
	}
}

// A MODIFY TTL naming a column the table does not have is refused rather than
// ignored, and the tables do not agree on what their time column is called:
// the exporter stamps `Timestamp` on logs and traces and `TimeUnix` on every
// metric table, the trace lookup keeps the trace's `Start`, the rollup buckets
// by `bucket`, and the two tables the operator writes itself use `timestamp`.
func TestEveryTableGetsItsOwnTimeColumnInTheTTL(t *testing.T) {
	store := newFakeStore(t)
	store.engine = ""

	if err := store.client(t).EnsureTelemetrySchema(context.Background(), 9); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}

	for table, column := range map[string]string{
		LogsTable:                        timeColumnLogs,
		TracesTable:                      timeColumnLogs,
		TracesIDLookupTable:              timeColumnLookup,
		MetricsGaugeTable:                timeColumnMetrics,
		MetricsSumTable:                  timeColumnMetrics,
		MetricsHistogramTable:            timeColumnMetrics,
		MetricsExponentialHistogramTable: timeColumnMetrics,
		MetricsSummaryTable:              timeColumnMetrics,
		MetricsRollupTable:               timeColumnRollup,
		EventsTable:                      timeColumnKitchen,
		FlowsTable:                       timeColumnKitchen,
	} {
		want := fmt.Sprintf("ALTER TABLE %s MODIFY TTL toDateTime(%s) + toIntervalDay(9)",
			qualified(table), column)
		if !store.sent(want) {
			t.Errorf("expected %q, got:\n%s", want, store.transcript())
		}
	}
}

func TestEnsureTelemetrySchemaCreatesEveryTable(t *testing.T) {
	store := newFakeStore(t)
	store.engine = ""

	if err := store.client(t).EnsureTelemetrySchema(context.Background(), 14); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}

	for _, table := range []string{
		LogsTable, EventsTable, FlowsTable, TracesTable, TracesIDLookupTable,
		MetricsGaugeTable, MetricsSumTable, MetricsHistogramTable,
		MetricsExponentialHistogramTable, MetricsSummaryTable, MetricsRollupTable,
	} {
		want := "CREATE TABLE IF NOT EXISTS " + qualified(table)
		if !store.sent(want) {
			t.Errorf("expected a statement containing %q, got:\n%s", want, store.transcript())
		}
	}
	// The views: one that makes a trace findable by id, and two that fill the
	// rollup from the two tables its numbers live in.
	for _, view := range []string{TracesIDLookupView, MetricsRollupGaugeView, MetricsRollupSumView} {
		want := "CREATE MATERIALIZED VIEW IF NOT EXISTS " + qualified(view)
		if !store.sent(want) {
			t.Errorf("expected a statement containing %q, got:\n%s", want, store.transcript())
		}
	}
}

// The rollup is fed from two tables because its numbers live in two: usage and
// limits are gauges, restarts and OOM kills are delta sums. A view reads one
// table, so there are two of them writing the same key.
func TestTheRollupIsFedFromBothMetricTables(t *testing.T) {
	gauge := createMetricsRollupGaugeView(testDatabase)
	if !strings.Contains(gauge, "FROM "+qualified(MetricsGaugeTable)) {
		t.Errorf("the gauge view should read the gauge table:\n%s", gauge)
	}
	for _, metric := range []string{
		MetricContainerCPUUsage, MetricContainerMemoryWorkingSet,
		MetricContainerCPULimit, MetricContainerMemoryLimit,
	} {
		if !strings.Contains(gauge, quoteLiteral(metric)) {
			t.Errorf("the gauge view should read %s:\n%s", metric, gauge)
		}
	}

	sum := createMetricsRollupSumView(testDatabase)
	if !strings.Contains(sum, "FROM "+qualified(MetricsSumTable)) {
		t.Errorf("the sum view should read the sum table:\n%s", sum)
	}
	for _, metric := range []string{MetricContainerRestartsDelta, MetricContainerOOMKilled} {
		if !strings.Contains(sum, quoteLiteral(metric)) {
			t.Errorf("the sum view should read %s:\n%s", metric, sum)
		}
	}

	// Both write the same key, which is what lets AggregatingMergeTree put the
	// two halves of a bucket back together.
	key := "GROUP BY bucket, project, environment, namespace, pod, container"
	if !strings.Contains(gauge, key) || !strings.Contains(sum, key) {
		t.Error("both rollup views have to group by the same key")
	}
}

func TestEnsureLogsSchemaRejectsNonsenseRetention(t *testing.T) {
	store := newFakeStore(t)

	if err := store.client(t).EnsureLogsSchema(context.Background(), 0); err == nil {
		t.Fatal("expected a retention of 0 days to be rejected")
	}
	if len(store.queries) != 0 {
		t.Errorf("expected no statements to be sent, got %d", len(store.queries))
	}
}

func TestEnsureLogsSchemaSurfacesStoreErrors(t *testing.T) {
	store := newFakeStore(t)
	store.failWith = "Code: 516. DB::Exception: kitchen: Authentication failed"

	err := store.client(t).EnsureLogsSchema(context.Background(), 30)
	if err == nil {
		t.Fatal("expected the store's error to surface")
	}
	if !strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("expected the store's own message in %q", err.Error())
	}
}

func TestQuerySendsCredentialsAndDatabase(t *testing.T) {
	var user, key, database string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user = r.Header.Get("X-ClickHouse-User")
		key = r.Header.Get("X-ClickHouse-Key")
		database = r.URL.Query().Get("database")
		_, _ = io.WriteString(w, "1")
	}))
	t.Cleanup(server.Close)

	endpoint, _ := url.Parse(server.URL)
	client := New(Config{
		Host: endpoint.Hostname(), HTTPPort: endpoint.Port(),
		Database: testDatabase, Username: testUsername, Password: testPassword,
	})

	answer, err := client.Query(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if answer != "1" {
		t.Errorf("answer = %q, want %q", answer, "1")
	}
	if user != testUsername || key != testPassword || database != testDatabase {
		t.Errorf("sent user=%q key=%q database=%q", user, key, database)
	}
}

func TestConfigFromSecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kitchen-clickhouse", Namespace: "kitchen-system"},
		Data: map[string][]byte{
			SecretKeyHost:     []byte("kitchen-clickhouse.kitchen-system.svc"),
			SecretKeyHTTPPort: []byte("8123"),
			SecretKeyDatabase: []byte(testDatabase),
			SecretKeyUsername: []byte(testUsername),
			SecretKeyPassword: []byte(testPassword),
		},
	}

	cfg, err := ConfigFromSecret(secret)
	if err != nil {
		t.Fatalf("ConfigFromSecret: %v", err)
	}
	if cfg.Host != "kitchen-clickhouse.kitchen-system.svc" || cfg.HTTPPort != "8123" {
		t.Errorf("unexpected config %+v", cfg)
	}

	delete(secret.Data, SecretKeyHost)
	delete(secret.Data, SecretKeyUsername)
	_, err = ConfigFromSecret(secret)
	if err == nil {
		t.Fatal("expected missing keys to be reported")
	}
	if !strings.Contains(err.Error(), "host, username") {
		t.Errorf("expected both missing keys in %q", err.Error())
	}

	// A database name that is not an identifier would be interpolated into
	// DDL; it is rejected rather than quoted and hoped for.
	secret.Data[SecretKeyHost] = []byte("clickhouse")
	secret.Data[SecretKeyUsername] = []byte(testUsername)
	secret.Data[SecretKeyDatabase] = []byte("kitchen`; DROP DATABASE kitchen; --")
	if _, err := ConfigFromSecret(secret); err == nil {
		t.Fatal("expected an unusable database name to be rejected")
	}
}

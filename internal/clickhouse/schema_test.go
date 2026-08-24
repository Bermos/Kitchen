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
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Bermos/Kitchen/internal/retention"
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

// sentNear is sent() for two fragments that have to be in the *same*
// statement. A DDL that names one table and a TTL that belongs to another
// would satisfy two separate sent() calls and mean nothing.
func (s *fakeStore) sentNear(first, second string) bool {
	for _, query := range s.queries {
		if strings.Contains(query, first) && strings.Contains(query, second) {
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

	if err := store.client(t).EnsureLogsSchema(context.Background(), 14, 14); err != nil {
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
	ddl := createLogsTable(testDatabase, 30, 30)

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
		LogsTable:         createLogsTable(testDatabase, 30, 30),
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
	ddl := createLogsTable(testDatabase, 30, 30)
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

// The pre-collector tables are left alone. The old `logs`, `traces` and
// `metrics` tables are not the ones this schema owns, so neither an ALTER nor a
// DROP against them would be a statement about anything Kitchen still writes —
// they age out on their own TTL, and an operator who wants the disk back can
// drop them by hand.
func TestTheSchemaLeavesThePreCollectorTablesAlone(t *testing.T) {
	store := newFakeStore(t)
	store.engine = singleLogTTL

	if err := store.client(t).EnsureTelemetrySchema(context.Background(), retention.Uniform(30)); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}
	for _, table := range []string{"logs", "traces", "metrics"} {
		for _, statement := range []string{"ALTER TABLE " + qualified(table), "DROP TABLE " + qualified(table)} {
			if store.sent(statement) {
				t.Errorf("the pre-collector tables are not touched, got %q in:\n%s", statement, store.transcript())
			}
		}
	}
}

// A column added to a table an earlier Kitchen already created reaches that
// installation only through an ALTER: `CREATE TABLE IF NOT EXISTS` does not
// reshape an existing table. So the ADD COLUMNs go out on every pass — they are
// `IF NOT EXISTS` and cost nothing on the table the CREATE just made — and this
// is what says so.
func TestTheSchemaAddsLaterColumnsToTablesThatPredateThem(t *testing.T) {
	store := newFakeStore(t)
	store.engine = singleLogTTL

	if err := store.client(t).EnsureTelemetrySchema(context.Background(), retention.Uniform(30)); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}
	for _, want := range []string{
		"ALTER TABLE " + qualified(LogsTable) + " ADD COLUMN IF NOT EXISTS `process`",
		"ALTER TABLE " + qualified(LogsTable) + " ADD COLUMN IF NOT EXISTS `run`",
		"ALTER TABLE " + qualified(EventsTable) + " ADD COLUMN IF NOT EXISTS `process`",
		"ALTER TABLE " + qualified(EventsTable) + " ADD COLUMN IF NOT EXISTS `run`",
	} {
		if !store.sent(want) {
			t.Errorf("the schema never sent %q:\n%s", want, store.transcript())
		}
	}
}

// Two spellings of one column is how a fresh installation and an upgraded one
// come to disagree about what a query means, so the ALTER's definition has to
// be the CREATE's, character for character.
func TestTheAddedColumnsMatchTheirCreateDefinitions(t *testing.T) {
	// The DDL pads its columns into a readable block; the comparison is over
	// what the statement says, not over how it is laid out.
	defines := func(ddl string, column addedColumn) bool {
		return strings.Contains(strings.Join(strings.Fields(ddl), " "),
			column.name+" "+strings.Join(strings.Fields(column.definition), " "))
	}
	for _, table := range []struct {
		ddl     string
		columns []addedColumn
	}{
		{createLogsTable(testDatabase, 30, 30), kitchenColumnsAdded},
		{createTracesTable(testDatabase, 30), kitchenColumnsAdded},
		{createEventsTable(testDatabase, 30), eventColumnsAdded},
	} {
		for _, column := range table.columns {
			if !defines(table.ddl, column) {
				t.Errorf("a CREATE does not define %q as the ALTER does (%q):\n%s",
					column.name, column.definition, table.ddl)
			}
		}
	}
}

func TestEnsureLogsSchemaAltersTTLWhenRetentionChanges(t *testing.T) {
	store := newFakeStore(t)
	// The table exists, retaining 30 days.
	store.engine = "MergeTree PARTITION BY toDate(Timestamp) ORDER BY (project, environment, Timestamp) " +
		"TTL toDateTime(Timestamp) + toIntervalDay(30)"

	if err := store.client(t).EnsureLogsSchema(context.Background(), 7, 7); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}

	want := "ALTER TABLE " + qualified(LogsTable) + " MODIFY TTL toDateTime(Timestamp) + toIntervalDay(7)"
	if !store.sent(want) {
		t.Errorf("expected %q, got:\n%s", want, store.transcript())
	}
}

func TestEnsureLogsSchemaLeavesAMatchingTTLAlone(t *testing.T) {
	store := newFakeStore(t)
	store.engine = singleLogTTL

	if err := store.client(t).EnsureLogsSchema(context.Background(), 30, 30); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}

	if store.sent("MODIFY TTL") {
		t.Errorf("the TTL already matched, no TTL change expected, got:\n%s", store.transcript())
	}
}

// A MODIFY TTL naming a column the table does not have is refused rather than
// ignored, and the tables do not agree on what their time column is called:
// the exporter stamps `Timestamp` on logs and traces and `TimeUnix` on every
// metric table, the trace lookup keeps the trace's `Start`, the rollups bucket
// by `bucket`, and the tables the operator writes itself use `timestamp` —
// except the request table, which is spelled the exporter's way.
//
// The days differ too, and only for the requests: §5 derives their retention
// from the one knob rather than applying it, so a MODIFY TTL that used the
// knob directly would quietly reset the ratios on every reconcile.
func TestEveryTableGetsItsOwnTimeColumnInTheTTL(t *testing.T) {
	store := newFakeStore(t)
	store.engine = ""

	if err := store.client(t).EnsureTelemetrySchema(context.Background(), retention.Uniform(9)); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}

	type retention struct {
		column string
		days   int32
	}
	for table, ttl := range map[string]retention{
		LogsTable:                        {timeColumnLogs, 9},
		TracesTable:                      {timeColumnLogs, 9},
		TracesIDLookupTable:              {timeColumnLookup, 9},
		MetricsGaugeTable:                {timeColumnMetrics, 9},
		MetricsSumTable:                  {timeColumnMetrics, 9},
		MetricsHistogramTable:            {timeColumnMetrics, 9},
		MetricsExponentialHistogramTable: {timeColumnMetrics, 9},
		MetricsSummaryTable:              {timeColumnMetrics, 9},
		MetricsRollupTable:               {timeColumnRollup, 9},
		EventsTable:                      {timeColumnKitchen, 9},
		FlowsTable:                       {timeColumnKitchen, 9},
		K8sEventsTable:                   {timeColumnKitchen, 9},
		RequestsTable:                    {timeColumnRequests, 7},
		RequestsMinuteTable:              {timeColumnRollup, 9},
		RequestsHourTable:                {timeColumnRollup, 108},
	} {
		want := fmt.Sprintf("ALTER TABLE %s MODIFY TTL toDateTime(%s) + toIntervalDay(%d)",
			qualified(table), ttl.column, ttl.days)
		if !store.sent(want) {
			t.Errorf("expected %q, got:\n%s", want, store.transcript())
		}
	}
}

// The raw rows are the densest thing in the store and the shortest-lived, and
// a retention below a week takes them with it rather than leaving one table
// outliving the knob.
func TestRequestRetentionIsDerivedFromTheOneKnob(t *testing.T) {
	for _, retention := range []struct {
		configured int32
		raw        int32
		hourly     int32
	}{
		{configured: 30, raw: 7, hourly: 360},
		{configured: 7, raw: 7, hourly: 84},
		{configured: 3, raw: 3, hourly: 36},
		{configured: 1, raw: 1, hourly: 12},
	} {
		if got := rawRequestRetention(retention.configured); got != retention.raw {
			t.Errorf("raw requests at a retention of %d: got %d days, want %d",
				retention.configured, got, retention.raw)
		}
		if got := hourlyRequestRetention(retention.configured); got != retention.hourly {
			t.Errorf("the hour rollup at a retention of %d: got %d days, want %d",
				retention.configured, got, retention.hourly)
		}
	}

	// A retention nothing would ever configure still has to produce a TTL that
	// keeps rows: an int32 that wrapped would come back negative, and a
	// negative TTL expires every row as it is written.
	if got := hourlyRequestRetention(math.MaxInt32); got < 1 {
		t.Errorf("the hour rollup's retention wrapped to %d", got)
	}
}

// Both rollup views read the raw table. Feeding the hour rollup from the
// minute one would be re-aggregating aggregate states through columns named
// after the columns they aggregate, which is the shadowing that made the
// metrics rollup's view refuse to be created at all.
func TestBothRequestViewsReadTheRawTable(t *testing.T) {
	for name, view := range map[string]string{
		RequestsMinuteView: createRequestsMinuteView(testDatabase),
		RequestsHourView:   createRequestsHourView(testDatabase),
	} {
		if !strings.Contains(view, "FROM "+qualified(RequestsTable)+" AS r") {
			t.Errorf("%s should read the raw request table:\n%s", name, view)
		}
		for _, rollup := range []string{RequestsMinuteTable, RequestsHourTable} {
			if strings.Contains(view, "FROM "+qualified(rollup)) {
				t.Errorf("%s reads a rollup rather than the raw table:\n%s", name, view)
			}
		}
		// Every argument is qualified, which is what keeps the state columns
		// nameable after the columns they aggregate.
		if !strings.Contains(view, "quantilesTDigestState(0.5, 0.95, 0.99)(r.duration_ms)") {
			t.Errorf("%s should read the raw duration through the table's alias:\n%s", name, view)
		}
		if !strings.Contains(view, "countState() AS requests") {
			t.Errorf("%s should keep the count as a state:\n%s", name, view)
		}
	}

	if !strings.Contains(createRequestsMinuteView(testDatabase), "toStartOfMinute(r.Timestamp)") {
		t.Error("the minute view should bucket by the minute")
	}
	if !strings.Contains(createRequestsHourView(testDatabase), "toStartOfHour(r.Timestamp)") {
		t.Error("the hour view should bucket by the hour")
	}
}

// Every product query is project-scoped, so every product table's ordering key
// leads with the project. The rollups add the dimensions the screens group by,
// because a key that stopped at the bucket would collapse them into one row.
func TestTheRequestTablesAreOrderedForProjectScopedReads(t *testing.T) {
	raw := createRequestsTable(testDatabase, 7)
	if !strings.Contains(raw, "ORDER BY (project, environment, Timestamp)") {
		t.Errorf("the request table is not ordered for a project's reads:\n%s", raw)
	}
	if !strings.Contains(raw, "PARTITION BY toDate(Timestamp)") {
		t.Errorf("the request table should partition by the day:\n%s", raw)
	}
	// The route is a dictionary because the follower bounds the set; the path
	// is not, because it is unbounded by definition.
	if !strings.Contains(raw, "route         LowCardinality(String)") ||
		!strings.Contains(raw, "path          String CODEC(ZSTD(1))") {
		t.Errorf("the request table's route and path types are the cardinality argument:\n%s", raw)
	}

	key := "ORDER BY (project, environment, bucket, host, route, method, status)"
	for name, ddl := range map[string]string{
		RequestsMinuteTable: createRequestsMinuteTable(testDatabase, 30),
		RequestsHourTable:   createRequestsHourTable(testDatabase, 360),
	} {
		if !strings.Contains(ddl, key) {
			t.Errorf("%s should be keyed %s:\n%s", name, key, ddl)
		}
		if !strings.Contains(ddl, "ENGINE = AggregatingMergeTree") {
			t.Errorf("%s has to merge states rather than rows:\n%s", name, ddl)
		}
		if !strings.Contains(ddl, "duration      AggregateFunction(quantilesTDigest(0.5, 0.95, 0.99), Float64)") {
			t.Errorf("%s should keep the latency as a mergeable digest:\n%s", name, ddl)
		}
	}

	// A year of daily partitions for a table holding a few thousand rows a day
	// is 365 partitions; the hour rollup is monthly for that reason.
	if !strings.Contains(createRequestsMinuteTable(testDatabase, 30), "PARTITION BY toDate(bucket)") {
		t.Error("the minute rollup should partition by the day")
	}
	if !strings.Contains(createRequestsHourTable(testDatabase, 360), "PARTITION BY toYYYYMM(bucket)") {
		t.Error("the hour rollup should partition by the month")
	}
}

// The cluster's events are the operator's own table, keyed like every other
// product table even though its most interesting rows — the ones explaining an
// install that never came up — belong to no project at all.
func TestTheClusterEventTableKeepsThePlatformsOwnEvents(t *testing.T) {
	ddl := createK8sEventsTable(testDatabase, 30)
	if !strings.Contains(ddl, "ORDER BY (project, environment, timestamp)") {
		t.Errorf("the cluster event table is not ordered for a project's reads:\n%s", ddl)
	}
	for _, column := range []string{
		"timestamp   DateTime64(3, 'UTC')", "namespace   LowCardinality(String)",
		"kind        LowCardinality(String)", "reason      LowCardinality(String)",
		"count       UInt32", "node        LowCardinality(String)",
	} {
		if !strings.Contains(ddl, column) {
			t.Errorf("the cluster event table is missing %q:\n%s", column, ddl)
		}
	}
}

func TestEnsureTelemetrySchemaCreatesEveryTable(t *testing.T) {
	store := newFakeStore(t)
	store.engine = ""

	if err := store.client(t).EnsureTelemetrySchema(context.Background(), retention.Uniform(14)); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}

	for _, table := range []string{
		LogsTable, EventsTable, FlowsTable, TracesTable, TracesIDLookupTable,
		MetricsGaugeTable, MetricsSumTable, MetricsHistogramTable,
		MetricsExponentialHistogramTable, MetricsSummaryTable, MetricsRollupTable,
		RequestsTable, RequestsMinuteTable, RequestsHourTable, K8sEventsTable,
	} {
		want := "CREATE TABLE IF NOT EXISTS " + qualified(table)
		if !store.sent(want) {
			t.Errorf("expected a statement containing %q, got:\n%s", want, store.transcript())
		}
	}
	// The views: one that makes a trace findable by id, two that fill the usage
	// rollup from the two tables its numbers live in, and one per request
	// rollup.
	for _, view := range []string{
		TracesIDLookupView, MetricsRollupGaugeView, MetricsRollupSumView,
		RequestsMinuteView, RequestsHourView,
	} {
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

	if err := store.client(t).EnsureLogsSchema(context.Background(), 0, 0); err == nil {
		t.Fatal("expected a retention of 0 days to be rejected")
	}
	if len(store.queries) != 0 {
		t.Errorf("expected no statements to be sent, got %d", len(store.queries))
	}
}

func TestEnsureLogsSchemaSurfacesStoreErrors(t *testing.T) {
	store := newFakeStore(t)
	store.failWith = "Code: 516. DB::Exception: kitchen: Authentication failed"

	err := store.client(t).EnsureLogsSchema(context.Background(), 30, 30)
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

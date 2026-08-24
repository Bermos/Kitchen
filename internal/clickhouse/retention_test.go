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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/retention"
)

// kitchenWithRetention is a singleton whose classes are set one by one, which
// is what "configurable per class" has to mean at the store.
func kitchenWithRetention(days map[retention.Class]int32) *kitchenv1alpha1.Kitchen {
	kitchen := &kitchenv1alpha1.Kitchen{}
	spec := &kitchen.Spec.Retention
	for class, value := range days {
		set := value
		switch class {
		case retention.ClassContainerLogs:
			spec.ContainerLogs = &set
		case retention.ClassBuildLogs:
			spec.BuildLogs = &set
		case retention.ClassFlows:
			spec.Flows = &set
		case retention.ClassMetrics:
			spec.Metrics = &set
		case retention.ClassTraces:
			spec.Traces = &set
		case retention.ClassRequests:
			spec.Requests = &set
		case retention.ClassClusterEvents:
			spec.ClusterEvents = &set
		case retention.ClassActivity:
			spec.Activity = &set
		case retention.ClassAudit:
			spec.Audit = &set
		}
	}
	return kitchen
}

// singleLogTTL is the log table's DDL as it reads with one date for both
// classes, which is what several of these tests start from.
const singleLogTTL = "MergeTree TTL toDateTime(Timestamp) + toIntervalDay(30)"

// splitLogTTL is the same table once the two classes disagree.
const splitLogTTL = singleLogTTL + " DELETE WHERE source != 'build', " +
	"toDateTime(Timestamp) + toIntervalDay(90) DELETE WHERE source = 'build'"

// Per-class retention where it is enforced: the store.
//
// Two classes share the log table, which is the whole of what is interesting
// here. One date over that table is a part drop and costs nothing; two dates
// need row-level expiry, and getting the mode wrong would make the shorter of
// the two a promise the store quietly does not keep.

// TestOneDateOverTheLogTableKeepsThePartDropMode is the common case: both
// classes the same, one TTL, `ttl_only_drop_parts` on.
func TestOneDateOverTheLogTableKeepsThePartDropMode(t *testing.T) {
	ddl := createLogsTable(testDatabase, 30, 30)
	if !strings.Contains(ddl, "TTL toDateTime(Timestamp) + toIntervalDay(30)\n") {
		t.Errorf("the log table's TTL is not the single expected form:\n%s", ttlLineOf(ddl))
	}
	if strings.Contains(ddl, "DELETE WHERE") {
		t.Errorf("two classes with one date produced a conditional TTL:\n%s", ttlLineOf(ddl))
	}
	if !strings.Contains(ddl, "ttl_only_drop_parts = 1") {
		t.Errorf("the log table lost the part-drop mode with one date:\n%s", ddl[len(ddl)-200:])
	}
}

// TestTwoDatesOverTheLogTableSplitByClassAndDropThePartMode is the case the
// feature exists for, and the one with the trap in it.
func TestTwoDatesOverTheLogTableSplitByClassAndDropThePartMode(t *testing.T) {
	ddl := createLogsTable(testDatabase, 30, 90)

	if !strings.Contains(ddl, "toIntervalDay(30) DELETE WHERE source != 'build'") {
		t.Errorf("container logs are not expired on their own date:\n%s", ttlLineOf(ddl))
	}
	if !strings.Contains(ddl, "toIntervalDay(90) DELETE WHERE source = 'build'") {
		t.Errorf("build logs are not expired on their own date:\n%s", ttlLineOf(ddl))
	}
	// With two rules a part almost never holds only expired rows, so
	// only-drop-parts would silently expire the shorter class at the longer
	// date — the exact failure this mode switch exists to prevent.
	if !strings.Contains(ddl, "ttl_only_drop_parts = 0") {
		t.Errorf("two dates over one table kept the part-drop mode, so the shorter class is not enforced:\n%s",
			ddl[len(ddl)-200:])
	}
}

// TestTheTwoConditionsAreExactComplements. A row matching both rules would
// have two dates and no answer; a row matching neither would never expire.
func TestTheTwoConditionsAreExactComplements(t *testing.T) {
	if buildLogsCondition != "source = 'build'" || containerLogsCondition != "source != 'build'" {
		t.Fatalf("the log table's two conditions are %q and %q, which are not complements",
			containerLogsCondition, buildLogsCondition)
	}
}

// TestASplitRetentionIsAppliedToAnExistingTable: the ALTER path, not the
// CREATE one. A table created when the two classes agreed has one interval,
// and moving one class has to rewrite the whole TTL and the setting with it.
func TestASplitRetentionIsAppliedToAnExistingTable(t *testing.T) {
	store := newFakeStore(t)
	store.engine = singleLogTTL

	if err := store.client(t).EnsureLogsSchema(context.Background(), 30, 90); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}
	if !store.sent("MODIFY TTL toDateTime(Timestamp) + toIntervalDay(30) DELETE WHERE source != 'build', " +
		"toDateTime(Timestamp) + toIntervalDay(90) DELETE WHERE source = 'build'") {
		t.Errorf("the split TTL was not applied to the existing table:\n%s", store.transcript())
	}
	if !store.sent("MODIFY SETTING ttl_only_drop_parts = 0") {
		t.Errorf("the part-drop mode was left on for a two-date table:\n%s", store.transcript())
	}
}

// TestAMatchingSplitRetentionIsLeftAlone. The reconcile runs this on every
// pass, and an ALTER on every pass would be a merge storm the platform
// performed on itself.
func TestAMatchingSplitRetentionIsLeftAlone(t *testing.T) {
	store := newFakeStore(t)
	store.engine = splitLogTTL

	if err := store.client(t).EnsureLogsSchema(context.Background(), 30, 90); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}
	if store.sent("MODIFY TTL") {
		t.Errorf("a matching split TTL was reapplied:\n%s", store.transcript())
	}
}

// TestGoingBackToOneDateRestoresThePartDropMode: the reverse move has to put
// the cheap mode back, or an installation that tried a split retention pays
// for it forever.
func TestGoingBackToOneDateRestoresThePartDropMode(t *testing.T) {
	store := newFakeStore(t)
	store.engine = splitLogTTL

	if err := store.client(t).EnsureLogsSchema(context.Background(), 30, 30); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}
	if !store.sent("MODIFY SETTING ttl_only_drop_parts = 1") {
		t.Errorf("the part-drop mode was not restored:\n%s", store.transcript())
	}
}

// TestEveryTelemetryClassReachesItsOwnTable is the enforcement half of the
// first acceptance criterion: nine numbers configured, and each one applied
// where that class lives.
func TestEveryTelemetryClassReachesItsOwnTable(t *testing.T) {
	store := newFakeStore(t)
	kitchen := kitchenWithRetention(map[retention.Class]int32{
		retention.ClassContainerLogs: 11,
		retention.ClassBuildLogs:     22,
		retention.ClassFlows:         33,
		retention.ClassMetrics:       44,
		retention.ClassTraces:        55,
		retention.ClassRequests:      66,
		retention.ClassClusterEvents: 77,
		retention.ClassActivity:      88,
	})

	if err := store.client(t).EnsureTelemetrySchema(context.Background(), retention.Resolve(kitchen)); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}
	for _, want := range []struct {
		class    retention.Class
		table    string
		interval string
	}{
		{retention.ClassContainerLogs, LogsTable, "toIntervalDay(11) DELETE WHERE source != 'build'"},
		{retention.ClassBuildLogs, LogsTable, "toIntervalDay(22) DELETE WHERE source = 'build'"},
		{retention.ClassFlows, FlowsTable, "toIntervalDay(33)"},
		{retention.ClassMetrics, MetricsGaugeTable, "toIntervalDay(44)"},
		{retention.ClassTraces, TracesTable, "toIntervalDay(55)"},
		{retention.ClassRequests, RequestsMinuteTable, "toIntervalDay(66)"},
		{retention.ClassClusterEvents, K8sEventsTable, "toIntervalDay(77)"},
		{retention.ClassActivity, EventsTable, "toIntervalDay(88)"},
	} {
		if !store.sentNear(qualified(want.table), want.interval) {
			t.Errorf("%s was configured for its own retention and %s never got %q:\n%s",
				want.class, want.table, want.interval, store.transcript())
		}
	}
}

// TestTheAuditClassIsNotPartOfTheTelemetrySchema. Turning telemetry down is a
// storage decision and shortening the evidence is a records one; the two
// arrive on different calls precisely so that one cannot do the other.
func TestTheAuditClassIsNotPartOfTheTelemetrySchema(t *testing.T) {
	store := newFakeStore(t)
	if err := store.client(t).EnsureTelemetrySchema(context.Background(), retention.Uniform(7)); err != nil {
		t.Fatalf("EnsureTelemetrySchema: %v", err)
	}
	if store.sent(qualified(AuditTable)) {
		t.Errorf("the telemetry schema touched the audit table:\n%s", store.transcript())
	}
}

// TestTheAuditTableLosesItsMutationPrivileges is the immutability claim, at
// exactly the size the docs state it: the platform's own credential can append
// and cannot rewrite.
func TestTheAuditTableLosesItsMutationPrivileges(t *testing.T) {
	store := newFakeStore(t)
	result := store.client(t).EnsureAuditImmutability(context.Background())

	if !result.Revoked {
		t.Fatalf("the revoke did not take: %s", result.Message)
	}
	for _, privilege := range []string{"ALTER UPDATE", "ALTER DELETE", "TRUNCATE", "DROP TABLE"} {
		if !store.sent(privilege) {
			t.Errorf("%s was not revoked:\n%s", privilege, store.transcript())
		}
	}
	if !store.sent("REVOKE") || !store.sent(qualified(AuditTable)) {
		t.Errorf("nothing was revoked on the audit table:\n%s", store.transcript())
	}
	// INSERT and SELECT are what the platform needs and must survive, and
	// ALTER TTL is what keeps the retention in step: revoking any of the
	// three would break the log rather than protect it.
	for _, kept := range []string{"INSERT", "SELECT", "ALTER TTL"} {
		if store.sent(kept) {
			t.Errorf("%s was revoked, which stops the platform using its own log:\n%s", kept, store.transcript())
		}
	}
}

// TestARefusedRevokeIsReportedAndNotFatal. An installation pointing Kitchen at
// a ClickHouse it does not administer keeps working, with a smaller and
// honestly stated claim about its log.
func TestARefusedRevokeIsReportedAndNotFatal(t *testing.T) {
	store := newFakeStore(t)
	store.failWith = "Not enough privileges"

	result := store.client(t).EnsureAuditImmutability(context.Background())
	if result.Revoked {
		t.Fatal("a refused revoke reported success")
	}
	if !strings.Contains(result.Message, "hash chain") {
		t.Errorf("the message does not say what the installation is left relying on: %s", result.Message)
	}
}

// TestAUserNameThatCannotBeSpelledSafelyIsNotSpelled. The user name comes out
// of a Secret and reaches a statement as an identifier, so anything unusual is
// left alone rather than quoted and hoped for.
func TestAUserNameThatCannotBeSpelledSafelyIsNotSpelled(t *testing.T) {
	store := newFakeStore(t)
	client := New(Config{
		Host:     hostOf(t, store),
		HTTPPort: portOf(t, store),
		Database: testDatabase,
		Username: "kitchen`; DROP TABLE audit_log; --",
	})

	result := client.EnsureAuditImmutability(context.Background())
	if result.Revoked {
		t.Fatal("a user name this package cannot spell was spelled anyway")
	}
	if store.sent("REVOKE") {
		t.Errorf("a statement was sent for an unusable user name:\n%s", store.transcript())
	}
}

// TestTheSweepMeasuresEveryClassAgainstItsOwnHorizon is the evidence half of
// the third acceptance criterion: what the record contains.
func TestTheSweepMeasuresEveryClassAgainstItsOwnHorizon(t *testing.T) {
	store := newMeasuringStore(t, 1200, 3, "2026-05-01T00:00:00Z")
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	observations := store.client(t).SweepRetention(context.Background(), retention.Uniform(30), now)
	if len(observations) != len(retention.Definitions()) {
		t.Fatalf("the sweep produced %d observations for %d classes",
			len(observations), len(retention.Definitions()))
	}
	for _, observation := range observations {
		if observation.Days != 30 {
			t.Errorf("%s was measured against %d days, want 30", observation.Class, observation.Days)
		}
		if want := now.AddDate(0, 0, -30); !observation.Horizon.Equal(want) {
			t.Errorf("%s carries the horizon %s, want %s", observation.Class, observation.Horizon, want)
		}
		if observation.Rows != 1200 {
			t.Errorf("%s reports %d rows, want 1200", observation.Class, observation.Rows)
		}
		if observation.Oldest == nil {
			t.Errorf("%s reports no oldest row, so it makes no claim about how far back it goes",
				observation.Class)
		}
	}
}

// TestTheSweepNeverDeletesWhatItCannotAttributeToOneClass. The audit table is
// never dropped from — a sweeper that could would be one that can delete the
// record of its own deletions — and neither is the log table, which two
// classes share.
func TestTheSweepNeverDeletesWhatItCannotAttributeToOneClass(t *testing.T) {
	store := newMeasuringStore(t, 10, 0, "2026-05-01T00:00:00Z")
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	store.client(t).SweepRetention(context.Background(), retention.Uniform(30), now)
	for _, protected := range []string{AuditTable, LogsTable} {
		if store.sentNear("DROP PARTITION", qualified(protected)) {
			t.Errorf("the sweep dropped a partition of %s:\n%s", protected, store.transcript())
		}
	}
}

// TestTheSweepDropsOnlyWhollyExpiredPartitions, and counts exactly what went.
func TestTheSweepDropsOnlyWhollyExpiredPartitions(t *testing.T) {
	store := newMeasuringStore(t, 500, 0, "2026-07-01T00:00:00Z")
	store.expiredPartitions = `{"partition":"2026-07-01","rows":300}
{"partition":"2026-07-02","rows":200}`
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	observations := store.client(t).SweepRetention(context.Background(), retention.Uniform(30), now)
	for _, observation := range observations {
		definition, _ := retention.DefinitionFor(observation.Class)
		if !definition.Sweepable {
			continue
		}
		if observation.Removed != 500 || observation.Partitions != 2 {
			t.Errorf("%s removed %d rows over %d partitions, want 500 over 2",
				observation.Class, observation.Removed, observation.Partitions)
		}
	}
	if !store.sent("DROP PARTITION '2026-07-01'") {
		t.Errorf("the wholly expired partition was not dropped:\n%s", store.transcript())
	}
}

// TestAPartitionNameTheSchemaDidNotWriteIsLeftAlone. The name comes back from
// the store and goes into a DROP, so a table somebody re-partitioned by hand
// is one the sweep does not touch.
func TestAPartitionNameTheSchemaDidNotWriteIsLeftAlone(t *testing.T) {
	store := newMeasuringStore(t, 10, 0, "2026-07-01T00:00:00Z")
	store.expiredPartitions = `{"partition":"tuple()","rows":9}`
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	store.client(t).SweepRetention(context.Background(), retention.Uniform(30), now)
	if store.sent("DROP PARTITION") {
		t.Errorf("a partition name this schema never wrote was dropped:\n%s", store.transcript())
	}
}

// TestAClassThatCannotBeMeasuredSaysSoRatherThanReadingEmpty. "We hold
// nothing" and "we could not ask" are the two answers a retention record must
// never confuse.
func TestAClassThatCannotBeMeasuredSaysSoRatherThanReadingEmpty(t *testing.T) {
	store := newFakeStore(t)
	store.failWith = "the store is down"
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	observations := store.client(t).SweepRetention(context.Background(), retention.Uniform(30), now)
	if len(observations) == 0 {
		t.Fatal("a store that is down produced no observations at all, so the sweep left no record")
	}
	for _, observation := range observations {
		if observation.Measured() {
			t.Errorf("%s reads as measured against a store that refused every query", observation.Class)
		}
	}
}

// measuringStore answers the two reads the sweep makes.
type measuringStore struct {
	*fakeStore
	rows              int64
	expired           int64
	oldest            string
	expiredPartitions string
}

func newMeasuringStore(t *testing.T, rows, expired int64, oldest string) *measuringStore {
	t.Helper()
	store := &measuringStore{
		fakeStore: &fakeStore{},
		rows:      rows,
		expired:   expired,
		oldest:    oldest,
	}
	store.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		query := string(body)
		store.queries = append(store.queries, query)

		switch {
		case strings.Contains(query, "FROM system.parts"):
			_, _ = io.WriteString(w, store.expiredPartitions)
		case strings.Contains(query, "AS oldest"):
			_, _ = io.WriteString(w, `{"rows":`+strconv.FormatInt(store.rows, 10)+`,"expired":`+strconv.FormatInt(store.expired, 10)+
				`,"oldest":"`+store.oldest+`"}`)
		}
	}))
	t.Cleanup(store.server.Close)
	return store
}

func hostOf(t *testing.T, store *fakeStore) string {
	t.Helper()
	endpoint, err := url.Parse(store.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint.Hostname()
}

func portOf(t *testing.T, store *fakeStore) string {
	t.Helper()
	endpoint, err := url.Parse(store.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint.Port()
}

// ttlLineOf pulls the TTL out of a DDL for a readable failure.
func ttlLineOf(ddl string) string {
	for _, line := range strings.Split(ddl, "\n") {
		if strings.HasPrefix(line, "TTL ") {
			return line
		}
	}
	return "(no TTL line)"
}

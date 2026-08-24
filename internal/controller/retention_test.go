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

package controller

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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/retention"
)

// The retention sweep, acceptance criterion by acceptance criterion:
//
//   - retention is configurable per class and enforced (the enforcement half
//     is internal/clickhouse; what is here is that the model reaches it and
//     that the status says what is in force);
//   - deletion on retention expiry produces a record;
//   - the record cannot itself be the thing that gets deleted;
//   - and the audit floor holds (internal/api and the CRD's own CEL rule carry
//     the refusals; internal/retention carries the number).
//
// The clock-drift criterion is in clocksync_test.go.

const retentionSecretName = "kitchen-clickhouse"

// retentionFixtures is a platform with a store that answers the sweep's two
// reads, and the sweeper wired to it.
type retentionFixtures struct {
	client  client.Client
	sweeper *RetentionSweeper
	store   *fakeTelemetryStore
	now     time.Time
}

// fakeTelemetryStore answers the queries the sweep makes and remembers what it
// was asked, which is how "the sweep never deletes what it cannot attribute to
// one class" is checked at this level too.
type fakeTelemetryStore struct {
	server     *httptest.Server
	statements []string

	rows              int64
	expired           int64
	oldest            string
	expiredPartitions string
}

func newFakeTelemetryStore(t *testing.T) *fakeTelemetryStore {
	t.Helper()
	store := &fakeTelemetryStore{rows: 4200, oldest: "2026-07-25T09:00:00Z"}
	store.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		query := string(body)
		store.statements = append(store.statements, query)

		switch {
		case strings.Contains(query, "FROM system.parts"):
			_, _ = io.WriteString(w, store.expiredPartitions)
		case strings.Contains(query, "AS oldest"):
			_, _ = io.WriteString(w, `{"rows":4200,"expired":`+
				strconv.FormatInt(store.expired, 10)+`,"oldest":"`+store.oldest+`"}`)
		}
	}))
	t.Cleanup(store.server.Close)
	return store
}

func (s *fakeTelemetryStore) sent(fragment string) bool {
	for _, statement := range s.statements {
		if strings.Contains(statement, fragment) {
			return true
		}
	}
	return false
}

func newRetentionFixtures(t *testing.T, kitchen *kitchenv1alpha1.Kitchen) *retentionFixtures {
	t.Helper()

	store := newFakeTelemetryStore(t)
	endpoint, err := url.Parse(store.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: PlatformNamespace, Name: retentionSecretName},
		Data: map[string][]byte{
			clickhouse.SecretKeyHost:     []byte(endpoint.Hostname()),
			clickhouse.SecretKeyHTTPPort: []byte(endpoint.Port()),
			clickhouse.SecretKeyDatabase: []byte("kitchen"),
			clickhouse.SecretKeyUsername: []byte("kitchen"),
			clickhouse.SecretKeyPassword: []byte("hunter2"),
		},
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(kitchen, secret).
		WithStatusSubresource(&kitchenv1alpha1.Kitchen{}).
		Build()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	return &retentionFixtures{
		client: c,
		store:  store,
		now:    now,
		sweeper: &RetentionSweeper{
			Client: c,
			Now:    func() time.Time { return now },
		},
	}
}

// retentionKitchen is a singleton with a store and a per-class model.
func retentionKitchen(store bool) *kitchenv1alpha1.Kitchen {
	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			BaseDomain: "example.com",
			Retention: kitchenv1alpha1.RetentionSpec{
				ContainerLogs: ptr.To(int32(14)),
				BuildLogs:     ptr.To(int32(180)),
			},
		},
	}
	kitchen.Spec.Observability.ClickHouse.RetentionDays = 30
	kitchen.Spec.Compliance.Audit.RetentionDays = 365
	kitchen.Spec.Compliance.Audit.Enabled = true
	if store {
		kitchen.Spec.Observability.ClickHouse.SecretRef =
			&kitchenv1alpha1.LocalObjectReference{Name: retentionSecretName}
	}
	return kitchen
}

// TestASweepMeasuresEveryClassAgainstTheModelInForce.
func TestASweepMeasuresEveryClassAgainstTheModelInForce(t *testing.T) {
	fixtures := newRetentionFixtures(t, retentionKitchen(true))

	report, err := fixtures.sweeper.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if report.Measured != len(retention.Definitions()) {
		t.Fatalf("the sweep measured %d of %d classes: %s",
			report.Measured, len(retention.Definitions()), report.Message)
	}

	byClass := map[retention.Class]clickhouse.RetentionObservation{}
	for _, observation := range report.Observations {
		byClass[observation.Class] = observation
	}
	for class, want := range map[retention.Class]int32{
		retention.ClassContainerLogs: 14,
		retention.ClassBuildLogs:     180,
		retention.ClassFlows:         30,
		retention.ClassAudit:         365,
	} {
		if got := byClass[class].Days; got != want {
			t.Errorf("%s was swept at %d days, want %d", class, got, want)
		}
	}
}

// TestTheSweepRecordsWhatItFound is the third acceptance criterion: expiry
// produces a record, it goes into the existing audit log rather than into a
// ledger of its own, and it carries what a reader needs — the class, the rule
// that was in force, the horizon, and what went.
func TestTheSweepRecordsWhatItFound(t *testing.T) {
	fixtures := newRetentionFixtures(t, retentionKitchen(true))
	fixtures.store.expiredPartitions = `{"partition":"2026-07-01","rows":120}`
	fixtures.sweeper.Audit = &audit.Recorder{
		Client:    fixtures.client,
		Namespace: PlatformNamespace,
		Singleton: KitchenSingletonName,
	}

	report, err := fixtures.sweeper.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if !report.Recorded {
		t.Fatal("the sweep produced no audit record, so expiry left no evidence behind")
	}

	appended := fixtures.recordedAudit(t)
	for _, want := range []string{
		audit.KindRetention,
		audit.ChangeRetentionSweep,
		string(retention.ClassContainerLogs),
		string(retention.ClassBuildLogs),
		"horizon",
		"removed",
	} {
		if !strings.Contains(appended, want) {
			t.Errorf("the deletion record does not carry %q:\n%s", want, appended)
		}
	}
	if report.Removed == 0 {
		t.Error("the sweep dropped partitions and reported removing nothing")
	}
}

// recordedAudit is the INSERT the recorder sent, which is the only place the
// record exists in this fixture.
func (f *retentionFixtures) recordedAudit(t *testing.T) string {
	t.Helper()
	for _, statement := range f.store.statements {
		if strings.Contains(statement, "INSERT INTO") && strings.Contains(statement, clickhouse.AuditTable) {
			return statement
		}
	}
	t.Fatalf("nothing was inserted into %s", clickhouse.AuditTable)
	return ""
}

// TestTheSweepPublishesWhatIsInForceOnTheSingleton. The status is what the API
// answers from, so a sweep that measured and published nothing would leave the
// retention route reporting configuration and no evidence.
func TestTheSweepPublishesWhatIsInForceOnTheSingleton(t *testing.T) {
	fixtures := newRetentionFixtures(t, retentionKitchen(true))

	if _, err := fixtures.sweeper.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	current := &kitchenv1alpha1.Kitchen{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Name: KitchenSingletonName}, current); err != nil {
		t.Fatal(err)
	}
	status := current.Status.Retention
	if status == nil {
		t.Fatal("the sweep published no retention status at all")
	}
	if status.LastSweep == nil {
		t.Error("the status carries no last-sweep time, so nothing dates the measurement")
	}
	if len(status.Classes) != len(retention.Definitions()) {
		t.Fatalf("the status reports %d classes, want %d", len(status.Classes), len(retention.Definitions()))
	}
	for _, class := range status.Classes {
		if !class.Enforced {
			t.Errorf("%s reads as not enforced: %s", class.Class, class.Message)
		}
		if class.Oldest == nil {
			t.Errorf("%s makes no claim about how far back it goes", class.Class)
		}
	}
}

// TestTheSweepNeverDeletesTheAuditLog is the rule that keeps the deletion
// evidence from being the thing that gets deleted.
func TestTheSweepNeverDeletesTheAuditLog(t *testing.T) {
	fixtures := newRetentionFixtures(t, retentionKitchen(true))
	fixtures.store.expiredPartitions = `{"partition":"2026-01-01","rows":99}`

	if _, err := fixtures.sweeper.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if fixtures.store.sent("DROP PARTITION") && fixtures.store.sent(clickhouse.AuditTable) {
		for _, statement := range fixtures.store.statements {
			if strings.Contains(statement, "DROP PARTITION") &&
				strings.Contains(statement, clickhouse.AuditTable) {
				t.Fatalf("the sweep deleted from the audit log: %s", statement)
			}
		}
	}
}

// TestAnInstallationWithNoStoreSaysSoRatherThanReportingZeroes.
func TestAnInstallationWithNoStoreSaysSoRatherThanReportingZeroes(t *testing.T) {
	fixtures := newRetentionFixtures(t, retentionKitchen(false))

	report, err := fixtures.sweeper.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if report.Message == "" {
		t.Fatal("a platform with no store swept silently")
	}
	if !strings.Contains(report.Message, "secretRef") {
		t.Errorf("the message does not say what to do about it: %s", report.Message)
	}

	current := &kitchenv1alpha1.Kitchen{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Name: KitchenSingletonName}, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Retention == nil || current.Status.Retention.Message == "" {
		t.Error("the singleton does not say why nothing is being retained")
	}
	for _, class := range current.Status.Retention.Classes {
		if class.Enforced {
			t.Errorf("%s reads as enforced on a platform with no store", class.Class)
		}
	}
}

// TestASweepIsDueOnceAnIntervalAndNotBefore.
func TestASweepIsDueOnceAnIntervalAndNotBefore(t *testing.T) {
	fixtures := newRetentionFixtures(t, retentionKitchen(true))
	fixtures.sweeper.SweepInterval = 24 * time.Hour

	first, err := fixtures.sweeper.SweepIfDue(context.Background())
	if err != nil {
		t.Fatalf("the first sweep: %v", err)
	}
	if first.Measured == 0 {
		t.Fatal("the first sweep measured nothing, so a fresh leader waits a day to make its first record")
	}

	again, err := fixtures.sweeper.SweepIfDue(context.Background())
	if err != nil {
		t.Fatalf("the second sweep: %v", err)
	}
	if again.Measured != 0 {
		t.Error("a second sweep ran inside the interval, so the log would carry two records for one day")
	}

	fixtures.sweeper.Now = func() time.Time { return fixtures.now.Add(25 * time.Hour) }
	later, err := fixtures.sweeper.SweepIfDue(context.Background())
	if err != nil {
		t.Fatalf("the sweep after the interval: %v", err)
	}
	if later.Measured == 0 {
		t.Error("no sweep ran after the interval elapsed")
	}
}

// TestTheConfiguredHalfOfTheStatusIsPublishedBeforeAnySweep. An operator who
// has just changed a retention should see it on the object within a reconcile,
// not within a day.
func TestTheConfiguredHalfOfTheStatusIsPublishedBeforeAnySweep(t *testing.T) {
	kitchen := retentionKitchen(true)
	applyRetentionStatus(kitchen, retention.Resolve(kitchen), "")

	status := kitchen.Status.Retention
	if status == nil {
		t.Fatal("nothing was published")
	}
	if status.LastSweep != nil {
		t.Error("a status with no sweep behind it claims a sweep time")
	}
	for _, class := range status.Classes {
		if class.Days < 1 || class.Source == "" {
			t.Errorf("%s is published as %d days from %q", class.Class, class.Days, class.Source)
		}
		if class.Enforced {
			t.Errorf("%s reads as enforced before anything measured it", class.Class)
		}
	}
}

// TestAChangedRetentionInvalidatesTheMeasurementBesideIt. A measurement is a
// claim about a horizon; reporting yesterday's oldest row beside a horizon
// that has just moved would be reporting a claim nothing has checked.
func TestAChangedRetentionInvalidatesTheMeasurementBesideIt(t *testing.T) {
	kitchen := retentionKitchen(true)
	kitchen.Status.Retention = &kitchenv1alpha1.RetentionStatus{
		Classes: []kitchenv1alpha1.RetentionClassStatus{
			{Class: string(retention.ClassContainerLogs), Days: 14, Enforced: true, Rows: 900},
			{Class: string(retention.ClassFlows), Days: 30, Enforced: true, Rows: 700},
		},
	}
	kitchen.Spec.Retention.ContainerLogs = ptr.To(int32(60))

	applyRetentionStatus(kitchen, retention.Resolve(kitchen), "")
	for _, class := range kitchen.Status.Retention.Classes {
		switch class.Class {
		case string(retention.ClassContainerLogs):
			if class.Enforced || class.Rows != 0 {
				t.Errorf("a measurement taken at 14 days survived the move to %d", class.Days)
			}
		case string(retention.ClassFlows):
			if !class.Enforced || class.Rows != 700 {
				t.Error("a measurement for a class that did not move was discarded")
			}
		}
	}
}

// TestTheSchemaConditionSaysWhatIsActuallyRetained. One number when every
// class agrees, and the classes named when they do not — a condition reading
// "retaining 30 days" on a platform keeping build logs for 180 would be a
// lie in the one place an operator looks first.
func TestTheSchemaConditionSaysWhatIsActuallyRetained(t *testing.T) {
	uniform := describeTelemetryRetention(retention.Uniform(30))
	if !strings.Contains(uniform, "retaining 30 days") {
		t.Errorf("a uniform model reads %q", uniform)
	}
	if strings.Contains(uniform, "containerLogs") {
		t.Errorf("a uniform model enumerates its classes: %q", uniform)
	}

	split := describeTelemetryRetention(retention.Resolve(retentionKitchen(true)))
	for _, want := range []string{"containerLogs", "14d", "buildLogs", "180d"} {
		if !strings.Contains(split, want) {
			t.Errorf("a split model does not mention %q: %q", want, split)
		}
	}
	if strings.Contains(split, "audit") {
		t.Errorf("the telemetry condition claims the audit log: %q", split)
	}
}

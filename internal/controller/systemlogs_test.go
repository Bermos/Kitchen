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
	"net/url"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The reclaim, from the reconcile that is supposed to perform it (#530).
//
// internal/clickhouse tests the sweep itself; what is asserted here is the
// wiring, which nothing else can see: that reconciling the telemetry schema
// *calls* it, that the connection secret's systemLogsBounded key is what
// decides whether it runs, and that what it dropped reaches the singleton's
// status. Removing the call from reconcileTelemetrySchema left every other
// suite in this repository green.

const (
	// The one orphan the fake store offers, as ClickHouse's default TSV.
	supersededTable = "text_log_0"
	supersededRows  = supersededTable + "\t1200\n"
)

// dropped is every DROP the store was sent, in order — the fake records all
// its statements, and these are the ones this file is about.
func dropped(store *fakeTelemetryStore) []string {
	var drops []string
	for _, statement := range store.statements {
		if strings.HasPrefix(statement, "DROP TABLE IF EXISTS system.") {
			drops = append(drops, statement)
		}
	}
	return drops
}

// telemetryFixtures is a platform whose telemetry store is the fake, with the
// connection secret the chart writes — carrying systemLogsBounded or not.
func telemetryFixtures(t *testing.T, store *fakeTelemetryStore, bounded bool) (
	*KitchenReconciler, *kitchenv1alpha1.Kitchen,
) {
	t.Helper()

	endpoint, err := url.Parse(store.server.URL)
	if err != nil {
		t.Fatalf("parsing the test server URL: %v", err)
	}
	data := map[string][]byte{
		clickhouse.SecretKeyHost:     []byte(endpoint.Hostname()),
		clickhouse.SecretKeyHTTPPort: []byte(endpoint.Port()),
		clickhouse.SecretKeyDatabase: []byte("kitchen"),
		clickhouse.SecretKeyUsername: []byte("kitchen"),
		clickhouse.SecretKeyPassword: []byte("hunter2"),
	}
	if bounded {
		data[clickhouse.SecretKeySystemLogsBounded] = []byte("true")
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: PlatformNamespace, Name: retentionSecretName},
		Data:       data,
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "example.com"},
	}
	kitchen.Spec.Observability.ClickHouse.SecretRef = &kitchenv1alpha1.LocalObjectReference{
		Name: retentionSecretName,
	}
	return &KitchenReconciler{Client: c}, kitchen
}

// noConditions is the setCond a test passes when the conditions are not what
// it is asking about.
func noConditions(string, metav1.ConditionStatus, string, string) {}

// The store the chart runs: the superseded table is dropped, and the singleton
// says so.
func TestReconcilingTheSchemaCollectsTheSupersededSystemLogs(t *testing.T) {
	store := newFakeTelemetryStore(t)
	store.orphanRows = supersededRows
	r, kitchen := telemetryFixtures(t, store, true)

	if !r.reconcileTelemetrySchema(context.Background(), kitchen, noConditions) {
		t.Fatal("the telemetry schema was not applied against the fake store")
	}

	want := "DROP TABLE IF EXISTS system.`" + supersededTable + "` SYNC"
	drops := dropped(store)
	if len(drops) != 1 || drops[0] != want {
		t.Fatalf("the reconcile sent %v; want exactly %q. Nothing else in this repository "+
			"notices if the reclaim is never called", drops, want)
	}

	recorded := kitchen.Status.SystemLogs
	if recorded == nil {
		t.Fatal("the singleton says nothing about a sweep that dropped a table, so an " +
			"operator has no way to know an upgrade reclaimed anything")
	}
	if recorded.LastReclaimed == nil {
		t.Error("status.systemLogs.lastReclaimed is unset after a table was dropped")
	}
	if len(recorded.Tables) != 1 || recorded.Tables[0] != supersededTable {
		t.Errorf("status.systemLogs.tables is %v, want [%s]", recorded.Tables, supersededTable)
	}
	if recorded.BytesReclaimed != 1200 {
		t.Errorf("status.systemLogs.bytesReclaimed is %d, want 1200", recorded.BytesReclaimed)
	}
	if recorded.Message != "" {
		t.Errorf("a sweep that worked left the message %q", recorded.Message)
	}
}

// An external store: the same superseded table is there and is left alone,
// because this platform did not put it there and it is not this platform's to
// delete. The key is absent from the secret, which is what the chart writes
// for a ClickHouse it does not run.
func TestAnExternalStoreIsNeverSwept(t *testing.T) {
	store := newFakeTelemetryStore(t)
	store.orphanRows = supersededRows
	r, kitchen := telemetryFixtures(t, store, false)

	if !r.reconcileTelemetrySchema(context.Background(), kitchen, noConditions) {
		t.Fatal("the telemetry schema was not applied against the fake store")
	}

	if len(dropped(store)) != 0 {
		t.Errorf("dropped %v in a store this installation does not run", dropped(store))
	}
	if kitchen.Status.SystemLogs != nil {
		t.Errorf("recorded %+v about a store it never swept", kitchen.Status.SystemLogs)
	}
}

// A sweep that ran and found nothing writes nothing, so a fresh installation's
// singleton is not carrying an empty record of work that never happened.
func TestASweepThatFindsNothingRecordsNothing(t *testing.T) {
	store := newFakeTelemetryStore(t)
	r, kitchen := telemetryFixtures(t, store, true)

	if !r.reconcileTelemetrySchema(context.Background(), kitchen, noConditions) {
		t.Fatal("the telemetry schema was not applied against the fake store")
	}
	if len(dropped(store)) != 0 {
		t.Errorf("dropped %v with nothing superseded", dropped(store))
	}
	if kitchen.Status.SystemLogs != nil {
		t.Errorf("recorded %+v after a sweep that found nothing", kitchen.Status.SystemLogs)
	}
}

// The list accumulates across sweeps, because the renames arrive one at a time
// — a list holding only the last pass would name the four-megabyte table and
// hide the eleven gigabytes collected that morning.
func TestLaterSweepsAddToWhatWasAlreadyCollected(t *testing.T) {
	store := newFakeTelemetryStore(t)
	store.orphanRows = supersededRows
	r, kitchen := telemetryFixtures(t, store, true)
	ctx := context.Background()

	if !r.reconcileTelemetrySchema(ctx, kitchen, noConditions) {
		t.Fatal("the first reconcile did not apply the schema")
	}
	// The quiet table, renamed hours later and collected by a later pass.
	store.orphanRows = "error_log_0\t300\n"
	if !r.reconcileTelemetrySchema(ctx, kitchen, noConditions) {
		t.Fatal("the second reconcile did not apply the schema")
	}

	recorded := kitchen.Status.SystemLogs
	if recorded == nil {
		t.Fatal("nothing was recorded")
	}
	want := []string{"error_log_0", supersededTable}
	if len(recorded.Tables) != len(want) {
		t.Fatalf("status.systemLogs.tables is %v, want %v", recorded.Tables, want)
	}
	for i, name := range want {
		if recorded.Tables[i] != name {
			t.Errorf("status.systemLogs.tables is %v, want %v", recorded.Tables, want)
			break
		}
	}
	if recorded.BytesReclaimed != 1500 {
		t.Errorf("status.systemLogs.bytesReclaimed is %d, want 1200 + 300",
			recorded.BytesReclaimed)
	}
}

// The cap, so that nothing about a store the operator does not control can
// make the status object grow forever.
func TestTheRecordedTableListIsBounded(t *testing.T) {
	recorded := []string{}
	for i := range maxReclaimedTablesRecorded + 5 {
		recorded = recordReclaimedTables(recorded, []string{"text_log_" + strconv.Itoa(i)})
	}
	if len(recorded) != maxReclaimedTablesRecorded {
		t.Fatalf("recorded %d tables, want the cap of %d",
			len(recorded), maxReclaimedTablesRecorded)
	}
	// A name already recorded is not recorded twice — which is what keeps the
	// list short in practice, since the server reuses the lowest free suffix.
	again := recordReclaimedTables(recorded, []string{recorded[0]})
	if len(again) != len(recorded) {
		t.Errorf("recording %q again grew the list to %d", recorded[0], len(again))
	}
}

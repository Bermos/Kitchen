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

package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// The recovery half of the CloudNativePG provisioner: the window it reads off
// the Cluster, the sibling Cluster it renders, and the four things it refuses
// rather than finding out about at the end of a recovery.

// CloudNativePG can reconstruct a database this cluster runs, from the
// archive #245 phase 2 configured. The interface is the contract's, and this
// is the assertion that the provider is actually offered through it.
var _ RecoverableProvisioner = (*CNPG)(nil)

const (
	recoveryName = "recovery-before-the-migration"
	// The source database's own shape and archive, which a recovery inherits
	// and these tests assert about in more than one place.
	sourceStorageSize  = "40Gi"
	sourceRetention    = "30d"
	sourceSchedulePlan = "0 30 2 * * *"
	archiveDestination = "s3://kitchen-archive/prod/databases"
)

// recoveredName is what the sibling Cluster is called: the source's name with
// the recovery's appended, so that two projects recovering under the same
// name in one database namespace are still two databases.
func recoveredName() string { return branchName(testCluster, recoveryName) }

// archivedCluster is a database with a backup policy in force: the object
// store block ConfigureBackup writes, a retention, and the shape a recovery
// inherits — image, storage and instance count.
func archivedCluster(t *testing.T, status map[string]any) *unstructured.Unstructured {
	t.Helper()
	cluster := managedCluster()
	set := func(value any, fields ...string) {
		if err := unstructured.SetNestedField(cluster.Object, value, fields...); err != nil {
			t.Fatal(err)
		}
	}
	set(map[string]any{
		"barmanObjectStore": barmanObjectStore(backedUpDestination(), testCluster),
		"retentionPolicy":   sourceRetention,
	}, "spec", "backup")
	set(postgisImage16, "spec", "imageName")
	set(map[string]any{"size": sourceStorageSize, "storageClass": "fast"}, "spec", "storage")
	set(int64(3), "spec", "instances")
	if status != nil {
		set(status, "status")
	}
	return cluster
}

// archivingStatus is what CloudNativePG reports about its own archive: the
// first moment it can still reconstruct, the last base backup it took, and
// whether the write-ahead log is reaching the object store between them.
func archivingStatus(first, last time.Time, archiving string) map[string]any {
	conditions := []any{
		map[string]any{"type": "Ready", "status": "True", "reason": "ClusterIsReady"},
		map[string]any{
			"type": "ContinuousArchiving", "status": archiving,
			"message": "the last WAL archival was refused by the object store",
		},
	}
	status := map[string]any{"phase": "Cluster in healthy state", "conditions": conditions}
	if !first.IsZero() {
		status["firstRecoverabilityPoint"] = first.UTC().Format(time.RFC3339)
	}
	if !last.IsZero() {
		status["lastSuccessfulBackup"] = last.UTC().Format(time.RFC3339)
	}
	return status
}

// sourceSchedule is the base backup schedule the source database has, which
// the recovered sibling inherits so that it is itself recoverable.
func sourceSchedule() *unstructured.Unstructured {
	schedule := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"cluster":  map[string]any{"name": testCluster},
		"schedule": sourceSchedulePlan,
	}}}
	schedule.SetGroupVersionKind(scheduledBackupGVK())
	schedule.SetNamespace(testDatabaseNamespace)
	schedule.SetName(scheduledBackupName(testCluster))
	return schedule
}

// makeReady plays CloudNativePG's part for a Cluster the provisioner created:
// the fake cluster has no operator in it, so the test says when the recovery
// has finished replaying.
func makeReady(t *testing.T, provisioner *CNPG, name string) {
	t.Helper()
	cluster := getCluster(t, provisioner, name)
	if err := unstructured.SetNestedSlice(cluster.Object, []any{
		map[string]any{"type": "Ready", "status": "True", "reason": "ClusterIsReady"},
	}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Client.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
}

func TestTheRecoveryWindowIsReadOffTheDatabaseAndEndsWhereTheArchiveDoes(t *testing.T) {
	first := time.Now().Add(-72 * time.Hour)
	cnpg := cnpgAgainstFakeCluster(t, archivedCluster(t, archivingStatus(first, time.Now().Add(-6*time.Hour), "True")))

	window, err := cnpg.RecoveryWindow(context.Background(), testInstanceID)
	if err != nil {
		t.Fatalf("reading the window: %v", err)
	}
	// The earliest edge is the database's own firstRecoverabilityPoint and
	// nothing the platform worked out for itself.
	if window.Earliest.Sub(first).Abs() > time.Second {
		t.Errorf("earliest = %s, want the reported recovery point %s", window.Earliest, first)
	}
	// The latest edge is bounded by archiving rather than by the clock: a
	// moment whose write-ahead log has not been shipped cannot be replayed
	// out of the object store, so the window stops one archive_timeout back.
	lag := time.Since(window.Latest)
	if lag < walArchiveLag || lag > walArchiveLag+time.Minute {
		t.Errorf("latest = %s, want about %s ago", window.Latest, walArchiveLag)
	}
	if !window.Contains(time.Now().Add(-time.Hour)) {
		t.Errorf("an hour ago is inside %v and the window says otherwise", window)
	}
}

func TestAFailingArchiveOnlyPromisesAsFarAsTheLastBaseBackup(t *testing.T) {
	first := time.Now().Add(-72 * time.Hour)
	last := time.Now().Add(-9 * time.Hour)
	cnpg := cnpgAgainstFakeCluster(t, archivedCluster(t, archivingStatus(first, last, "False")))

	window, err := cnpg.RecoveryWindow(context.Background(), testInstanceID)
	if err != nil {
		t.Fatalf("reading the window: %v", err)
	}
	if window.Latest.Sub(last).Abs() > time.Second {
		t.Errorf("latest = %s, want the last base backup %s — WAL that never reached the object store "+
			"cannot be replayed out of it", window.Latest, last)
	}
	if window.Contains(time.Now().Add(-time.Hour)) {
		t.Error("an hour ago is offered while the write-ahead log is not being archived")
	}
}

func TestADatabaseWithNothingArchivedYetHasNoWindowAndSaysWhy(t *testing.T) {
	ctx := context.Background()

	nothing := cnpgAgainstFakeCluster(t, archivedCluster(t, archivingStatus(time.Time{}, time.Time{}, "True")))
	_, err := nothing.RecoveryWindow(ctx, testInstanceID)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("error %v, want ErrNotReady: a first base backup that has not been taken yet is a wait", err)
	}
	if !strings.Contains(err.Error(), "recovery point") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}

	// Archiving broken before anything was ever backed up: there is no moment
	// to offer, and the provider's own complaint travels with the refusal.
	broken := cnpgAgainstFakeCluster(t,
		archivedCluster(t, archivingStatus(time.Now().Add(-time.Hour), time.Time{}, "False")))
	_, err = broken.RecoveryWindow(ctx, testInstanceID)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("error %v, want ErrNotReady", err)
	}
	if !strings.Contains(err.Error(), "refused by the object store") {
		t.Errorf("the database's own words about its archive are missing: %v", err)
	}
}

func TestADatabaseWithNoBackupPolicyOffersNoRecoveryAndNamesTheFix(t *testing.T) {
	// A preview's Cluster is exactly this cluster: built from the shape of
	// its parent and never from its spec, so it carries no object store and
	// is never recoverable.
	cnpg := cnpgAgainstFakeCluster(t, managedCluster())

	_, err := cnpg.RecoveryWindow(context.Background(), testInstanceID)
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("error %v, want ErrUnsatisfiable: a database with no archive cannot be recovered", err)
	}
	if !strings.Contains(err.Error(), "spec.backup") {
		t.Errorf("the refusal does not name the fix: %v", err)
	}
	if _, err := cnpg.RecoverTo(context.Background(), testInstanceID, recoveryName, time.Now().Add(-time.Hour)); !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("recovering anyway: %v", err)
	}
}

func TestADatabaseThePlatformDidNotCreateIsNotRecoveredFrom(t *testing.T) {
	adopted := archivedCluster(t, archivingStatus(time.Now().Add(-48*time.Hour), time.Now(), "True"))
	adopted.SetLabels(map[string]string{})
	cnpg := cnpgAgainstFakeCluster(t, adopted)

	_, err := cnpg.RecoveryWindow(context.Background(), testInstanceID)
	if !errors.Is(err, ErrBackupNotManaged) {
		t.Fatalf("error %v, want ErrBackupNotManaged: the archive of a handed-over database is not this "+
			"platform's to recover from", err)
	}
	_, err = cnpg.RecoverTo(context.Background(), testInstanceID, recoveryName, time.Now().Add(-time.Hour))
	if !errors.Is(err, ErrBackupNotManaged) {
		t.Fatalf("error %v, want ErrBackupNotManaged", err)
	}
}

func TestRecoverToBootstrapsASiblingFromTheArchiveAtTheMoment(t *testing.T) {
	ctx := context.Background()
	at := time.Now().Add(-30 * time.Hour).UTC().Truncate(time.Second)
	cnpg := cnpgAgainstFakeCluster(t,
		archivedCluster(t, archivingStatus(time.Now().Add(-72*time.Hour), time.Now().Add(-6*time.Hour), "True")),
		sourceSchedule(), appSecret(recoveredName()))

	// First pass: the sibling is created and is replaying.
	if _, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, at); !errors.Is(err, ErrNotReady) {
		t.Fatalf("error %v, want ErrNotReady — a recovery takes minutes and the claim waits Pending", err)
	}

	recovered := getCluster(t, cnpg, recoveredName())
	recovery, found, err := unstructured.NestedMap(recovered.Object, "spec", "bootstrap", "recovery")
	if err != nil || !found {
		t.Fatalf("the sibling does not bootstrap from a recovery: %v", err)
	}
	if recovery["source"] != testCluster {
		t.Errorf("recovery source = %v, want the archive of %s", recovery["source"], testCluster)
	}
	if recovery["database"] != applicationDatabase || recovery["owner"] != applicationUser {
		t.Errorf("the recovered database names %v/%v, and the binding is what reads them",
			recovery["database"], recovery["owner"])
	}
	target, _ := recovery["recoveryTarget"].(map[string]any)
	if target["targetTime"] != at.Format(time.RFC3339) {
		t.Errorf("targetTime = %v, want %s written with its timezone spelled out",
			target["targetTime"], at.Format(time.RFC3339))
	}
	if _, found, _ := unstructured.NestedMap(recovered.Object, "spec", "bootstrap", "initdb"); found {
		t.Error("the sibling initialises an empty database rather than recovering one")
	}

	// The archive is named as an external cluster under the *source's* server
	// name: that prefix is the whole of how barman finds what to restore.
	externals, _, err := unstructured.NestedSlice(recovered.Object, "spec", "externalClusters")
	if err != nil || len(externals) != 1 {
		t.Fatalf("externalClusters = %v", externals)
	}
	external, _ := externals[0].(map[string]any)
	if external["name"] != testCluster {
		t.Errorf("the external cluster is called %v, and bootstrap.recovery.source names %v",
			external["name"], recovery["source"])
	}
	store, _ := external["barmanObjectStore"].(map[string]any)
	if store["serverName"] != testCluster || store["destinationPath"] != archiveDestination {
		t.Errorf("the sibling recovers from %v, want the source's own prefix", store)
	}

	// Its own archive goes to the same destination under a server name of its
	// own — CloudNativePG refuses to share one, and a recovery archiving over
	// its own source would destroy what it was recovered from.
	own, _, err := unstructured.NestedMap(recovered.Object, "spec", "backup", "barmanObjectStore")
	if err != nil {
		t.Fatal(err)
	}
	if own["serverName"] != recoveredName() {
		t.Errorf("the sibling archives under %v, want its own name", own["serverName"])
	}
	if own["destinationPath"] != store["destinationPath"] {
		t.Errorf("the sibling archives to %v and was recovered from %v",
			own["destinationPath"], store["destinationPath"])
	}
	if got := nestedString(recovered, "spec", "backup", "retentionPolicy"); got != sourceRetention {
		t.Errorf("retentionPolicy = %q, want the source's", got)
	}

	// The shape is the source's: the same Postgres, the same volume, the same
	// number of instances, because a promoted recovery is production.
	if image := nestedString(recovered, "spec", "imageName"); image != postgisImage16 {
		t.Errorf("image %q, want the source's — PostgreSQL will not start on another major's data", image)
	}
	if size := nestedString(recovered, "spec", "storage", "size"); size != sourceStorageSize {
		t.Errorf("storage %q, want the source's", size)
	}
	if class := nestedString(recovered, "spec", "storage", "storageClass"); class != "fast" {
		t.Errorf("storageClass %q, want the source's", class)
	}
	if instances, _, _ := unstructured.NestedInt64(recovered.Object, "spec", "instances"); instances != 3 {
		t.Errorf("instances = %d, want the source's", instances)
	}
	if recovered.GetLabels()[managedByLabel] != managedByValue {
		t.Errorf("labels %v, want the platform's own", recovered.GetLabels())
	}

	// Nothing is written to the source: a recovery is a second database and
	// never a rewind of the first.
	source := getCluster(t, cnpg, testCluster)
	if got := nestedString(source, "spec", "backup", "barmanObjectStore", "serverName"); got != testCluster {
		t.Errorf("the source's own archive moved to %q", got)
	}
	if _, found, _ := unstructured.NestedMap(source.Object, "spec", "bootstrap", "recovery"); found {
		t.Error("the source was bootstrapped from its own archive — nothing is recovered in place")
	}

}

// The second pass, once CloudNativePG has fetched the base backup and replayed
// the write-ahead log over it: the sibling binds, and it is itself backed up.
func TestARecoveredSiblingBindsAndInheritsTheBackupSchedule(t *testing.T) {
	ctx := context.Background()
	at := time.Now().Add(-30 * time.Hour).UTC().Truncate(time.Second)
	cnpg := cnpgAgainstFakeCluster(t,
		archivedCluster(t, archivingStatus(time.Now().Add(-72*time.Hour), time.Now().Add(-6*time.Hour), "True")),
		sourceSchedule(), appSecret(recoveredName()))
	if _, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, at); !errors.Is(err, ErrNotReady) {
		t.Fatalf("error %v, want ErrNotReady", err)
	}

	makeReady(t, cnpg, recoveredName())
	branch, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, at)
	if err != nil {
		t.Fatalf("recovering: %v", err)
	}
	if branch.ID != testDatabaseNamespace+"/"+recoveredName() {
		t.Errorf("branch ID %q", branch.ID)
	}
	if branch.Provenance != ProvenanceProduction {
		t.Errorf("provenance %q: a recovery of a production database is production data at an earlier "+
			"moment", branch.Provenance)
	}
	if !strings.Contains(branch.Binding.URL, recoveredName()+"-rw."+testDatabaseNamespace+".svc") {
		t.Errorf("the binding does not reach the sibling: %q", branch.Binding.URL)
	}

	// A serving sibling inherits the source's schedule, so that a recovery
	// somebody promotes is itself recoverable rather than a production
	// database with no archive behind it.
	inherited := &unstructured.Unstructured{}
	inherited.SetGroupVersionKind(scheduledBackupGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: scheduledBackupName(recoveredName())}
	if err := cnpg.Client.Get(ctx, key, inherited); err != nil {
		t.Fatalf("the sibling has no base backup schedule: %v", err)
	}
	if got := nestedString(inherited, "spec", "schedule"); got != sourceSchedulePlan {
		t.Errorf("schedule %q, want the source's", got)
	}
	if got := nestedString(inherited, "spec", "cluster", "name"); got != recoveredName() {
		t.Errorf("the sibling's schedule backs up %q", got)
	}
}

func TestASecondRecoveryToTheSameNameIsTheSameDatabase(t *testing.T) {
	ctx := context.Background()
	at := time.Now().Add(-30 * time.Hour)
	cnpg := cnpgAgainstFakeCluster(t,
		archivedCluster(t, archivingStatus(time.Now().Add(-72*time.Hour), time.Now().Add(-6*time.Hour), "True")),
		appSecret(recoveredName()))

	if _, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, at); !errors.Is(err, ErrNotReady) {
		t.Fatalf("error %v, want ErrNotReady", err)
	}
	makeReady(t, cnpg, recoveredName())

	// A reconcile that runs again — with a moment that has since fallen out
	// of the window, which is what retention does while somebody looks at the
	// copy — finds the database it made rather than starting a second
	// recovery or refusing the one that exists.
	branch, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, time.Now().Add(-200*time.Hour))
	if err != nil {
		t.Fatalf("recovering again: %v", err)
	}
	if branch.ID != testDatabaseNamespace+"/"+recoveredName() {
		t.Errorf("branch ID %q", branch.ID)
	}
	recovered := getCluster(t, cnpg, recoveredName())
	target, _, _ := unstructured.NestedMap(recovered.Object, "spec", "bootstrap", "recovery", "recoveryTarget")
	if target["targetTime"] != at.UTC().Format(time.RFC3339) {
		t.Errorf("targetTime = %v, want the moment the database was actually recovered to", target["targetTime"])
	}
}

func TestRecoverToRefusesAMomentTheArchiveCannotServe(t *testing.T) {
	ctx := context.Background()
	first := time.Now().Add(-24 * time.Hour)
	cnpg := cnpgAgainstFakeCluster(t,
		archivedCluster(t, archivingStatus(first, time.Now().Add(-6*time.Hour), "True")))

	_, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, first.Add(-time.Hour))
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("error %v, want ErrUnsatisfiable: the archive does not reach that far back", err)
	}
	if !strings.Contains(err.Error(), first.UTC().Format(time.RFC3339)) {
		t.Errorf("the refusal does not say how far back it does reach: %v", err)
	}
	// Nothing was created on the way to refusing.
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: recoveredName()}
	if err := cnpg.Client.Get(ctx, key, cluster); !apierrors.IsNotFound(err) {
		t.Errorf("a refused recovery left a Cluster behind: %v", err)
	}

	// A moment nearer than the write-ahead log has been shipped is refused
	// too: PostgreSQL fails a recovery whose target is past the end of the
	// log it was given, rather than stopping politely short of it.
	if _, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, time.Now()); !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("error %v, want ErrUnsatisfiable for a moment the archive has not caught up with", err)
	}
	if _, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, time.Time{}); err == nil {
		t.Fatal("a recovery with no moment to recover to was accepted")
	}
}

func TestARecoveryTheDatabaseOperatorGaveUpOnFailsRatherThanWaitingForever(t *testing.T) {
	ctx := context.Background()
	at := time.Now().Add(-30 * time.Hour)
	cnpg := cnpgAgainstFakeCluster(t,
		archivedCluster(t, archivingStatus(time.Now().Add(-72*time.Hour), time.Now().Add(-6*time.Hour), "True")))

	if _, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, at); !errors.Is(err, ErrNotReady) {
		t.Fatalf("error %v, want ErrNotReady", err)
	}
	recovered := getCluster(t, cnpg, recoveredName())
	if err := unstructured.SetNestedField(recovered.Object,
		"Cluster is unrecoverable and needs manual intervention", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := cnpg.Client.Update(ctx, recovered); err != nil {
		t.Fatal(err)
	}

	// Not ErrNotReady: a recovery that reads Pending for every minute of a
	// wait that is never going to end teaches everybody to ignore the word.
	_, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, at)
	if err == nil || errors.Is(err, ErrNotReady) {
		t.Fatalf("error %v, want a failure carrying the database operator's own phase", err)
	}
	if !strings.Contains(err.Error(), "unrecoverable") {
		t.Errorf("the failure does not carry what CloudNativePG said: %v", err)
	}
}

func TestDiscardingARecoveryDeletesItsCluster(t *testing.T) {
	ctx := context.Background()
	at := time.Now().Add(-30 * time.Hour)
	cnpg := cnpgAgainstFakeCluster(t,
		archivedCluster(t, archivingStatus(time.Now().Add(-72*time.Hour), time.Now().Add(-6*time.Hour), "True")),
		sourceSchedule(), appSecret(recoveredName()))
	if _, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, at); !errors.Is(err, ErrNotReady) {
		t.Fatalf("error %v, want ErrNotReady", err)
	}
	makeReady(t, cnpg, recoveredName())
	if _, err := cnpg.RecoverTo(ctx, testInstanceID, recoveryName, at); err != nil {
		t.Fatalf("recovering: %v", err)
	}

	// The reconciler discards a recovery nobody asks for any more through
	// DeleteBranch, with the ID RecoverTo answered.
	if err := cnpg.DeleteBranch(ctx, testInstanceID, testDatabaseNamespace+"/"+recoveredName()); err != nil {
		t.Fatalf("discarding the recovery: %v", err)
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: recoveredName()}
	if err := cnpg.Client.Get(ctx, key, cluster); !apierrors.IsNotFound(err) {
		t.Errorf("the recovered database is still there: %v", err)
	}
	// Its schedule goes with it: a ScheduledBackup naming a Cluster that is
	// gone is a job failing nightly about a database nobody has.
	schedule := &unstructured.Unstructured{}
	schedule.SetGroupVersionKind(scheduledBackupGVK())
	scheduleKey := types.NamespacedName{
		Namespace: testDatabaseNamespace, Name: scheduledBackupName(recoveredName()),
	}
	if err := cnpg.Client.Get(ctx, scheduleKey, schedule); !apierrors.IsNotFound(err) {
		t.Errorf("the recovered database's schedule is still there: %v", err)
	}
	// And the source, with its own schedule, is untouched by it.
	getCluster(t, cnpg, testCluster)
	sourceKey := types.NamespacedName{Namespace: testDatabaseNamespace, Name: scheduledBackupName(testCluster)}
	if err := cnpg.Client.Get(ctx, sourceKey, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": scheduledBackupGVK().GroupVersion().String(), "kind": scheduledBackupGVK().Kind,
	}}); err != nil {
		t.Errorf("discarding a recovery took the source's own schedule: %v", err)
	}
}

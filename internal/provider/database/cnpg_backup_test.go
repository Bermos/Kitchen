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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// pruningClient is an API server whose CRD no longer declares
// spec.backup.barmanObjectStore: it accepts the write and answers with an
// object that does not carry the field, saying nothing about it. That silence
// is the whole reason ConfigureBackup reads its own write back.
type pruningClient struct{ client.Client }

func (c pruningClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if err := c.Client.Update(ctx, obj, opts...); err != nil {
		return err
	}
	if object, ok := obj.(*unstructured.Unstructured); ok {
		unstructured.RemoveNestedField(object.Object, "spec", "backup", "barmanObjectStore")
	}
	return nil
}

// The backup half of the CloudNativePG provisioner: what it writes onto a
// Cluster, what it refuses to write, and what it reads back.

const testInstanceID = testDatabaseNamespace + "/" + testCluster

// destinationSecret is where a claim's destination keeps everything
// CloudNativePG takes by reference: the key pair, the region, and — for a
// store inside this cluster — the certificate it is verified against.
const destinationSecret = "db-backup-destination"

// backedUpDestination is the policy every test here configures, with a stored
// credential so the rendered s3Credentials are the interesting case.
func backedUpDestination() BackupDestination {
	return BackupDestination{
		Bucket:               "kitchen-archive",
		Prefix:               "prod/databases",
		Region:               "eu-central-1",
		Endpoint:             "https://minio.example.com",
		ServerSideEncryption: "AES256",
		CredentialsSecret:    destinationSecret,
		AccessKeyIDKey:       "accessKeyId",
		SecretAccessKeyKey:   "secretAccessKey",
		RegionKey:            "region",
	}
}

// managedCluster is a Cluster this platform created, which is the only kind
// backup configuration is ever written onto.
func managedCluster() *unstructured.Unstructured {
	cluster := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"instances": int64(1)}}}
	cluster.SetGroupVersionKind(clusterGVK())
	cluster.SetNamespace(testDatabaseNamespace)
	cluster.SetName(testCluster)
	cluster.SetLabels(map[string]string{managedByLabel: managedByValue, naming.LabelProject: shopProject})
	return cluster
}

func readCluster(t *testing.T, provisioner *CNPG) *unstructured.Unstructured {
	t.Helper()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: testCluster}
	if err := provisioner.Client.Get(context.Background(), key, cluster); err != nil {
		t.Fatalf("reading the cluster back: %v", err)
	}
	return cluster
}

func readSchedule(t *testing.T, provisioner *CNPG) (*unstructured.Unstructured, error) {
	t.Helper()
	schedule := &unstructured.Unstructured{}
	schedule.SetGroupVersionKind(scheduledBackupGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: scheduledBackupName(testCluster)}
	err := provisioner.Client.Get(context.Background(), key, schedule)
	return schedule, err
}

func TestBackupConfigurationRendersTheObjectStore(t *testing.T) {
	provisioner := cnpgAgainstFakeCluster(t, managedCluster())
	policy := BackupPolicy{
		Enabled:         true,
		Schedule:        "0 30 2 * * *",
		RetentionPolicy: "30d",
		Destination:     backedUpDestination(),
	}
	if err := provisioner.ConfigureBackup(context.Background(), testInstanceID, policy); err != nil {
		t.Fatalf("configuring the backup: %v", err)
	}

	cluster := readCluster(t, provisioner)
	store, found, err := unstructured.NestedMap(cluster.Object, "spec", "backup", "barmanObjectStore")
	if err != nil || !found {
		t.Fatalf("no barmanObjectStore on the cluster: %v", err)
	}
	if store["destinationPath"] != "s3://kitchen-archive/prod/databases" {
		t.Errorf("destinationPath = %v", store["destinationPath"])
	}
	// The serverName is what gives every claim a prefix of its own: barman
	// writes under <destinationPath>/<serverName>, so two databases sharing
	// the platform's bucket never share a path.
	if store["serverName"] != testCluster {
		t.Errorf("serverName = %v, want the cluster's own name", store["serverName"])
	}
	if store["endpointURL"] != "https://minio.example.com" {
		t.Errorf("endpointURL = %v", store["endpointURL"])
	}
	credentials, _ := store["s3Credentials"].(map[string]any)
	accessKey, _ := credentials["accessKeyId"].(map[string]any)
	if accessKey["name"] != destinationSecret || accessKey["key"] != "accessKeyId" {
		t.Errorf("accessKeyId = %v, want a reference to the secret in the database namespace", accessKey)
	}
	// CloudNativePG takes its region by secret reference and not as a plain
	// string, which is the whole reason the destination carries a key name.
	region, _ := credentials["region"].(map[string]any)
	if region["name"] != destinationSecret || region["key"] != "region" {
		t.Errorf("region = %v, want a reference and not a literal", region)
	}
	if credentials["inheritFromIAMRole"] != nil {
		t.Errorf("a destination with a stored key must not also ask for the ambient chain: %v", credentials)
	}
	data, _ := store["data"].(map[string]any)
	wal, _ := store["wal"].(map[string]any)
	if data["encryption"] != "AES256" || wal["encryption"] != "AES256" {
		t.Errorf("both halves must be encrypted: data=%v wal=%v", data, wal)
	}
	if got := nestedString(cluster, "spec", "backup", "retentionPolicy"); got != "30d" {
		t.Errorf("retentionPolicy = %q", got)
	}
}

// A destination inside this cluster is served by an authority no image's trust
// store has heard of — the object store this platform bundles, since #382 — so
// barman is handed the certificate. Without it the connection is refused, which
// is the right answer to an unverifiable store and the wrong end of a backup
// that silently stops.
func TestAnInClusterDestinationCarriesTheAuthorityThatSignedIt(t *testing.T) {
	provisioner := cnpgAgainstFakeCluster(t, managedCluster())
	destination := backedUpDestination()
	destination.Endpoint = "https://kitchen-objectstore.kitchen-system.svc:9000"
	destination.EndpointCASecret = destinationSecret
	destination.EndpointCAKey = "ca.crt"
	policy := BackupPolicy{Enabled: true, Destination: destination}
	if err := provisioner.ConfigureBackup(context.Background(), testInstanceID, policy); err != nil {
		t.Fatalf("configuring the backup: %v", err)
	}

	cluster := readCluster(t, provisioner)
	store, _, _ := unstructured.NestedMap(cluster.Object, "spec", "backup", "barmanObjectStore")
	ca, _ := store["endpointCA"].(map[string]any)
	if ca["name"] != destinationSecret || ca["key"] != "ca.crt" {
		t.Errorf("endpointCA = %v, want a reference to the CA beside the credential", ca)
	}

	// And nothing is written for a destination on the internet, where the
	// image's own roots are the answer and a CA of the platform's would be
	// the wrong one.
	provisioner = cnpgAgainstFakeCluster(t, managedCluster())
	if err := provisioner.ConfigureBackup(context.Background(), testInstanceID,
		BackupPolicy{Enabled: true, Destination: backedUpDestination()}); err != nil {
		t.Fatalf("configuring the backup: %v", err)
	}
	store, _, _ = unstructured.NestedMap(readCluster(t, provisioner).Object,
		"spec", "backup", "barmanObjectStore")
	if _, ok := store["endpointCA"]; ok {
		t.Errorf("a destination outside the cluster was given an authority of the "+
			"platform's: %v", store)
	}
}

func TestBackupWithNoStoredKeyAsksForTheAmbientChain(t *testing.T) {
	provisioner := cnpgAgainstFakeCluster(t, managedCluster())
	policy := BackupPolicy{Enabled: true, Destination: BackupDestination{Bucket: "kitchen-archive"}}
	if err := provisioner.ConfigureBackup(context.Background(), testInstanceID, policy); err != nil {
		t.Fatalf("configuring the backup: %v", err)
	}
	cluster := readCluster(t, provisioner)
	store, _, _ := unstructured.NestedMap(cluster.Object, "spec", "backup", "barmanObjectStore")
	credentials, _ := store["s3Credentials"].(map[string]any)
	if credentials["inheritFromIAMRole"] != true {
		t.Errorf("no stored key means the pod's own credential chain, got %v", credentials)
	}
	if _, ok := store["endpointURL"]; ok {
		t.Errorf("an empty endpoint must not be written at all: %v", store)
	}
}

func TestScheduledBackupIsCreatedAndKeepsItsBackupsOutOfGarbageCollection(t *testing.T) {
	provisioner := cnpgAgainstFakeCluster(t, managedCluster())
	policy := BackupPolicy{Enabled: true, Schedule: "0 0 4 * * *", Destination: backedUpDestination()}
	if err := provisioner.ConfigureBackup(context.Background(), testInstanceID, policy); err != nil {
		t.Fatalf("configuring the backup: %v", err)
	}

	schedule, err := readSchedule(t, provisioner)
	if err != nil {
		t.Fatalf("reading the schedule: %v", err)
	}
	if got := nestedString(schedule, "spec", "schedule"); got != "0 0 4 * * *" {
		t.Errorf("schedule = %q", got)
	}
	if got := nestedString(schedule, "spec", "cluster", "name"); got != testCluster {
		t.Errorf("the schedule must name its cluster, got %q", got)
	}
	// Deleting the database must not look like deleting the record of what
	// was backed up.
	if got := nestedString(schedule, "spec", "backupOwnerReference"); got != "none" {
		t.Errorf("backupOwnerReference = %q, want none", got)
	}
	// Without this the first recovery point does not exist until the first
	// scheduled hour, which is a whole day of a database reporting nothing.
	if immediate, _, _ := unstructured.NestedBool(schedule.Object, "spec", "immediate"); !immediate {
		t.Error("a schedule that does not take a backup immediately leaves the database with no recovery point")
	}
	if schedule.GetLabels()[managedByLabel] != managedByValue {
		t.Errorf("the schedule must be marked as this platform's: %v", schedule.GetLabels())
	}

	// Idempotent: a second pass over an unchanged policy writes nothing new.
	if err := provisioner.ConfigureBackup(context.Background(), testInstanceID, policy); err != nil {
		t.Fatalf("configuring the backup twice: %v", err)
	}
	again, err := readSchedule(t, provisioner)
	if err != nil {
		t.Fatalf("reading the schedule again: %v", err)
	}
	if again.GetResourceVersion() != schedule.GetResourceVersion() {
		t.Error("reconciling an unchanged policy rewrote the schedule")
	}
}

func TestBackupDefaultsToANightlySchedule(t *testing.T) {
	provisioner := cnpgAgainstFakeCluster(t, managedCluster())
	policy := BackupPolicy{Enabled: true, Destination: backedUpDestination()}
	if err := provisioner.ConfigureBackup(context.Background(), testInstanceID, policy); err != nil {
		t.Fatalf("configuring the backup: %v", err)
	}
	schedule, err := readSchedule(t, provisioner)
	if err != nil {
		t.Fatalf("reading the schedule: %v", err)
	}
	if got := nestedString(schedule, "spec", "schedule"); got != DefaultClaimBackupSchedule {
		t.Errorf("schedule = %q, want the platform's nightly default", got)
	}
}

func TestAFiveFieldScheduleIsRefusedRatherThanMisread(t *testing.T) {
	provisioner := cnpgAgainstFakeCluster(t, managedCluster())
	policy := BackupPolicy{Enabled: true, Schedule: "0 3 * * *", Destination: backedUpDestination()}
	err := provisioner.ConfigureBackup(context.Background(), testInstanceID, policy)
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("want a refusal naming the dialect, got %v", err)
	}
	if !strings.Contains(err.Error(), DefaultClaimBackupSchedule) {
		t.Errorf("the refusal should show the shape that works: %v", err)
	}
	if _, err := readSchedule(t, provisioner); !apierrors.IsNotFound(err) {
		t.Errorf("a refused schedule must not have been written: %v", err)
	}
}

func TestAPreviewsDatabaseInheritsTheShapeAndNotTheBackupPolicy(t *testing.T) {
	// "Previews default off" is structural here rather than a default
	// somebody could change: a preview's database is a Cluster of its own,
	// built from the shape of the parent and never from its spec, so it
	// carries no object store and gets no schedule. That matters twice —
	// archiving a fresh, empty database is pure cost, and a branch that had
	// copied the parent's block would carry the parent's serverName and
	// write over production's own backups.
	parent := managedCluster()
	if err := unstructured.SetNestedSlice(parent.Object, []any{
		map[string]any{"type": "Ready", "status": "True", "reason": "ClusterIsReady"},
	}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	provisioner := cnpgAgainstFakeCluster(t, parent)
	policy := BackupPolicy{Enabled: true, RetentionPolicy: "30d", Destination: backedUpDestination()}
	if err := provisioner.ConfigureBackup(context.Background(), testInstanceID, policy); err != nil {
		t.Fatalf("configuring the parent's backup: %v", err)
	}

	// First pass creates the branch and reports it not ready; the object is
	// there either way, which is what this test is about.
	if _, err := provisioner.CreateBranch(context.Background(), testInstanceID, "pr-7"); err == nil {
		t.Log("the branch was ready immediately")
	}
	branch := &unstructured.Unstructured{}
	branch.SetGroupVersionKind(clusterGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: branchName(testCluster, "pr-7")}
	if err := provisioner.Client.Get(context.Background(), key, branch); err != nil {
		t.Fatalf("reading the preview's database: %v", err)
	}
	if _, found, _ := unstructured.NestedMap(branch.Object, "spec", "backup"); found {
		t.Error("a preview's database carries a backup policy")
	}
	schedule := &unstructured.Unstructured{}
	schedule.SetGroupVersionKind(scheduledBackupGVK())
	scheduleKey := types.NamespacedName{
		Namespace: testDatabaseNamespace,
		Name:      scheduledBackupName(branchName(testCluster, "pr-7")),
	}
	if err := provisioner.Client.Get(context.Background(), scheduleKey, schedule); !apierrors.IsNotFound(err) {
		t.Errorf("a preview's database was given a base backup schedule: %v", err)
	}
}

func TestBackupIsNeverWrittenOntoAClusterThePlatformDidNotCreate(t *testing.T) {
	// A database an installation handed over: no managed-by label, so it is
	// somebody else's arrangement and stays that way.
	adopted := managedCluster()
	adopted.SetLabels(map[string]string{naming.LabelProject: shopProject})

	provisioner := cnpgAgainstFakeCluster(t, adopted)
	policy := BackupPolicy{Enabled: true, Destination: backedUpDestination()}
	err := provisioner.ConfigureBackup(context.Background(), testInstanceID, policy)
	if !errors.Is(err, ErrBackupNotManaged) {
		t.Fatalf("want ErrBackupNotManaged, got %v", err)
	}
	cluster := readCluster(t, provisioner)
	if _, found, _ := unstructured.NestedMap(cluster.Object, "spec", "backup"); found {
		t.Error("backup configuration was written onto a cluster the platform does not own")
	}
	if _, err := readSchedule(t, provisioner); !apierrors.IsNotFound(err) {
		t.Errorf("a schedule was created for a cluster the platform does not own: %v", err)
	}
}

func TestSwitchingBackupsOffRemovesTheConfigurationAndNothingElse(t *testing.T) {
	provisioner := cnpgAgainstFakeCluster(t, managedCluster())
	on := BackupPolicy{Enabled: true, RetentionPolicy: "30d", Destination: backedUpDestination()}
	if err := provisioner.ConfigureBackup(context.Background(), testInstanceID, on); err != nil {
		t.Fatalf("configuring the backup: %v", err)
	}
	if err := provisioner.ConfigureBackup(context.Background(), testInstanceID, BackupPolicy{}); err != nil {
		t.Fatalf("switching the backup off: %v", err)
	}

	cluster := readCluster(t, provisioner)
	if _, found, _ := unstructured.NestedMap(cluster.Object, "spec", "backup"); found {
		t.Error("spec.backup survived a policy that was switched off")
	}
	if _, err := readSchedule(t, provisioner); !apierrors.IsNotFound(err) {
		t.Errorf("the schedule survived a policy that was switched off: %v", err)
	}
	// Nothing else on the Cluster moves, and nothing at the destination is
	// touched at all: switching a policy off is not a deletion, and neither
	// is deleting the claim.
	if instances, found, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances"); !found || instances != 1 {
		t.Errorf("removing the policy touched something else: %v", cluster.Object["spec"])
	}
}

// defaultingClient is an API server that does what CloudNativePG's CRD asks
// of one: `spec.backup.target` is declared with a default, so a block written
// without it comes back carrying it. It counts the Cluster writes it is asked
// for, which is the thing the test is actually about.
type defaultingClient struct {
	client.Client
	clusterWrites int
}

func (c *defaultingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	object, ok := obj.(*unstructured.Unstructured)
	if ok && object.GetKind() == clusterGVK().Kind {
		c.clusterWrites++
		if _, found, _ := unstructured.NestedMap(object.Object, "spec", "backup"); found {
			if err := unstructured.SetNestedField(
				object.Object, "prefer-standby", "spec", "backup", "target"); err != nil {
				return err
			}
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestAnUnchangedPolicyIsNotWrittenAgainEveryPass(t *testing.T) {
	// The failure this pins is a loop rather than a wrong value. The database
	// operator's own CRD defaults a field inside spec.backup, so a block
	// compared as a whole differs from what was sent on every pass — and a
	// reconciler that then writes it again bumps the Cluster's generation
	// forever, asking the database to reconsider itself each time.
	provisioner := cnpgAgainstFakeCluster(t, managedCluster())
	counting := &defaultingClient{Client: provisioner.Client}
	provisioner.Client = counting

	policy := BackupPolicy{Enabled: true, RetentionPolicy: "30d", Destination: backedUpDestination()}
	for pass := range 3 {
		if err := provisioner.ConfigureBackup(context.Background(), testInstanceID, policy); err != nil {
			t.Fatalf("configuring the backup on pass %d: %v", pass, err)
		}
	}
	if counting.clusterWrites != 1 {
		t.Errorf("the cluster was written %d times for one unchanged policy, want 1", counting.clusterWrites)
	}
	// And the field the API server defaulted is still there: the platform
	// writes the two fields it owns into spec.backup rather than over it.
	cluster := readCluster(t, provisioner)
	if got := nestedString(cluster, "spec", "backup", "target"); got != "prefer-standby" {
		t.Errorf("spec.backup.target = %q, want the value the database's own operator put there", got)
	}
}

func TestAPrunedConfigurationIsReportedRatherThanLookingApplied(t *testing.T) {
	// What a CloudNativePG that has removed the in-tree object store does:
	// the API server prunes the field it no longer declares, silently. The
	// fake client cannot prune, so the test plays the API server's part by
	// handing back a cluster the write did not stick to.
	provisioner := cnpgAgainstFakeCluster(t, managedCluster())
	provisioner.Client = pruningClient{Client: provisioner.Client}

	policy := BackupPolicy{Enabled: true, Destination: backedUpDestination()}
	err := provisioner.ConfigureBackup(context.Background(), testInstanceID, policy)
	if !errors.Is(err, ErrBackupUnsupported) {
		t.Fatalf("want ErrBackupUnsupported, got %v", err)
	}
	if !strings.Contains(err.Error(), "plugin") {
		t.Errorf("the refusal should name what would be needed instead: %v", err)
	}
}

func TestBackupStateReadsWhatTheDatabaseReportsAboutItself(t *testing.T) {
	cluster := managedCluster()
	if err := unstructured.SetNestedMap(cluster.Object, map[string]any{
		"barmanObjectStore": map[string]any{"destinationPath": "s3://kitchen-archive/prod/databases"},
		"retentionPolicy":   "30d",
	}, "spec", "backup"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedMap(cluster.Object, map[string]any{
		"firstRecoverabilityPoint": "2026-08-01T03:14:00Z",
		"lastSuccessfulBackup":     "2026-09-03T03:00:00Z",
		"lastFailedBackup":         "2026-08-30T03:00:00Z",
		"conditions": []any{
			map[string]any{"type": "ContinuousArchiving", "status": "True", "message": "continuous archiving is working"},
		},
	}, "status"); err != nil {
		t.Fatal(err)
	}

	provisioner := cnpgAgainstFakeCluster(t, cluster)
	if err := provisioner.ConfigureBackup(context.Background(), testInstanceID, BackupPolicy{
		Enabled: true, Schedule: "0 0 3 * * *", RetentionPolicy: "30d", Destination: backedUpDestination(),
	}); err != nil {
		t.Fatalf("configuring the backup: %v", err)
	}

	state, err := provisioner.BackupState(context.Background(), testInstanceID)
	if err != nil {
		t.Fatalf("reading the state: %v", err)
	}
	if !state.Configured {
		t.Error("a cluster with a destination path is configured")
	}
	want := time.Date(2026, 8, 1, 3, 14, 0, 0, time.UTC)
	if state.FirstRecoverablePoint == nil || !state.FirstRecoverablePoint.Equal(want) {
		t.Errorf("firstRecoverablePoint = %v, want %v", state.FirstRecoverablePoint, want)
	}
	if state.LastBackup == nil || !state.LastBackup.Equal(time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("lastBackup = %v", state.LastBackup)
	}
	if state.LastFailure == nil {
		t.Error("a reported failed backup should reach the state")
	}
	if state.Archiving != ArchivingHealthy {
		t.Errorf("archiving = %q, want healthy", state.Archiving)
	}
	if state.RetentionPolicy != "30d" || state.Schedule != "0 0 3 * * *" {
		t.Errorf("the policy should be read back, got schedule=%q retention=%q",
			state.Schedule, state.RetentionPolicy)
	}
}

func TestBackupStateReportsArchivingThatIsFailing(t *testing.T) {
	cluster := managedCluster()
	if err := unstructured.SetNestedSlice(cluster.Object, []any{
		map[string]any{
			"type": "ContinuousArchiving", "status": "False",
			"reason": "ContinuousArchivingFailing", "message": "unable to upload WAL: access denied",
		},
	}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	provisioner := cnpgAgainstFakeCluster(t, cluster)
	state, err := provisioner.BackupState(context.Background(), testInstanceID)
	if err != nil {
		t.Fatalf("reading the state: %v", err)
	}
	if state.Archiving != ArchivingFailing {
		t.Errorf("archiving = %q, want failing", state.Archiving)
	}
	if !strings.Contains(state.ArchivingMessage, "access denied") {
		t.Errorf("the database's own words should travel with it, got %q", state.ArchivingMessage)
	}
	// The half that fails invisibly is reported apart from the schedule: a
	// database whose WAL is not archiving still has a green schedule.
	if state.FirstRecoverablePoint != nil {
		t.Error("a database that has never backed up has no recovery point")
	}
}

func TestBackupStateOfADatabaseThatIsNotThereIsEmptyRatherThanAnError(t *testing.T) {
	provisioner := cnpgAgainstFakeCluster(t)
	state, err := provisioner.BackupState(context.Background(), testInstanceID)
	if err != nil {
		t.Fatalf("want an empty state, got %v", err)
	}
	if state.Configured || state.FirstRecoverablePoint != nil {
		t.Errorf("state = %+v", state)
	}
}

func TestNeonReportsThatItKeepsItsOwnBackups(t *testing.T) {
	var provisioner Provisioner = &Neon{}
	selfBacking, ok := provisioner.(SelfBackingProvisioner)
	if !ok {
		t.Fatal("a provider that keeps its own history has to say so")
	}
	if _, isBacker := provisioner.(BackupProvisioner); isBacker {
		t.Error("a provider the platform cannot configure must not claim it takes a policy")
	}
	if note := selfBacking.ManagedBackupNote(); !strings.Contains(note, "retention") {
		t.Errorf("the note should say what decides how much is kept, got %q", note)
	}
}

func TestCNPGIsABackupProvisionerAndNotASelfBackingOne(t *testing.T) {
	var provisioner Provisioner = cnpgAgainstFakeCluster(t)
	if _, ok := provisioner.(BackupProvisioner); !ok {
		t.Error("the in-cluster provider takes a policy")
	}
	if _, ok := provisioner.(SelfBackingProvisioner); ok {
		t.Error("a database this platform runs is not backed up by anybody else")
	}
}

func TestBackupDestinationPathNeverEchoesACredential(t *testing.T) {
	destination := backedUpDestination()
	path := destination.Path()
	if path != "s3://kitchen-archive/prod/databases" {
		t.Errorf("path = %q", path)
	}
	if strings.Contains(path, destination.CredentialsSecret) {
		t.Errorf("a description must not name the credential: %q", path)
	}
	if (BackupDestination{}).Configured() {
		t.Error("a destination with no bucket is not configured")
	}
}

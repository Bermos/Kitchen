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
	"fmt"
	"reflect"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// Backing up a database this cluster runs: continuous WAL archiving to an
// object store, plus a base backup on a schedule.
//
// **Which mechanism, settled against the pinned chart** (#245 phase 2's first
// requirement). `DefaultCNPGChartVersion = "0.29.0"` ships CloudNativePG
// **1.30.0**, and at that version:
//
//   - `spec.backup.barmanObjectStore` is present in the Cluster CRD, is the
//     default backup method, and works. It is deprecated — since 1.26, in
//     favour of the barman-cloud CNPG-I plugin — and 1.30.0's own release
//     notes move the removal to **1.31.0**.
//   - The plugin is the successor and is not required here. It would have to
//     be installed beside the operator, it needs its own CRD and its own
//     `ObjectStore` object per destination, and — the part that decides it —
//     **it cannot be installed into a CloudNativePG the platform merely
//     found.** An adopted installation (`cloudnative-pg` Addon with
//     `status.managed: false`) is one Kitchen must not write releases into,
//     and a mechanism that needs one would mean no claim backups at all
//     there.
//   - The three status fields this phase reports — `firstRecoverabilityPoint`,
//     `lastSuccessfulBackup`, `lastFailedBackup` — are documented at 1.30.0
//     as *"not set for backup plugins"*. The in-tree mechanism is the one the
//     operator still reports about itself.
//
// So Kitchen writes the in-tree configuration, installs nothing extra, and
// works identically on an adopted CloudNativePG. What that buys is bounded by
// the removal, and the bound is **enforced rather than remembered**: a
// Kubernetes API server prunes fields a CRD no longer declares *silently*, so
// the write below is read back and a configuration that did not survive is
// reported as ErrBackupUnsupported naming the plugin — instead of a Cluster
// that looks configured and archives nothing. Bumping
// DefaultCNPGChartVersion onto an operator that has dropped the field is
// therefore a failing claim with a sentence on it, not a silent loss.

const (
	// DefaultClaimBackupSchedule is when a base backup is taken for a claim
	// whose Connection names no schedule: 03:00 UTC, nightly.
	//
	// It is **six fields, seconds first** — CloudNativePG's schedule is
	// robfig/cron's and not Kubernetes', which is a real difference and the
	// single easiest thing to get wrong here. "0 3 * * *" is a valid
	// five-field expression that this operator reads as something else
	// entirely, so a five-field schedule is refused rather than passed
	// through.
	DefaultClaimBackupSchedule = "0 0 3 * * *"

	// cnpgScheduleFields is what a CloudNativePG schedule has.
	cnpgScheduleFields = 6

	// scheduledBackupSuffix names a Cluster's ScheduledBackup after it. A
	// Cluster name is capped at maxClusterName, which leaves room.
	scheduledBackupSuffix = "-backup"
)

// scheduledBackupGVK is CloudNativePG's schedule object.
func scheduledBackupGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "ScheduledBackup"}
}

// scheduledBackupName is what a Cluster's schedule object is called.
func scheduledBackupName(cluster string) string { return cluster + scheduledBackupSuffix }

// ConfigureBackup points a Cluster at an object store and gives it a base
// backup schedule — or takes both away when the policy is off.
//
// Three refusals, each of which is a fact worth reporting on the claim rather
// than an error to retry:
//
//   - a Cluster the platform did not create is never written to
//     (ErrBackupNotManaged), the same rule every other object here keeps;
//   - a schedule in the wrong dialect is refused before it is written, because
//     the wrong one is *valid* and means something else;
//   - a configuration the API server pruned is ErrBackupUnsupported, which is
//     the guard against the deprecation this mechanism is under.
func (c *CNPG) ConfigureBackup(ctx context.Context, instanceID string, policy BackupPolicy) error {
	cluster, err := c.cluster(ctx, instanceID)
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: the database is not there yet, so there is nothing to back up", ErrNotReady)
	case err != nil:
		return err
	}

	if cluster.GetLabels()[managedByLabel] != managedByValue {
		return fmt.Errorf("%w: database %s was handed to this platform rather than created by it, and "+
			"Kitchen does not write backup configuration onto a Cluster it does not own — whoever runs it "+
			"keeps backing it up", ErrBackupNotManaged, cluster.GetName())
	}

	if !policy.Enabled {
		return c.removeBackupConfiguration(ctx, cluster)
	}
	if !policy.Destination.Configured() {
		return fmt.Errorf("a backup policy needs a destination: there is deliberately no local one, since " +
			"an archive on the cluster it exists to survive the loss of is not a backup")
	}
	schedule, err := validCNPGSchedule(policy.Schedule)
	if err != nil {
		return err
	}

	if err := c.applyObjectStore(ctx, cluster, policy); err != nil {
		return err
	}
	return c.ensureScheduledBackup(ctx, cluster, schedule)
}

// applyObjectStore writes spec.backup onto the Cluster and proves it landed.
//
// It writes the two fields it owns **into** whatever spec.backup already
// holds, rather than replacing the block. That is not politeness: CloudNativePG
// declares `spec.backup.target` with a default, so a block written as a whole
// comes back from the API server with a field the platform never sent — and a
// comparison against the whole block would then differ on every pass and write
// the Cluster again forever, bumping its generation and asking the database's
// operator to reconsider itself each time.
func (c *CNPG) applyObjectStore(
	ctx context.Context,
	cluster *unstructured.Unstructured,
	policy BackupPolicy,
) error {
	current, _, err := unstructured.NestedMap(cluster.Object, "spec", "backup")
	if err != nil {
		return err
	}
	if current == nil {
		current = map[string]any{}
	}
	backup := map[string]any{}
	for key, value := range current {
		backup[key] = value
	}
	backup["barmanObjectStore"] = barmanObjectStore(policy.Destination, cluster.GetName())
	if retention := strings.TrimSpace(policy.RetentionPolicy); retention != "" {
		backup["retentionPolicy"] = retention
	} else {
		delete(backup, "retentionPolicy")
	}

	if !reflect.DeepEqual(current, backup) {
		if err := unstructured.SetNestedMap(cluster.Object, backup, "spec", "backup"); err != nil {
			return err
		}
		if err := c.Client.Update(ctx, cluster); err != nil {
			if meta.IsNoMatchError(err) {
				return notInstalled(err)
			}
			return err
		}
	}

	// The object in hand is now the API server's own answer — Update decodes
	// the response into it — so this reads what was *accepted* and not what
	// was sent. A structural schema prunes what it does not declare without
	// a word, so an empty destinationPath here is the deprecation having
	// landed, and it is reported rather than left to look like a working
	// configuration that archives nothing.
	if nestedString(cluster, "spec", "backup", "barmanObjectStore", "destinationPath") == "" {
		return fmt.Errorf("%w: this CloudNativePG did not keep the in-tree object store configuration "+
			"written onto Cluster %s, which is what a version that has removed "+
			"spec.backup.barmanObjectStore does. Backing this database up needs the barman-cloud CNPG-I "+
			"plugin, which the platform does not install; see docs/BACKUP.md",
			ErrBackupUnsupported, cluster.GetName())
	}
	return nil
}

// barmanObjectStore is the object store block CloudNativePG takes.
//
// serverName is the Cluster's own name, and that is what gives every claim a
// prefix of its own: barman writes under <destinationPath>/<serverName>, so
// two databases sharing the platform's bucket never share a path and neither
// of them shares one with the platform archive.
func barmanObjectStore(destination BackupDestination, cluster string) map[string]any {
	store := map[string]any{
		"destinationPath": destination.Path(),
		"serverName":      cluster,
	}
	if endpoint := strings.TrimSpace(destination.Endpoint); endpoint != "" {
		store["endpointURL"] = endpoint
	}

	credentials := map[string]any{}
	if secret := strings.TrimSpace(destination.CredentialsSecret); secret != "" {
		credentials["accessKeyId"] = map[string]any{"name": secret, "key": destination.AccessKeyIDKey}
		credentials["secretAccessKey"] = map[string]any{"name": secret, "key": destination.SecretAccessKeyKey}
		if destination.RegionKey != "" {
			credentials["region"] = map[string]any{"name": secret, "key": destination.RegionKey}
		}
	} else {
		// No stored key: the pod's own credential chain — an instance role,
		// IRSA — which is the better answer where it is available.
		credentials["inheritFromIAMRole"] = true
	}
	store["s3Credentials"] = credentials

	// The archive of a database is the database, so a store that can encrypt
	// it at rest is told to. Both halves: a base backup nobody encrypted is
	// not made safer by encrypted WAL.
	if encryption := strings.TrimSpace(destination.ServerSideEncryption); encryption != "" {
		store["data"] = map[string]any{"encryption": encryption}
		store["wal"] = map[string]any{"encryption": encryption}
	}
	return store
}

// removeBackupConfiguration takes the policy off a Cluster and deletes its
// schedule. It removes **configuration only**: everything already at the
// destination stays, because a policy switched off is not a deletion and the
// recovery point is the last thing to destroy by implication.
func (c *CNPG) removeBackupConfiguration(ctx context.Context, cluster *unstructured.Unstructured) error {
	if err := c.deleteScheduledBackup(ctx, cluster.GetNamespace(), cluster.GetName()); err != nil {
		return err
	}
	if _, found, _ := unstructured.NestedMap(cluster.Object, "spec", "backup"); !found {
		return nil
	}
	unstructured.RemoveNestedField(cluster.Object, "spec", "backup")
	if err := c.Client.Update(ctx, cluster); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	return nil
}

// ensureScheduledBackup creates or updates the Cluster's base backup schedule.
//
// Two fields are decisions rather than defaults:
//
//   - `immediate: true` takes a base backup as soon as the schedule exists.
//     Without it a database configured at 09:00 has no recovery point at all
//     until 03:00 the next morning, and `firstRecoverablePoint` — the number
//     this whole phase exists to publish — stays empty through the entire
//     first day.
//   - `backupOwnerReference: none` keeps the Backup objects out of the
//     Cluster's garbage collection. Deleting the database must not look like
//     deleting the record of what was backed up.
func (c *CNPG) ensureScheduledBackup(
	ctx context.Context,
	cluster *unstructured.Unstructured,
	schedule string,
) error {
	name := scheduledBackupName(cluster.GetName())
	key := types.NamespacedName{Namespace: cluster.GetNamespace(), Name: name}

	desired := map[string]any{
		"cluster":              map[string]any{"name": cluster.GetName()},
		"schedule":             schedule,
		"immediate":            true,
		"suspend":              false,
		"backupOwnerReference": "none",
		"method":               "barmanObjectStore",
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(scheduledBackupGVK())
	err := c.Client.Get(ctx, key, existing)
	switch {
	case err == nil:
		current, _, _ := unstructured.NestedMap(existing.Object, "spec")
		if reflect.DeepEqual(current, desired) {
			return nil
		}
		if err := unstructured.SetNestedMap(existing.Object, desired, "spec"); err != nil {
			return err
		}
		return c.Client.Update(ctx, existing)
	case meta.IsNoMatchError(err):
		return notInstalled(err)
	case !apierrors.IsNotFound(err):
		return err
	}

	object := &unstructured.Unstructured{Object: map[string]any{"spec": desired}}
	object.SetGroupVersionKind(scheduledBackupGVK())
	object.SetNamespace(key.Namespace)
	object.SetName(key.Name)
	labels := map[string]string{managedByLabel: managedByValue}
	if project := cluster.GetLabels()[naming.LabelProject]; project != "" {
		labels[naming.LabelProject] = project
	}
	object.SetLabels(labels)
	if err := c.Client.Create(ctx, object); err != nil && !apierrors.IsAlreadyExists(err) {
		if meta.IsNoMatchError(err) {
			return notInstalled(err)
		}
		return err
	}
	return nil
}

// deleteScheduledBackup removes a Cluster's schedule, treating an absent one
// — and a cluster that no longer serves CloudNativePG at all — as done.
func (c *CNPG) deleteScheduledBackup(ctx context.Context, namespace, cluster string) error {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(scheduledBackupGVK())
	object.SetNamespace(namespace)
	object.SetName(scheduledBackupName(cluster))
	err := c.Client.Delete(ctx, object)
	if err == nil || apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	return err
}

// BackupState reads what the object store holds for a database, out of what
// CloudNativePG reports about itself.
//
// Everything here is read and nothing is echoed: the destination is the one
// on the Cluster the operator is actually reconciling, not the one the policy
// asked for, so a configuration that never landed cannot report itself as in
// force.
func (c *CNPG) BackupState(ctx context.Context, instanceID string) (BackupState, error) {
	cluster, err := c.cluster(ctx, instanceID)
	switch {
	case apierrors.IsNotFound(err):
		return BackupState{}, nil
	case err != nil:
		return BackupState{}, err
	}

	state := BackupState{
		Destination:     nestedString(cluster, "spec", "backup", "barmanObjectStore", "destinationPath"),
		RetentionPolicy: nestedString(cluster, "spec", "backup", "retentionPolicy"),
	}
	state.Configured = state.Destination != ""
	state.LastBackup = cnpgTime(nestedString(cluster, "status", "lastSuccessfulBackup"))
	state.LastFailure = cnpgTime(nestedString(cluster, "status", "lastFailedBackup"))
	state.FirstRecoverablePoint = cnpgTime(nestedString(cluster, "status", "firstRecoverabilityPoint"))
	state.Archiving, state.ArchivingMessage = continuousArchiving(cluster)

	schedule := &unstructured.Unstructured{}
	schedule.SetGroupVersionKind(scheduledBackupGVK())
	key := types.NamespacedName{Namespace: cluster.GetNamespace(), Name: scheduledBackupName(cluster.GetName())}
	if err := c.Client.Get(ctx, key, schedule); err == nil {
		if suspended, found, _ := unstructured.NestedBool(schedule.Object, "spec", "suspend"); !found || !suspended {
			state.Schedule = nestedString(schedule, "spec", "schedule")
		}
	} else if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return state, err
	}
	return state, nil
}

// continuousArchiving reads the ContinuousArchiving condition — the one that
// says whether WAL is reaching the object store between base backups, which
// is the half of a backup that fails invisibly.
func continuousArchiving(cluster *unstructured.Unstructured) (ArchivingHealth, string) {
	conditions, found, err := unstructured.NestedSlice(cluster.Object, "status", "conditions")
	if err != nil || !found {
		return ArchivingUnknown, ""
	}
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok || condition["type"] != "ContinuousArchiving" {
			continue
		}
		message, _ := condition["message"].(string)
		if condition["status"] == string(metav1.ConditionTrue) {
			return ArchivingHealthy, message
		}
		if message == "" {
			reason, _ := condition["reason"].(string)
			message = reason
		}
		return ArchivingFailing, message
	}
	return ArchivingUnknown, ""
}

// cnpgTime reads one of CloudNativePG's RFC3339 status timestamps, answering
// nil for the empty string a database nothing has backed up yet reports.
func cnpgTime(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	when, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}
	return &when
}

// validCNPGSchedule refuses a schedule in the wrong dialect before it is
// written.
//
// This is not pedantry about a field count. CloudNativePG's schedule is
// robfig/cron's six-field form with a leading seconds field, and Kubernetes'
// is the five-field form — so "0 3 * * *", meant as three in the morning, is
// accepted by this operator and read as *every hour at three minutes past*.
// A silently different schedule is the exact failure this phase exists to
// prevent, so the refusal names the fix.
func validCNPGSchedule(schedule string) (string, error) {
	schedule = strings.TrimSpace(schedule)
	if schedule == "" {
		return DefaultClaimBackupSchedule, nil
	}
	if fields := len(strings.Fields(schedule)); fields != cnpgScheduleFields {
		return "", fmt.Errorf(
			"%w: a CloudNativePG backup schedule has %d fields with seconds first, and this one has %d "+
				"(%q). A five-field schedule is not rejected by the database operator, it is read as "+
				"something else — write %q for %s",
			ErrUnsatisfiable, cnpgScheduleFields, fields, schedule,
			DefaultClaimBackupSchedule, "03:00 UTC nightly")
	}
	return schedule, nil
}

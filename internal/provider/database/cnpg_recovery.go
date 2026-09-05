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
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// Recovering a database this cluster runs to a moment in the past (#247), the
// CloudNativePG half of the RecoverableProvisioner contract Neon's half
// already implements.
//
// CloudNativePG cannot rewind a Cluster in place and does not try to. A
// recovery is a **new** Cluster bootstrapped from the object store #245 phase
// 2 configured, with a `recoveryTarget` naming the moment; what comes back is
// a sibling database holding the old data under an address of its own. That
// is precisely the primitive the platform's recover-then-promote is built on,
// and it is the same shape as Neon's branch at a parent timestamp — the two
// providers differ in where the history is kept and in nothing else the
// platform can see.
//
// Three facts follow, and each of them is a refusal with a sentence rather
// than a surprise at the end of a recovery:
//
//   - **Recovery here is the backup policy's second half.** There is nothing
//     to bootstrap *from* until the claim has an object store and a base
//     backup in it, so a claim without one offers no recovery and says which
//     of the two it is short of. A preview's Cluster is built from the shape
//     of its parent and never from its spec, so it carries no object store,
//     and a preview reports the same sentence rather than a broken picker.
//   - **A Cluster the platform did not create is not recovered from.** Its
//     archives are whoever runs it's, along with the credential that reaches
//     them — the same rule that stops ConfigureBackup writing onto an adopted
//     Cluster stops this reading one's destination and bootstrapping from it.
//   - **The window is read, never declared.** Its earliest edge is the
//     Cluster's own `firstRecoverabilityPoint` and its latest edge is bounded
//     by WAL archiving rather than by the clock: what has not reached the
//     object store cannot be replayed out of it.
//
// The recovered Cluster archives to the same destination under a serverName
// of its own, and inherits the source's schedule once it is up. That is not
// tidiness: CloudNativePG's own documentation refuses to share one serverName
// between two clusters, and a promoted recovery that carried no policy would
// be a production database with no archive behind it from the moment it took
// over.

const (
	// walArchiveLag is how far behind the present the recoverable window
	// ends.
	//
	// CloudNativePG sets `archive_timeout = '5min'` among its global
	// defaults, so the segment covering the last few minutes of writes has
	// not necessarily been shipped yet — and PostgreSQL does not stop
	// politely short of a target it cannot reach, it fails the recovery
	// ("recovery ended before configured recovery target was reached"). So
	// the window ends one archive_timeout back, which is the difference
	// between a picker offering moments the archive can serve and one
	// offering a failed Cluster.
	walArchiveLag = 5 * time.Minute

	// recoveryTargetTimeLayout is how the moment is written into
	// `recoveryTarget.targetTime`. RFC 3339 is what CloudNativePG documents,
	// and the timezone is always spelled out: a timestamp without one is read
	// as UTC by this operator and as local time by half the tools that would
	// produce it, which is a recovery to the wrong moment rather than an
	// error.
	recoveryTargetTimeLayout = time.RFC3339
)

// cnpgTerminalPhases are the Cluster phases a recovery does not come back
// from on its own. They are matched rather than merely waited on because the
// alternative is a recovery that reads "Pending" forever: a database that is
// still coming up and one CloudNativePG has given up on look identical from
// the outside until somebody reads the phase.
//
// The check lives here rather than in ready(), which every provision also
// goes through, because turning a slow provision into a failed claim is a
// change to another feature's behaviour and not this one's.
var cnpgTerminalPhases = []string{
	"Cluster is unrecoverable and needs manual intervention",
	"Invalid cluster definition",
	"Unable to create required cluster objects",
	"Cluster has incomplete or invalid image catalog",
}

// RecoveryWindow is what this database can actually be reconstructed to,
// read off the Cluster CloudNativePG is reconciling and never off the policy
// that asked for it.
func (c *CNPG) RecoveryWindow(ctx context.Context, instanceID string) (RecoveryWindow, error) {
	cluster, err := c.recoverySource(ctx, instanceID)
	if err != nil {
		return RecoveryWindow{}, err
	}
	return windowOf(cluster, time.Now().UTC())
}

// RecoverTo creates (or finds) the sibling Cluster holding this database's
// data as it was at `at`, bootstrapped from the claim's own archives.
//
// It is idempotent by name, like every other operation here: a reconcile that
// runs twice finds the Cluster it made rather than starting a second recovery
// a minute later, and answers ErrNotReady for the several minutes
// CloudNativePG spends fetching a base backup and replaying WAL over it.
func (c *CNPG) RecoverTo(ctx context.Context, instanceID, name string, at time.Time) (Branch, error) {
	if at.IsZero() {
		return Branch{}, fmt.Errorf("recovering %s needs a moment to recover to", name)
	}
	source, err := c.recoverySource(ctx, instanceID)
	if err != nil {
		return Branch{}, err
	}

	child := branchName(source.GetName(), name)
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(clusterGVK())
	err = c.Client.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: child}, existing)
	switch {
	case err == nil:
		return c.recoveredBranch(ctx, source, existing)
	case !apierrors.IsNotFound(err):
		return Branch{}, err
	}

	// The moment is checked only on the way to *creating* the Cluster. A
	// recovery already made is not un-made by retention sliding past the
	// moment it holds — the data is in that database now, whatever the
	// archive can still reach.
	window, err := windowOf(source, time.Now().UTC())
	if err != nil {
		return Branch{}, err
	}
	if !window.Contains(at) {
		return Branch{}, fmt.Errorf("%w: %s cannot be recovered to %s: its archive reaches back to %s and "+
			"no further forward than %s, which is where the write-ahead log has been shipped to",
			ErrUnsatisfiable, source.GetName(), at.UTC().Format(time.RFC3339),
			window.Earliest.UTC().Format(time.RFC3339), window.Latest.UTC().Format(time.RFC3339))
	}

	desired, err := c.desiredRecoveryCluster(source, child, at)
	if err != nil {
		return Branch{}, err
	}
	if err := c.ensureNamespace(ctx); err != nil {
		return Branch{}, err
	}
	if err := c.Client.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return Branch{}, err
		}
		// Two reconciles raced; the winner's object is the one to read.
		if err := c.Client.Get(ctx,
			types.NamespacedName{Namespace: c.Namespace, Name: child}, desired); err != nil {
			return Branch{}, err
		}
	}
	return c.recoveredBranch(ctx, source, desired)
}

// recoveredBranch answers for a recovery Cluster that exists: failed, still
// replaying, or serving with a binding of its own.
func (c *CNPG) recoveredBranch(
	ctx context.Context,
	source *unstructured.Unstructured,
	recovered *unstructured.Unstructured,
) (Branch, error) {
	if phase := terminalPhase(recovered); phase != "" {
		return Branch{}, fmt.Errorf("recovering %s from the archive of %s failed: %s",
			recovered.GetName(), source.GetName(), phase)
	}
	if err := c.ready(recovered); err != nil {
		return Branch{}, err
	}
	// The schedule is given to the recovery once it is serving, not while it
	// is still replaying: a base backup asked for of a database that has not
	// finished coming up is a failed Backup object and a line in the log for
	// every minute of a normal wait.
	if err := c.inheritBackupSchedule(ctx, source, recovered); err != nil {
		return Branch{}, err
	}
	binding, err := c.binding(ctx, recovered.GetName())
	if err != nil {
		return Branch{}, err
	}
	return Branch{
		ID:      c.Namespace + "/" + recovered.GetName(),
		Binding: binding,
		// A point-in-time recovery of a production database is production
		// data at an earlier moment. Correct by construction rather than by
		// policy — the same declaration Neon's recovery branch carries.
		Provenance: ProvenanceProduction,
	}, nil
}

// recoverySource reads the Cluster a recovery would be taken from and refuses
// the two cases that have nothing behind them, each with the sentence that
// names the fix.
func (c *CNPG) recoverySource(ctx context.Context, instanceID string) (*unstructured.Unstructured, error) {
	cluster, err := c.cluster(ctx, instanceID)
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("%w: the database is not there yet, so there is nothing to recover from",
			ErrNotReady)
	case err != nil:
		return nil, err
	}

	if cluster.GetLabels()[managedByLabel] != managedByValue {
		return nil, fmt.Errorf("%w: database %s was handed to this platform rather than created by it, so "+
			"its archives — and the credential that reaches them — are whoever runs it's. Kitchen recovers "+
			"from an object store it configured and from no other",
			ErrBackupNotManaged, cluster.GetName())
	}
	if nestedString(cluster, "spec", "backup", "barmanObjectStore", "destinationPath") == "" {
		return nil, fmt.Errorf("%w: recovering a database this cluster runs means bootstrapping a new one "+
			"from its archive, and %s has no backup policy to archive to. Give the claim one — spec.backup, "+
			"or the platform's own backup destination, which every claim inherits — and the window follows "+
			"its first base backup", ErrUnsatisfiable, cluster.GetName())
	}
	return cluster, nil
}

// windowOf is the span the archive can serve, from what CloudNativePG reports
// about itself.
//
// Both edges are observed. The earliest is the operator's own
// firstRecoverabilityPoint, which moves forward as retention prunes; the
// latest is bounded by *archiving* and not by the clock, because a moment
// whose write-ahead log has not been shipped is a moment the archive cannot
// replay to. While continuous archiving is healthy that bound is one
// archive_timeout back from now; while it is not, it is the last base backup
// that was read back — the newest moment the platform can still stand behind.
func windowOf(cluster *unstructured.Unstructured, now time.Time) (RecoveryWindow, error) {
	earliest := cnpgTime(nestedString(cluster, "status", "firstRecoverabilityPoint"))
	if earliest == nil {
		return RecoveryWindow{}, fmt.Errorf("%w: %s has not reported a recovery point yet, which is the "+
			"state a database is in until its first base backup has been taken and read back",
			ErrNotReady, cluster.GetName())
	}

	health, message := continuousArchiving(cluster)
	latest := now.Add(-walArchiveLag)
	if health != ArchivingHealthy {
		last := cnpgTime(nestedString(cluster, "status", "lastSuccessfulBackup"))
		if last == nil {
			return RecoveryWindow{}, fmt.Errorf("%w: the write-ahead log of %s is not reaching its object "+
				"store and no base backup has been read back either, so there is no moment it can be "+
				"reconstructed to%s", ErrNotReady, cluster.GetName(), archivingAside(message))
		}
		latest = last.UTC()
	}
	if !earliest.Before(latest) {
		return RecoveryWindow{}, fmt.Errorf("%w: %s can reach back no further than it can reach forward "+
			"— its archive holds %s and nothing newer that has been shipped%s", ErrNotReady,
			cluster.GetName(), earliest.UTC().Format(time.RFC3339), archivingAside(message))
	}
	return RecoveryWindow{Earliest: earliest.UTC(), Latest: latest}, nil
}

// archivingAside appends what the database's operator said about its
// archiving, where it said anything. The sentence in front of it is the
// platform's; this is the provider's, and the two are kept apart.
func archivingAside(message string) string {
	if strings.TrimSpace(message) == "" {
		return ""
	}
	return " (" + message + ")"
}

// terminalPhase reports the phase of a Cluster CloudNativePG has stopped
// working on, and "" for one it has not.
func terminalPhase(cluster *unstructured.Unstructured) string {
	phase := nestedString(cluster, "status", "phase")
	for _, terminal := range cnpgTerminalPhases {
		if phase == terminal {
			return phase
		}
	}
	return ""
}

// desiredRecoveryCluster builds the sibling: the source's shape, bootstrapped
// from the source's archive at the requested moment.
//
// The two object store blocks are deliberately different in exactly one
// field. `externalClusters` names the **source's** serverName, which is the
// prefix the archive was written under and the whole of how barman finds it;
// `spec.backup` names the recovery's own, because CloudNativePG refuses to
// share one serverName between two clusters and a recovery archiving over its
// own source would destroy the thing it was recovered from.
func (c *CNPG) desiredRecoveryCluster(
	source *unstructured.Unstructured,
	name string,
	at time.Time,
) (*unstructured.Unstructured, error) {
	// Two copies of the source's archive configuration: NestedMap answers a
	// deep copy each time, so the two can differ in the one field they have
	// to differ in without either of them aliasing the source's own block.
	from, found, err := unstructured.NestedMap(source.Object, "spec", "backup", "barmanObjectStore")
	if err != nil || !found {
		return nil, fmt.Errorf("%w: the archive configuration of %s could not be read", ErrUnsatisfiable,
			source.GetName())
	}
	store, _, err := unstructured.NestedMap(source.Object, "spec", "backup", "barmanObjectStore")
	if err != nil {
		return nil, err
	}
	from["serverName"] = source.GetName()
	store["serverName"] = name

	storage := map[string]any{"size": DefaultStorageSize}
	if inherited, found, err := unstructured.NestedMap(source.Object, "spec", "storage"); err == nil && found {
		storage = inherited
	}
	instances := int64(c.Instances)
	if inherited, found, err := unstructured.NestedInt64(source.Object, "spec", "instances"); err == nil && found {
		instances = inherited
	}

	backup := map[string]any{"barmanObjectStore": store}
	if retention := nestedString(source, "spec", "backup", "retentionPolicy"); retention != "" {
		backup["retentionPolicy"] = retention
	}

	spec := map[string]any{
		"instances": instances,
		"storage":   storage,
		"backup":    backup,
		"bootstrap": map[string]any{"recovery": map[string]any{
			"source": source.GetName(),
			// The application database and its owner are named rather than
			// left to default, for the same reason initdb names them: the
			// binding Secret is the interface, and the operator creates the
			// role and publishes the credential once the recovery is
			// promoted to primary.
			"database":       applicationDatabase,
			"owner":          applicationUser,
			"recoveryTarget": map[string]any{"targetTime": at.UTC().Format(recoveryTargetTimeLayout)},
		}},
		"externalClusters": []any{map[string]any{
			"name":              source.GetName(),
			"barmanObjectStore": from,
		}},
	}
	if image := nestedString(source, "spec", "imageName"); image != "" {
		// The same image as the source, always: PostgreSQL will not start on
		// a data directory written by a newer major, and a recovery is that
		// data directory.
		spec["imageName"] = image
	}

	cluster := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	cluster.SetGroupVersionKind(clusterGVK())
	cluster.SetName(name)
	cluster.SetNamespace(c.Namespace)
	labels := map[string]string{managedByLabel: managedByValue}
	if project := source.GetLabels()[naming.LabelProject]; project != "" {
		labels[naming.LabelProject] = project
	}
	cluster.SetLabels(labels)
	return cluster, nil
}

// inheritBackupSchedule gives a serving recovery the base backup schedule its
// source has, so that a sibling somebody promotes is itself recoverable from
// the moment it takes over.
//
// Continuous archiving alone would not be enough for that: a recovery point
// is a base backup with write-ahead log after it, and a Cluster nothing ever
// takes a base backup of reports no first recoverable point however healthy
// its archiving is. A source with no schedule hands on none, which is the
// honest answer for a database whose policy was switched off.
func (c *CNPG) inheritBackupSchedule(
	ctx context.Context,
	source *unstructured.Unstructured,
	recovered *unstructured.Unstructured,
) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(scheduledBackupGVK())
	key := types.NamespacedName{
		Namespace: source.GetNamespace(),
		Name:      scheduledBackupName(source.GetName()),
	}
	if err := c.Client.Get(ctx, key, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	schedule := nestedString(existing, "spec", "schedule")
	if schedule == "" {
		return nil
	}
	return c.ensureScheduledBackup(ctx, recovered, schedule)
}

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
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What the platform did to its own dependencies, kept after it finished.
//
// An Addon's `status.charts` is singular and current. The operator carries a
// dependency forward whenever its pins move — that is what
// `addonVersionsMoved` is for — and the previous versions are overwritten by
// the reconcile that observes the new ones. Six projects degrade at 04:05, an
// addon was upgraded at 04:03, and by the time anybody looks the only evidence
// is a version number that has always said what it says now. The install job
// that carried it is reaped an hour after it finishes.
//
// So an upgrade leaves an AddonUpgrade behind, which is PlatformUpdate's shape
// one layer down: an immutable object per attempt, kept after it finishes, so
// the list is the history.
//
// Two properties are worth stating, because both are the reason this is not a
// bounded slice on the Addon's status:
//
//   - it is written *before* the outcome is known, so a failed attempt is in
//     the history too — the upgrade that broke something is at least as
//     interesting as the one that worked;
//   - the record is keyed to the install job, which is named after the
//     versions it installs and is a *new* job for every bump. One attempt is
//     one job is one record, including the seeded KEDA pair, whose two charts
//     move together and are recorded together.

// addonUpgradeUIDChars is how much of the install job's UID disambiguates one
// attempt from the next at the same pins.
//
// The name would otherwise be the job's, which is derived from the versions:
// a failed install is retried once its finished job is reaped, and a retry is
// a new attempt that must not overwrite the record of the one that failed. It
// is the job's identity rather than a clock so that a reconcile which runs
// twice writes one record.
const addonUpgradeUIDChars = 5

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=addonupgrades,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=addonupgrades/status,verbs=get;update;patch

// recordUpgrade keeps this entry's upgrade record in step with the install job
// carrying it: created when the job is first seen, closed when the job is.
//
// It is called before anything writes to the Addon's status, because
// `status.charts` is the only statement of what was installed *before* and the
// settle at the end of the reconcile is what overwrites it.
func (r *AddonReconciler) recordUpgrade(
	ctx context.Context,
	addon *kitchenv1alpha1.Addon,
	entry addonEntry,
	cfg AddonInstallConfig,
	observed addonObservation,
) error {
	// An installation that asks for nothing, or was granted nothing, has no
	// job of the platform's own making to record.
	if !addon.Spec.Install || !observed.permitted {
		return nil
	}

	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: addonInstallJobName(entry, cfg)}
	if err := r.Get(ctx, key, job); err != nil {
		return client.IgnoreNotFound(err)
	}

	record := &kitchenv1alpha1.AddonUpgrade{}
	name := addonUpgradeName(job)
	err := r.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: name}, record)
	switch {
	case apierrors.IsNotFound(err):
		var created bool
		if record, created, err = r.openUpgradeRecord(ctx, addon, entry, cfg, observed, job); err != nil {
			return err
		} else if !created {
			return nil
		}
	case err != nil:
		return err
	}
	return r.closeUpgradeRecord(ctx, record, job)
}

// openUpgradeRecord writes the record of an upgrade that has started, and
// reports whether there was one to write.
//
// There is not, in two cases that look alike and are not. An entry the
// platform has never installed has nothing to move *from*: that is a first
// install, which the Addon's own condition already states and which no history
// is missing. And a job whose pins are the ones already recorded is the
// reconcile after a finished upgrade rather than a new one — the record it
// would duplicate is the one being closed.
func (r *AddonReconciler) openUpgradeRecord(
	ctx context.Context,
	addon *kitchenv1alpha1.Addon,
	entry addonEntry,
	cfg AddonInstallConfig,
	observed addonObservation,
	job *batchv1.Job,
) (*kitchenv1alpha1.AddonUpgrade, bool, error) {
	from := addon.Status.Charts
	to := addonTargetCharts(entry, cfg)
	if len(from) == 0 || !addonChartsDiffer(from, to) {
		return nil, false, nil
	}

	record := &kitchenv1alpha1.AddonUpgrade{
		ObjectMeta: metav1.ObjectMeta{
			Name:      addonUpgradeName(job),
			Namespace: PlatformNamespace,
			Labels:    platformLabels(entry.ID, entry.ID),
		},
		Spec: kitchenv1alpha1.AddonUpgradeSpec{
			Addon:     addon.Name,
			From:      from,
			To:        to,
			Namespace: installedInto(job, observed.namespace),
			JobName:   job.Name,
		},
	}
	if err := r.Create(ctx, record); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Two reconciles of the same addon raced. The one that lost reads
			// the record back rather than deciding there is none.
			key := types.NamespacedName{Namespace: PlatformNamespace, Name: record.Name}
			return record, true, r.Get(ctx, key, record)
		}
		return nil, false, err
	}
	logf.FromContext(ctx).Info("recorded an addon upgrade",
		"addon", addon.Name, "upgrade", record.Name, "to", addonChartSummary(to))
	return record, true, nil
}

// closeUpgradeRecord moves the record to what the job now says. It is derived
// from the job on every pass rather than remembered, so the reconcile that
// reports an upgrade finished need not be the one that opened it.
func (r *AddonReconciler) closeUpgradeRecord(
	ctx context.Context,
	record *kitchenv1alpha1.AddonUpgrade,
	job *batchv1.Job,
) error {
	status := kitchenv1alpha1.AddonUpgradeStatus{
		Phase:     kitchenv1alpha1.AddonUpgradeRunning,
		StartedAt: addonUpgradeStartedAt(job),
	}
	complete, failed, message := jobOutcome(job)
	switch {
	case complete:
		status.Phase = kitchenv1alpha1.AddonUpgradeSucceeded
		status.CompletedAt = addonUpgradeFinishedAt(job)
	case failed:
		status.Phase = kitchenv1alpha1.AddonUpgradeFailed
		status.CompletedAt = addonUpgradeFinishedAt(job)
		status.Message = fmt.Sprintf("the install job failed: %s", message)
	}

	if record.Status.Phase == status.Phase {
		return nil
	}
	record.Status = status
	return r.Status().Update(ctx, record)
}

// addonUpgradeFinishedAt is when the job stopped.
//
// A job that failed usually carries no completion time — Kubernetes writes
// that one on success — so the condition's own transition is what a failure is
// timed by, and the reconcile that read it only where even that is missing.
func addonUpgradeFinishedAt(job *batchv1.Job) *metav1.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.DeepCopy()
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Type != batchv1.JobComplete && condition.Type != batchv1.JobFailed {
			continue
		}
		if !condition.LastTransitionTime.IsZero() {
			return condition.LastTransitionTime.DeepCopy()
		}
	}
	return ptr.To(metav1.Now())
}

// addonUpgradeStartedAt is when the platform started changing something: the
// install job's own creation, not this record's, so that the timestamp
// somebody correlates against is the change's and not the observer's.
func addonUpgradeStartedAt(job *batchv1.Job) *metav1.Time {
	if job.Status.StartTime != nil {
		return job.Status.StartTime.DeepCopy()
	}
	return job.CreationTimestamp.DeepCopy()
}

// addonUpgradeName names one attempt after the job that carried it. See
// addonUpgradeUIDChars for why the job's name alone will not do.
func addonUpgradeName(job *batchv1.Job) string {
	uid := string(job.UID)
	if len(uid) > addonUpgradeUIDChars {
		uid = uid[:addonUpgradeUIDChars]
	}
	if uid == "" {
		return job.Name
	}
	return job.Name + "-" + uid
}

// addonTargetCharts is what an install of this entry at this config puts in
// the cluster, in the order the entry installs them.
func addonTargetCharts(entry addonEntry, cfg AddonInstallConfig) []kitchenv1alpha1.AddonChartStatus {
	charts := make([]kitchenv1alpha1.AddonChartStatus, 0, len(entry.Charts))
	for _, chart := range entry.Charts {
		charts = append(charts, kitchenv1alpha1.AddonChartStatus{
			Name: chart.Chart, Version: cfg.version(chart),
		})
	}
	return charts
}

// addonChartsDiffer reports whether two records of an entry name different
// versions. A chart missing from one side differs from one present on the
// other, which is how a pair that gains a chart reads as a change.
func addonChartsDiffer(from, to []kitchenv1alpha1.AddonChartStatus) bool {
	if len(from) != len(to) {
		return true
	}
	installed := make(map[string]string, len(from))
	for _, chart := range from {
		installed[chart.Name] = chart.Version
	}
	for _, chart := range to {
		version, named := installed[chart.Name]
		if !named || version != chart.Version {
			return true
		}
	}
	return false
}

// addonChartSummary names a set of charts and versions for a log line.
func addonChartSummary(charts []kitchenv1alpha1.AddonChartStatus) string {
	parts := make([]string, 0, len(charts))
	for _, chart := range charts {
		parts = append(parts, chart.Name+" "+chart.Version)
	}
	return strings.Join(parts, " and ")
}

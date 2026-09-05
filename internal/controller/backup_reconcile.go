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
	"encoding/json"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/backup"
)

// The scheduled backup: a CronJob of `/backup --upload`, and the status that
// says whether it has been working.
//
// It belongs to the operator rather than to the chart for the reason the
// shared Gateway, the registry's route and the per-environment HTTPScaledObject
// do: the schedule and the destination are runtime configuration, edited
// through the REST API, and a Helm template only ever sees deploy-time values.
// A schedule somebody changed on the Backup screen has to move the CronJob
// without a `helm upgrade`, which is the whole of "the chart creates what Helm
// can create safely, the operator creates what needs to wait for something".
//
// The half that matters most is at the bottom of this file. Every other line
// here makes backups happen; backupComponent is the only one that makes their
// *absence* visible, and a backup system's characteristic failure is not a
// corrupt archive — it is six weeks of no archive that nobody noticed.

const (
	// BackupCronJobName is the scheduled backup, in the platform namespace.
	BackupCronJobName = "kitchen-backup"

	// backupComponentName is what it is called in the component survey and on
	// the label every run's pods carry.
	backupComponentName = "backup"

	// ConditionBackupReady is whether this platform's archive is current. It
	// is exported because the REST API serves it rather than deriving a
	// second opinion of its own.
	ConditionBackupReady = "BackupReady"

	// DefaultBackupTimeout bounds one run where the singleton names none —
	// which is what an installation predating the field has, since the CRD's
	// own default only reaches an object that is written again.
	DefaultBackupTimeout = 30 * time.Minute

	// backupStartingDeadline is how late a missed run may still start. It
	// exists so that a controller outage does not fire a thundering herd of
	// catch-up runs on recovery: past this, the run is simply missed, and the
	// next one is the answer.
	backupStartingDeadline = int64(10 * 60)

	// backupHistoryLimit is how many finished Jobs of each outcome are kept.
	// The failed ones are the ones anybody needs to read.
	backupHistoryLimit = int32(3)

	// backupGrace is how long after a scheduled run's own deadline the
	// platform waits before calling the backup late. A run that is still
	// going is not a run that failed.
	backupGrace = 10 * time.Minute

	// backupScratchPath is where a run stages the archive before uploading
	// it. The container's root filesystem is read-only, as everything the
	// operator creates is, so the staging directory is an emptyDir.
	backupScratchPath = "/scratch"
	backupScratchName = "scratch"

	// InternalCAMountPath is where the platform's CA bundle is mounted in
	// every pod that talks to one of its stores, and the path the connection
	// secrets name. It is the chart's `kitchen.internalCAMountPath` — the
	// operator's own pod, the telemetry agent and the identity provider all
	// get it from there — and it is here because a backup run's pod is
	// written by this operator rather than by the chart.
	InternalCAMountPath = "/etc/kitchen/internal-ca"
	internalCAVolume    = "internal-ca"
)

// reconcileBackup writes, updates or removes the CronJob, and reports what
// the schedule has been doing.
func (r *KitchenReconciler) reconcileBackup(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	spec := kitchen.Spec.Backup

	if !spec.Configured() {
		// No schedule, so no CronJob — and anything already there comes down,
		// because a CronJob whose configuration has been removed would keep
		// exporting every credential the platform holds to a destination
		// nobody is looking at any more.
		if err := r.removeBackupCronJob(ctx); err != nil {
			setCond(ConditionBackupReady, metav1.ConditionFalse, "CleanupFailed", err.Error())
			return false
		}
		kitchen.Status.Backup = &kitchenv1alpha1.BackupStatus{
			Message: "no scheduled backup is configured, so this platform is backed up exactly as " +
				"often as somebody remembers to take one. Set a schedule and a destination on the " +
				"Backup screen.",
		}
		// False, deliberately, and it does not requeue: an installation with
		// no scheduled backup is not broken, it is unprotected, and this is
		// the one place that says so without being asked.
		setCond(ConditionBackupReady, metav1.ConditionFalse, "NotScheduled", kitchen.Status.Backup.Message)
		return true
	}

	image := r.backupImage()
	if image == "" {
		setCond(ConditionBackupReady, metav1.ConditionFalse, "NoImage",
			"the operator was not told which image to run the backup from, so the schedule cannot be "+
				"created. The chart passes it as --backup-image; upgrade the release.")
		return true
	}

	if err := r.applyBackupCronJob(ctx, kitchen, image); err != nil {
		setCond(ConditionBackupReady, metav1.ConditionFalse, "CronJobFailed", err.Error())
		return false
	}

	status, ready, reason, message := r.surveyBackup(ctx, kitchen, time.Now().UTC())
	kitchen.Status.Backup = status
	if ready {
		setCond(ConditionBackupReady, metav1.ConditionTrue, reason, message)
	} else {
		setCond(ConditionBackupReady, metav1.ConditionFalse, reason, message)
	}
	// A late or failed backup is not something a requeue fixes — the next
	// scheduled run is what fixes it — so this reports rather than retries.
	return true
}

// backupImage is the image a run executes. It is the operator's own, because
// the archive and the code that reads it should be the same release, and a
// pod cannot read its own image back — so the chart passes it in.
//
// The preview gate's image is the same image, and is the fallback for an
// installation whose chart is older than --backup-image.
func (r *KitchenReconciler) backupImage() string {
	if r.BackupImage != "" {
		return r.BackupImage
	}
	return r.PreviewGateImage
}

// applyBackupCronJob writes the schedule.
func (r *KitchenReconciler) applyBackupCronJob(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	image string,
) error {
	spec := kitchen.Spec.Backup
	labels := platformLabels(BackupCronJobName, backupComponentName)
	timeout := BackupTimeoutOf(kitchen)

	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{
		Name: BackupCronJobName, Namespace: PlatformNamespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cron, func() error {
		cron.Labels = labels
		cron.Spec.Schedule = spec.Schedule
		// Cron is UTC, here as everywhere else on this platform. It is worth
		// being explicit about on a platform that already measures node clock
		// drift: "03:00" means 03:00 UTC and not 03:00 wherever the operator
		// is sitting.
		cron.Spec.TimeZone = nil
		cron.Spec.Suspend = ptr.To(spec.Suspend)
		// Two exports competing for connections on the identity provider's
		// Postgres achieves nothing, so a run that overruns its slot skips
		// the next one rather than doubling up.
		cron.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
		cron.Spec.StartingDeadlineSeconds = ptr.To(backupStartingDeadline)
		cron.Spec.SuccessfulJobsHistoryLimit = ptr.To(backupHistoryLimit)
		cron.Spec.FailedJobsHistoryLimit = ptr.To(backupHistoryLimit)

		job := &cron.Spec.JobTemplate
		job.Labels = labels
		// A failed backup is never retried on its own. What a failure needs
		// is somebody reading the log — and the next scheduled run is the
		// retry, against a destination that may by then be reachable.
		job.Spec.BackoffLimit = ptr.To(int32(0))
		job.Spec.ActiveDeadlineSeconds = ptr.To(int64(timeout.Seconds()))
		job.Spec.Template.Labels = labels
		job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
		job.Spec.Template.Spec.ServiceAccountName = r.BackupServiceAccount
		job.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name:         backupScratchName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}, {
			// The CA the accounts database's certificate is verified against.
			// A run dumps that database, so it is a client of it, and the DSN
			// it reads out of the connection secret names this file.
			//
			// Optional, unlike the identity provider's: the same CronJob is
			// written on an installation whose database is reached in the
			// clear, where the ConfigMap does not exist and a required mount
			// would be a scheduled backup that never runs. The DSN is what
			// decides whether the file is needed, and a run that needs it and
			// cannot read it fails naming it rather than connecting anyway.
			Name: internalCAVolume,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: InternalCAConfigMapName},
				Optional:             ptr.To(true),
			}},
		}}
		job.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:    backupComponentName,
			Image:   image,
			Command: []string{"/backup"},
			Args: []string{
				"--upload",
				"--namespace=" + PlatformNamespace,
				"--scratch=" + backupScratchPath,
			},
			VolumeMounts: []corev1.VolumeMount{{
				Name: backupScratchName, MountPath: backupScratchPath,
			}, {
				Name: internalCAVolume, MountPath: InternalCAMountPath, ReadOnly: true,
			}},
			// The run leaves its result here as JSON, and surveyBackup below
			// reads it back — the same mechanism digestFromTerminationMessage
			// already uses to get a build's digest out of a finished pod.
			TerminationMessagePath:   terminationLogPath,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				ReadOnlyRootFilesystem:   ptr.To(true),
				RunAsNonRoot:             ptr.To(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		}}
		return nil
	})
	return err
}

// removeBackupCronJob takes the schedule down, and the Jobs it created with
// it.
func (r *KitchenReconciler) removeBackupCronJob(ctx context.Context) error {
	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{
		Name: BackupCronJobName, Namespace: PlatformNamespace,
	}}
	err := r.Delete(ctx, cron, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// BackupTimeoutOf is how long one run may take, as the run would actually get
// it: the singleton's setting, or the operator's own default where it names
// none.
func BackupTimeoutOf(kitchen *kitchenv1alpha1.Kitchen) time.Duration {
	if timeout := kitchen.Spec.Backup.Timeout.Duration; timeout > 0 {
		return timeout
	}
	return DefaultBackupTimeout
}

// surveyBackup reads what the schedule has been doing, and decides whether
// that is healthy.
//
// Three sources, in order of how long they survive. The CronJob's own status
// carries the last schedule and the last success, and it outlives every Job;
// the run pods carry the detail — which archive, how big, how many are at the
// destination — and are reaped on the history limit; and the spec carries what
// was asked for. Nothing here fails a reconcile: a status that could not be
// read is a message, not an error.
func (r *KitchenReconciler) surveyBackup(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	now time.Time,
) (*kitchenv1alpha1.BackupStatus, bool, string, string) {
	spec := kitchen.Spec.Backup
	status := &kitchenv1alpha1.BackupStatus{
		Schedule:    spec.Schedule,
		Suspended:   spec.Suspend,
		Destination: backup.Describe(spec.Destination),
	}

	cron := &batchv1.CronJob{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: BackupCronJobName}
	if err := r.Get(ctx, key, cron); err != nil {
		status.Message = "the scheduled backup could not be read back: " + err.Error()
		return status, false, "ScheduleUnreadable", status.Message
	}
	status.LastRun = cron.Status.LastScheduleTime.DeepCopy()
	status.LastSuccess = cron.Status.LastSuccessfulTime.DeepCopy()

	// The detail — which archive, how big, how many are left — is on the run
	// pods' termination messages, and only for as long as those pods survive
	// the history limit.
	succeeded, failed := r.backupRuns(ctx)
	if succeeded != nil {
		status.LastSuccessArchive = succeeded.result.Archive
		status.LastSuccessBytes = succeeded.result.Bytes
		status.Archives = succeeded.result.Archives
		if status.LastSuccess == nil {
			// The CronJob's status is the authority on when a run last
			// worked, and it outlives every pod — but a manual run started
			// through POST /platform/backup/runs is not the CronJob's, so
			// the pod is what carries it.
			status.LastSuccess = &metav1.Time{Time: succeeded.at}
		}
	}
	if failed != nil {
		status.LastFailure = &metav1.Time{Time: failed.at}
		status.Message = failed.result.Error
	}

	ready, reason, message := backupHealth(status, BackupTimeoutOf(kitchen), now)
	if status.Message == "" || ready {
		status.Message = message
	}
	return status, ready, reason, message
}

// backupHealth is the judgement, kept apart from the reading of it so that it
// can be tested against a status rather than against a cluster.
//
// The question it answers is "is this platform's archive current", and the
// three ways the answer is no are: a run failed, a run was scheduled and never
// succeeded, and a schedule that has been running long enough to have produced
// something and has not.
func backupHealth(
	status *kitchenv1alpha1.BackupStatus,
	timeout time.Duration,
	now time.Time,
) (bool, string, string) {
	switch {
	case status.Suspended:
		// A pause somebody asked for is not a failure. It is still worth
		// saying out loud, because a pause nobody lifted is how six weeks of
		// no archive begins.
		message := "the backup schedule is suspended"
		if status.LastSuccess != nil {
			message += fmt.Sprintf("; the last archive was taken %s", since(status.LastSuccess.Time, now))
		}
		return true, "Suspended", message

	case status.LastFailure != nil &&
		(status.LastSuccess == nil || status.LastFailure.Time.After(status.LastSuccess.Time)):
		message := fmt.Sprintf("the backup taken %s failed", since(status.LastFailure.Time, now))
		if status.Message != "" {
			message += ": " + status.Message
		}
		return false, "RunFailed", message

	case status.LastSuccess != nil && status.LastRun != nil &&
		status.LastRun.Time.After(status.LastSuccess.Time.Add(timeout+backupGrace)):
		return false, "RunLate", fmt.Sprintf(
			"a backup was due %s and the last archive is from %s, so a run has been started and has not "+
				"finished. Read the logs of the newest job in %s.",
			since(status.LastRun.Time, now), since(status.LastSuccess.Time, now), PlatformNamespace)

	case status.LastSuccess == nil && status.LastRun != nil &&
		now.After(status.LastRun.Time.Add(timeout+backupGrace)):
		return false, "NeverSucceeded", fmt.Sprintf(
			"a backup was scheduled %s and no archive has ever been written to %s. "+
				"Read the logs of the newest job in %s, or press Run now on the Backup screen.",
			since(status.LastRun.Time, now), status.Destination, PlatformNamespace)

	case status.LastSuccess == nil:
		return true, "Scheduled", fmt.Sprintf(
			"backups run %s to %s; none has been taken yet", status.Schedule, status.Destination)

	default:
		return true, "BackedUp", fmt.Sprintf("the last archive was written to %s %s",
			status.Destination, since(status.LastSuccess.Time, now))
	}
}

// since is a duration as a sentence reads it. Backups are measured in days
// and this is read by somebody deciding whether to worry, so an hour's
// precision is more than enough.
func since(then, now time.Time) string {
	elapsed := now.Sub(then)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(elapsed.Minutes()))
	case elapsed < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(elapsed.Hours()/24))
	}
}

// backupRun is one finished run's own report, and when it finished.
type backupRun struct {
	result backup.Result
	at     time.Time
}

// backupRuns reads the newest successful and the newest failed run off the
// pods' termination messages.
//
// Both are wanted, and neither replaces the other: a run that failed last
// night does not erase which archive is at the destination, and an archive
// from a week ago does not excuse last night's failure. The two together are
// what the Backup screen shows and what backupHealth judges.
func (r *KitchenReconciler) backupRuns(ctx context.Context) (*backupRun, *backupRun) {
	pods := &corev1.PodList{}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		// The uncached reader, for components.go's reason: the manager keeps
		// no informer over pods, and a handful of backup pods is not a reason
		// to start one.
		reader = r.APIReader
	}
	if err := reader.List(ctx, pods,
		client.InNamespace(PlatformNamespace),
		client.MatchingLabels{labelComponentKey: BackupCronJobName},
	); err != nil {
		return nil, nil
	}

	var succeeded, failed *backupRun
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, container := range pod.Status.ContainerStatuses {
			terminated := container.State.Terminated
			if terminated == nil || terminated.Message == "" {
				continue
			}
			result := backup.Result{}
			if err := json.Unmarshal([]byte(terminated.Message), &result); err != nil {
				// A message that is not a result is a container that died
				// before it could write one. Its exit is still the run's
				// outcome, and the CronJob's own status carries that.
				continue
			}
			run := &backupRun{result: result, at: terminated.FinishedAt.Time}
			switch {
			case result.Error != "" || !result.Verified:
				if failed == nil || run.at.After(failed.at) {
					failed = run
				}
			default:
				if succeeded == nil || run.at.After(succeeded.at) {
					succeeded = run
				}
			}
		}
	}
	return succeeded, failed
}

// backupComponent is the scheduled backup as a row in the component survey,
// and it is the highest-value line in this file.
//
// status.components is the list an operator already reads, and this is the
// one entry in it that is not a workload's pod count: it reports whether the
// platform's archive is current. It follows the shape the clock-sync check
// already established — a survey row under a kind that is not a Deployment —
// rather than bending the list to fit.
func backupComponent(kitchen *kitchenv1alpha1.Kitchen) *kitchenv1alpha1.ComponentStatus {
	if !kitchen.Spec.Backup.Configured() {
		// Nothing scheduled is not an unhealthy schedule; it is no schedule,
		// and the BackupReady condition is where that is said.
		return nil
	}
	status := kitchen.Status.Backup
	if status == nil {
		return nil
	}
	healthy, _, message := backupHealth(status, BackupTimeoutOf(kitchen), time.Now().UTC())
	available := int32(0)
	if healthy {
		available = 1
	}
	return &kitchenv1alpha1.ComponentStatus{
		Name:      backupComponentName,
		Kind:      "CronJob",
		Healthy:   healthy,
		Available: available,
		Desired:   1,
		Message:   message,
	}
}

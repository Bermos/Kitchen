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
	"time"

	"github.com/blang/semver/v4"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

const (
	// DefaultHelmImage runs the self-update job when the chart names no
	// other. It is pinned rather than floating: the job it runs rewrites
	// every object the platform is made of, so which helm does it is part of
	// the release, not something to inherit from a moving tag.
	DefaultHelmImage = "alpine/helm:3.19.0"

	// DefaultSelfUpdateTimeout is how long helm is given to finish. It waits
	// for the whole release to come back up, StatefulSets included, so it is
	// generous by the standards of the API call that starts it.
	DefaultSelfUpdateTimeout = 15 * time.Minute

	// selfUpdateDeadlineBuffer is how much longer the Job may run than helm
	// is given. The Job's deadline has to lose the race: a Job killed while
	// helm is mid-upgrade leaves the release in pending-upgrade, which needs
	// a manual `helm rollback` — exactly the state a self-update exists to
	// avoid.
	selfUpdateDeadlineBuffer = 5 * time.Minute

	// selfUpdateJobTTLSeconds keeps the finished job (and its pod) around
	// long enough for the log collector to ship the last of helm's output,
	// then lets the cluster reclaim it. The PlatformUpdate record outlives
	// it; the logs outlive it in ClickHouse.
	selfUpdateJobTTLSeconds = 3600

	// selfUpdateComponent names the job in collected logs, and is what the
	// dashboard filters on to show an upgrade's output.
	selfUpdateComponent = "self-update"

	labelPlatformUpdate = "kitchen.bermos.dev/platform-update"

	// devVersion is what internal/version reports on a build that no linker
	// stamped. Nothing is published under it, so there is nothing to upgrade
	// from.
	devVersion = "dev"
)

// SelfUpdateConfig is what the chart tells the operator about upgrading the
// platform's own Helm release.
//
// It arrives as flags on the manager Deployment rather than as fields on the
// Kitchen singleton, and that is deliberate: the singleton is applied as a
// post-install hook and is not re-applied on upgrade, so a value flipped in a
// `helm upgrade` would create the ServiceAccount and never reach the operator.
// The Deployment is re-applied every time, so its flags cannot go stale.
type SelfUpdateConfig struct {
	// Chart is the reference the update pulls from, e.g.
	// "oci://ghcr.io/bermos/charts/kitchen". Empty disables self-update.
	Chart string

	// Release is the Helm release name to upgrade, which is whatever the
	// installation was installed as.
	Release string

	// ServiceAccount is what the job runs as. It is bound to cluster-admin,
	// because a helm upgrade of this chart applies CRDs, ClusterRoles, the
	// namespace and cert-manager; it is a separate account from the
	// manager's so that the grant is visible, revocable and gone on
	// uninstall.
	ServiceAccount string

	// HelmImage runs the upgrade.
	HelmImage string

	// Timeout is what helm is given to finish.
	Timeout time.Duration

	// AllowMinor permits an upgrade that crosses a minor version. While
	// Kitchen is pre-1.0 the minor is where breaking changes land, so those
	// upgrades are the ones that may need manual steps, and they are opted
	// into separately from self-update itself.
	AllowMinor bool
}

// Enabled reports whether this installation can update itself. Both fields are
// required: the chart sets them together, and half a configuration would mean
// creating a job that cannot authenticate or has nothing to pull.
func (c SelfUpdateConfig) Enabled() bool {
	return c.Chart != "" && c.ServiceAccount != ""
}

// PlatformUpdateReconciler drives a PlatformUpdate: it checks that the upgrade
// is one this installation is allowed and able to make, then runs it as a Job.
//
// The job exists because the operator cannot run its own upgrade in-process.
// helm would apply the new manager Deployment, Kubernetes would terminate the
// pod running helm, and the release would be left marked pending-upgrade with
// no process left to finish or roll it back. A Job is not restarted by the
// manager's rollout, so it survives the change it is making.
type PlatformUpdateReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// SelfUpdate is the chart's configuration. A zero value means the
	// installation was not installed with self-update enabled, and every
	// PlatformUpdate is rejected with a message saying how to enable it.
	SelfUpdate SelfUpdateConfig

	// CurrentVersion is the release this operator was built from, from
	// internal/version. It is passed in rather than read directly so that the
	// preflight checks can be tested at versions this binary was never built
	// at.
	CurrentVersion string

	// Activity feeds the dashboard's recent-activity feed. May be nil.
	Activity *activity.Recorder
	// Audit appends this reconciler's state transitions to the tamper-evident
	// log. Unlike Activity it is waited on: a transition it refuses is a
	// transition this reconciler does not make. May be nil.
	Audit *audit.Recorder
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=platformupdates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=platformupdates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=platformupdates/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch

// Reconcile drives a PlatformUpdate from preflight through a helm job.
func (r *PlatformUpdateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	update := &kitchenv1alpha1.PlatformUpdate{}
	if err := r.Get(ctx, req.NamespacedName, update); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Terminal first, and not only to save work: the job is reaped by its
	// TTL, and without this a finished update would find no job and start a
	// second upgrade.
	if isUpdateTerminal(update.Status.Phase) || !update.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	jobName := selfUpdateJobName(update.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: jobName}, job)
	switch {
	case apierrors.IsNotFound(err):
		if reason, message, ok := r.preflight(ctx, update); !ok {
			return r.failUpdate(ctx, update, reason, message)
		}
		if waiting, res := r.gateConcurrentUpdates(ctx, update); waiting {
			return res, nil
		}
		if err := r.createJob(ctx, update, jobName); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("platform update job created",
			"job", jobName, "from", r.CurrentVersion, "to", update.Spec.Version)
		update.Status.Phase = kitchenv1alpha1.PlatformUpdateRunning
		update.Status.FromVersion = r.CurrentVersion
		update.Status.JobName = jobName
		update.Status.StartedAt = ptr.To(metav1.Now())
		update.Status.Message = fmt.Sprintf("upgrading the platform to %s", update.Spec.Version)
		meta.SetStatusCondition(&update.Status.Conditions, metav1.Condition{
			Type: condReady, Status: metav1.ConditionFalse, Reason: "UpgradeRunning",
			Message: update.Status.Message, ObservedGeneration: update.Generation,
		})
		return ctrl.Result{}, r.Status().Update(ctx, update)
	case err != nil:
		return ctrl.Result{}, err
	}

	complete, failed, message := jobOutcome(job)
	switch {
	case complete:
		return r.succeedUpdate(ctx, update, job)
	case failed:
		// --atomic means helm rolled the release back before the job
		// reported failure, so the platform is on the version it started on.
		return r.failUpdate(ctx, update, "UpgradeFailed",
			fmt.Sprintf("the helm upgrade to %s failed and was rolled back: %s", update.Spec.Version, message))
	default:
		if update.Status.Phase != kitchenv1alpha1.PlatformUpdateRunning {
			update.Status.Phase = kitchenv1alpha1.PlatformUpdateRunning
			return ctrl.Result{}, r.Status().Update(ctx, update)
		}
		// The operator is replaced part-way through its own upgrade, and the
		// watch on Jobs does not survive the restart that the upgrade causes.
		// Polling is what closes out an update whose completion event landed
		// while this process did not exist.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
}

// preflight answers whether this upgrade can be attempted at all, before
// anything is created. Everything it rejects is rejected with the cluster
// untouched, which is the whole point of doing it here: a helm upgrade that
// fails part-way does not roll back what it already applied unless helm
// itself observed the failure.
func (r *PlatformUpdateReconciler) preflight(
	ctx context.Context,
	update *kitchenv1alpha1.PlatformUpdate,
) (reason, message string, ok bool) {
	if !r.SelfUpdate.Enabled() {
		return "SelfUpdateDisabled", "this installation cannot update itself: install or upgrade the chart with " +
			"`--set selfUpdate.enabled=true` to create the update job's ServiceAccount. It is bound to " +
			"cluster-admin, because the upgrade applies the whole chart, so it is off by default.", false
	}

	if r.CurrentVersion == devVersion || r.CurrentVersion == "" {
		return "NotAPublishedRelease", "this operator reports version \"" + devVersion + "\", so it was not built " +
			"from a published release and there is no version to upgrade from. Upgrade with helm directly.", false
	}

	current, err := semver.Parse(r.CurrentVersion)
	if err != nil {
		return "CurrentVersionUnreadable", fmt.Sprintf(
			"the running version %q is not valid SemVer, so the upgrade cannot be checked: %s", r.CurrentVersion, err), false
	}
	target, err := semver.Parse(update.Spec.Version)
	if err != nil {
		return "VersionUnreadable", fmt.Sprintf(
			"%q is not valid SemVer: %s", update.Spec.Version, err), false
	}

	switch {
	case target.EQ(current):
		return "AlreadyCurrent", fmt.Sprintf("the platform is already running %s", update.Spec.Version), false
	case target.LT(current):
		return "DowngradeRefused", fmt.Sprintf(
			"%s is older than the running %s, and a downgrade can strip fields from the CRDs that stored objects "+
				"still use. Roll back with `helm rollback` instead, which knows what the previous release contained.",
			update.Spec.Version, r.CurrentVersion), false
	case !r.SelfUpdate.AllowMinor && (target.Major != current.Major || target.Minor != current.Minor):
		return "MinorUpgradeNotAllowed", fmt.Sprintf(
			"upgrading from %s to %s crosses a minor version, and while Kitchen is pre-1.0 that is where breaking "+
				"changes land — the release notes say whether manual steps are needed. Set "+
				"`selfUpdate.allowMinor=true` to allow these from the dashboard, or upgrade with helm.",
			r.CurrentVersion, update.Spec.Version), false
	}

	// The grant is checked rather than assumed. The flags say the chart was
	// rendered with self-update on; this says the account it promised is
	// actually there, which is the difference between failing now and failing
	// with a Forbidden half-way through rewriting the platform.
	sa := &corev1.ServiceAccount{}
	saKey := types.NamespacedName{Namespace: PlatformNamespace, Name: r.SelfUpdate.ServiceAccount}
	if err := r.Get(ctx, saKey, sa); err != nil {
		return "ServiceAccountMissing", fmt.Sprintf(
			"the update job's ServiceAccount %q is missing from %s: %s. The chart creates it when "+
				"selfUpdate.enabled and rbac.create are both true.", saKey.Name, PlatformNamespace, err), false
	}

	return "", "", true
}

// gateConcurrentUpdates keeps a second upgrade waiting while one is in flight.
// Two helm processes writing the same release at once is how a release ends up
// in pending-upgrade with nothing to recover it.
func (r *PlatformUpdateReconciler) gateConcurrentUpdates(
	ctx context.Context,
	update *kitchenv1alpha1.PlatformUpdate,
) (bool, ctrl.Result) {
	updates := &kitchenv1alpha1.PlatformUpdateList{}
	if err := r.List(ctx, updates); err != nil {
		return false, ctrl.Result{}
	}
	running := ""
	for _, other := range updates.Items {
		if other.Name != update.Name && other.Status.Phase == kitchenv1alpha1.PlatformUpdateRunning {
			running = other.Name
			break
		}
	}
	if running == "" {
		return false, ctrl.Result{}
	}

	message := fmt.Sprintf("waiting for the update %s to finish", running)
	if update.Status.Phase != kitchenv1alpha1.PlatformUpdatePending || update.Status.Message != message {
		update.Status.Phase = kitchenv1alpha1.PlatformUpdatePending
		update.Status.Message = message
		meta.SetStatusCondition(&update.Status.Conditions, metav1.Condition{
			Type: condReady, Status: metav1.ConditionFalse, Reason: "UpgradeInFlight",
			Message: message, ObservedGeneration: update.Generation,
		})
		if err := r.Status().Update(ctx, update); err != nil {
			return true, ctrl.Result{}
		}
	}
	return true, ctrl.Result{RequeueAfter: 30 * time.Second}
}

// createJob runs the upgrade.
//
// The argv is built here and takes nothing from the PlatformUpdate but the
// version, which the CRD has already constrained to a SemVer string. That is
// the security boundary of the whole feature: the job is bound to
// cluster-admin, so an upgrade that forwarded caller-supplied helm arguments
// would be a way to apply arbitrary objects as cluster-admin, and
// `selfUpdate.enabled` would not be the gate it looks like.
func (r *PlatformUpdateReconciler) createJob(
	ctx context.Context,
	update *kitchenv1alpha1.PlatformUpdate,
	jobName string,
) error {
	timeout := r.SelfUpdate.Timeout
	if timeout <= 0 {
		timeout = DefaultSelfUpdateTimeout
	}
	image := r.SelfUpdate.HelmImage
	if image == "" {
		image = DefaultHelmImage
	}

	args := []string{
		"upgrade", r.SelfUpdate.Release, r.SelfUpdate.Chart,
		"--version", update.Spec.Version,
		"--namespace", PlatformNamespace,
		// Not --reuse-values, which ignores defaults the new chart version
		// introduces and would carry an installation forward missing every
		// value added since it was installed. This one takes the new chart's
		// defaults and re-applies the overrides on top, which is what
		// "upgrade, keep my configuration" has to mean.
		"--reset-then-reuse-values",
		// Roll back if the release does not come up. --atomic implies --wait,
		// so the job's exit status is the whole platform's, not just the API
		// server's acceptance of the manifests.
		"--atomic",
		"--timeout", timeout.String(),
	}

	labels := map[string]string{
		labelPlatformUpdate: update.Name,
		labelManagedByKey:   labelManagedByValue,
		labelComponentKind:  selfUpdateComponent,
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: PlatformNamespace, Labels: labels},
		Spec: batchv1.JobSpec{
			// A failed upgrade is never retried on its own. Re-running helm
			// over a release it has just half-applied is how a recoverable
			// failure becomes an unrecoverable one; a retry is a person
			// reading the log and creating a new PlatformUpdate.
			BackoffLimit: ptr.To(int32(0)),
			// Comfortably longer than helm's own timeout, so that helm
			// always loses the race and gets to roll back.
			ActiveDeadlineSeconds:   ptr.To(int64((timeout + selfUpdateDeadlineBuffer).Seconds())),
			TTLSecondsAfterFinished: ptr.To(int32(selfUpdateJobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: r.SelfUpdate.ServiceAccount,
					Containers: []corev1.Container{{
						Name:    "helm",
						Image:   image,
						Command: []string{"helm"},
						Args:    args,
						Env: []corev1.EnvVar{
							// helm writes its cache, config and data under
							// $HOME by default, which is not writable in the
							// image. Everything it needs here is disposable.
							{Name: "HELM_CACHE_HOME", Value: "/tmp/helm/cache"},
							{Name: "HELM_CONFIG_HOME", Value: "/tmp/helm/config"},
							{Name: "HELM_DATA_HOME", Value: "/tmp/helm/data"},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "helm-home", MountPath: "/tmp"}},
					}},
					Volumes: []corev1.Volume{{
						Name:         "helm-home",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					}},
				},
			},
		},
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *PlatformUpdateReconciler) succeedUpdate(
	ctx context.Context,
	update *kitchenv1alpha1.PlatformUpdate,
	job *batchv1.Job,
) (ctrl.Result, error) {
	if err := r.Audit.Record(ctx, audit.Transition{
		Object:     update,
		Kind:       audit.KindPlatformUpdate,
		Controller: actorPlatformUpdateController,
		From:       string(update.Status.Phase),
		To:         string(kitchenv1alpha1.PlatformUpdateSucceeded),
		Reason: fmt.Sprintf("platform upgraded from %s to %s",
			updateFromVersion(update), update.Spec.Version),
		Details: map[string]any{
			"fromVersion": update.Status.FromVersion,
			"toVersion":   update.Spec.Version,
		},
	}); err != nil {
		return ctrl.Result{}, err
	}
	update.Status.Phase = kitchenv1alpha1.PlatformUpdateSucceeded
	update.Status.Message = fmt.Sprintf("the platform is on %s", update.Spec.Version)
	if job.Status.CompletionTime != nil {
		update.Status.CompletedAt = job.Status.CompletionTime
	} else {
		update.Status.CompletedAt = ptr.To(metav1.Now())
	}
	meta.SetStatusCondition(&update.Status.Conditions, metav1.Condition{
		Type: condReady, Status: metav1.ConditionTrue, Reason: "UpgradeSucceeded",
		Message: update.Status.Message, ObservedGeneration: update.Generation,
	})
	r.Activity.Record(ctx, clickhouse.Event{
		Type: clickhouse.EventPlatformUpdated,
		Message: fmt.Sprintf("platform upgraded from %s to %s",
			updateFromVersion(update), update.Spec.Version),
	})
	return ctrl.Result{}, r.Status().Update(ctx, update)
}

func (r *PlatformUpdateReconciler) failUpdate(
	ctx context.Context,
	update *kitchenv1alpha1.PlatformUpdate,
	reason, message string,
) (ctrl.Result, error) {
	if err := r.Audit.Record(ctx, audit.Transition{
		Object:     update,
		Kind:       audit.KindPlatformUpdate,
		Controller: actorPlatformUpdateController,
		From:       string(update.Status.Phase),
		To:         string(kitchenv1alpha1.PlatformUpdateFailed),
		Reason:     fmt.Sprintf("upgrade to %s failed: %s", update.Spec.Version, message),
		Details: map[string]any{
			"fromVersion": update.Status.FromVersion,
			"toVersion":   update.Spec.Version,
			"reason":      reason,
		},
	}); err != nil {
		return ctrl.Result{}, err
	}
	update.Status.Phase = kitchenv1alpha1.PlatformUpdateFailed
	update.Status.Message = message
	update.Status.CompletedAt = ptr.To(metav1.Now())
	if update.Status.FromVersion == "" {
		update.Status.FromVersion = r.CurrentVersion
	}
	meta.SetStatusCondition(&update.Status.Conditions, metav1.Condition{
		Type: condReady, Status: metav1.ConditionFalse, Reason: reason,
		Message: message, ObservedGeneration: update.Generation,
	})
	r.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventPlatformUpdateFailed,
		Message: fmt.Sprintf("upgrade to %s failed: %s", update.Spec.Version, message),
	})
	return ctrl.Result{}, r.Status().Update(ctx, update)
}

// updateFromVersion is what the platform was on before the upgrade, falling
// back to a placeholder for a record written before the field existed.
func updateFromVersion(update *kitchenv1alpha1.PlatformUpdate) string {
	if update.Status.FromVersion == "" {
		return "an earlier version"
	}
	return update.Status.FromVersion
}

func isUpdateTerminal(phase kitchenv1alpha1.PlatformUpdatePhase) bool {
	switch phase {
	case kitchenv1alpha1.PlatformUpdateSucceeded, kitchenv1alpha1.PlatformUpdateFailed:
		return true
	default:
		return false
	}
}

// selfUpdateJobName is derived from the update rather than generated, so that
// a status write lost to a restart cannot produce a second job for the same
// upgrade: the reconciler that comes back finds the job by name and adopts it.
func selfUpdateJobName(updateName string) string {
	name := "kitchen-self-update-" + updateName
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// mapJobToUpdate enqueues the PlatformUpdate a labeled job belongs to.
func (r *PlatformUpdateReconciler) mapJobToUpdate(_ context.Context, obj client.Object) []ctrl.Request {
	name, ok := obj.GetLabels()[labelPlatformUpdate]
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: name}}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *PlatformUpdateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.PlatformUpdate{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(r.mapJobToUpdate)).
		Named("platformupdate").
		Complete(r)
}

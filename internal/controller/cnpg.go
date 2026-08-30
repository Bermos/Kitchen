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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Installing the platform's own database operator.
//
// This is the KEDA path with one dependency swapped, and deliberately so: the
// decision it embodies is the same one, reached the same way. A chart cannot
// safely bundle CloudNativePG — it ships CRDs and an admission webhook of its
// own, and it is a popular thing for a cluster to already have, which Helm
// will not adopt — while the premise Kitchen is built on says a platform that
// owns its cluster should not answer "install this yourself first" to the
// question "give me a database". The operator is under none of Helm's
// constraints, so the operator installs it, on request, with every property
// the KEDA install has: off by default, its own cluster-admin account that
// the chart only creates when asked, a pinned chart version, nothing from a
// request in the argv, and a refusal — never a takeover — where somebody
// else's CloudNativePG is already serving.

const (
	// condDatabasesReady is where the platform says whether it can provision
	// a database into this cluster at all. It is a fact about the cluster,
	// not about any one claim: a claim's own conditions say whether *it*
	// bound.
	condDatabasesReady = "DatabasesReady"

	// DefaultCNPGChartRepository is where the chart is pulled from.
	// CloudNativePG publishes a classic HTTP repository rather than OCI
	// artifacts, so this is a repository URL handed to helm with --repo,
	// which needs no `helm repo add` and so no writable repository cache.
	DefaultCNPGChartRepository = "https://cloudnative-pg.github.io/charts"

	// DefaultCNPGChartVersion is pinned rather than floated, and it is pinned
	// next to the thing that depends on it: the operator version this chart
	// installs is what decides which Cluster fields exist and which images
	// are current, and the image catalogue those claims resolve against is
	// database.DefaultPostgresImages. A bump reads that list again — an
	// entry there is a promise the platform refuses claims on.
	DefaultCNPGChartVersion = "0.29.0"

	// DefaultCNPGInstallTimeout is what the helm run is given. It --waits, so
	// this is time for the operator's pods to become ready and not merely for
	// manifests to be accepted.
	DefaultCNPGInstallTimeout = 10 * time.Minute

	// cnpgReleaseName is upstream's own instruction's release name. Using
	// anything else would make an installation that later wants to take
	// CloudNativePG over by hand harder to reason about, for no gain.
	cnpgReleaseName = "cnpg"

	// cnpgChartName is the chart in that repository.
	cnpgChartName = "cloudnative-pg"

	// cnpgInstallComponent names the job in collected logs, and is what the
	// dashboard filters on to show what helm said.
	cnpgInstallComponent = "cnpg-install"

	// cnpgInstallDeadlineBuffer is how much longer the Job may run than helm
	// is given, so that helm always loses the race and gets to report its own
	// failure.
	cnpgInstallDeadlineBuffer = 5 * time.Minute

	// cnpgInstallJobTTLSeconds keeps the finished job and its pod around long
	// enough for the log collector to ship helm's output. What the job
	// achieved outlives it in status.databases.
	cnpgInstallJobTTLSeconds = 3600
)

// cnpgClusterGVK is CloudNativePG's database kind. Kitchen writes these — one
// per claim — but here it is only a probe: a cluster that serves this kind
// runs CloudNativePG, and so must not be installed into.
func cnpgClusterGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
}

// CNPGInstallConfig is what the chart tells the operator about installing the
// platform's own database operator.
//
// Like KedaInstallConfig it arrives as flags on the manager Deployment rather
// than as fields on the Kitchen singleton, and for the same reason: the
// singleton is applied as a post-install hook and is not re-applied on
// upgrade, so a value flipped in a `helm upgrade` would create the
// ServiceAccount and never reach the operator. The Deployment is re-applied
// every time. What *is* on the singleton is the decision —
// `spec.databases.install` — because that is the half a person changes.
type CNPGInstallConfig struct {
	// ServiceAccount is what the install job runs as. It is bound to
	// cluster-admin, because installing CloudNativePG applies CRDs,
	// ClusterRoles, a webhook configuration and a namespace; it is a separate
	// account from the manager's so that the grant is visible, revocable and
	// gone on uninstall. Empty means the chart was rendered without the
	// grant, and no install is attempted.
	ServiceAccount string

	// HelmImage runs the install.
	HelmImage string

	// Repository the chart is pulled from.
	Repository string

	// ChartVersion pins what is installed.
	ChartVersion string

	// Timeout is what the helm run is given.
	Timeout time.Duration
}

// Enabled reports whether this installation may install CloudNativePG for
// itself. Only the account is required: everything else has a default the
// operator compiles in, and half a configuration should still install
// something known.
func (c CNPGInstallConfig) Enabled() bool { return c.ServiceAccount != "" }

// withDefaults fills in everything the chart did not say.
func (c CNPGInstallConfig) withDefaults() CNPGInstallConfig {
	if c.HelmImage == "" {
		c.HelmImage = DefaultHelmImage
	}
	if c.Repository == "" {
		c.Repository = DefaultCNPGChartRepository
	}
	if c.ChartVersion == "" {
		c.ChartVersion = DefaultCNPGChartVersion
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultCNPGInstallTimeout
	}
	return c
}

// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch

// databasesPlan is what one reconcile has decided to do about CloudNativePG,
// before anything is read or written. Keeping the decision separate from
// carrying it out is what lets every branch of it be exercised against a
// cluster whose cnpg state is whatever the test harness happens to have.
type databasesPlan struct {
	// clear removes the condition and the record entirely: this cluster runs
	// no CloudNativePG and has not been asked to, so neither is worth
	// carrying on every reconcile. The guidance is not lost by staying quiet
	// here — a cnpg Connection in a cluster without the operator says exactly
	// this, on the connection, where somebody is looking.
	clear bool

	// install runs the helm install. Mutually exclusive with a condition this
	// plan sets itself — the job's own progress is the condition then.
	install bool

	// record replaces status.databases. Nil leaves what is there.
	record *kitchenv1alpha1.DatabasesStatus

	status  metav1.ConditionStatus
	reason  string
	message string

	// ready is what reconcileDatabases returns: whether this reconcile needs
	// requeueing. A refusal is *ready*, because nothing will change without a
	// spec edit or a chart upgrade, and both reconcile the object again on
	// their own.
	ready bool
}

// planDatabases decides what to do about the platform's database operator.
//
// Nothing here ever plans a write to a release the operator did not create. A
// cluster that already serves postgresql.cnpg.io is recorded and left alone,
// permanently — and, unlike KEDA, it is *used* either way: a claim provisions
// through whichever CloudNativePG is there. What the record decides is who
// may upgrade it, not who may use it.
func planDatabases(
	kitchen *kitchenv1alpha1.Kitchen,
	cfg CNPGInstallConfig,
	namespace string,
	served bool,
) databasesPlan {
	recorded := kitchen.Status.Databases
	managed := recorded != nil && recorded.Managed

	if served && !managed {
		return databasesPlan{
			record: &kitchenv1alpha1.DatabasesStatus{Namespace: namespace},
			status: metav1.ConditionTrue, reason: "OperatorPresent",
			message: "CloudNativePG is already serving in this cluster; the platform installed nothing and " +
				"manages nothing about it. A postgres claim through a cnpg connection provisions into it",
			ready: true,
		}
	}

	if served && managed && !cnpgVersionMoved(kitchen, cfg) {
		return databasesPlan{
			status: metav1.ConditionTrue, reason: "OperatorInstalled",
			message: fmt.Sprintf("the platform installed CloudNativePG %s into %s",
				recorded.Version, recorded.Namespace),
			ready: true,
		}
	}

	if !kitchen.Spec.Databases.Install {
		return databasesPlan{clear: true, ready: true}
	}

	if !cfg.Enabled() {
		return databasesPlan{
			status: metav1.ConditionFalse, reason: "InstallNotPermitted",
			message: "spec.databases.install is set, but this installation did not grant the operator an account " +
				"to install with. Upgrade the chart with `--set databases.install.enabled=true`, which creates " +
				"the install job's ServiceAccount — it is bound to cluster-admin, because installing " +
				"CloudNativePG applies CRDs, ClusterRoles and a webhook configuration, so it is off by default",
			ready: true,
		}
	}

	if !dnsLabel.MatchString(namespace) {
		return databasesPlan{
			status: metav1.ConditionFalse, reason: "NamespaceInvalid",
			message: fmt.Sprintf("spec.databases.operatorNamespace is %q, which is not a valid namespace name, "+
				"so there is nowhere to install CloudNativePG", namespace),
			ready: true,
		}
	}

	return databasesPlan{install: true}
}

// reconcileDatabases answers whether the platform can provision a database
// into its own cluster, and — where the installation has asked it to and
// granted it the account — makes that true by installing CloudNativePG.
func (r *KitchenReconciler) reconcileDatabases(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	namespace := cnpgNamespace(kitchen)
	cfg := r.CNPGInstall.withDefaults()

	served, err := r.apiServed(ctx, cnpgClusterGVK())
	if err != nil {
		setCond(condDatabasesReady, metav1.ConditionFalse, "OperatorProbeFailed",
			"could not tell whether CloudNativePG is installed: "+err.Error())
		return false
	}

	plan := planDatabases(kitchen, cfg, namespace, served)
	switch {
	case plan.clear:
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condDatabasesReady)
		kitchen.Status.Databases = nil
		return true
	case plan.install:
		return r.runCNPGInstall(ctx, kitchen, cfg, namespace, setCond)
	}
	if plan.record != nil {
		kitchen.Status.Databases = plan.record
	}
	setCond(condDatabasesReady, plan.status, plan.reason, plan.message)
	return plan.ready
}

// cnpgNamespace is where CloudNativePG itself runs — upstream's own default
// unless the singleton says otherwise.
func cnpgNamespace(kitchen *kitchenv1alpha1.Kitchen) string {
	if namespace := kitchen.Spec.Databases.OperatorNamespace; namespace != "" {
		return namespace
	}
	return DefaultCNPGOperatorNamespace
}

// DefaultCNPGOperatorNamespace is where CloudNativePG's own documentation
// installs it, which is what an installation taking it over by hand later
// will expect.
const DefaultCNPGOperatorNamespace = "cnpg-system"

// cnpgVersionMoved reports whether the operator's pin has moved since it
// installed. It is what makes an operator upgrade carry its dependency
// forward instead of leaving the platform on whatever the first install
// happened to pull.
func cnpgVersionMoved(kitchen *kitchenv1alpha1.Kitchen, cfg CNPGInstallConfig) bool {
	recorded := kitchen.Status.Databases
	if recorded == nil {
		return false
	}
	// Only an installation that still permits the install may act on drift;
	// otherwise the answer is "leave it exactly as it is".
	if !kitchen.Spec.Databases.Install || !cfg.Enabled() {
		return false
	}
	return recorded.Version != cfg.ChartVersion
}

// runCNPGInstall creates the install job, or reads the outcome of the one it
// created earlier.
//
// The job is named after the version it installs, which is what makes an
// upgrade a new job rather than a rerun of a finished one — and what stops a
// failed install from being retried in a tight loop: the failed job stays
// until its TTL reaps it, and only then is the same install attempted again.
func (r *KitchenReconciler) runCNPGInstall(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	cfg CNPGInstallConfig,
	namespace string,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	name := cnpgInstallJobName(cfg)

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: name}, job)
	switch {
	case apierrors.IsNotFound(err):
		// The grant is checked rather than assumed: the flags say the chart
		// promised an account, and this says the account is there — the
		// difference between failing now and failing with a Forbidden
		// part-way through installing a CRD.
		sa := &corev1.ServiceAccount{}
		saKey := types.NamespacedName{Namespace: PlatformNamespace, Name: cfg.ServiceAccount}
		if err := r.Get(ctx, saKey, sa); err != nil {
			setCond(condDatabasesReady, metav1.ConditionFalse, "ServiceAccountMissing", fmt.Sprintf(
				"the install job's ServiceAccount %q is missing from %s: %s. The chart creates it when "+
					"databases.install.enabled is set", saKey.Name, PlatformNamespace, err))
			return false
		}
		if err := r.Create(ctx, cnpgInstallJob(name, namespace, cfg)); err != nil && !apierrors.IsAlreadyExists(err) {
			setCond(condDatabasesReady, metav1.ConditionFalse, "InstallJobNotCreated", err.Error())
			return false
		}
		setCond(condDatabasesReady, metav1.ConditionFalse, "Installing",
			fmt.Sprintf("installing CloudNativePG %s into %s", cfg.ChartVersion, namespace))
		return false
	case err != nil:
		setCond(condDatabasesReady, metav1.ConditionFalse, "InstallJobUnreadable", err.Error())
		return false
	}

	complete, failed, message := jobOutcome(job)
	switch {
	case complete:
		// Recorded before the API is probed again: helm --wait returned, so
		// the release exists whether or not the CRD has reached this
		// operator's RESTMapper yet. The next reconcile reads the record and
		// the served API together and settles the condition.
		kitchen.Status.Databases = &kitchenv1alpha1.DatabasesStatus{
			Managed:   true,
			Namespace: namespace,
			Version:   cfg.ChartVersion,
		}
		setCond(condDatabasesReady, metav1.ConditionTrue, "OperatorInstalled",
			fmt.Sprintf("the platform installed CloudNativePG %s into %s", cfg.ChartVersion, namespace))
		return true
	case failed:
		setCond(condDatabasesReady, metav1.ConditionFalse, "InstallFailed", fmt.Sprintf(
			"installing CloudNativePG %s failed: %s. The job's logs are in the platform's own, under the "+
				"component %q; it is retried once the finished job is reaped",
			cfg.ChartVersion, message, cnpgInstallComponent))
		return false
	default:
		setCond(condDatabasesReady, metav1.ConditionFalse, "Installing",
			fmt.Sprintf("installing CloudNativePG %s into %s", cfg.ChartVersion, namespace))
		return false
	}
}

// cnpgInstallJobName names the job after what it installs, so that a version
// bump is a different job and a finished one is never re-read as the answer
// to a question it was not asked.
func cnpgInstallJobName(cfg CNPGInstallConfig) string {
	version := nonAlphanumeric.ReplaceAllString(cfg.ChartVersion, "-")
	name := "kitchen-cnpg-install-" + strings.Trim(strings.ToLower(version), "-")
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	return name
}

// cnpgInstallJob builds the install.
//
// Nothing in the argv comes from a request. The job is bound to cluster-admin,
// so an install that forwarded caller-supplied helm arguments would be a way
// to apply arbitrary objects as cluster-admin, and `databases.install.enabled`
// would not be the gate it looks like. The one value that comes from the
// singleton is the namespace, which is checked against a DNS label first and
// reaches helm as its own argument, never a shell's — there is no shell in
// this pod at all.
func cnpgInstallJob(name, namespace string, cfg CNPGInstallConfig) *batchv1.Job {
	labels := map[string]string{
		labelManagedByKey:  labelManagedByValue,
		labelComponentKind: cnpgInstallComponent,
	}

	// --install, so a rerun at a new version upgrades what is there rather
	// than failing; --wait, so a claim that provisions immediately afterwards
	// finds an operator that is actually serving. Nothing is --atomic: rolling
	// a database operator back out from under Clusters it is reconciling is a
	// worse state than a half-finished install somebody can look at.
	args := []string{
		"upgrade", cnpgReleaseName, cnpgChartName,
		"--install",
		"--repo", cfg.Repository,
		"--version", cfg.ChartVersion,
		"--namespace", namespace,
		"--create-namespace",
		"--wait",
		"--timeout", cfg.Timeout.String(),
	}

	env := []corev1.EnvVar{
		// helm writes its cache, config and data under $HOME by default,
		// which is not writable in the image.
		{Name: "HELM_CACHE_HOME", Value: "/tmp/helm/cache"},
		{Name: "HELM_CONFIG_HOME", Value: "/tmp/helm/config"},
		{Name: "HELM_DATA_HOME", Value: "/tmp/helm/data"},
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PlatformNamespace, Labels: labels},
		Spec: batchv1.JobSpec{
			// Never retried in place. A helm run that failed half-way is
			// re-entered by the next job of the same name, once the TTL has
			// reaped this one and the reconciler has read what it said.
			BackoffLimit:            ptr.To(int32(0)),
			ActiveDeadlineSeconds:   ptr.To(int64((cfg.Timeout + cnpgInstallDeadlineBuffer).Seconds())),
			TTLSecondsAfterFinished: ptr.To(int32(cnpgInstallJobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cfg.ServiceAccount,
					Containers: []corev1.Container{{
						Name:         "cloudnative-pg",
						Image:        cfg.HelmImage,
						Command:      []string{"helm"},
						Args:         args,
						Env:          env,
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
}

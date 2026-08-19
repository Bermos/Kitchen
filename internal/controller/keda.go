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
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	// condScaleToZeroReady is where the platform says whether it can idle an
	// environment to zero at all — which is a fact about the cluster, not
	// about any one environment. Each Environment's own ScaleToZero condition
	// answers the narrower question of whether it is idling.
	condScaleToZeroReady = "ScaleToZeroReady"

	// DefaultKedaChartRepository is where the two charts are pulled from.
	// KEDA publishes them as a classic HTTP repository rather than as OCI
	// artifacts, so this is a repository URL and helm is given it with
	// --repo, which needs no `helm repo add` and so no writable repository
	// cache to add it to.
	DefaultKedaChartRepository = "https://kedacore.github.io/charts"

	// DefaultKedaChartVersion and DefaultKedaHTTPChartVersion are pinned, and
	// pinned *as a pair*: the add-on tracks KEDA's own CRD and API closely
	// enough that the two versions are chosen together, the way
	// BuildpacksBuilderImage is pinned rather than floated. Bumping one means
	// checking the other's compatibility note.
	//
	// The interceptor's Service name and port follow from this pair too. At
	// these versions the add-on names its proxy
	// "keda-add-ons-http-interceptor-proxy" on port 8080, which is what
	// InterceptorSpec defaults to — a bump that moved either would have to
	// move those defaults with it.
	DefaultKedaChartVersion     = "2.20.2"
	DefaultKedaHTTPChartVersion = "0.15.0"

	// DefaultKedaInstallTimeout is what each of the two helm runs is given.
	// Both --wait, so this is time for pods to become ready and not merely
	// for manifests to be accepted.
	DefaultKedaInstallTimeout = 10 * time.Minute

	// kedaReleaseName and kedaHTTPReleaseName are the release names upstream's
	// own instructions use. Using anything else would make an installation
	// that later wants to take KEDA over by hand harder to reason about, for
	// no gain.
	kedaReleaseName     = "keda"
	kedaHTTPReleaseName = "keda-add-ons-http"

	// kedaInstallComponent names the job in collected logs, and is what the
	// dashboard filters on to show what helm said.
	kedaInstallComponent = "keda-install"

	// kedaInstallDeadlineBuffer is how much longer the Job may run than the
	// two helm runs together are given, so that helm always loses the race and
	// gets to report its own failure.
	kedaInstallDeadlineBuffer = 5 * time.Minute

	// kedaInstallJobTTLSeconds keeps the finished job and its pod around long
	// enough for the log collector to ship helm's output. What the job
	// achieved outlives it in status.scaleToZero.
	kedaInstallJobTTLSeconds = 3600
)

// scaledObjectGVK is KEDA's own scaling record. Kitchen never writes one — the
// HTTP add-on does, for its own interceptor fleet — but its presence is how the
// operator recognises a cluster that already runs KEDA, and so must not be
// installed into.
func scaledObjectGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}
}

// KedaInstallConfig is what the chart tells the operator about installing the
// platform's own scale-to-zero dependencies.
//
// Like SelfUpdateConfig it arrives as flags on the manager Deployment rather
// than as fields on the Kitchen singleton, and for the same reason: the
// singleton is applied as a post-install hook and is not re-applied on upgrade,
// so a value flipped in a `helm upgrade` would create the ServiceAccount and
// never reach the operator. The Deployment is re-applied every time.
//
// What *is* on the singleton is the decision — `spec.scaleToZero.install` —
// because that is the half a person changes.
type KedaInstallConfig struct {
	// ServiceAccount is what the install job runs as. It is bound to
	// cluster-admin, because installing KEDA applies CRDs, ClusterRoles and a
	// namespace; it is a separate account from the manager's so that the grant
	// is visible, revocable and gone on uninstall. Empty means the chart was
	// rendered without the grant, and no install is attempted.
	ServiceAccount string

	// HelmImage runs the install.
	HelmImage string

	// Repository the two charts are pulled from.
	Repository string

	// ChartVersion and AddOnChartVersion pin what is installed.
	ChartVersion      string
	AddOnChartVersion string

	// Timeout is what each helm run is given.
	Timeout time.Duration
}

// Enabled reports whether this installation may install KEDA for itself. Only
// the account is required: everything else has a default the operator compiles
// in, and half a configuration should still install something known.
func (c KedaInstallConfig) Enabled() bool { return c.ServiceAccount != "" }

// withDefaults fills in everything the chart did not say.
func (c KedaInstallConfig) withDefaults() KedaInstallConfig {
	if c.HelmImage == "" {
		c.HelmImage = DefaultHelmImage
	}
	if c.Repository == "" {
		c.Repository = DefaultKedaChartRepository
	}
	if c.ChartVersion == "" {
		c.ChartVersion = DefaultKedaChartVersion
	}
	if c.AddOnChartVersion == "" {
		c.AddOnChartVersion = DefaultKedaHTTPChartVersion
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultKedaInstallTimeout
	}
	return c
}

// dnsLabel is what a namespace name has to look like. The name reaches helm as
// its own argv element rather than through a shell, so this buys a legible
// failure rather than safety: a cluster-admin job that dies on an unparseable
// --namespace has already been created, and refusing before that is cheaper.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create

// kedaPlan is what one reconcile has decided to do about KEDA, before anything
// is read or written. Keeping the decision separate from carrying it out is
// what lets every branch of it be exercised against a cluster whose KEDA state
// is whatever the test harness happens to have.
type kedaPlan struct {
	// clear removes the condition and the record entirely: the platform idles
	// nothing, so neither is worth carrying on every reconcile.
	clear bool

	// install runs the two helm installs. Mutually exclusive with a condition
	// this plan sets itself — the job's own progress is the condition then.
	install bool

	// record replaces status.scaleToZero. Nil leaves what is there.
	record *kitchenv1alpha1.ScaleToZeroStatus

	status  metav1.ConditionStatus
	reason  string
	message string

	// ready is what reconcileKeda returns: whether this reconcile needs
	// requeueing. A refusal is *ready*, because nothing will change without a
	// spec edit or a chart upgrade, and both reconcile the object again on
	// their own — requeueing at 30s forever would only reprint the message.
	ready bool
}

// planKeda decides what to do about the platform's scale-to-zero dependency.
//
// addOnServed is whether this cluster serves the add-on's API — the only thing
// the platform actually needs. kedaServed is consulted only when it does not,
// and so is only probed then.
//
// Nothing here ever plans a write to a release the operator did not create. A
// cluster that already serves the add-on's API is recorded and left alone,
// permanently: an installation that would rather run its own KEDA — a shared
// one, a pinned one, one its GitOps owns — has to be able to, and a platform
// that "helpfully" upgraded it would be a worse neighbour than one that never
// offered.
func planKeda(
	kitchen *kitchenv1alpha1.Kitchen,
	cfg KedaInstallConfig,
	namespace string,
	addOnServed, kedaServed bool,
) kedaPlan {
	if !kitchen.Spec.ScaleToZero.Enabled {
		return kedaPlan{clear: true, ready: true}
	}

	recorded := kitchen.Status.ScaleToZero
	managed := recorded != nil && recorded.Managed

	if addOnServed && !managed {
		return kedaPlan{
			record: &kitchenv1alpha1.ScaleToZeroStatus{Namespace: namespace},
			status: metav1.ConditionTrue, reason: "AddOnPresent",
			message: "the KEDA HTTP add-on is already serving in this cluster; the operator installed nothing " +
				"and manages nothing about it",
			ready: true,
		}
	}

	if addOnServed && managed && !kedaVersionsMoved(kitchen, cfg) {
		return kedaPlan{
			status: metav1.ConditionTrue, reason: "AddOnInstalled",
			message: fmt.Sprintf("the platform installed KEDA %s and its HTTP add-on %s into %s",
				recorded.Version, recorded.AddOnVersion, recorded.Namespace),
			ready: true,
		}
	}

	if !kitchen.Spec.ScaleToZero.Install {
		return kedaPlan{
			status: metav1.ConditionFalse, reason: "NotInstalled",
			message: "spec.scaleToZero is enabled but the KEDA HTTP add-on is not serving, so no environment " +
				"idles. Set spec.scaleToZero.install to have the platform install KEDA and the add-on itself, " +
				"or install the two Helm releases yourself (see the chart README)",
			ready: true,
		}
	}

	if !cfg.Enabled() {
		return kedaPlan{
			status: metav1.ConditionFalse, reason: "InstallNotPermitted",
			message: "spec.scaleToZero.install is set, but this installation did not grant the operator an " +
				"account to install with. Upgrade the chart with `--set scaleToZero.install.enabled=true`, which " +
				"creates the install job's ServiceAccount — it is bound to cluster-admin, because installing KEDA " +
				"applies CRDs and ClusterRoles, so it is off by default",
			ready: true,
		}
	}

	if !dnsLabel.MatchString(namespace) {
		return kedaPlan{
			status: metav1.ConditionFalse, reason: "NamespaceInvalid",
			message: fmt.Sprintf("spec.scaleToZero.interceptor.namespace is %q, which is not a valid namespace "+
				"name, so there is nowhere to install KEDA", namespace),
			ready: true,
		}
	}

	// KEDA without its add-on, and not ours. Installing the add-on beside it
	// would work; installing KEDA over it would not, and helm would find that
	// out half-way through.
	if kedaServed && !managed {
		return kedaPlan{
			status: metav1.ConditionFalse, reason: "KedaNotOurs",
			message: "KEDA is already installed in this cluster but its HTTP add-on is not, and the platform " +
				"will not install over a release it does not own. Install the add-on beside your KEDA " +
				"(`helm install keda-add-ons-http kedacore/keda-add-ons-http`), and point " +
				"spec.scaleToZero.interceptor at it if it is not in " + defaultInterceptorNamespace,
			ready: true,
		}
	}

	return kedaPlan{install: true}
}

// reconcileKeda answers whether the platform can idle an environment to zero,
// and — where the installation has asked it to and granted it the account —
// makes that true by installing KEDA and its HTTP add-on itself.
//
// The whole feature exists because the reason the *chart* cannot bundle them is
// Helm's and not Kubernetes': Helm builds and validates a release's entire
// manifest before applying any of it, so the add-on's ScaledObject cannot
// resolve against a CRD arriving in the same release. An operator applies in
// whatever order it likes and can wait in between, which is the same reason the
// cert-manager ClusterIssuer and Certificate are the operator's rather than the
// chart's.
func (r *KitchenReconciler) reconcileKeda(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	if !kitchen.Spec.ScaleToZero.Enabled {
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condScaleToZeroReady)
		kitchen.Status.ScaleToZero = nil
		return true
	}

	namespace := interceptorBackend(kitchen).Namespace
	cfg := r.KedaInstall.withDefaults()

	addOnServed, err := r.apiServed(ctx, HTTPScaledObjectGVK())
	if err != nil {
		setCond(condScaleToZeroReady, metav1.ConditionFalse, "AddOnProbeFailed",
			"could not tell whether the KEDA HTTP add-on is installed: "+err.Error())
		return false
	}
	kedaServed := false
	if !addOnServed {
		if kedaServed, err = r.apiServed(ctx, scaledObjectGVK()); err != nil {
			setCond(condScaleToZeroReady, metav1.ConditionFalse, "KedaProbeFailed",
				"could not tell whether KEDA is installed: "+err.Error())
			return false
		}
	}

	plan := planKeda(kitchen, cfg, namespace, addOnServed, kedaServed)
	switch {
	case plan.clear:
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condScaleToZeroReady)
		kitchen.Status.ScaleToZero = nil
		return true
	case plan.install:
		return r.runKedaInstall(ctx, kitchen, cfg, namespace, setCond)
	}

	if plan.record != nil {
		kitchen.Status.ScaleToZero = plan.record
	}
	setCond(condScaleToZeroReady, plan.status, plan.reason, plan.message)
	return plan.ready
}

// kedaVersionsMoved reports whether the operator's pinned pair has moved since
// it installed. It is what makes an operator upgrade carry its dependency
// forward instead of leaving the platform on whatever the first install
// happened to pull.
func kedaVersionsMoved(kitchen *kitchenv1alpha1.Kitchen, cfg KedaInstallConfig) bool {
	recorded := kitchen.Status.ScaleToZero
	if recorded == nil {
		return false
	}
	// Only an installation that still permits the install may act on drift;
	// otherwise the answer is "leave it exactly as it is".
	if !kitchen.Spec.ScaleToZero.Install || !cfg.Enabled() {
		return false
	}
	return recorded.Version != cfg.ChartVersion || recorded.AddOnVersion != cfg.AddOnChartVersion
}

// runKedaInstall creates the install job, or reads the outcome of the one it
// created earlier.
//
// The job is named after the version pair it installs, which is what makes an
// upgrade a new job rather than a rerun of a finished one — and what stops a
// failed install from being retried in a tight loop: the failed job stays until
// its TTL reaps it, and only then is the same install attempted again. That
// hourly retry is deliberate here where it would be wrong for a self-update: a
// dependency that failed to install because a registry was briefly unreachable
// should end up installed without anybody watching for the moment to try again.
func (r *KitchenReconciler) runKedaInstall(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	cfg KedaInstallConfig,
	namespace string,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	name := kedaInstallJobName(cfg)

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: name}, job)
	switch {
	case apierrors.IsNotFound(err):
		// The grant is checked rather than assumed, the way a self-update
		// checks it: the flags say the chart promised an account, and this
		// says the account is there — the difference between failing now and
		// failing with a Forbidden part-way through installing a CRD.
		sa := &corev1.ServiceAccount{}
		saKey := types.NamespacedName{Namespace: PlatformNamespace, Name: cfg.ServiceAccount}
		if err := r.Get(ctx, saKey, sa); err != nil {
			setCond(condScaleToZeroReady, metav1.ConditionFalse, "ServiceAccountMissing", fmt.Sprintf(
				"the install job's ServiceAccount %q is missing from %s: %s. The chart creates it when "+
					"scaleToZero.install.enabled and rbac.create are both true", saKey.Name, PlatformNamespace, err))
			return false
		}
		if err := r.Create(ctx, kedaInstallJob(name, namespace, cfg)); err != nil && !apierrors.IsAlreadyExists(err) {
			setCond(condScaleToZeroReady, metav1.ConditionFalse, "InstallJobNotCreated", err.Error())
			return false
		}
		setCond(condScaleToZeroReady, metav1.ConditionFalse, "Installing", fmt.Sprintf(
			"installing KEDA %s and its HTTP add-on %s into %s",
			cfg.ChartVersion, cfg.AddOnChartVersion, namespace))
		return false
	case err != nil:
		setCond(condScaleToZeroReady, metav1.ConditionFalse, "InstallJobUnreadable", err.Error())
		return false
	}

	complete, failed, message := jobOutcome(job)
	switch {
	case complete:
		// Recorded before the API is probed again: helm --wait returned, so
		// the releases exist whether or not the CRD has reached this
		// operator's RESTMapper yet. The next reconcile reads the record and
		// the served API together and settles the condition.
		kitchen.Status.ScaleToZero = &kitchenv1alpha1.ScaleToZeroStatus{
			Managed:      true,
			Namespace:    namespace,
			Version:      cfg.ChartVersion,
			AddOnVersion: cfg.AddOnChartVersion,
		}
		setCond(condScaleToZeroReady, metav1.ConditionTrue, "AddOnInstalled", fmt.Sprintf(
			"the platform installed KEDA %s and its HTTP add-on %s into %s",
			cfg.ChartVersion, cfg.AddOnChartVersion, namespace))
		return true
	case failed:
		setCond(condScaleToZeroReady, metav1.ConditionFalse, "InstallFailed", fmt.Sprintf(
			"installing KEDA %s and its HTTP add-on %s failed: %s. The job's logs are in the platform's own, "+
				"under the component %q; it is retried once the finished job is reaped",
			cfg.ChartVersion, cfg.AddOnChartVersion, message, kedaInstallComponent))
		return false
	default:
		setCond(condScaleToZeroReady, metav1.ConditionFalse, "Installing", fmt.Sprintf(
			"installing KEDA %s and its HTTP add-on %s into %s",
			cfg.ChartVersion, cfg.AddOnChartVersion, namespace))
		return false
	}
}

// kedaInstallJobName names the job after what it installs, so that a version
// bump is a different job and a finished one is never re-read as the answer to
// a question it was not asked.
func kedaInstallJobName(cfg KedaInstallConfig) string {
	versions := nonAlphanumeric.ReplaceAllString(cfg.ChartVersion+"-"+cfg.AddOnChartVersion, "-")
	name := "kitchen-keda-install-" + strings.Trim(strings.ToLower(versions), "-")
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	return name
}

var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// kedaInstallJob builds the install.
//
// The ordering that Helm cannot express within one release is expressed here by
// two containers: KEDA goes in as an init container, which runs to completion
// before the add-on's container starts, so the add-on's own `ScaledObject` is
// applied against a CRD that is already established. Both helm runs --wait, so
// "completed" means the workloads are up and not merely that the manifests were
// accepted.
//
// Neither argv takes anything from a request. The job is bound to
// cluster-admin, so an install that forwarded caller-supplied helm arguments
// would be a way to apply arbitrary objects as cluster-admin, and
// `scaleToZero.install.enabled` would not be the gate it looks like. The one
// value that comes from the singleton is the namespace, which is checked
// against a DNS label first and reaches helm as its own argument, never a
// shell's.
func kedaInstallJob(name, namespace string, cfg KedaInstallConfig) *batchv1.Job {
	labels := map[string]string{
		labelManagedByKey:  labelManagedByValue,
		labelComponentKind: kedaInstallComponent,
	}

	// --install, so a rerun at a new version upgrades what is there rather
	// than failing; --wait, so the next release finds the last one running.
	// Nothing is --atomic: rolling KEDA back out from under an add-on that
	// installed against it is a worse state than a half-finished install
	// somebody can look at.
	helmArgs := func(release, chart, version string) []string {
		return []string{
			"upgrade", release, chart,
			"--install",
			"--repo", cfg.Repository,
			"--version", version,
			"--namespace", namespace,
			"--create-namespace",
			"--wait",
			"--timeout", cfg.Timeout.String(),
		}
	}

	env := []corev1.EnvVar{
		// helm writes its cache, config and data under $HOME by default,
		// which is not writable in the image. Everything it needs here is
		// disposable, and shared between the two runs so the second does not
		// re-fetch the repository index.
		{Name: "HELM_CACHE_HOME", Value: "/tmp/helm/cache"},
		{Name: "HELM_CONFIG_HOME", Value: "/tmp/helm/config"},
		{Name: "HELM_DATA_HOME", Value: "/tmp/helm/data"},
	}
	mounts := []corev1.VolumeMount{{Name: "helm-home", MountPath: "/tmp"}}

	total := 2 * cfg.Timeout
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PlatformNamespace, Labels: labels},
		Spec: batchv1.JobSpec{
			// Never retried in place. A helm run that failed half-way is
			// re-entered by the next job of the same name, once the TTL has
			// reaped this one and the reconciler has read what it said.
			BackoffLimit:            ptr.To(int32(0)),
			ActiveDeadlineSeconds:   ptr.To(int64((total + kedaInstallDeadlineBuffer).Seconds())),
			TTLSecondsAfterFinished: ptr.To(int32(kedaInstallJobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cfg.ServiceAccount,
					InitContainers: []corev1.Container{{
						Name:         "keda",
						Image:        cfg.HelmImage,
						Command:      []string{"helm"},
						Args:         helmArgs(kedaReleaseName, "keda", cfg.ChartVersion),
						Env:          env,
						VolumeMounts: mounts,
					}},
					Containers: []corev1.Container{{
						Name:         "keda-http-add-on",
						Image:        cfg.HelmImage,
						Command:      []string{"helm"},
						Args:         helmArgs(kedaHTTPReleaseName, "keda-add-ons-http", cfg.AddOnChartVersion),
						Env:          env,
						VolumeMounts: mounts,
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

// apiServed reports whether the cluster serves a kind at all. A CRD that is not
// installed is a RESTMapper no-match rather than an error worth reporting, which
// is the same signal reconcileScaleToZero falls back on when it cannot write an
// HTTPScaledObject.
//
// It reads through APIReader rather than the cache on purpose: caching every
// object of a kind in the cluster to answer "does this kind exist" would cost
// far more than it saves, and starting an informer for a kind that may not
// exist is how a probe becomes a wait.
func (r *KitchenReconciler) apiServed(ctx context.Context, gvk schema.GroupVersionKind) (bool, error) {
	var reader client.Reader = r.APIReader
	if reader == nil {
		reader = r.Client
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List",
	})
	err := reader.List(ctx, list, client.Limit(1))
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err), apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

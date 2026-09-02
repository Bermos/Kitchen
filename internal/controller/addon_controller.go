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
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The one engine every platform dependency is installed by.
//
// It was written twice before this — internal/controller/keda.go and
// internal/controller/cnpg.go, 1,185 lines of the same shape with the nouns
// swapped, the second of which said so in its own header. Everything that was
// common is here; everything that differed is a field of addonEntry.
//
// The properties the two copies established are the engine's now, and a new
// entry gets them without asking:
//
//   - off by default, behind an account the chart creates only when asked;
//   - a refusal, never a takeover, where somebody else's release is serving;
//   - a pinned version, and an upgrade that carries the dependency forward;
//   - nothing from a request in the argv, so the grant means what it says;
//   - ownership re-derived from the install job rather than remembered.
const (
	// addonInstallDeadlineBuffer is how much longer the Job may run than its
	// helm runs together are given, so that helm always loses the race and
	// gets to report its own failure.
	addonInstallDeadlineBuffer = 5 * time.Minute

	// addonInstallJobTTLSeconds keeps the finished job and its pod around
	// long enough for the log collector to ship helm's output. What the job
	// achieved outlives it in the Addon's status — and, for as long as the
	// job is there, in the job itself.
	addonInstallJobTTLSeconds = 3600

	// addonFinalizer is what makes deleting an Addon a decision the operator
	// gets to answer rather than a row disappearing: an entry a Connection
	// or a claim depends on is refused, and one that goes says what went.
	addonFinalizer = "kitchen.bermos.dev/addon"

	// addonRequeueDelay is how often an Addon looks again — an install
	// running, a dependency not up yet, or simply the cluster it is a
	// statement about.
	//
	// A settled Addon requeues too, and that is not belt-and-braces. Every
	// verdict it reaches rests on a probe of the cluster — is this kind
	// served — and nothing notifies it when the answer changes. Somebody
	// installing KEDA by hand five minutes after the platform came up
	// changes it, and an Addon that stopped looking would go on reporting
	// "not serving" about a cluster plainly serving it, for good.
	addonRequeueDelay = 30 * time.Second

	// DefaultAddonInstallTimeout is what each of an entry's helm runs is
	// given unless the chart says otherwise. They --wait, so it is time for
	// workloads to become ready and not merely for manifests to be accepted.
	DefaultAddonInstallTimeout = 10 * time.Minute
)

// AddonInstallConfig is what the chart tells the operator about one catalogue
// entry.
//
// It arrives as flags on the manager Deployment rather than as fields on the
// Kitchen singleton, and for the reason the two configs it replaces did: the
// singleton is applied as a post-install hook and is not re-applied on
// upgrade, so a value flipped in a `helm upgrade` would create the
// ServiceAccount and never reach the operator. The Deployment is re-applied
// every time.
//
// What is *not* here is the request. That is the Addon object.
type AddonInstallConfig struct {
	// ServiceAccount is what the install job runs as, and it is the grant:
	// empty means this installation did not permit the entry, and an Addon
	// asking for it is Refused rather than attempted. It is a separate
	// account from the manager's so that the grant is visible, revocable and
	// gone on uninstall.
	ServiceAccount string

	// HelmImage runs the install.
	HelmImage string

	// Repository overrides the entry's, for a cluster that reaches a mirror
	// and not the upstream index.
	Repository string

	// Versions overrides the entry's pins, by chart name. A chart the entry
	// does not install is ignored rather than added: the catalogue decides
	// what is installed, and a flag decides which version of it.
	Versions map[string]string

	// Timeout is what each helm run is given.
	Timeout time.Duration
}

// AddonInstalls is what the chart permitted, by catalogue entry ID. An entry
// absent from it is one this installation did not grant an account for.
type AddonInstalls map[string]AddonInstallConfig

// permits reports whether this installation granted an account for an entry.
func (a AddonInstalls) permits(id string) bool {
	return a[id].ServiceAccount != ""
}

// forEntry is the config an entry is installed with: what the chart said,
// with the catalogue's own defaults behind it.
func (a AddonInstalls) forEntry(entry addonEntry) AddonInstallConfig {
	cfg := a[entry.ID]
	if cfg.HelmImage == "" {
		cfg.HelmImage = DefaultHelmImage
	}
	if cfg.Repository == "" {
		cfg.Repository = entry.Repository
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = entry.Timeout
	}
	versions := entry.chartVersions()
	for chart, version := range cfg.Versions {
		if _, known := versions[chart]; known && version != "" {
			versions[chart] = version
		}
	}
	cfg.Versions = versions
	return cfg
}

// version is what will be installed for one chart.
func (c AddonInstallConfig) version(chart addonChart) string {
	if version := c.Versions[chart.Chart]; version != "" {
		return version
	}
	return chart.DefaultVersion
}

// AddonReconciler installs the platform's own dependencies, one Addon at a
// time.
type AddonReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads unstructured probes straight from the API server. The
	// cached client would start an informer for a kind that may not exist,
	// which is how a probe becomes a wait.
	APIReader client.Reader

	// Installs is what the chart permitted, by entry.
	Installs AddonInstalls
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=addons,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=addons/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=addons/finalizers,verbs=update

// Reconcile brings one catalogue entry to what the Addon asks of it.
func (r *AddonReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	addon := &kitchenv1alpha1.Addon{}
	if err := r.Get(ctx, req.NamespacedName, addon); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	entry, known := lookupAddon(addon.Name)

	if addon.DeletionTimestamp != nil {
		return r.finalize(ctx, addon, entry, known)
	}
	if controllerutil.AddFinalizer(addon, addonFinalizer) {
		if err := r.Update(ctx, addon); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !known {
		return r.settle(ctx, addon, addonPlan{
			status: metav1.ConditionFalse, reason: kitchenv1alpha1.AddonRefused,
			message: fmt.Sprintf("there is no catalogue entry named %q; this operator installs %s. The "+
				"catalogue is compiled in — an Addon cannot name a chart to install, because its install job "+
				"can apply CRDs and ClusterRoles", addon.Name, strings.Join(addonIDs(), ", ")),
			ready: true,
		})
	}

	served, err := apiServed(ctx, r.reader(), entry.Probe)
	if err != nil {
		return r.settle(ctx, addon, addonPlan{
			status: metav1.ConditionFalse, reason: "ProbeFailed",
			message: fmt.Sprintf("could not tell whether %s is installed: %s", entry.Title, err),
		})
	}

	installJob, err := latestCompletedInstall(ctx, r.Client, entry.Component)
	if err != nil {
		return r.settle(ctx, addon, addonPlan{
			status: metav1.ConditionFalse, reason: "InstallJobUnreadable",
			message: fmt.Sprintf("could not tell whether the platform installed %s itself: %s", entry.Title, err),
		})
	}

	cfg := r.Installs.forEntry(entry)
	namespace := addonNamespace(addon, entry)
	observed := addonObservation{
		served:    served,
		permitted: r.Installs.permits(entry.ID),
		namespace: namespace,
		installed: addonInstalled(installJob, entry, cfg, namespace),
	}
	if !observed.served && entry.Partial != nil {
		if observed.partiallyServed, err = apiServed(ctx, r.reader(), entry.Partial.Probe); err != nil {
			return r.settle(ctx, addon, addonPlan{
				status: metav1.ConditionFalse, reason: "ProbeFailed",
				message: fmt.Sprintf("could not tell whether %s is partly installed: %s", entry.Title, err),
			})
		}
	}
	if observed.blockedBy, err = r.unreadyDependency(ctx, entry); err != nil {
		return ctrl.Result{}, err
	}

	plan := planAddon(addon, entry, cfg, observed)
	if plan.install {
		return r.runInstall(ctx, addon, entry, cfg, namespace)
	}
	log.V(1).Info("reconciled addon", "addon", addon.Name, "reason", plan.reason)
	return r.settle(ctx, addon, plan)
}

// addonObservation is everything one reconcile read about the cluster before
// it decided anything. Keeping the reading and the deciding apart is what
// lets every branch of planAddon be exercised without a cluster in whatever
// state the harness happens to have left it.
type addonObservation struct {
	// served is whether the entry's own API answers.
	served bool
	// partiallyServed is whether the entry's Partial probe answers — half of
	// the entry, installed by somebody else.
	partiallyServed bool
	// permitted is whether the chart granted an account for this entry.
	permitted bool
	// namespace the entry would be installed into.
	namespace string
	// installed is what the platform's own install job says it installed,
	// nil where there is no such job.
	installed *addonRecord
	// blockedBy names a dependency that is not Ready yet, empty where there
	// is none.
	blockedBy string
}

// addonRecord is what an entry's install left behind: where it went and at
// what versions.
type addonRecord struct {
	namespace string
	charts    []kitchenv1alpha1.AddonChartStatus
}

// addonPlan is what one reconcile decided, before anything is written.
type addonPlan struct {
	// install runs the entry's helm charts. Mutually exclusive with a
	// condition this plan sets itself — the job's own progress is the
	// condition then.
	install bool

	// states is whether this plan is a verdict about ownership. Only three
	// are: the entry is somebody else's, the entry is ours, and an install
	// that has just finished. Everything else — installing, failed, refused,
	// waiting on a dependency — leaves the record exactly as it found it.
	//
	// It is not a detail. The record is the only evidence of ownership while
	// an install is in flight, because the job that will be the durable half
	// of it has not completed yet; a plan that wrote `managed: false` on its
	// way past would erase it, and the next reconcile would read the cluster
	// as somebody else's and adopt what it had just installed. That is issue
	// #244 again, one branch further along.
	states bool
	// managed is whether the platform installed what is serving.
	managed bool
	// serving is whether the entry's API answers.
	serving bool
	// namespace and charts are the record to write.
	namespace string
	charts    []kitchenv1alpha1.AddonChartStatus

	status  metav1.ConditionStatus
	reason  string
	message string

	// ready is whether this reconcile reached a verdict rather than watching
	// something in flight. Both kinds look again — see addonRequeueDelay —
	// and what this decides is the logging and, for the singleton's roll-up,
	// whether the platform is still waiting on it.
	ready bool
}

// planAddon decides what to do about one catalogue entry.
//
// Nothing here ever plans a write to a release the operator did not create. A
// cluster already serving the entry's API is recorded and left alone,
// permanently: an installation that would rather run its own — a shared one, a
// pinned one, one its GitOps owns — has to be able to, and a platform that
// "helpfully" upgraded it would be a worse neighbour than one that never
// offered.
//
// installed is what the cluster itself says the platform installed, and it is
// what ownership is read from first; the status record is second, and only
// where there is no job left to read. See installevidence.go for why that
// order is the one that matters.
func planAddon(
	addon *kitchenv1alpha1.Addon,
	entry addonEntry,
	cfg AddonInstallConfig,
	observed addonObservation,
) addonPlan {
	owned := observed.installed
	if owned == nil && addon.Status.Managed {
		owned = &addonRecord{namespace: addon.Status.Namespace, charts: addon.Status.Charts}
	}

	if observed.served && owned == nil {
		return addonPlan{
			states: true, serving: true, namespace: observed.namespace,
			status: metav1.ConditionTrue, reason: "AlreadyServing",
			message: fmt.Sprintf("%s is already serving in this cluster; the platform installed nothing and "+
				"manages nothing about it", entry.Title),
			ready: true,
		}
	}

	if observed.served && owned != nil && !addonVersionsMoved(addon, entry, cfg, observed, owned) {
		return addonPlan{
			states: true, managed: true, serving: true,
			namespace: owned.namespace, charts: owned.charts,
			status: metav1.ConditionTrue, reason: "Installed",
			message: addonInstalledMessage(entry, owned),
			ready:   true,
		}
	}

	if !addon.Spec.Install {
		return addonPlan{
			namespace: observed.namespace,
			status:    metav1.ConditionFalse, reason: "NotInstalled",
			message: fmt.Sprintf("%s is not serving in this cluster and this Addon does not ask for it. Set "+
				"spec.install to have the platform install it, or install the Helm release yourself", entry.Title),
			ready: true,
		}
	}

	if !observed.permitted {
		return addonPlan{
			namespace: observed.namespace,
			status:    metav1.ConditionFalse, reason: kitchenv1alpha1.AddonRefused,
			message: fmt.Sprintf("this Addon asks for %s, but this installation did not grant the operator an "+
				"account to install it with. Upgrade the chart with `--set %s=true`, which creates the install "+
				"job's ServiceAccount — %s, so it is off by default",
				entry.Title, entry.ChartValue, entry.Grant.Because),
			ready: true,
		}
	}

	if !dnsLabel.MatchString(observed.namespace) {
		return addonPlan{
			status: metav1.ConditionFalse, reason: "NamespaceInvalid",
			message: fmt.Sprintf("spec.namespace is %q, which is not a valid namespace name, so there is "+
				"nowhere to install %s", observed.namespace, entry.Title),
			ready: true,
		}
	}

	if observed.blockedBy != "" {
		return addonPlan{
			namespace: observed.namespace,
			status:    metav1.ConditionFalse, reason: "DependencyNotReady",
			message: fmt.Sprintf("%s needs %s, which is not serving yet; it is installed first and this waits "+
				"for it", entry.Title, observed.blockedBy),
		}
	}

	if observed.partiallyServed && owned == nil {
		return addonPlan{
			namespace: observed.namespace,
			status:    metav1.ConditionFalse, reason: entry.Partial.Reason,
			message: entry.Partial.Message,
			ready:   true,
		}
	}

	return addonPlan{install: true}
}

// addonVersionsMoved reports whether the operator's pins have moved since the
// entry was installed at the versions given. It is what makes an operator
// upgrade carry its dependencies forward instead of leaving the platform on
// whatever the first install happened to pull.
//
// An *unknown* installed version counts as moved, which is how an install job
// from before the operator labelled them settles: reinstalling at the pin is
// what a helm `upgrade --install` at the version already installed does
// anyway, and it ends with the version recorded.
func addonVersionsMoved(
	addon *kitchenv1alpha1.Addon,
	entry addonEntry,
	cfg AddonInstallConfig,
	observed addonObservation,
	owned *addonRecord,
) bool {
	// Only an installation that still asks for the install, and still
	// permits it, may act on drift; otherwise the answer is "leave it
	// exactly as it is".
	if !addon.Spec.Install || !observed.permitted {
		return false
	}
	installed := map[string]string{}
	for _, chart := range owned.charts {
		installed[chart.Name] = chart.Version
	}
	for _, chart := range entry.Charts {
		if installed[chart.Chart] != cfg.version(chart) {
			return true
		}
	}
	return false
}

// addonInstalled turns the platform's own evidence — a completed install job
// it created — into the record that evidence justifies. Nil where there is no
// such job, which is the only state in which the status record is taken at
// its word.
func addonInstalled(
	job *batchv1.Job,
	entry addonEntry,
	cfg AddonInstallConfig,
	namespace string,
) *addonRecord {
	if job == nil {
		return nil
	}
	// One job installs the whole entry, so a name that identifies one
	// version identifies all of them.
	atPin := addonInstallJobName(entry, cfg)
	charts := make([]kitchenv1alpha1.AddonChartStatus, 0, len(entry.Charts))
	for _, chart := range entry.Charts {
		charts = append(charts, kitchenv1alpha1.AddonChartStatus{
			Name:    chart.Chart,
			Version: installedVersion(job, chart.VersionLabel, atPin, cfg.version(chart)),
		})
	}
	return &addonRecord{namespace: installedInto(job, namespace), charts: charts}
}

// addonInstalledMessage says what the platform installed, and stays legible
// where an install job from before the labels leaves a version unknown.
func addonInstalledMessage(entry addonEntry, owned *addonRecord) string {
	parts := make([]string, 0, len(owned.charts))
	for _, chart := range owned.charts {
		if chart.Version == "" {
			return fmt.Sprintf("the platform installed %s into %s", entry.Title, owned.namespace)
		}
		parts = append(parts, chart.Name+" "+chart.Version)
	}
	return fmt.Sprintf("the platform installed %s into %s", strings.Join(parts, " and "), owned.namespace)
}

// addonNamespace is where the entry goes: what the Addon asks for, or the
// entry's own default, which is upstream's.
func addonNamespace(addon *kitchenv1alpha1.Addon, entry addonEntry) string {
	if addon.Spec.Namespace != "" {
		return addon.Spec.Namespace
	}
	return entry.DefaultNamespace
}

// unreadyDependency names the first entry this one depends on that is not
// serving yet, so that an ordering the catalogue declares is enforced rather
// than hoped for. A dependency with no Addon at all is as unready as one that
// has not finished.
func (r *AddonReconciler) unreadyDependency(ctx context.Context, entry addonEntry) (string, error) {
	for _, id := range entry.DependsOn {
		dependency := &kitchenv1alpha1.Addon{}
		key := types.NamespacedName{Namespace: PlatformNamespace, Name: id}
		err := r.Get(ctx, key, dependency)
		switch {
		case apierrors.IsNotFound(err):
			return id, nil
		case err != nil:
			return "", err
		case !dependency.Status.Serving:
			return id, nil
		}
	}
	return "", nil
}

// settle writes the plan onto the Addon and says when to look again.
func (r *AddonReconciler) settle(
	ctx context.Context,
	addon *kitchenv1alpha1.Addon,
	plan addonPlan,
) (ctrl.Result, error) {
	before := addon.Status.DeepCopy()

	if plan.states {
		addon.Status.Managed = plan.managed
		addon.Status.Serving = plan.serving
		if plan.charts != nil {
			addon.Status.Charts = plan.charts
		}
	}
	addon.Status.ObservedGeneration = addon.Generation
	if plan.namespace != "" {
		addon.Status.Namespace = plan.namespace
	}
	setAddonCondition(addon, plan.status, plan.reason, plan.message)

	// An unchanged status is not written. A settled Addon is reconciled by
	// every watch event its install job produces, and writing an identical
	// status each time would be a status update per event forever.
	if !equality.Semantic.DeepEqual(before, &addon.Status) {
		if err := r.Status().Update(ctx, addon); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: addonRequeueDelay}, nil
}

// setAddonCondition writes the Addon's one condition.
func setAddonCondition(addon *kitchenv1alpha1.Addon, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
		Type:               kitchenv1alpha1.AddonReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: addon.Generation,
	})
}

// runInstall creates the install job, or reads the outcome of the one it
// created earlier.
//
// The job is named after the versions it installs, which is what makes an
// upgrade a new job rather than a rerun of a finished one — and what stops a
// failed install from being retried in a tight loop: the failed job stays
// until its TTL reaps it, and only then is the same install attempted again.
// That hourly retry is deliberate: a dependency that failed to install
// because a registry was briefly unreachable should end up installed without
// anybody watching for the moment to try again.
func (r *AddonReconciler) runInstall(
	ctx context.Context,
	addon *kitchenv1alpha1.Addon,
	entry addonEntry,
	cfg AddonInstallConfig,
	namespace string,
) (ctrl.Result, error) {
	name := addonInstallJobName(entry, cfg)
	installing := fmt.Sprintf("installing %s into %s", addonPins(entry, cfg), namespace)

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
			return r.settle(ctx, addon, addonPlan{
				namespace: namespace,
				status:    metav1.ConditionFalse, reason: "ServiceAccountMissing",
				message: fmt.Sprintf("the install job's ServiceAccount %q is missing from %s: %s. The chart "+
					"creates it when %s is set", saKey.Name, PlatformNamespace, err, entry.ChartValue),
			})
		}
		if err := r.Create(ctx, addonInstallJob(name, namespace, entry, cfg)); err != nil &&
			!apierrors.IsAlreadyExists(err) {
			return r.settle(ctx, addon, addonPlan{
				namespace: namespace,
				status:    metav1.ConditionFalse, reason: "InstallJobNotCreated", message: err.Error(),
			})
		}
		return r.settle(ctx, addon, addonPlan{
			namespace: namespace,
			status:    metav1.ConditionFalse, reason: "Installing", message: installing,
		})
	case err != nil:
		return r.settle(ctx, addon, addonPlan{
			namespace: namespace,
			status:    metav1.ConditionFalse, reason: "InstallJobUnreadable", message: err.Error(),
		})
	}

	complete, failed, message := jobOutcome(job)
	switch {
	case complete:
		// Recorded before the API is probed again: helm --wait returned, so
		// the releases exist whether or not the CRD has reached this
		// operator's RESTMapper yet. The next reconcile reads the record and
		// the served API together and settles the condition.
		charts := make([]kitchenv1alpha1.AddonChartStatus, 0, len(entry.Charts))
		for _, chart := range entry.Charts {
			charts = append(charts, kitchenv1alpha1.AddonChartStatus{
				Name: chart.Chart, Version: cfg.version(chart),
			})
		}
		return r.settle(ctx, addon, addonPlan{
			states: true, managed: true, serving: true, namespace: namespace, charts: charts,
			status: metav1.ConditionTrue, reason: "Installed",
			message: fmt.Sprintf("the platform installed %s into %s", addonPins(entry, cfg), namespace),
			ready:   true,
		})
	case failed:
		return r.settle(ctx, addon, addonPlan{
			namespace: namespace,
			status:    metav1.ConditionFalse, reason: "InstallFailed",
			message: fmt.Sprintf("installing %s failed: %s. The job's logs are in the platform's own, under "+
				"the component %q; it is retried once the finished job is reaped",
				addonPins(entry, cfg), message, entry.Component),
		})
	default:
		return r.settle(ctx, addon, addonPlan{
			namespace: namespace,
			status:    metav1.ConditionFalse, reason: "Installing", message: installing,
		})
	}
}

// addonPins names what is being installed, at what versions, for a message.
func addonPins(entry addonEntry, cfg AddonInstallConfig) string {
	parts := make([]string, 0, len(entry.Charts))
	for _, chart := range entry.Charts {
		parts = append(parts, chart.Chart+" "+cfg.version(chart))
	}
	return strings.Join(parts, " and ")
}

// addonInstallJobName names the job after what it installs, so that a version
// bump is a different job and a finished one is never re-read as the answer
// to a question it was not asked.
func addonInstallJobName(entry addonEntry, cfg AddonInstallConfig) string {
	versions := make([]string, 0, len(entry.Charts))
	for _, chart := range entry.Charts {
		versions = append(versions, cfg.version(chart))
	}
	suffix := nonAlphanumeric.ReplaceAllString(strings.Join(versions, "-"), "-")
	name := "kitchen-" + entry.ID + "-install-" + strings.Trim(strings.ToLower(suffix), "-")
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	return name
}

// addonInstallJob builds the install.
//
// The ordering that Helm cannot express within one release is expressed here
// by containers: every chart but the last is an init container, which runs to
// completion before the next starts, so a chart shipping a custom resource of
// an earlier chart's CRD is applied against a CRD that is already
// established. Every helm run --waits, so "completed" means the workloads are
// up and not merely that the manifests were accepted.
//
// No argv takes anything from a request. The job is bound to an account that
// can apply CRDs and ClusterRoles, so an install that forwarded
// caller-supplied helm arguments would be a way to apply arbitrary objects as
// cluster-admin, and the chart value would not be the gate it looks like. The
// one value that comes from the Addon is the namespace, which is checked
// against a DNS label first and reaches helm as its own argument, never a
// shell's — there is no shell in this pod at all.
func addonInstallJob(name, namespace string, entry addonEntry, cfg AddonInstallConfig) *batchv1.Job {
	// The job says what it installed and where, because it is what a later
	// reconcile reads ownership from — see installevidence.go.
	versions := make(map[string]string, len(entry.Charts))
	for _, chart := range entry.Charts {
		versions[chart.VersionLabel] = cfg.version(chart)
	}
	labels := installLabels(map[string]string{
		labelManagedByKey:  labelManagedByValue,
		labelComponentKind: entry.Component,
	}, namespace, versions)

	env := []corev1.EnvVar{
		// helm writes its cache, config and data under $HOME by default,
		// which is not writable in the image. Everything it needs here is
		// disposable, and shared between the runs so a later one does not
		// re-fetch the repository index.
		{Name: "HELM_CACHE_HOME", Value: "/tmp/helm/cache"},
		{Name: "HELM_CONFIG_HOME", Value: "/tmp/helm/config"},
		{Name: "HELM_DATA_HOME", Value: "/tmp/helm/data"},
	}
	mounts := []corev1.VolumeMount{{Name: "helm-home", MountPath: "/tmp"}}

	container := func(chart addonChart) corev1.Container {
		// --install, so a rerun at a new version upgrades what is there
		// rather than failing; --wait, so the next release finds the last one
		// running. Nothing is --atomic: rolling a dependency back out from
		// under the resources it is reconciling is a worse state than a
		// half-finished install somebody can look at.
		return corev1.Container{
			Name:    chart.Chart,
			Image:   cfg.HelmImage,
			Command: []string{"helm"},
			Args: []string{
				"upgrade", chart.Release, chart.Chart,
				"--install",
				"--repo", cfg.Repository,
				"--version", cfg.version(chart),
				"--namespace", namespace,
				"--create-namespace",
				"--wait",
				"--timeout", cfg.Timeout.String(),
			},
			Env:          env,
			VolumeMounts: mounts,
		}
	}

	last := len(entry.Charts) - 1
	initContainers := make([]corev1.Container, 0, last)
	for _, chart := range entry.Charts[:last] {
		initContainers = append(initContainers, container(chart))
	}

	total := time.Duration(len(entry.Charts)) * cfg.Timeout
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PlatformNamespace, Labels: labels},
		Spec: batchv1.JobSpec{
			// Never retried in place. A helm run that failed half-way is
			// re-entered by the next job of the same name, once the TTL has
			// reaped this one and the reconciler has read what it said.
			BackoffLimit:            ptr.To(int32(0)),
			ActiveDeadlineSeconds:   ptr.To(int64((total + addonInstallDeadlineBuffer).Seconds())),
			TTLSecondsAfterFinished: ptr.To(int32(addonInstallJobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cfg.ServiceAccount,
					InitContainers:     initContainers,
					Containers:         []corev1.Container{container(entry.Charts[last])},
					Volumes: []corev1.Volume{{
						Name:         "helm-home",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					}},
				},
			},
		},
	}
}

// SetupWithManager registers the reconciler.
func (r *AddonReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Addon{}).
		Owns(&batchv1.Job{}).
		Named("addon").
		Complete(r)
}

// reader is what probes read through: the API server directly where the
// manager gave us an APIReader, and the client otherwise.
func (r *AddonReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// AddonFlags collects the per-entry settings the chart passes as repeatable
// manager flags, and builds the AddonInstalls the reconcilers read.
//
// One flag per entry would mean three more flags per dependency added, which
// is how the two installers this replaced came to have eleven between them.
// These take an entry ID instead, so a new catalogue entry adds a file and no
// flags at all — and an entry the operator has never heard of is refused at
// startup rather than silently ignored, because a chart that granted an
// account for an entry this binary cannot install has said something worth
// answering.
type AddonFlags struct {
	// ServiceAccounts is `<entry>=<name>`: the grant, one entry at a time.
	ServiceAccounts EntryValues
	// Repositories is `<entry>=<url>`, for a cluster that reaches a mirror
	// and not the upstream index.
	Repositories EntryValues
	// Versions is `<entry>/<chart>=<version>`, which is what makes a pin
	// overridable per chart for an entry that installs several.
	Versions EntryValues
	// Images is `<entry>=<image>`: what that entry's install job runs helm
	// from.
	Images EntryValues
	// Timeouts is `<entry>=<duration>`: what each of that entry's helm runs
	// is given. Empty takes the catalogue entry's own, which is what the
	// chart's default renders.
	Timeouts EntryValues
}

// Installs is what the reconcilers take. An entry named by a flag that the
// catalogue does not have is an error rather than a silent no-op.
func (f AddonFlags) Installs() (AddonInstalls, error) {
	installs := AddonInstalls{}
	set := func(id string, apply func(cfg *AddonInstallConfig)) error {
		if _, known := lookupAddon(id); !known {
			return fmt.Errorf("no addon catalogue entry named %q; this operator installs %s",
				id, strings.Join(addonIDs(), ", "))
		}
		cfg := installs[id]
		apply(&cfg)
		installs[id] = cfg
		return nil
	}

	for id, account := range f.ServiceAccounts {
		if err := set(id, func(cfg *AddonInstallConfig) { cfg.ServiceAccount = account }); err != nil {
			return nil, err
		}
	}
	for id, repository := range f.Repositories {
		if err := set(id, func(cfg *AddonInstallConfig) { cfg.Repository = repository }); err != nil {
			return nil, err
		}
	}
	for key, version := range f.Versions {
		id, chart, ok := strings.Cut(key, "/")
		if !ok {
			return nil, fmt.Errorf("addon chart version %q names no chart: it is <entry>/<chart>=<version>", key)
		}
		if err := set(id, func(cfg *AddonInstallConfig) {
			if cfg.Versions == nil {
				cfg.Versions = map[string]string{}
			}
			cfg.Versions[chart] = version
		}); err != nil {
			return nil, err
		}
	}

	for id, image := range f.Images {
		if err := set(id, func(cfg *AddonInstallConfig) { cfg.HelmImage = image }); err != nil {
			return nil, err
		}
	}
	for id, raw := range f.Timeouts {
		timeout, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("addon install timeout for %q: %w", id, err)
		}
		if err := set(id, func(cfg *AddonInstallConfig) { cfg.Timeout = timeout }); err != nil {
			return nil, err
		}
	}
	return installs, nil
}

// EntryValues is a repeatable `key=value` flag.
type EntryValues map[string]string

// String renders the flag's value, in key order so it reads the same twice.
func (v EntryValues) String() string {
	keys := make([]string, 0, len(v))
	for key := range v {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+v[key])
	}
	return strings.Join(pairs, ",")
}

// Set takes one `key=value`.
func (v EntryValues) Set(raw string) error {
	key, value, ok := strings.Cut(raw, "=")
	if !ok || key == "" || value == "" {
		return fmt.Errorf("expected key=value, got %q", raw)
	}
	v[key] = value
	return nil
}

// Permitted is every entry this installation granted an account for, in name
// order — what the manager logs at startup, so the grant is visible in the
// first ten lines rather than only in the chart.
func (a AddonInstalls) Permitted() []string {
	permitted := make([]string, 0, len(a))
	for id, cfg := range a {
		if cfg.ServiceAccount != "" {
			permitted = append(permitted, id)
		}
	}
	sort.Strings(permitted)
	return permitted
}

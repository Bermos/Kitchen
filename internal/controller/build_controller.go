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
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/framework"
	"github.com/Bermos/Kitchen/internal/gitprovider"
	"github.com/Bermos/Kitchen/internal/provider"
	"github.com/Bermos/Kitchen/internal/repoconfig"
)

const (
	labelBuild   = "kitchen.bermos.dev/build"
	labelBuildNS = "kitchen.bermos.dev/build-namespace"

	// BuildkitImage runs the in-cluster builds.
	BuildkitImage = "moby/buildkit:v0.23.2-rootless"

	// SBOMGeneratorImage is the scanner BuildKit runs over a finished image
	// to produce its bill of materials.
	//
	// It is pinned to a version tag for the same reason the builder above
	// is, and the reason bites harder here: the tag BuildKit reaches for by
	// default is `stable-1`, a floating tag on an image nobody in this
	// repository owns. Evidence about an artifact should not change because
	// somebody else's tag moved overnight — and a scanner that changed under
	// an installation would produce a differently-shaped bill of materials
	// for the same image, which reads as the image having changed.
	//
	// It is also pulled on **every** build that asks for an SBOM: the build
	// pod is ephemeral, so nothing caches it between builds. That is the
	// cost the Kitchen object's `compliance.attestation.build.sbom` switch
	// exists to let an installation decline.
	SBOMGeneratorImage = "docker/buildkit-syft-scanner:1.12.0"

	// BuilderID identifies the build platform in the provenance it produces.
	//
	// SLSA's point in asking for it is that a verifier decides how much a
	// provenance statement is worth by who produced it, so the identifier has
	// to name the platform rather than the run. It carries no version: the
	// platform's version is in Kitchen's own build record, attached to the
	// same artifact, and a builder id that moved every release would make
	// every policy that pinned it fail on upgrade.
	BuilderID = "https://kitchen.bermos.dev/builder/buildkit"

	// buildJobTTLSeconds is how long a finished build Job (and its pod)
	// sticks around. It is a collection window, not a retention policy:
	// the collector tails the pod's log file, so the pod has to outlive
	// the last flush. An hour is far past the seconds it actually takes
	// and still keeps finished jobs from piling up in the namespace.
	buildJobTTLSeconds = 3600

	// buildDeadlineSeconds is the longest a build Job may be active before
	// the job-controller ends it. It is generous — an hour is far past any
	// build this platform is meant to run — because the point of it is that
	// a build has an end at all, not that it is a time budget.
	buildDeadlineSeconds = 3600

	// DefaultBuildConcurrency is how many builds run at once when the Kitchen
	// object names no limit. It is exported because the API reports the queue
	// against it, and a status bar reading "1 of 0" would be its own bug.
	DefaultBuildConcurrency = 2

	// dockerConfigDir is where a build pod finds the registry credentials
	// it pushes with, and the value of DOCKER_CONFIG in every builder.
	dockerConfigDir = "/kitchen/.docker"

	// gitCredentialDir is where a build pod finds the token it clones the
	// repository with, and gitCredentialFile the one file in it. Neither is
	// ever the token itself: the value stays in a mounted Secret, so it
	// reaches no pod spec, no argv and no clone URL.
	gitCredentialDir  = "/kitchen/.git-credentials"
	gitCredentialFile = gitCredentialDir + "/token"

	// volumeGitCredential is that mount's volume, named in both pod shapes.
	volumeGitCredential = "git-credential"

	// terminationLogPath is where a builder leaves what the reconciler needs
	// back from it — the digest of the image it pushed. Kubernetes surfaces
	// the file as the container's termination message, so nothing has to be
	// read out of the pod's log.
	terminationLogPath = "/dev/termination-log"

	// buildkitMetadataPath is where BuildKit writes that digest first, before
	// anything copies it to the termination log.
	//
	// It cannot write it there directly. `--metadata-file` is written
	// atomically — a temporary file beside the destination, then a rename —
	// and the destination's directory is /dev: a runtime-created tmpfs owned
	// by root and mode 0755 in every cluster. The file itself is
	// world-writable, which is why the CNB lifecycle's in-place `-report`
	// write works, but a builder running as UID 1000 cannot create a
	// neighbour for it. The result was a build that pushed its image, its
	// manifest and its cache and then died on the last line, exactly as
	// buildkitCacheArgs describes for the cache export one step earlier.
	//
	// /tmp is the builder's own: buildctl-daemonless.sh already mkdirs its
	// daemon state there. It has to be a path kubelet has not pre-created,
	// which rules out pointing terminationMessagePath at it — kubelet makes
	// that file root-owned, and the rename into sticky /tmp is refused.
	buildkitMetadataPath = "/tmp/kitchen-build-metadata.json"

	// reasonFrameworkNotDetected is a repository the platform read and did
	// not recognise, with no Dockerfile to fall back to. It is the failure
	// "auto" exists to be able to report: the alternative is a builder's own
	// error about a file the repository never had.
	reasonFrameworkNotDetected = "FrameworkNotDetected"

	// reasonRepositoryUnreadable is the repository itself not being readable:
	// it is not there, or the connection's credential cannot see it. It is
	// its own reason because it is not a repository the platform read and did
	// not recognise — nothing was read — and the two are otherwise the same
	// 404 one layer apart, which is how it came to be reported as a root
	// directory that is not there.
	reasonRepositoryUnreadable = "RepositoryUnreadable"

	// reasonSourceUnreadable is the platform not being able to look at the
	// repository at all. It keeps the Build queued rather than failing it —
	// nothing about the commit caused it.
	reasonSourceUnreadable = "SourceUnreadable"

	// reasonConfigInvalid is the commit's own kitchen.json being wrong: bad
	// JSON, a key nothing recognises, or a declaration the file is not
	// allowed to make. It fails the build rather than being built around,
	// because the file exists so that committing a change to it changes the
	// deploy — a setting quietly ignored would make that untrue in exactly
	// the case where somebody is watching for it.
	reasonConfigInvalid = "ConfigInvalid"

	// reasonBuildFailed marks the one failure that is the repository's own:
	// the build ran and the image did not come out. Every other reason is
	// the platform failing to run it at all, which reports differently on
	// the commit.
	reasonBuildFailed = "BuildFailed"

	// reasonSourceUnreviewed is a build refused because the commit cannot be
	// shown to have arrived through a reviewed pull request. It is a distinct
	// reason from a build that failed, because nothing was built: the change
	// was not run, and somebody has to be able to tell that from a broken
	// compile without reading the message.
	reasonSourceUnreviewed = "SourceUnreviewed"
)

// BuildReconciler reconciles a Build: it runs a BuildKit Job for the commit,
// records the pushed image digest, creates the resulting Release, and
// auto-promotes it to the production Environment when the branch matches.
type BuildReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Activity feeds the dashboard's recent-activity feed. May be nil.
	Activity *activity.Recorder
	// Audit appends this reconciler's state transitions to the tamper-evident
	// log. Unlike Activity it is waited on: a transition it refuses is a
	// transition this reconciler does not make. May be nil.
	Audit *audit.Recorder
	// GitProviders resolves a Provider for a Connection, for reporting the
	// build's outcome back onto its commit. Defaults to gitprovider.Default;
	// tests inject fakes.
	GitProviders gitprovider.Factory
	// QualityGateImage is the image the gate publisher runs. It is the
	// operator's own image — the publisher is another binary in it — and the
	// chart passes it in, because a pod cannot read its own image back.
	// Without it, gates are configured and never run.
	QualityGateImage string

	// Attesters resolves how signed evidence reaches the registry a build
	// pushed to. Nil talks to the real registry with the build's own
	// credential; tests point it at an in-process one.
	Attesters AttesterFactory
	// CacheProbes resolves how the reconciler asks a registry whether a
	// layer cache is there. Nil asks the real one; tests answer without a
	// registry at all.
	CacheProbes CacheProbeFactory

	// PodLogs reads the tail of a failing build container's output, which is
	// what turns "the job failed" into a diagnosis. SetupWithManager fills it
	// in from the manager's own configuration; tests answer without a
	// kubelet. Nil records the failure without its log rather than failing.
	PodLogs PodLogReader

	// APIReader reads straight from the API server, bypassing the cache.
	// Events are what it is for: the field selectors that find the warnings
	// on a build Job are not served by the cache, and a Job whose pods were
	// refused before they existed says nothing anywhere else.
	// SetupWithManager fills it in; nil reports the stall without its reason
	// rather than failing.
	APIReader client.Reader
}

// buildTarget is where a build pushed and how: the registry, the credential
// it authenticated with, and the strategy it was built under. It travels
// together because everything downstream of a finished build — the digest, the
// evidence attached to it, the pull secret the deployment needs — is a
// question about the same registry.
type buildTarget struct {
	// Connection is the dockerRegistry Connection the project pushes
	// through, in the platform namespace.
	Connection *kitchenv1alpha1.Connection
	// Registry is that connection resolved: prefix, server and base URL.
	Registry provider.RegistryTarget
	// Strategy the build was actually run under, after detection.
	Strategy kitchenv1alpha1.BuildStrategy
	// Tag is the reference the builder was told to push, before the digest
	// is known.
	Tag string
	// Namespace is the application namespace the Job ran in.
	Namespace string
}

// correlationFor ties every record a commit produces together: the build's own
// transitions, the Release it leaves behind, and the Environment that takes
// it. The commit is the cause, so the commit is the correlation.
func correlationFor(build *kitchenv1alpha1.Build) string {
	return build.Spec.Git.SHA
}

// git reports the build's progress back to the repository the commit came
// from. Posting is best effort: it never fails a build.
func (r *BuildReconciler) git() gitReporting {
	return gitReporting{Client: r.Client, Factory: r.GitProviders}
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=builds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=builds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=builds/finalizers,verbs=update
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=projects;connections;kitchens,verbs=get;list;watch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=releases;environments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=promotions,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=exceptions,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

// Reconcile drives a Build from Queued through a BuildKit Job to a Release.
func (r *BuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	build := &kitchenv1alpha1.Build{}
	if err := r.Get(ctx, req.NamespacedName, build); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !build.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if isTerminal(build.Status.Phase) {
		// The build is over and nothing below can change it. Two things still
		// happen to a finished build. One is a pull request the platform was
		// told about after this build ended, which is a preview environment
		// owed to a release that already exists. The other is the evidence
		// that accretes onto the artifact afterwards: the quality gates, which
		// run over something that already exists and hold nothing up by taking
		// their time.
		if err := r.adoptLatePreview(ctx, build); err != nil {
			return ctrl.Result{}, err
		}
		return r.reconcileGates(ctx, build)
	}

	project := &kitchenv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: build.Namespace, Name: build.Spec.ProjectRef.Name}, project); err != nil {
		return r.pending(ctx, build, "ProjectMissing", err)
	}

	builds := r.platformBuilds(ctx)
	strategy := resolveStrategy(project, build, builds.DefaultStrategy)
	switch strategy {
	case kitchenv1alpha1.BuildStrategyDockerfile, kitchenv1alpha1.BuildStrategyBuildpacks,
		kitchenv1alpha1.BuildStrategyAuto:
	default:
		return r.fail(ctx, build, project, "StrategyUnsupported",
			fmt.Sprintf("build strategy %q is not supported yet", strategy))
	}

	registryConn := &kitchenv1alpha1.Connection{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: build.Namespace, Name: project.Spec.Registry.ConnectionRef.Name}, registryConn); err != nil {
		return r.pending(ctx, build, "RegistryConnectionMissing", err)
	}
	registry, err := provider.Registry(registryConn)
	if err != nil {
		return r.pending(ctx, build, "RegistryConfigInvalid", err)
	}

	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}
	credsSecret, err := r.syncRegistrySecret(ctx, registryConn, build.Namespace, appNS)
	if err != nil {
		return r.pending(ctx, build, "RegistryCredentialsMissing", err)
	}
	// A repository nobody can read anonymously needs the source Connection's
	// token to clone, and one anybody can read needs nothing. Which of the
	// two this is cannot be known from here — GitHub answers a private
	// repository and a repository that does not exist identically — so a
	// credential that cannot be resolved parks nothing: the build runs
	// anonymously and says, if it fails, what it did not have.
	gitCreds := r.resolveGitCredential(ctx, project, build.Namespace, appNS)

	tagRef := fmt.Sprintf("%s/%s:%s", registry.Prefix, project.Name, shortSHA(build.Spec.Git.SHA))
	target := buildTarget{
		Connection: registryConn,
		Registry:   registry,
		Strategy:   strategy,
		Tag:        tagRef,
		Namespace:  appNS,
	}

	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: build.Name}, job)
	switch {
	case apierrors.IsNotFound(err):
		// How the commit reached the branch is established here, before the
		// Job exists — that is what "refuse before the build is scheduled"
		// means, and it is where the compute is. The Build stays and says why
		// it was refused: refusing without a record would be the platform
		// quietly dropping changes.
		if build.Status.Source == nil {
			source, refusal := r.resolveSourceProvenance(ctx, build, project)
			build.Status.Source = source
			if refusal != nil {
				return r.fail(ctx, build, project, reasonSourceUnreviewed, refusal.Error())
			}
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}
		}
		if waiting, res := r.gateConcurrency(ctx, build, builds.Concurrency); waiting {
			return res, nil
		}
		// What the commit itself asked for, read once here and recorded
		// before anything is created: the build Job is planned from it, and
		// the Release this build writes is merged from it on a later
		// reconcile, which cannot read the repository again without spending
		// a second request on a question that has an answer.
		if stop, err := r.readConfig(ctx, build, project); stop != nil {
			return *stop, err
		}
		strategy = resolveStrategy(project, build, builds.DefaultStrategy)
		target.Strategy = strategy
		detected, stop, err := r.decide(ctx, build, project, strategy)
		if stop != nil {
			return *stop, err
		}
		if detected.Strategy != "" {
			strategy = detected.Strategy
			target.Strategy = strategy
		}
		// Planned before the Job exists, because the plan is part of the pod
		// spec and a Job's template cannot be edited afterwards.
		cache := r.planCache(ctx, build, project, builds.Cache, target)
		if err := r.createJob(ctx, build, project, strategy, detected, cache, appNS, credsSecret, gitCreds.Secret, tagRef); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("build job created",
			"namespace", appNS, "job", build.Name, "image", tagRef,
			"strategy", strategy, "framework", detected.Name,
			"cache", cache.Ref, "cacheWarm", cache.Warm)
		if err := r.Audit.Record(ctx, audit.Transition{
			Object:      build,
			Kind:        audit.KindBuild,
			Controller:  actorBuildController,
			Correlation: correlationFor(build),
			From:        string(build.Status.Phase),
			To:          string(kitchenv1alpha1.BuildRunning),
			Project:     project.Name,
			Reason:      "the build job was created",
			Details: map[string]any{
				"commit":    build.Spec.Git.SHA,
				"branch":    build.Spec.Git.Branch,
				"strategy":  string(strategy),
				"framework": detected.Name,
				"image":     tagRef,
				"cacheWarm": cache.Warm,
			},
		}); err != nil {
			return ctrl.Result{}, err
		}
		build.Status.Phase = kitchenv1alpha1.BuildRunning
		build.Status.DetectedFramework = detected.Name
		build.Status.Cache = cache
		build.Status.StartedAt = ptr.To(metav1.Now())
		meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
			Type: condReady, Status: metav1.ConditionFalse, Reason: "BuildRunning",
			Message: "build job is running", ObservedGeneration: build.Generation,
		})
		// The commit gets its pending check here and its verdict when the
		// job finishes: both phases are entered once, since a terminal
		// Build returns above and a running one never comes back here.
		r.git().reportBuild(ctx, project, build, gitprovider.CommitPending, "the build is running")
		return ctrl.Result{}, r.Status().Update(ctx, build)
	case err != nil:
		return ctrl.Result{}, err
	}

	complete, failed, message := jobOutcome(job)
	switch {
	case complete:
		return r.succeed(ctx, build, project, job, target)
	case failed:
		// The Job's own message names no container and no exit code, so the
		// pod is asked before it is collected: what failed, how, and the last
		// thing it printed. It is written onto the Build because the pod is
		// deleted with the job and the Build is not.
		build.Status.Failure = r.diagnoseJobFailure(ctx, job, message)
		return r.fail(ctx, build, project, reasonBuildFailed,
			gitCreds.explain(failureMessage(build.Status.Failure, message)))
	default:
		return r.observeRunning(ctx, build, project, job)
	}
}

// platformBuilds is the Kitchen singleton's build configuration, or its zero
// value when the singleton cannot be read. Both fields have a defined meaning
// when unset, so a build never waits on the platform object to answer.
func (r *BuildReconciler) platformBuilds(ctx context.Context) kitchenv1alpha1.BuildsSpec {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return kitchenv1alpha1.BuildsSpec{}
	}
	return kitchen.Spec.Builds
}

// platformAttestation is what the builder is asked to attest, resolved against
// the platform singleton.
//
// A singleton that cannot be read answers "nothing", rather than the defaults
// the CRD would have applied. That is deliberate and it is the conservative
// direction: asking for attestations the installation may have turned off
// costs every build a scanner pull and changes the shape of what is pushed,
// where not asking costs a build nothing it can not get back by reconciling
// again once the object is readable.
func (r *BuildReconciler) platformAttestation(ctx context.Context) kitchenv1alpha1.BuildAttestationSpec {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return kitchenv1alpha1.BuildAttestationSpec{}
	}
	if !kitchen.Spec.Compliance.Attestation.Enabled {
		// Nothing will be signed, so nothing is worth the build time: an
		// unsigned statement sitting in the registry is not evidence, it is
		// a claim anything with push access could have written.
		return kitchenv1alpha1.BuildAttestationSpec{}
	}
	return kitchen.Spec.Compliance.Attestation.Build
}

// sbomGenerator is the scanner image to run, defaulting to the pinned one.
func sbomGenerator(attest kitchenv1alpha1.BuildAttestationSpec) string {
	if attest.SBOMGenerator != "" {
		return attest.SBOMGenerator
	}
	return SBOMGeneratorImage
}

// resolveStrategy is how a build gets made, as far as configuration decides
// it. The commit's own kitchen.json is asked first, then the Project; one
// left on "auto" takes the platform's default, which is where an operator
// says what an unconfigured project should do.
//
// The file comes first because it is the only one of the three that travels
// with the code: a commit that added a Dockerfile said so in the same change.
//
// "auto" all the way down stays "auto", and is answered by reading the
// repository — see decide. An operator who wants one strategy for everything
// says so in Kitchen.spec.builds.defaultStrategy, and detection never runs.
func resolveStrategy(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	platformDefault kitchenv1alpha1.BuildStrategy,
) kitchenv1alpha1.BuildStrategy {
	strategy := repoconfig.Strategy(build.Status.Config, project.Spec.Build.Strategy)
	if strategy == "" || strategy == kitchenv1alpha1.BuildStrategyAuto {
		strategy = platformDefault
	}
	if strategy == "" {
		return kitchenv1alpha1.BuildStrategyAuto
	}
	return strategy
}

// decide turns the configured strategy into a framework, by reading the
// repository where configuration left the question open.
//
// The two strategies ask for different things from detection. On "auto" it is
// the decision itself, and a repository that cannot be read or cannot be
// recognised has to stop the build — reporting that is the whole point of the
// feature. On an explicit "buildpacks" the project has already decided, and
// detection only adds what the lifecycle is told about a static site: it is
// best effort there, and a failure to detect is logged and built through.
//
// A non-nil second return is the Build having been parked or failed: the
// caller returns that result and error rather than building anything.
func (r *BuildReconciler) decide(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	strategy kitchenv1alpha1.BuildStrategy,
) (framework.Framework, *ctrl.Result, error) {
	if strategy == kitchenv1alpha1.BuildStrategyDockerfile {
		// Nothing to learn: the repository's Dockerfile is the instructions.
		return framework.Framework{}, nil, nil
	}

	detected, err := r.detectFramework(ctx, project, build, strategy)
	if err == nil {
		return detected, nil, nil
	}

	if strategy == kitchenv1alpha1.BuildStrategyBuildpacks {
		logf.FromContext(ctx).Info("building with buildpacks without a detected framework",
			"project", project.Name, "build", build.Name, "cause", err.Error())
		return framework.Framework{}, nil, nil
	}

	if errors.Is(err, errSourceUnreadable) {
		// The platform cannot look right now. The Build waits rather than
		// failing a commit for the provider being unreachable.
		res, updateErr := r.pending(ctx, build, reasonSourceUnreadable, err)
		return framework.Framework{}, &res, updateErr
	}

	if errors.Is(err, errRepositoryUnreadable) {
		// The repository is not there, or this connection may not see it.
		// That is the project's configuration rather than the provider
		// having a bad minute, so the Build fails saying so instead of
		// queueing for a repository that is not going to appear — and it
		// fails saying that, rather than describing a root directory.
		res, updateErr := r.fail(ctx, build, project, reasonRepositoryUnreadable, err.Error())
		return framework.Framework{}, &res, updateErr
	}
	res, updateErr := r.fail(ctx, build, project, reasonFrameworkNotDetected, err.Error())
	return framework.Framework{}, &res, updateErr
}

// gateConcurrency keeps the Build queued while the platform-wide concurrency
// limit is reached.
func (r *BuildReconciler) gateConcurrency(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	concurrency int32,
) (bool, ctrl.Result) {
	limit := int32(DefaultBuildConcurrency)
	if concurrency > 0 {
		limit = concurrency
	}

	builds := &kitchenv1alpha1.BuildList{}
	if err := r.List(ctx, builds, client.InNamespace(build.Namespace)); err != nil {
		return false, ctrl.Result{}
	}
	running := int32(0)
	for _, b := range builds.Items {
		if b.Name != build.Name && b.Status.Phase == kitchenv1alpha1.BuildRunning {
			running++
		}
	}
	if running < limit {
		return false, ctrl.Result{}
	}

	if build.Status.Phase != kitchenv1alpha1.BuildQueued {
		build.Status.Phase = kitchenv1alpha1.BuildQueued
		meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
			Type: condReady, Status: metav1.ConditionFalse, Reason: "Queued",
			Message: "waiting for a free build slot", ObservedGeneration: build.Generation,
		})
		if err := r.Status().Update(ctx, build); err != nil {
			return true, ctrl.Result{}
		}
	}
	return true, ctrl.Result{RequeueAfter: 15 * time.Second}
}

func (r *BuildReconciler) createJob(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	strategy kitchenv1alpha1.BuildStrategy,
	detected framework.Framework,
	cache *kitchenv1alpha1.BuildCacheStatus,
	appNS, credsSecret, gitSecret, tagRef string,
) error {
	template := dockerfilePod(project, build, cache, credsSecret, gitSecret, tagRef, r.platformAttestation(ctx))
	if strategy == kitchenv1alpha1.BuildStrategyBuildpacks {
		template = buildpacksPod(project, build, detected, cache, credsSecret, gitSecret, tagRef)
	}

	labels := map[string]string{
		labelProject:      project.Name,
		labelBuild:        build.Name,
		labelBuildNS:      build.Namespace,
		labelManagedByKey: labelManagedByValue,
	}
	// The pod carries the same labels as the Job: they are what the log
	// collector reads to file the build's output under its build.
	template.Labels = labels

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: build.Name, Namespace: appNS, Labels: labels},
		Spec: batchv1.JobSpec{
			// A rebuild is a new Build object, never a pod retry.
			BackoffLimit: ptr.To(int32(0)),
			// Keep the finished pod around long enough for the log
			// collector to catch up with its output — the build log only
			// exists on disk while the pod does — then let the cluster
			// reclaim it. The logs themselves live on in ClickHouse.
			TTLSecondsAfterFinished: ptr.To(int32(buildJobTTLSeconds)),
			// A build that is still going an hour later is not going to
			// finish, and BackoffLimit 0 gives a Job no other end: nothing
			// retries, so nothing ever reaches a limit. The deadline is the
			// job-controller's own, which means a build that hangs where the
			// reconciler cannot see it — a pod the scheduler never places, a
			// builder waiting on a registry that never answers — still ends
			// in a Failed condition the Build can read.
			ActiveDeadlineSeconds: ptr.To(int64(buildDeadlineSeconds)),
			Template:              template,
		},
	}
	return r.Create(ctx, job)
}

// dockerfilePod is a build that runs the repository's own Dockerfile through
// BuildKit, which fetches the commit itself: the git context is the build's
// only input, and the image comes out the far end pushed.
func dockerfilePod(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	cache *kitchenv1alpha1.BuildCacheStatus,
	credsSecret, gitSecret, tagRef string,
	attest kitchenv1alpha1.BuildAttestationSpec,
) corev1.PodTemplateSpec {
	buildContext := repoCloneURL(project) + "#" + build.Spec.Git.SHA
	if root := buildRootDir(project); root != "" {
		buildContext += ":" + root
	}
	dockerfile := buildDockerfilePath(project, build)
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}

	output := "type=image,name=" + tagRef + ",push=true"
	attestations := []string{}
	if attest.Provenance {
		// mode=max records the base images it resolved and the parameters it
		// ran with, not just that it ran; version=v1 is SLSA 1.0, which
		// BuildKit can emit but does not default to. builder-id is the one
		// field BuildKit cannot fill in for itself — left unset it writes an
		// empty string, and provenance that does not say who produced it
		// answers the first question a verifier asks with nothing.
		attestations = append(attestations,
			"attest:provenance=mode=max,version=v1,builder-id="+BuilderID)
	}
	if attest.SBOM {
		attestations = append(attestations, "attest:sbom=generator="+sbomGenerator(attest))
	}

	args := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--opt", "context=" + buildContext,
		"--opt", "filename=" + dockerfile,
	}
	for _, attestation := range attestations {
		args = append(args, "--opt", attestation)
	}
	if len(attestations) > 0 {
		// Attestations are pushed as extra manifests beside the image, under
		// an index — which the OCI media types describe and Docker's older
		// ones do not. It is set only when something is being attested, so a
		// build with the feature off pushes exactly what it pushed before.
		output += ",oci-mediatypes=true"
	}
	args = append(args,
		"--output", output,
		// BuildKit writes its result metadata (including the image digest)
		// to a scratch path it owns; buildkitEntrypoint copies it to the
		// termination log, which is where the reconciler reads it from the
		// pod status. See buildkitMetadataPath for why it cannot be written
		// there in the first place.
		"--metadata-file", buildkitMetadataPath,
		// One log line per step, no cursor tricks: the collector ships
		// these lines into ClickHouse, and the interactive renderer would
		// arrive there as a wall of escape codes.
		"--progress", "plain",
	)
	args = append(args, buildkitCacheArgs(cache)...)
	if gitSecret != "" {
		// BuildKit resolves the git context itself, and GIT_AUTH_TOKEN is
		// the secret it looks for when the remote asks for authentication.
		// Only the path is an argument: the token is read from the mounted
		// file inside the pod, so it appears in no pod spec and no argv.
		args = append(args, "--secret", "id=GIT_AUTH_TOKEN,src="+gitCredentialFile)
	}

	mounts := []corev1.VolumeMount{dockerConfigMount()}
	volumes := []corev1.Volume{dockerConfigVolume(credsSecret)}
	if gitSecret != "" {
		mounts = append(mounts, gitCredentialMount())
		volumes = append(volumes, gitCredentialVolume(gitSecret))
	}

	// The two relaxations below are what rootless BuildKit costs, and they
	// are not optional: buildkitd runs unprivileged here (see
	// --oci-worker-no-process-sandbox above), which means it has to create a
	// nested user namespace and mount its own overlayfs inside it. The
	// runtime's default seccomp profile blocks the first and the default
	// AppArmor profile blocks the second, so a builder admitted under either
	// fails at startup rather than building anything.
	//
	// Pod Security admits both at the privileged level alone, which is why
	// the namespace this lands in is labelled explicitly — see
	// appNamespaceLabels. On a baseline application namespace the Job is
	// created, no pod ever is, and the build never starts.
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"container.apparmor.security.beta.kubernetes.io/buildkit": "unconfined",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "buildkit",
				Image: BuildkitImage,
				// $0 names the shell's own argv[0] for any diagnostic it
				// prints; the build's arguments follow it as "$@".
				Command: []string{
					"sh", "-c",
					buildkitEntrypoint(buildkitMetadataPath, terminationLogPath),
					"buildctl-daemonless.sh",
				},
				Args: args,
				Env: []corev1.EnvVar{
					{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
					{Name: "DOCKER_CONFIG", Value: dockerConfigDir},
				},
				VolumeMounts: mounts,
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:      ptr.To(int64(1000)),
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
				},
			}},
			Volumes: volumes,
		},
	}
}

// buildkitEntrypoint is the program the build container runs: the build
// itself, and then the copy of its metadata into the termination log — which
// needs no rename, because that file is already there and world-writable.
//
// It is a shell line rather than a second container because a pod has no
// "after" container: an init container runs before the build, a sidecar
// alongside it. Nothing from the request reaches the script — buildctl's
// arguments arrive as positional parameters and are forwarded as "$@", so no
// value is ever interpolated into shell source. That is the property
// keda.go gets by refusing a shell outright, which it has to because that job
// is bound to cluster-admin; a build pod is not.
//
// The copy is conditional on the build having succeeded. A failed build has
// no digest to report, and its termination message is what the failure is
// described with — see terminatedMessage.
func buildkitEntrypoint(metadataPath, terminationPath string) string {
	return `buildctl-daemonless.sh "$@"
code=$?
if [ "$code" -eq 0 ]; then
	cat ` + metadataPath + ` > ` + terminationPath + `
fi
exit "$code"
`
}

// repoCloneURL is where a build fetches the project's repository from.
//
// It carries no credential, deliberately: what a private repository needs is
// mounted into the pod instead — see resolveGitCredential — so the URL is the
// same one anybody would type, in the pod spec and in `git remote -v` alike.
//
// TODO: derive the host from the project's git Connection once git providers
// beyond GitHub are wired up.
func repoCloneURL(project *kitchenv1alpha1.Project) string {
	return fmt.Sprintf("https://github.com/%s.git", project.Spec.Source.Repo)
}

// buildRootDir is the directory within the repository the build builds, empty
// when that is the repository itself.
func buildRootDir(project *kitchenv1alpha1.Project) string {
	root := project.Spec.Build.RootDirectory
	if root == "." {
		return ""
	}
	return strings.Trim(root, "/")
}

// dockerConfigVolume mounts the registry credentials the build pushes with.
// Both strategies read them the same way: BuildKit and the CNB lifecycle
// alike take a docker config directory from DOCKER_CONFIG.
func dockerConfigVolume(credsSecret string) corev1.Volume {
	return corev1.Volume{
		Name: "docker-config",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: credsSecret,
			Items: []corev1.KeyToPath{{
				Key:  corev1.DockerConfigJsonKey,
				Path: "config.json",
			}},
		}},
	}
}

func dockerConfigMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: "docker-config", MountPath: dockerConfigDir, ReadOnly: true}
}

// gitCredentialVolume mounts the token a build clones a private repository
// with. One key, projected to one file, because that is all either strategy
// reads: BuildKit takes the path as a build secret, and the clone container's
// askpass reads it.
func gitCredentialVolume(gitSecret string) corev1.Volume {
	return corev1.Volume{
		Name: volumeGitCredential,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: gitSecret,
			Items:      []corev1.KeyToPath{{Key: gitCredentialsTokenKey, Path: "token"}},
		}},
	}
}

func gitCredentialMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: volumeGitCredential, MountPath: gitCredentialDir, ReadOnly: true}
}

// succeed records the digest, creates the Release, and promotes it when the
// build is for the production branch.
func (r *BuildReconciler) succeed(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	job *batchv1.Job,
	target buildTarget,
) (ctrl.Result, error) {
	image, digestFound := r.imageWithDigest(ctx, target.Namespace, build.Name, target.Tag)

	// The Job's times are stamped before anything is attested or recorded, so
	// that the evidence carries the build's own start and finish rather than
	// "roughly now".
	if job.Status.CompletionTime != nil {
		build.Status.CompletedAt = job.Status.CompletionTime
	} else {
		build.Status.CompletedAt = ptr.To(metav1.Now())
	}

	// Attesting comes before the Release exists, because it is what decides
	// which digest the artifact *is*. A builder asked for provenance or an
	// SBOM pushes an index and reports that; the evidence inside it is about
	// the image manifest, so the image manifest is the artifact, and a
	// Release created from the reported digest would deploy something no
	// evidence describes.
	build.Status.Artifact = r.attestBuild(ctx, build, project, target, image)
	if artifact := build.Status.Artifact; artifact != nil && artifact.Repository != "" && artifact.Digest != "" {
		image = attestation.ArtifactRef(artifact.Repository, artifact.Digest)
	}

	if err := r.Audit.Record(ctx, audit.Transition{
		Object:      build,
		Kind:        audit.KindBuild,
		Controller:  actorBuildController,
		Correlation: correlationFor(build),
		From:        string(build.Status.Phase),
		To:          string(kitchenv1alpha1.BuildSucceeded),
		Project:     project.Name,
		Reason:      "the build job completed and pushed an image",
		Details: map[string]any{
			"commit":      build.Spec.Git.SHA,
			"image":       image,
			"digestKnown": digestFound,
		},
	}); err != nil {
		return ctrl.Result{}, err
	}

	// The project's settings, with the commit's own kitchen.json applied over
	// them. It is refused rather than merged where the two cannot both be
	// true — a file that names a variable the project binds to a credential —
	// and that refusal fails the build here, after the image was pushed,
	// because the conflict is between a file read before the build and a
	// project that could have been edited during it.
	snapshot, err := repoconfig.Snapshot(kitchenv1alpha1.ConfigSnapshot{
		Env:       project.Spec.Env,
		Runtime:   runtimeFor(project, build),
		Processes: project.Spec.Processes,
	}, build.Status.Config)
	if err != nil {
		return r.fail(ctx, build, project, reasonConfigInvalid, err.Error())
	}

	release := &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{
			Name:      releaseName(project.Name, build.Spec.Git.SHA),
			Namespace: build.Namespace,
			Labels:    map[string]string{labelProject: project.Name, labelManagedByKey: labelManagedByValue},
		},
		Spec: kitchenv1alpha1.ReleaseSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			BuildRef:       kitchenv1alpha1.LocalObjectReference{Name: build.Name},
			Image:          image,
			ConfigSnapshot: snapshot,
		},
	}
	if err := r.Audit.Record(ctx, audit.Transition{
		Object:      release,
		Kind:        audit.KindRelease,
		Operation:   clickhouse.AuditCreate,
		Controller:  actorBuildController,
		Correlation: correlationFor(build),
		To:          image,
		Project:     project.Name,
		Reason:      fmt.Sprintf("build %s produced a release", build.Name),
		Details: map[string]any{
			"build":  build.Name,
			"commit": build.Spec.Git.SHA,
			"image":  image,
		},
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, release); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}

	switch pullRequest := build.PullRequestNumber(); {
	case pullRequest != nil:
		// Previews are never routed through a Promotion: the preview *is* the
		// review vehicle — it exists so the change can be looked at before
		// anything judges it — and gating its deployment on evidence would
		// gate producing the evidence.
		if err := r.routePreview(ctx, build, project, *pullRequest, release.Name); err != nil {
			return ctrl.Result{}, err
		}
	case build.Spec.Git.Branch == project.Spec.Source.ProductionBranch:
		// The build's target is stage one of the project's pipeline when it
		// has one, and the production target when it does not (with no stages
		// the entry point and the production target are the same environment)
		// — and the release reaches it directly or through a Promotion,
		// depending on whether the target sets a bar.
		envName := ProductionTargetEnvironmentName(project)
		if stages := project.Spec.Promotion; stages != nil && len(stages.Stages) > 0 {
			envName = stages.Stages[0].Environment
		}
		if err := r.promoteOrFlip(ctx, build.Namespace, project, envName, release.Name, build); err != nil {
			return ctrl.Result{}, err
		}
	default:
		// A commit on neither the production branch nor any request the
		// platform has heard of. The release is real and can be deployed by
		// hand; nothing is going to deploy it on its own, and saying so is the
		// difference between that and a preview that failed to appear.
		logf.FromContext(ctx).Info("release is attached to no environment",
			"build", build.Name, "release", release.Name, "branch", build.Spec.Git.Branch)
	}

	build.Status.Phase = kitchenv1alpha1.BuildSucceeded
	build.Status.Image = image
	reason, msg := "BuildSucceeded", fmt.Sprintf("image %s pushed", image)
	if !digestFound {
		reason, msg = "ImageDigestUnavailable", "build succeeded but the image digest could not be read; recorded the tag reference"
	}
	meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
		Type: condReady, Status: metav1.ConditionTrue, Reason: reason,
		Message: msg, ObservedGeneration: build.Generation,
	})
	r.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventBuildSucceeded,
		Project: project.Name,
		Build:   build.Name,
		Release: release.Name,
		Message: fmt.Sprintf("build %s succeeded", build.Name),
		Value:   buildDurationSeconds(build),
	})
	r.git().reportBuild(ctx, project, build, gitprovider.CommitSuccess, succeededDescription(build))
	return ctrl.Result{}, r.Status().Update(ctx, build)
}

// runtimeFor is the runtime the Release freezes: the project's own, with the
// one thing a project may leave to the platform filled in.
//
// A project that names no port takes the detected framework's, because that
// is the number the framework's own tooling uses and the one an application
// that ignores $PORT will be listening on. It is resolved here, once, into
// the snapshot — so a release keeps the port it was built with even if the
// same project detects differently later, and so the number is visible rather
// than implied.
func runtimeFor(project *kitchenv1alpha1.Project, build *kitchenv1alpha1.Build) kitchenv1alpha1.RuntimeSpec {
	runtimeSpec := project.Spec.Runtime
	if runtimeSpec.Port != 0 {
		return runtimeSpec
	}
	if detected, ok := framework.ByName(build.Status.DetectedFramework); ok {
		runtimeSpec.Port = detected.Port
	}
	return runtimeSpec
}

// routePreview puts a build's release on its pull request's preview
// environment, and records on the build that it did.
//
// The record is the whole reason this is one function rather than four lines
// in succeed(): a request opened after its branch was pushed is heard about
// late, so the routing has to be attemptable again afterwards, and something
// has to be able to answer "has this build's release already been routed?"
// without asking the world. Asking the world gets it wrong — a preview torn
// down when its request closed is indistinguishable from one that was never
// made, and re-creating that is a closed request's preview coming back to
// life hours later.
//
// The caller persists the status: succeed() writes it with the rest of the
// build's outcome, and adoptLatePreview writes it on its own.
func (r *BuildReconciler) routePreview(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	pullRequest int32,
	releaseName string,
) error {
	if !previewsEnabled(project) {
		return nil
	}
	envName := PreviewEnvironmentName(project.Name, pullRequest)
	preview := &kitchenv1alpha1.PreviewInfo{
		PullRequest: pullRequest,
		Branch:      build.Spec.Git.Branch,
	}
	if err := r.ensureEnvironment(ctx, build.Namespace, project, envName,
		kitchenv1alpha1.EnvironmentPreview, preview, releaseName, build); err != nil {
		return err
	}
	build.Status.Preview = envName
	return nil
}

// adoptLatePreview gives a finished build the preview environment it would
// have got had the platform known about the pull request in time.
//
// A branch is normally pushed before a request is opened for it — sometimes
// days before — and every provider delivers the push first. The push creates
// the Build, and the request event that follows finds the name taken. Before
// this existed it was discarded as a redelivery, and since a Build's spec is
// immutable and a terminal Build never re-enters succeed(), the request was
// never heard of again: the branch built, produced a release, and no preview
// ever appeared. The receiver now annotates the existing Build instead, and
// this is the other half — the case where the build had already finished by
// the time the annotation arrived.
//
// It insists the association came from the annotation, which is what keeps it
// to exactly the builds this repairs. A build whose *spec* names a request was
// routed by succeed() with everything it needed to know; re-routing it here
// would recreate previews for every request an installation has ever closed
// the first time it upgrades.
func (r *BuildReconciler) adoptLatePreview(ctx context.Context, build *kitchenv1alpha1.Build) error {
	if build.Status.Phase != kitchenv1alpha1.BuildSucceeded || build.Status.Preview != "" {
		return nil
	}
	if build.Spec.Git.PullRequest != nil {
		return nil
	}
	pullRequest := build.PullRequestNumber()
	if pullRequest == nil {
		return nil
	}

	project := &kitchenv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: build.Namespace, Name: build.Spec.ProjectRef.Name,
	}, project); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !previewsEnabled(project) {
		return nil
	}
	// The release is the one this build produced, named the way succeed()
	// named it. A build that succeeded has one; a build whose release somebody
	// deleted has nothing left to deploy and is left alone.
	release := &kitchenv1alpha1.Release{}
	name := releaseName(project.Name, build.Spec.Git.SHA)
	if err := r.Get(ctx, types.NamespacedName{Namespace: build.Namespace, Name: name}, release); err != nil {
		return client.IgnoreNotFound(err)
	}
	if err := r.routePreview(ctx, build, project, *pullRequest, release.Name); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("routed a finished build to the preview of a pull request it was told about late",
		"build", build.Name, "release", release.Name,
		"pullRequest", *pullRequest, "environment", build.Status.Preview)
	return r.Status().Update(ctx, build)
}

// promoteOrFlip routes a production-branch build's release to its target
// environment through the right door.
//
// An environment that declares no requirements takes the release exactly the
// way it always has: ensureEnvironment flips spec.releaseRef directly — zero
// behaviour change for every installation that never set a bar. An
// environment that *does* declare requirements is never written to from here:
// a Promotion is created instead, and the promotion reconciler applies it if
// and only if the policy allows. A target that does not exist yet is created
// by ensureEnvironment — an environment must exist to be promoted into, and a
// fresh one declares no requirements, so the fast path is also the right one.
func (r *BuildReconciler) promoteOrFlip(
	ctx context.Context,
	namespace string,
	project *kitchenv1alpha1.Project,
	envName, releaseName string,
	build *kitchenv1alpha1.Build,
) error {
	env := &kitchenv1alpha1.Environment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: envName}, env)
	if apierrors.IsNotFound(err) {
		return r.ensureEnvironment(ctx, namespace, project, envName,
			kitchenv1alpha1.EnvironmentProduction, nil, releaseName, build)
	}
	if err != nil {
		return err
	}
	// A stage naming another project's environment is a misconfiguration the
	// admission layer cannot catch (cross-object CEL does not exist); refuse
	// it here rather than deploying into a stranger's environment.
	if env.Spec.ProjectRef.Name != project.Name {
		return fmt.Errorf("environment %s belongs to project %s, not %s: fix spec.promotion.stages",
			envName, env.Spec.ProjectRef.Name, project.Name)
	}
	if env.Spec.Requirements == nil {
		// The hard check behind issue #137: the fast path refuses to land a
		// classified project's release on an environment rated below it.
		// Auto-created environments inherit the project's class and never
		// trip this; only one somebody narrowed does — which is the point.
		if refusal := DataClassRefusal(project, env); refusal != "" {
			return r.refuseFlipOnDataClass(ctx, build, project, env, releaseName, refusal)
		}
		return r.ensureEnvironment(ctx, namespace, project, envName,
			kitchenv1alpha1.EnvironmentProduction, nil, releaseName, build)
	}
	return createAutomaticPromotion(ctx, r.Client, r.Audit, actorBuildController,
		correlationFor(build), namespace, project, envName, releaseName,
		audit.ControllerActor(actorBuildController),
		fmt.Sprintf("build %s succeeded", build.Name))
}

// condPromoted is the Build condition that says what happened to a
// successful build's release after the build itself: absent for a build
// nothing promotes (a preview, a side branch), and False with the refusal
// when the fast path would not land the release. The build itself still
// succeeds — the artifact exists and is evidenced — so the refusal cannot
// live on the Ready condition.
const condPromoted = "Promoted"

// refuseFlipOnDataClass is the fast path saying no, loudly and terminally:
// the audit record first (fail-closed — a refusal the log cannot record is a
// requeue), then the refusal on the Build's Promoted condition, naming both
// classes and the fix. It returns nil error on the recorded refusal because
// the mismatch is a configuration to correct, not a fault to requeue against;
// the next production build meets the corrected classes.
func (r *BuildReconciler) refuseFlipOnDataClass(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	releaseName, refusal string,
) error {
	if err := r.Audit.Record(ctx, dataClassFlipRefusalTransition(build, project, env, releaseName, refusal)); err != nil {
		return err
	}
	meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
		Type: condPromoted, Status: metav1.ConditionFalse, Reason: "DataClassExceedsEnvironment",
		Message: refusal, ObservedGeneration: build.Generation,
	})
	logf.FromContext(ctx).Info("a release was refused its environment over data classification",
		"build", build.Name, "environment", env.Name, "release", releaseName,
		"projectDataClass", string(project.Spec.DataClass), "environmentDataClass", string(env.Spec.DataClass))
	return nil
}

// dataClassFlipRefusalTransition is the refusal's audit record, built apart
// from the recording so a test can hold it up to the light without a store.
func dataClassFlipRefusalTransition(
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	releaseName, refusal string,
) audit.Transition {
	return audit.Transition{
		Object:      env,
		Kind:        audit.KindEnvironment,
		Controller:  actorBuildController,
		Correlation: correlationFor(build),
		Project:     project.Name,
		Reason:      fmt.Sprintf("release %s was not promoted to %s: %s", releaseName, env.Name, refusal),
		Details: map[string]any{
			"release":              releaseName,
			"build":                build.Name,
			"projectDataClass":     string(project.Spec.DataClass),
			"environmentDataClass": string(env.Spec.DataClass),
		},
	}
}

// ensureEnvironment points the named Environment at the Release, creating it
// on the first build for its target (the first production build, or a
// pull request's first preview build).
func (r *BuildReconciler) ensureEnvironment(
	ctx context.Context,
	namespace string,
	project *kitchenv1alpha1.Project,
	envName string,
	envType kitchenv1alpha1.EnvironmentType,
	preview *kitchenv1alpha1.PreviewInfo,
	releaseName string,
	build *kitchenv1alpha1.Build,
) error {
	buildName := build.Name
	env := &kitchenv1alpha1.Environment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: envName}, env)
	if apierrors.IsNotFound(err) {
		env = &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      envName,
				Namespace: namespace,
				Labels:    map[string]string{labelProject: project.Name, labelManagedByKey: labelManagedByValue},
			},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project.Name},
				Type:       envType,
				// Issue #137's inheritance: an environment the platform
				// creates takes the project's class at creation, so a
				// classified project's own environments can hold its data by
				// construction. Owners may narrow it later; existing
				// environments are never touched.
				DataClass:  project.Spec.DataClass,
				Preview:    preview,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
			},
		}
		details := map[string]any{"type": string(envType), "release": releaseName, "build": buildName}
		if project.Spec.DataClass.Classified() {
			details["dataClass"] = string(project.Spec.DataClass)
		}
		if err := r.Audit.Record(ctx, audit.Transition{
			Object:      env,
			Kind:        audit.KindEnvironment,
			Operation:   clickhouse.AuditCreate,
			Controller:  actorBuildController,
			Correlation: correlationFor(build),
			To:          releaseName,
			Project:     project.Name,
			Reason:      fmt.Sprintf("build %s created environment %s", buildName, envName),
			Details:     details,
		}); err != nil {
			return err
		}
		if err := r.Create(ctx, env); err != nil {
			return err
		}
		if envType == kitchenv1alpha1.EnvironmentPreview && preview != nil {
			r.Activity.Record(ctx, clickhouse.Event{
				Type:        clickhouse.EventPreviewCreated,
				Project:     project.Name,
				Environment: envName,
				Release:     releaseName,
				Message:     fmt.Sprintf("preview for PR #%d created", preview.PullRequest),
			})
		} else {
			r.Activity.Record(ctx, clickhouse.Event{
				Type:        clickhouse.EventReleasePromoted,
				Project:     project.Name,
				Environment: envName,
				Release:     releaseName,
				Message:     fmt.Sprintf("release %s went live on %s", releaseName, envName),
			})
		}
		return nil
	}
	if err != nil {
		return err
	}
	if env.Spec.ReleaseRef.Name == releaseName {
		return nil
	}
	outgoing := env.Spec.ReleaseRef.Name
	if err := r.Audit.Record(ctx, audit.Transition{
		Object:      env,
		Kind:        audit.KindEnvironment,
		Controller:  actorBuildController,
		Correlation: correlationFor(build),
		From:        outgoing,
		To:          releaseName,
		Project:     project.Name,
		Reason:      fmt.Sprintf("build %s promoted release %s to %s", buildName, releaseName, envName),
		Details:     map[string]any{"release": releaseName, "previousRelease": outgoing, "build": buildName},
	}); err != nil {
		return err
	}
	env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: releaseName}
	if err := r.Update(ctx, env); err != nil {
		return err
	}
	r.Activity.Record(ctx, clickhouse.Event{
		Type:        clickhouse.EventReleasePromoted,
		Project:     project.Name,
		Environment: envName,
		Release:     releaseName,
		Message:     fmt.Sprintf("release %s promoted to %s", releaseName, envName),
	})
	if env.RecordReleaseMove(outgoing, kitchenv1alpha1.ReleaseMovePromoted, buildName) {
		return r.Status().Update(ctx, env)
	}
	return nil
}

// previewsEnabled reports whether the project wants preview environments.
// The API server defaults spec.previews.enabled to true, and an unset field
// reads the same way.
func previewsEnabled(project *kitchenv1alpha1.Project) bool {
	return project.Spec.Previews.IsEnabled()
}

// imageWithDigest reads the builder's own account of what it pushed from the
// build pod's termination message, and returns the digest-qualified image
// reference. It falls back to the tag reference when the digest cannot be
// found.
func (r *BuildReconciler) imageWithDigest(ctx context.Context, appNS, jobName, tagRef string) (string, bool) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(appNS), client.MatchingLabels{"job-name": jobName}); err != nil {
		return tagRef, false
	}
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated == nil {
				continue
			}
			if digest := digestFromTerminationMessage(cs.State.Terminated.Message); digest != "" {
				return tagRef + "@" + digest, true
			}
		}
	}
	return tagRef, false
}

// reportDigest matches the digest line of the CNB lifecycle's report, which is
// TOML: `digest = "sha256:..."` under [image].
var reportDigest = regexp.MustCompile(`digest\s*=\s*"(sha256:[a-fA-F0-9]+)"`)

// digestFromTerminationMessage reads the pushed image's digest out of whatever
// the builder left behind. The two builders write different files to the same
// place: BuildKit its JSON metadata, the CNB lifecycle its TOML report. Both
// say the same thing, and a build is only ever one of them, so trying each in
// turn keeps the caller from having to know which ran.
func digestFromTerminationMessage(message string) string {
	if message == "" {
		return ""
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(message), &metadata); err == nil {
		digest, _ := metadata["containerimage.digest"].(string)
		return digest
	}
	if match := reportDigest.FindStringSubmatch(message); match != nil {
		return match[1]
	}
	return ""
}

// syncRegistrySecret copies the registry Connection's credentials into the
// application namespace so build pods can mount them.
func (r *BuildReconciler) syncRegistrySecret(
	ctx context.Context,
	conn *kitchenv1alpha1.Connection,
	srcNS, appNS string,
) (string, error) {
	src := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: srcNS, Name: conn.Spec.CredentialsSecretRef.Name}, src); err != nil {
		return "", err
	}
	name := registrySecretName(conn.Name)
	dst := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dst, func() error {
		if dst.CreationTimestamp.IsZero() {
			dst.Type = src.Type
		}
		dst.Labels = map[string]string{labelManagedByKey: labelManagedByValue}
		dst.Data = src.Data
		return nil
	})
	return name, err
}

// registrySecretName is the docker config a project's application namespace
// holds for one registry Connection. The environment reconciler pulls images
// with the same secret, so the name is shared rather than spelled twice.
func registrySecretName(connectionName string) string {
	return "kitchen-registry-" + connectionName
}

// gitCredential is what a build has to clone a private repository with, or
// why it has nothing. Both halves matter: the second is the only thing that
// can turn a builder's "could not read Username" into a sentence naming the
// Connection somebody has to go and look at.
type gitCredential struct {
	// Secret is the name of the Secret in the application namespace holding
	// the token, empty when the build clones anonymously.
	Secret string
	// Absent is why there is no token, empty when there is one.
	Absent string
}

// explain appends what the build did not have to a failure message, when it
// did not have a credential. A public repository builds without one and this
// says nothing; a private one fails inside git with an error about a terminal
// that does not exist, and this is what names the cause.
func (g gitCredential) explain(message string) string {
	if g.Absent == "" {
		return message
	}
	return message + " (the build cloned anonymously: " + g.Absent +
		" — a private repository cannot be cloned without one)"
}

// gitSecretName is the token a project's application namespace holds for one
// git source Connection.
func gitSecretName(connectionName string) string {
	return "kitchen-git-" + connectionName
}

// resolveGitCredential syncs the project's git source token into the
// application namespace, so the build pod can mount it.
//
// Nothing here fails a build. A repository the platform can clone anonymously
// is the common case and needs none of this, and there is no way to tell one
// from a private repository without trying — so every reason a token cannot
// be resolved is recorded rather than raised, and reaches the operator only
// if the build then goes on to fail.
func (r *BuildReconciler) resolveGitCredential(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	srcNS, appNS string,
) gitCredential {
	connName := project.Spec.Source.ConnectionRef.Name
	if connName == "" {
		return gitCredential{Absent: "the project names no source connection"}
	}

	conn := &kitchenv1alpha1.Connection{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: srcNS, Name: connName}, conn); err != nil {
		return gitCredential{Absent: fmt.Sprintf("source connection %q could not be read: %v", connName, err)}
	}
	src := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: srcNS, Name: conn.Spec.CredentialsSecretRef.Name}, src); err != nil {
		return gitCredential{Absent: fmt.Sprintf("the credentials of source connection %q could not be read: %v", connName, err)}
	}
	if len(src.Data[gitCredentialsTokenKey]) == 0 {
		return gitCredential{Absent: fmt.Sprintf(
			"source connection %q has no %q in its credentials", connName, gitCredentialsTokenKey)}
	}

	name := gitSecretName(conn.Name)
	dst := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dst, func() error {
		dst.Labels = map[string]string{labelManagedByKey: labelManagedByValue}
		dst.Data = map[string][]byte{gitCredentialsTokenKey: src.Data[gitCredentialsTokenKey]}
		return nil
	}); err != nil {
		return gitCredential{Absent: fmt.Sprintf("the credentials of source connection %q could not be synced: %v", connName, err)}
	}
	return gitCredential{Secret: name}
}

func (r *BuildReconciler) pending(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	reason string,
	cause error,
) (ctrl.Result, error) {
	if build.Status.Phase == "" {
		build.Status.Phase = kitchenv1alpha1.BuildQueued
	}
	meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
		Type: condReady, Status: metav1.ConditionFalse, Reason: reason,
		Message: cause.Error(), ObservedGeneration: build.Generation,
	})
	if err := r.Status().Update(ctx, build); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *BuildReconciler) fail(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	reason, message string,
) (ctrl.Result, error) {
	if err := r.Audit.Record(ctx, audit.Transition{
		Object:      build,
		Kind:        audit.KindBuild,
		Controller:  actorBuildController,
		Correlation: correlationFor(build),
		From:        string(build.Status.Phase),
		To:          string(kitchenv1alpha1.BuildFailed),
		Project:     build.Spec.ProjectRef.Name,
		Reason:      message,
		Details:     map[string]any{"commit": build.Spec.Git.SHA, "reason": reason},
	}); err != nil {
		return ctrl.Result{}, err
	}
	build.Status.Phase = kitchenv1alpha1.BuildFailed
	build.Status.CompletedAt = ptr.To(metav1.Now())
	meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
		Type: condReady, Status: metav1.ConditionFalse, Reason: reason,
		Message: message, ObservedGeneration: build.Generation,
	})
	// A build the platform could not run at all is an error rather than a
	// failure: the distinction is the provider's, and it separates "your
	// Dockerfile is broken" from "the platform is".
	state := gitprovider.CommitFailure
	if reason != reasonBuildFailed {
		state = gitprovider.CommitError
	}
	r.git().reportBuild(ctx, project, build, state, message)
	r.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventBuildFailed,
		Project: build.Spec.ProjectRef.Name,
		Build:   build.Name,
		Message: fmt.Sprintf("build %s failed: %s", build.Name, message),
		Value:   buildDurationSeconds(build),
	})
	return ctrl.Result{}, r.Status().Update(ctx, build)
}

// succeededDescription is the line a status check carries on a green build.
// How long it took is the one thing a reader of a commit wants from it that
// the commit does not already say — and next to it, whether there was a cache,
// because a build that was slow for having nothing to reuse should say so
// rather than read as a regression.
func succeededDescription(build *kitchenv1alpha1.Build) string {
	seconds := buildDurationSeconds(build)
	if seconds <= 0 {
		return "the image was built and pushed" + cacheSuffix(build)
	}
	return fmt.Sprintf("image built and pushed in %s%s",
		(time.Duration(seconds) * time.Second).String(), cacheSuffix(build))
}

// cacheSuffix is how a status line says what the layer cache did, and nothing
// when there was none to speak of: an installation that turned caching off
// does not want every commit told about it.
func cacheSuffix(build *kitchenv1alpha1.Build) string {
	cache := build.Status.Cache
	if cache == nil || !cache.Enabled {
		return ""
	}
	if cache.Warm {
		return ", cache warm"
	}
	return ", cache cold"
}

// buildDurationSeconds is how long a finished build ran, 0 when unknown.
func buildDurationSeconds(build *kitchenv1alpha1.Build) float64 {
	if build.Status.StartedAt == nil || build.Status.CompletedAt == nil {
		return 0
	}
	duration := build.Status.CompletedAt.Sub(build.Status.StartedAt.Time).Seconds()
	if duration < 0 {
		return 0
	}
	return duration
}

func isTerminal(phase kitchenv1alpha1.BuildPhase) bool {
	switch phase {
	case kitchenv1alpha1.BuildSucceeded, kitchenv1alpha1.BuildFailed, kitchenv1alpha1.BuildCancelled:
		return true
	default:
		return false
	}
}

func jobOutcome(job *batchv1.Job) (complete, failed bool, message string) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			complete = true
		case batchv1.JobFailed:
			failed = true
			message = c.Message
			if message == "" {
				message = "build job failed"
			}
		}
	}
	return complete, failed, message
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func releaseName(projectName, sha string) string {
	return fmt.Sprintf("%s-rel-%s", projectName, shortSHA(sha))
}

// mapJobToBuild enqueues the owning Build for a labeled build Job.
func (r *BuildReconciler) mapJobToBuild(_ context.Context, obj client.Object) []ctrl.Request {
	labels := obj.GetLabels()
	name, ok := labels[labelBuild]
	if !ok {
		return nil
	}
	ns, ok := labels[labelBuildNS]
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *BuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Container logs are a subresource the kubelet serves, which the
	// manager's cached client does not speak. The typed client is built here
	// rather than passed in so that every caller gets one without knowing it
	// needs one — a test that has set its own is left alone.
	if r.PodLogs == nil {
		clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			return err
		}
		r.PodLogs = clientsetPodLogs(clientset)
	}
	// Warning events are read through this rather than the cache; see
	// APIReader.
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Build{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(r.mapJobToBuild)).
		Named("build").
		Complete(r)
}

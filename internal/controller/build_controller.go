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

	// DefaultBuildConcurrency is how many builds run at once when the Kitchen
	// object names no limit. It is exported because the API reports the queue
	// against it, and a status bar reading "1 of 0" would be its own bug.
	DefaultBuildConcurrency = 2

	// dockerConfigDir is where a build pod finds the registry credentials
	// it pushes with, and the value of DOCKER_CONFIG in every builder.
	dockerConfigDir = "/kitchen/.docker"

	// terminationLogPath is where a builder writes what the reconciler needs
	// back from it — the digest of the image it pushed. Kubernetes surfaces
	// the file as the container's termination message, so nothing has to be
	// read out of the pod's log.
	terminationLogPath = "/dev/termination-log"

	// reasonFrameworkNotDetected is a repository the platform read and did
	// not recognise, with no Dockerfile to fall back to. It is the failure
	// "auto" exists to be able to report: the alternative is a builder's own
	// error about a file the repository never had.
	reasonFrameworkNotDetected = "FrameworkNotDetected"

	// reasonSourceUnreadable is the platform not being able to look at the
	// repository at all. It keeps the Build queued rather than failing it —
	// nothing about the commit caused it.
	reasonSourceUnreadable = "SourceUnreadable"

	// reasonBuildFailed marks the one failure that is the repository's own:
	// the build ran and the image did not come out. Every other reason is
	// the platform failing to run it at all, which reports differently on
	// the commit.
	reasonBuildFailed = "BuildFailed"
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
	// Attesters resolves how signed evidence reaches the registry a build
	// pushed to. Nil talks to the real registry with the build's own
	// credential; tests point it at an in-process one.
	Attesters AttesterFactory
	// CacheProbes resolves how the reconciler asks a registry whether a
	// layer cache is there. Nil asks the real one; tests answer without a
	// registry at all.
	CacheProbes CacheProbeFactory
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
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

// Reconcile drives a Build from Queued through a BuildKit Job to a Release.
func (r *BuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	build := &kitchenv1alpha1.Build{}
	if err := r.Get(ctx, req.NamespacedName, build); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if isTerminal(build.Status.Phase) || !build.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	project := &kitchenv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: build.Namespace, Name: build.Spec.ProjectRef.Name}, project); err != nil {
		return r.pending(ctx, build, "ProjectMissing", err)
	}

	builds := r.platformBuilds(ctx)
	strategy := resolveStrategy(project, builds.DefaultStrategy)
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
		if waiting, res := r.gateConcurrency(ctx, build, builds.Concurrency); waiting {
			return res, nil
		}
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
		if err := r.createJob(ctx, build, project, strategy, detected, cache, appNS, credsSecret, tagRef); err != nil {
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
		return r.fail(ctx, build, project, reasonBuildFailed, message)
	default:
		if build.Status.Phase != kitchenv1alpha1.BuildRunning {
			build.Status.Phase = kitchenv1alpha1.BuildRunning
			return ctrl.Result{}, r.Status().Update(ctx, build)
		}
		return ctrl.Result{}, nil
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
// it. A Project that names a strategy is obeyed; one left on "auto" takes the
// platform's default, which is where an operator says what an unconfigured
// project should do.
//
// "auto" all the way down stays "auto", and is answered by reading the
// repository — see decide. An operator who wants one strategy for everything
// says so in Kitchen.spec.builds.defaultStrategy, and detection never runs.
func resolveStrategy(
	project *kitchenv1alpha1.Project,
	platformDefault kitchenv1alpha1.BuildStrategy,
) kitchenv1alpha1.BuildStrategy {
	strategy := project.Spec.Build.Strategy
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
	appNS, credsSecret, tagRef string,
) error {
	template := dockerfilePod(project, build, cache, credsSecret, tagRef, r.platformAttestation(ctx))
	if strategy == kitchenv1alpha1.BuildStrategyBuildpacks {
		template = buildpacksPod(project, build, detected, cache, credsSecret, tagRef)
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
			Template:                template,
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
	credsSecret, tagRef string,
	attest kitchenv1alpha1.BuildAttestationSpec,
) corev1.PodTemplateSpec {
	buildContext := repoCloneURL(project) + "#" + build.Spec.Git.SHA
	if root := buildRootDir(project); root != "" {
		buildContext += ":" + root
	}
	dockerfile := project.Spec.Build.DockerfilePath
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
		// to the termination log so the reconciler can read it from the
		// pod status without any extra plumbing.
		"--metadata-file", terminationLogPath,
		// One log line per step, no cursor tricks: the collector ships
		// these lines into ClickHouse, and the interactive renderer would
		// arrive there as a wall of escape codes.
		"--progress", "plain",
	)
	args = append(args, buildkitCacheArgs(cache)...)

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"container.apparmor.security.beta.kubernetes.io/buildkit": "unconfined",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "buildkit",
				Image:   BuildkitImage,
				Command: []string{"buildctl-daemonless.sh"},
				Args:    args,
				Env: []corev1.EnvVar{
					{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
					{Name: "DOCKER_CONFIG", Value: dockerConfigDir},
				},
				VolumeMounts: []corev1.VolumeMount{dockerConfigMount()},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:      ptr.To(int64(1000)),
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
				},
			}},
			Volumes: []corev1.Volume{dockerConfigVolume(credsSecret)},
		},
	}
}

// repoCloneURL is where a build fetches the project's repository from.
//
// TODO: derive the clone URL (and auth) from the project's git Connection
// once git providers beyond public GitHub are wired up.
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

	release := &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{
			Name:      releaseName(project.Name, build.Spec.Git.SHA),
			Namespace: build.Namespace,
			Labels:    map[string]string{labelProject: project.Name, labelManagedByKey: labelManagedByValue},
		},
		Spec: kitchenv1alpha1.ReleaseSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: build.Name},
			Image:      image,
			ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
				Env:     project.Spec.Env,
				Runtime: runtimeFor(project, build),
			},
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

	switch {
	case build.Spec.Git.PullRequest != nil:
		if previewsEnabled(project) {
			preview := &kitchenv1alpha1.PreviewInfo{
				PullRequest: *build.Spec.Git.PullRequest,
				Branch:      build.Spec.Git.Branch,
			}
			envName := fmt.Sprintf("%s-pr-%d", project.Name, *build.Spec.Git.PullRequest)
			if err := r.ensureEnvironment(ctx, build.Namespace, project, envName,
				kitchenv1alpha1.EnvironmentPreview, preview, release.Name, build); err != nil {
				return ctrl.Result{}, err
			}
		}
	case build.Spec.Git.Branch == project.Spec.Source.ProductionBranch:
		envName := project.Name + "-production"
		if err := r.ensureEnvironment(ctx, build.Namespace, project, envName,
			kitchenv1alpha1.EnvironmentProduction, nil, release.Name, build); err != nil {
			return ctrl.Result{}, err
		}
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
				Preview:    preview,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
			},
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
			Details:     map[string]any{"type": string(envType), "release": releaseName, "build": buildName},
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
// The API server defaults spec.previews.enabled to true.
func previewsEnabled(project *kitchenv1alpha1.Project) bool {
	return project.Spec.Previews.Enabled
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Build{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(r.mapJobToBuild)).
		Named("build").
		Complete(r)
}

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
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/gitprovider"
	"github.com/Bermos/Kitchen/internal/provider"
)

const (
	labelBuild   = "kitchen.bermos.dev/build"
	labelBuildNS = "kitchen.bermos.dev/build-namespace"

	// BuildkitImage runs the in-cluster builds.
	BuildkitImage = "moby/buildkit:v0.23.2-rootless"

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
	// GitProviders resolves a Provider for a Connection, for reporting the
	// build's outcome back onto its commit. Defaults to gitprovider.Default;
	// tests inject fakes.
	GitProviders gitprovider.Factory
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
	case kitchenv1alpha1.BuildStrategyDockerfile, kitchenv1alpha1.BuildStrategyBuildpacks:
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

	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: build.Name}, job)
	switch {
	case apierrors.IsNotFound(err):
		if waiting, res := r.gateConcurrency(ctx, build, builds.Concurrency); waiting {
			return res, nil
		}
		if err := r.createJob(ctx, build, project, strategy, appNS, credsSecret, tagRef); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("build job created",
			"namespace", appNS, "job", build.Name, "image", tagRef, "strategy", strategy)
		build.Status.Phase = kitchenv1alpha1.BuildRunning
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
		return r.succeed(ctx, build, project, job, appNS, tagRef)
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

// resolveStrategy is how a build actually gets made. A Project that names a
// strategy is obeyed; one left on "auto" takes the platform's default, which
// is where an operator says what an unconfigured project should do.
//
// "auto" is meant to detect the framework and choose for itself, and does not
// yet — issue #69. Until it does, a platform default of "auto" resolves to a
// Dockerfile build, which is what every build did before buildpacks existed.
func resolveStrategy(
	project *kitchenv1alpha1.Project,
	platformDefault kitchenv1alpha1.BuildStrategy,
) kitchenv1alpha1.BuildStrategy {
	strategy := project.Spec.Build.Strategy
	if strategy == "" || strategy == kitchenv1alpha1.BuildStrategyAuto {
		strategy = platformDefault
	}
	if strategy == "" || strategy == kitchenv1alpha1.BuildStrategyAuto {
		return kitchenv1alpha1.BuildStrategyDockerfile
	}
	return strategy
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
	appNS, credsSecret, tagRef string,
) error {
	template := dockerfilePod(project, build, credsSecret, tagRef)
	if strategy == kitchenv1alpha1.BuildStrategyBuildpacks {
		template = buildpacksPod(project, build, credsSecret, tagRef)
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
	credsSecret, tagRef string,
) corev1.PodTemplateSpec {
	buildContext := repoCloneURL(project) + "#" + build.Spec.Git.SHA
	if root := buildRootDir(project); root != "" {
		buildContext += ":" + root
	}
	dockerfile := project.Spec.Build.DockerfilePath
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}

	args := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--opt", "context=" + buildContext,
		"--opt", "filename=" + dockerfile,
		"--output", "type=image,name=" + tagRef + ",push=true",
		// BuildKit writes its result metadata (including the image digest)
		// to the termination log so the reconciler can read it from the
		// pod status without any extra plumbing.
		"--metadata-file", terminationLogPath,
		// One log line per step, no cursor tricks: the collector ships
		// these lines into ClickHouse, and the interactive renderer would
		// arrive there as a wall of escape codes.
		"--progress", "plain",
	}

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
	appNS, tagRef string,
) (ctrl.Result, error) {
	image, digestFound := r.imageWithDigest(ctx, appNS, build.Name, tagRef)

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
				Runtime: project.Spec.Runtime,
			},
		},
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
				kitchenv1alpha1.EnvironmentPreview, preview, release.Name, build.Name); err != nil {
				return ctrl.Result{}, err
			}
		}
	case build.Spec.Git.Branch == project.Spec.Source.ProductionBranch:
		envName := project.Name + "-production"
		if err := r.ensureEnvironment(ctx, build.Namespace, project, envName,
			kitchenv1alpha1.EnvironmentProduction, nil, release.Name, build.Name); err != nil {
			return ctrl.Result{}, err
		}
	}

	build.Status.Phase = kitchenv1alpha1.BuildSucceeded
	build.Status.Image = image
	if job.Status.CompletionTime != nil {
		build.Status.CompletedAt = job.Status.CompletionTime
	} else {
		build.Status.CompletedAt = ptr.To(metav1.Now())
	}
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
	buildName string,
) error {
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
// the commit does not already say.
func succeededDescription(build *kitchenv1alpha1.Build) string {
	seconds := buildDurationSeconds(build)
	if seconds <= 0 {
		return "the image was built and pushed"
	}
	return fmt.Sprintf("image built and pushed in %s", (time.Duration(seconds) * time.Second).String())
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

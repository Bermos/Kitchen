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

	defaultBuildConcurrency = 2
)

// registryConfig is the expected shape of a dockerRegistry Connection's config.
type registryConfig struct {
	// URL is the registry prefix images are pushed under, e.g.
	// "harbor.example.com/kitchen".
	URL string `json:"url"`
}

// BuildReconciler reconciles a Build: it runs a BuildKit Job for the commit,
// records the pushed image digest, creates the resulting Release, and
// auto-promotes it to the production Environment when the branch matches.
type BuildReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

	strategy := project.Spec.Build.Strategy
	if strategy == "" || strategy == kitchenv1alpha1.BuildStrategyAuto {
		// Framework detection is not implemented yet; auto falls back to
		// a Dockerfile build.
		strategy = kitchenv1alpha1.BuildStrategyDockerfile
	}
	if strategy != kitchenv1alpha1.BuildStrategyDockerfile {
		return r.fail(ctx, build, "StrategyUnsupported",
			fmt.Sprintf("build strategy %q is not supported yet", strategy))
	}

	registryConn := &kitchenv1alpha1.Connection{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: build.Namespace, Name: project.Spec.Registry.ConnectionRef.Name}, registryConn); err != nil {
		return r.pending(ctx, build, "RegistryConnectionMissing", err)
	}
	registry, err := parseRegistryConfig(registryConn)
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

	tagRef := fmt.Sprintf("%s/%s:%s", registry.URL, project.Name, shortSHA(build.Spec.Git.SHA))

	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: build.Name}, job)
	switch {
	case apierrors.IsNotFound(err):
		if waiting, res := r.gateConcurrency(ctx, build); waiting {
			return res, nil
		}
		if err := r.createJob(ctx, build, project, appNS, credsSecret, tagRef); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("build job created", "namespace", appNS, "job", build.Name, "image", tagRef)
		build.Status.Phase = kitchenv1alpha1.BuildRunning
		build.Status.StartedAt = ptr.To(metav1.Now())
		meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
			Type: condReady, Status: metav1.ConditionFalse, Reason: "BuildRunning",
			Message: "build job is running", ObservedGeneration: build.Generation,
		})
		return ctrl.Result{}, r.Status().Update(ctx, build)
	case err != nil:
		return ctrl.Result{}, err
	}

	complete, failed, message := jobOutcome(job)
	switch {
	case complete:
		return r.succeed(ctx, build, project, job, appNS, tagRef)
	case failed:
		return r.fail(ctx, build, "BuildFailed", message)
	default:
		if build.Status.Phase != kitchenv1alpha1.BuildRunning {
			build.Status.Phase = kitchenv1alpha1.BuildRunning
			return ctrl.Result{}, r.Status().Update(ctx, build)
		}
		return ctrl.Result{}, nil
	}
}

// gateConcurrency keeps the Build queued while the platform-wide concurrency
// limit is reached.
func (r *BuildReconciler) gateConcurrency(ctx context.Context, build *kitchenv1alpha1.Build) (bool, ctrl.Result) {
	limit := int32(defaultBuildConcurrency)
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err == nil {
		if kitchen.Spec.Builds.Concurrency > 0 {
			limit = kitchen.Spec.Builds.Concurrency
		}
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
	appNS, credsSecret, tagRef string,
) error {
	// TODO: derive the clone URL (and auth) from the project's git
	// Connection once git providers beyond public GitHub are wired up.
	gitURL := fmt.Sprintf("https://github.com/%s.git", project.Spec.Source.Repo)
	buildContext := gitURL + "#" + build.Spec.Git.SHA
	if root := project.Spec.Build.RootDirectory; root != "" && root != "." {
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
		"--metadata-file", "/dev/termination-log",
		// One log line per step, no cursor tricks: the collector ships
		// these lines into ClickHouse, and the interactive renderer would
		// arrive there as a wall of escape codes.
		"--progress", "plain",
	}

	labels := map[string]string{
		labelProject:      project.Name,
		labelBuild:        build.Name,
		labelBuildNS:      build.Namespace,
		labelManagedByKey: labelManagedByValue,
	}

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
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
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
							{Name: "DOCKER_CONFIG", Value: "/kitchen/.docker"},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "docker-config",
							MountPath: "/kitchen/.docker",
							ReadOnly:  true,
						}},
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:      ptr.To(int64(1000)),
							SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "docker-config",
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: credsSecret,
							Items: []corev1.KeyToPath{{
								Key:  corev1.DockerConfigJsonKey,
								Path: "config.json",
							}},
						}},
					}},
				},
			},
		},
	}
	return r.Create(ctx, job)
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
				kitchenv1alpha1.EnvironmentPreview, preview, release.Name); err != nil {
				return ctrl.Result{}, err
			}
		}
	case build.Spec.Git.Branch == project.Spec.Source.ProductionBranch:
		envName := project.Name + "-production"
		if err := r.ensureEnvironment(ctx, build.Namespace, project, envName,
			kitchenv1alpha1.EnvironmentProduction, nil, release.Name); err != nil {
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
		return r.Create(ctx, env)
	}
	if err != nil {
		return err
	}
	if env.Spec.ReleaseRef.Name == releaseName {
		return nil
	}
	env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: releaseName}
	return r.Update(ctx, env)
}

// previewsEnabled reports whether the project wants preview environments.
// The API server defaults spec.previews.enabled to true.
func previewsEnabled(project *kitchenv1alpha1.Project) bool {
	return project.Spec.Previews.Enabled
}

// imageWithDigest reads BuildKit's metadata from the build pod's termination
// message and returns the digest-qualified image reference. It falls back to
// the tag reference when the digest cannot be found.
func (r *BuildReconciler) imageWithDigest(ctx context.Context, appNS, jobName, tagRef string) (string, bool) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(appNS), client.MatchingLabels{"job-name": jobName}); err != nil {
		return tagRef, false
	}
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated == nil || cs.State.Terminated.Message == "" {
				continue
			}
			var metadata map[string]any
			if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &metadata); err != nil {
				continue
			}
			if digest, ok := metadata["containerimage.digest"].(string); ok && digest != "" {
				return tagRef + "@" + digest, true
			}
		}
	}
	return tagRef, false
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
	name := "kitchen-registry-" + conn.Name
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
	reason, message string,
) (ctrl.Result, error) {
	build.Status.Phase = kitchenv1alpha1.BuildFailed
	build.Status.CompletedAt = ptr.To(metav1.Now())
	meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
		Type: condReady, Status: metav1.ConditionFalse, Reason: reason,
		Message: message, ObservedGeneration: build.Generation,
	})
	return ctrl.Result{}, r.Status().Update(ctx, build)
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

func parseRegistryConfig(conn *kitchenv1alpha1.Connection) (*registryConfig, error) {
	cfg := &registryConfig{}
	if conn.Spec.Config != nil {
		if err := json.Unmarshal(conn.Spec.Config.Raw, cfg); err != nil {
			return nil, fmt.Errorf("invalid registry config: %w", err)
		}
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("registry connection %q has no url configured", conn.Name)
	}
	return cfg, nil
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

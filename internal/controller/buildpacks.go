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
	"path"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/framework"
)

const (
	// BuildpacksBuilderImage is the Cloud Native Buildpacks builder the
	// buildpacks strategy runs. Paketo's jammy "base" builder carries the
	// buildpacks for the languages a project is likely to be written in —
	// Node, Go, Python, Java, .NET — without the extra utilities of "full".
	//
	// It is pinned rather than floating on :latest, because the builder is
	// what decides the contents of the image: a moving tag would mean the
	// same commit rebuilt tomorrow produces something else.
	BuildpacksBuilderImage = "paketobuildpacks/builder-jammy-base:0.4.625"

	// GitCloneImage fetches the commit a buildpacks build builds. BuildKit
	// fetches its own git context; the CNB lifecycle only ever builds a
	// directory that is already on disk, so the clone runs in front of it as
	// an init container.
	GitCloneImage = "alpine/git:v2.54.0"

	// BuildpacksPlatformAPI is the version of the CNB platform contract the
	// job speaks. The lifecycle refuses to start without being told one —
	// it has no default — and 0.13 is supported by every lifecycle from 0.17
	// on, so it survives the builder being moved a few releases either way.
	BuildpacksPlatformAPI = "0.13"

	// cnbUID and cnbGID are the builder image's own unprivileged user: the
	// CNB_USER_ID and CNB_GROUP_ID of the builder pinned above, so moving one
	// means moving the other. The lifecycle chowns its directories and drops
	// to that user before it runs anything from the repository, and entering
	// as it already is what makes both steps no-ops — which is why a
	// buildpacks build needs none of the privileges a BuildKit one does.
	//
	// The clone runs as the same user, because buildpacks write into the
	// application directory (npm's modules, the start script the Node
	// buildpack generates): a clone owned by anyone else fails the build
	// halfway through.
	cnbUID = int64(1001)
	cnbGID = int64(1000)

	// Where the clone lands and where the lifecycle assembles the image.
	// Both are emptyDir volumes: a build gets its own, and nothing survives
	// it. What survives a build is the cache image the lifecycle exports —
	// see cnbCacheArgs — which the next build restores these layers from.
	buildpacksWorkspaceDir = "/workspace"
	buildpacksSourceDir    = buildpacksWorkspaceDir + "/source"
	buildpacksLayersDir    = "/layers"

	volumeWorkspace = "workspace"
	volumeLayers    = "layers"
)

// cloneScript fetches exactly the commit under build, and nothing else: the
// history is not what is being built, and a shallow fetch of one revision is
// the cheapest thing a large repository can be asked for.
//
// The repository and the commit arrive as environment variables rather than
// substituted into the script. Both come out of a Project's spec, and nothing
// constrains a repository name to characters a shell reads literally.
//
// A private repository is cloned with the token mounted at
// KITCHEN_GIT_TOKEN_FILE, and git is told about it the one way that keeps the
// value out of everything that is written down: an askpass helper, which git
// runs with this process's environment and which reads the file itself. The
// URL keeps no credential, so `git remote -v` says what the pod spec says.
// GIT_TERMINAL_PROMPT=0 is what turns "wait forever for a username nobody can
// type" into an error, credential or not.
const cloneScript = `set -e
export GIT_TERMINAL_PROMPT=0
if [ -n "$KITCHEN_GIT_TOKEN_FILE" ]; then
	cat >"$KITCHEN_ASKPASS" <<'EOF'
#!/bin/sh
case "$1" in
Username*) printf 'x-access-token' ;;
*) cat "$KITCHEN_GIT_TOKEN_FILE" ;;
esac
EOF
	chmod 0700 "$KITCHEN_ASKPASS"
	export GIT_ASKPASS="$KITCHEN_ASKPASS"
fi
git init -q "$KITCHEN_SOURCE_DIR"
cd "$KITCHEN_SOURCE_DIR"
git remote add origin "$KITCHEN_GIT_URL"
git fetch -q --depth 1 origin "$KITCHEN_GIT_SHA"
git checkout -q FETCH_HEAD`

// buildpacksPod is a build that hands the repository to the Cloud Native
// Buildpacks lifecycle: no Dockerfile, no instructions of any kind — the
// buildpacks in the builder decide what the repository is and how it is run.
//
// `creator` is the whole lifecycle in one process (detect, restore, build,
// export), which is what a build with no cache between phases wants: the
// alternative is five containers passing volumes between them for no gain.
//
// What the lifecycle is told about the repository comes from detection: a
// framework that starts a server of its own needs nothing, and one that
// builds into a directory of files needs the web-server buildpack pointed at
// that directory — there is no other way to say "serve this with NGINX".
func buildpacksPod(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	detected framework.Framework,
	cache *kitchenv1alpha1.BuildCacheStatus,
	credsSecret, gitSecret, tagRef string,
) corev1.PodTemplateSpec {
	// The clone lands the whole repository and the lifecycle is pointed
	// inside it: the build root is what is built, exactly as it is for the
	// container strategy, which reaches the same meaning by scoping its git
	// context instead. What is above the build root is on the volume and in
	// no build — the lifecycle only ever reads `-app`.
	appDir := path.Join(buildpacksSourceDir, buildRootDir(project))

	workspace := corev1.VolumeMount{Name: volumeWorkspace, MountPath: buildpacksWorkspaceDir}
	layers := corev1.VolumeMount{Name: volumeLayers, MountPath: buildpacksLayersDir}

	cloneEnv := []corev1.EnvVar{
		{Name: "KITCHEN_SOURCE_DIR", Value: buildpacksSourceDir},
		{Name: "KITCHEN_GIT_URL", Value: repoCloneURL(project)},
		{Name: "KITCHEN_GIT_SHA", Value: build.Spec.Git.SHA},
		// git wants a home to look for configuration in, and the
		// user it runs as here has none of its own.
		{Name: "HOME", Value: buildpacksWorkspaceDir},
	}
	cloneMounts := []corev1.VolumeMount{workspace}
	volumes := []corev1.Volume{
		{Name: volumeWorkspace, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: volumeLayers, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		dockerConfigVolume(credsSecret),
	}
	if gitSecret != "" {
		cloneEnv = append(cloneEnv,
			corev1.EnvVar{Name: "KITCHEN_GIT_TOKEN_FILE", Value: gitCredentialFile},
			// The askpass helper is written into the workspace, which is
			// the one directory this pod has that it can write to.
			corev1.EnvVar{Name: "KITCHEN_ASKPASS", Value: buildpacksWorkspaceDir + "/askpass"},
		)
		cloneMounts = append(cloneMounts, gitCredentialMount())
		volumes = append(volumes, gitCredentialVolume(gitSecret))
	}

	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  ptr.To(cnbUID),
				RunAsGroup: ptr.To(cnbGID),
			},
			InitContainers: []corev1.Container{{
				Name:         "clone",
				Image:        GitCloneImage,
				Command:      []string{"/bin/sh", "-c", cloneScript},
				Env:          cloneEnv,
				VolumeMounts: cloneMounts,
			}},
			Containers: []corev1.Container{{
				Name:    "creator",
				Image:   BuildpacksBuilderImage,
				Command: []string{"/cnb/lifecycle/creator"},
				Args: append(cnbCacheArgs(cache), []string{
					"-app=" + appDir,
					"-layers=" + buildpacksLayersDir,
					// The lifecycle's report carries the digest of what it
					// pushed. Writing it to the termination log puts it
					// exactly where the reconciler already reads BuildKit's
					// metadata from — see digestFromTerminationMessage,
					// which reads both shapes.
					"-report=" + terminationLogPath,
					// The collector ships this log into ClickHouse, where a
					// colour escape is a character like any other.
					"-no-color",
					tagRef,
				}...),
				Env: append([]corev1.EnvVar{
					{Name: "DOCKER_CONFIG", Value: dockerConfigDir},
					{Name: "CNB_PLATFORM_API", Value: BuildpacksPlatformAPI},
				}, frameworkEnv(detected)...),
				VolumeMounts: []corev1.VolumeMount{workspace, layers, dockerConfigMount()},
			}},
			Volumes: volumes,
		},
	}
}

// cnbCacheArgs point the lifecycle at the cache image it restores from and
// exports to. One flag does both directions, and it is the whole of buildpacks
// caching: the lifecycle decides for itself which of a buildpack's layers it
// can reuse, and there is no mode to choose between.
//
// A cache image the lifecycle cannot read or cannot write is a warning it
// prints and builds through, which is the degradation this needs and the
// reason it can be passed without knowing what the registry supports.
func cnbCacheArgs(cache *kitchenv1alpha1.BuildCacheStatus) []string {
	if cache == nil || !cache.Enabled {
		return nil
	}
	return []string{"-cache-image=" + cache.Ref}
}

// frameworkEnv is what detection tells the lifecycle, in the order the
// framework package sorted it: a Job's pod template cannot be edited after it
// is created, so the same repository has to produce the same spec every time
// rather than one that depends on map iteration order.
func frameworkEnv(detected framework.Framework) []corev1.EnvVar {
	env := make([]corev1.EnvVar, 0, len(detected.BuildEnv))
	for _, v := range detected.BuildEnv {
		env = append(env, corev1.EnvVar{Name: v.Name, Value: v.Value})
	}
	return env
}

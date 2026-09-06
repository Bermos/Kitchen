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

// buildpacksPod is a build that hands the repository to the Cloud Native
// Buildpacks lifecycle: no Dockerfile, no instructions of any kind — the
// buildpacks in the builder decide what the repository is and how it is run.
//
// The lifecycle runs as its five phases rather than as `creator`, which is the
// one process that does all five (#424). The phases are the same work in the
// same order over the same two volumes; what differs is that each is a
// container of its own, and a container only holds the credential its phase
// needs:
//
//   - analyze, restore and export talk to the registry. They are the
//     lifecycle's own binaries out of the pinned builder image, and nothing
//     from the repository runs in them.
//   - detect and build run the *buildpacks*, which run the repository's own
//     build — `npm install` and its lifecycle scripts, `pip install`,
//     whatever the buildpack invokes. They mount no registry credential at
//     all, so reading one out of `$DOCKER_CONFIG/config.json` from a
//     `postinstall` script finds an empty directory.
//   - only export pushes, so only export holds the credential that can. The
//     two phases that merely read hold the read-only one where the registry
//     issues one.
//
// `creator` is otherwise identical and was what this ran until the split: at
// platform API 0.13 it resolves its inputs exactly as the phases do, run
// image and all, so nothing about what is built or pushed changes.
//
// What the lifecycle is told about the repository comes from detection: a
// framework that starts a server of its own needs nothing, and one that
// builds into a directory of files needs the web-server buildpack pointed at
// that directory — there is no other way to say "serve this with NGINX".
func buildpacksPod(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	plan buildPlan,
	detected framework.Framework,
	cache *kitchenv1alpha1.BuildCacheStatus,
	credentials registryCredentialsForPod,
	gitSecret string,
) corev1.PodTemplateSpec {
	// The clone lands the whole repository and the lifecycle is pointed
	// inside it: the build root is what is built, exactly as it is for the
	// container strategy, which reaches the same meaning by scoping its git
	// context instead. What is above the build root is on the volume and in
	// no build — the lifecycle only ever reads `-app`.
	//
	// Which build root that is comes from the plan: one commit can produce
	// several images, and which of the repository's directories this one is
	// comes from the workload that declared it.
	appDir := path.Join(buildpacksSourceDir, plan.RootDirectory)

	workspace := corev1.VolumeMount{Name: volumeWorkspace, MountPath: buildpacksWorkspaceDir}
	layers := corev1.VolumeMount{Name: volumeLayers, MountPath: buildpacksLayersDir}

	// The clone is the only container in this pod that holds the git
	// credential, and it keeps the repository's `.git`: the lifecycle has
	// been handed a checkout with one since buildpacks builds existed, and a
	// buildpack may read it.
	clone := gitClone{
		SourceDir:  buildpacksSourceDir,
		ScratchDir: buildpacksWorkspaceDir,
		Mount:      workspace,
		Secret:     gitSecret,
		KeepGitDir: true,
	}.container(project, build)

	volumes := []corev1.Volume{
		{Name: volumeWorkspace, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: volumeLayers, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		dockerConfigVolume(credentials.Push),
		readDockerConfigVolume(credentials.Read),
	}
	if gitSecret != "" {
		volumes = append(volumes, gitCredentialVolume(gitSecret))
	}

	// What every phase is told, whichever credential it holds: the platform
	// contract it speaks, and what detection made of the repository. The
	// framework's variables are the buildpacks' own configuration (BP_*) and
	// are read in both the phases that run them, so they are given to every
	// phase rather than guessed at one.
	lifecycleEnv := func(credential string) []corev1.EnvVar {
		env := []corev1.EnvVar{{Name: "CNB_PLATFORM_API", Value: BuildpacksPlatformAPI}}
		if credential != "" {
			env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: dockerConfigDir})
		}
		return append(env, frameworkEnv(detected)...)
	}
	// The credential-holding phases mount one docker config each; the two
	// that run the repository's code mount neither.
	readMounts := []corev1.VolumeMount{workspace, layers, readDockerConfigMount()}
	pushMounts := []corev1.VolumeMount{workspace, layers, dockerConfigMount()}
	hermeticMounts := []corev1.VolumeMount{workspace, layers}

	// One phase, as a container. Every phase is told the same two things
	// about where it works — the layers directory each writes its part of
	// the build into, and, where it reads the repository, the build root —
	// and none of them draws colour: the collector ships these logs into
	// ClickHouse, where a colour escape is a character like any other.
	//
	// -no-color leads the arguments because the image reference trails them,
	// and a flag after a positional argument is a positional argument.
	phase := func(name string, mounts []corev1.VolumeMount, credential string, args ...string) corev1.Container {
		return corev1.Container{
			Name:         name,
			Image:        BuildpacksBuilderImage,
			Command:      []string{"/cnb/lifecycle/" + name},
			Args:         append([]string{"-no-color"}, args...),
			Env:          lifecycleEnv(credential),
			VolumeMounts: mounts,
		}
	}
	layersArg := "-layers=" + buildpacksLayersDir
	appArg := "-app=" + appDir

	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  ptr.To(cnbUID),
				RunAsGroup: ptr.To(cnbGID),
			},
			InitContainers: []corev1.Container{
				clone,
				// What is already in the registry under this tag, and what
				// the image will be built on. It writes analyzed.toml, which
				// is where export reads the run image from.
				phase("analyzer", readMounts, credentials.Read, layersArg, plan.Tag),
				// Which buildpacks claim the repository. This is the first
				// phase that runs somebody else's code.
				phase("detector", hermeticMounts, "", appArg, layersArg),
				// The layers the cache image still has.
				phase("restorer", readMounts, credentials.Read,
					append(cnbCacheArgs(cache), layersArg)...),
				// The repository's own build.
				phase("builder", hermeticMounts, "", appArg, layersArg),
			},
			Containers: []corev1.Container{
				// The push, and the only container in the pod that can. Its
				// report carries the digest of what it pushed; writing it to
				// the termination log puts it exactly where the reconciler
				// already reads BuildKit's metadata from — see
				// digestFromTerminationMessage, which reads both shapes, and
				// imageWithDigest, which reads the pod's containers rather
				// than its init containers, so the phase that pushes has to
				// be the pod's own container.
				phase("exporter", pushMounts, credentials.Push,
					append(cnbCacheArgs(cache), appArg, layersArg,
						"-report="+terminationLogPath, plan.Tag)...),
			},
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

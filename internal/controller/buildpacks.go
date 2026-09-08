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
	"fmt"
	"path"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/framework"
)

const (
	// BuildpacksPlatformAPI is the version of the CNB platform contract the
	// job speaks. The lifecycle refuses to start without being told one —
	// it has no default — and 0.13 is supported by every lifecycle from 0.17
	// on, so it survives the builder being moved a few releases either way.
	BuildpacksPlatformAPI = "0.13"

	// cnbUID and cnbGID are the builder image's own unprivileged user: the
	// CNB_USER_ID and CNB_GROUP_ID of BuildpacksBuilderImage, which is
	// pinned in images.go, so moving one means moving the other. The
	// lifecycle chowns its directories and drops to that user before it runs
	// anything from the repository, and entering as it already is what makes
	// both steps no-ops — which is why a buildpacks build needs none of the
	// privileges a BuildKit one does.
	//
	// The clone runs as the same user, because buildpacks write into the
	// application directory (npm's modules, the start script the Node
	// buildpack generates): a clone owned by anyone else fails the build
	// halfway through.
	cnbUID = int64(1001)
	cnbGID = int64(1000)

	// herokuUID and herokuGID are the same two numbers for HerokuBuilderImage,
	// and they are not the same two numbers. Heroku's builder enters as
	// 1000:1000 where Paketo's enters as 1001:1000, which is exactly the
	// mismatch that kills a lifecycle run in `Privileges()` — a pod that
	// entered as one unprivileged user cannot setuid to another. So the pod's
	// user follows the builder it runs rather than being a constant of the
	// platform.
	herokuUID = int64(1000)
	herokuGID = int64(1000)

	// Where the clone lands and where the lifecycle assembles the image.
	// Both are emptyDir volumes: a build gets its own, and nothing survives
	// it. What survives a build is the cache image the lifecycle exports —
	// see cnbCacheArgs — which the next build restores these layers from.
	buildpacksWorkspaceDir = "/workspace"
	buildpacksSourceDir    = buildpacksWorkspaceDir + "/source"
	buildpacksLayersDir    = "/layers"

	// buildpacksPlatformDir is the platform directory, which is the *only*
	// way a platform configures a buildpack. The lifecycle builds the
	// environment it runs a buildpack in with `env.NewBuildEnv(os.Environ())`
	// and that keeps a fixed list — CNB_STACK_ID, HOSTNAME, HOME, the proxy
	// variables and PATH's siblings — and discards everything else it
	// inherited; what it then adds back is one file per variable out of
	// `<platform>/env`, named for the variable and containing its value.
	// A BP_* variable on the container therefore reaches the lifecycle binary
	// and no buildpack, which is what `pack build --env` writes files for.
	//
	// It is Kitchen's own directory rather than the lifecycle's default
	// `/platform`, so that whatever the builder image has there is left
	// alone; the two phases that run buildpacks are pointed at it with
	// `-platform`, which only they accept.
	buildpacksPlatformDir = "/kitchen/platform"

	volumeWorkspace   = "workspace"
	volumeLayers      = "layers"
	volumePlatformEnv = "platform-env"
)

// buildPlatformEnvName is the ConfigMap one build Job's platform directory is
// mounted from: one key per variable, named exactly as the variable, which is
// what makes it a directory of env files. A ConfigMap key admits
// `[-._a-zA-Z0-9]+` and every name the platform sets is a BP_ or NODE_ one,
// so no name has to be escaped on the way in or read back out on the way out.
//
// It is named after the Job and owned by it, so the build's TTL collects it
// rather than leaving one object per build behind in the application
// namespace.
func buildPlatformEnvName(jobName string) string { return jobName + "-platform" }

// cnbBuilder is one Cloud Native Buildpacks builder: the image the five
// lifecycle phases run out of, and the unprivileged user that image enters
// as. The two travel together because they cannot disagree — the lifecycle
// drops to the builder's own user before it runs anything from the
// repository, and a pod that entered as a different one dies there.
type cnbBuilder struct {
	Image    string
	UID, GID int64
}

// buildpacksBuilder is which builder a build runs, from what detection made
// of the repository.
//
// Paketo's is the platform's builder and answers for everything it can build.
// The second is reached only where the first has no path at all: a Node
// repository locked by pnpm, which Paketo would install with npm against a
// lockfile npm never wrote (#568). A build with no detected framework — an
// explicit `strategy: buildpacks` over a repository detection did not
// recognise — is the zero Framework, and lands on Paketo's, which is where it
// has always landed.
func buildpacksBuilder(detected framework.Framework) cnbBuilder {
	if detected.Builder == framework.BuilderHeroku {
		return cnbBuilder{Image: HerokuBuilderImage, UID: herokuUID, GID: herokuGID}
	}
	return cnbBuilder{Image: BuildpacksBuilderImage, UID: cnbUID, GID: cnbGID}
}

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
//   - detect and build run the *buildpacks*, which run the repository's own
//     build — `npm install` and its lifecycle scripts, `pip install`,
//     whatever the buildpack invokes. They mount no registry credential at
//     all, so reading one out of `$DOCKER_CONFIG/config.json` from a
//     `postinstall` script finds an empty directory.
//   - analyze, restore and export talk to the registry. They are the
//     lifecycle's own binaries out of the pinned builder image, and nothing
//     from the repository runs in them. Which credential each holds follows
//     from what its phase does to the registry, not from which of them is
//     the push.
//   - restore only reads — the layers the cache image still has — so it
//     holds the read-only credential where the registry issues one.
//   - export and analyze both hold the one that can push. export pushes.
//     analyze is handed the output tag, and the lifecycle verifies read
//     *and* write access to it before anything else runs, deliberately, so
//     that a build which cannot publish fails in seconds rather than after
//     the whole build — so the read-only credential failed every buildpacks
//     build in its first phase, on exactly the installations that had gone
//     to the trouble of supplying one (#534).
//
// The principle the split is worth anything for is therefore not "only the
// phase that pushes holds the credential that can" — analyze needs it too —
// but *no phase that runs the repository's code holds any credential at
// all*, which is untouched: detect and build are the two that run somebody
// else's code, and they mount neither.
//
// `creator` is otherwise identical and was what this ran until the split: at
// platform API 0.13 it resolves its inputs exactly as the phases do, run
// image and all, so nothing about what is built or pushed changes.
//
// What the lifecycle is told about the repository comes from detection: a
// framework that starts a server of its own needs nothing, and one that
// builds into a directory of files needs the web-server buildpack pointed at
// that directory — there is no other way to say "serve this with NGINX". None
// of it is in this spec, though the pod is where it is read: it reaches the
// buildpacks as a directory of files, which is the only channel they have,
// and the pod names the object it is mounted from. See buildPlatformEnvName.
//
// Detection also decides *which* builder those five phases run out of, and
// that one is in this spec, because it is the image every phase names and the
// user the pod enters as. See buildpacksBuilder.
func buildpacksPod(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	plan buildPlan,
	detected framework.Framework,
	cache *kitchenv1alpha1.BuildCacheStatus,
	credentials registryCredentialsForPod,
	gitSecret string,
) corev1.PodTemplateSpec {
	// Which builder runs, and as whom. Detection chooses it, because the
	// choice is made by the same reading of the same directory that chooses
	// everything else the lifecycle is told — and because the builder and its
	// user are one decision: entering as the other builder's user is a build
	// that dies before it starts.
	builder := buildpacksBuilder(detected)
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
		// What detection tells the buildpacks. The object is written beside
		// the Job — see applyBuildPlatformEnv — and is there whether or not
		// there is anything to say, so that the pod spec does not depend on
		// what was detected.
		{Name: volumePlatformEnv, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: buildPlatformEnvName(plan.Job),
				},
			},
		}},
	}
	if gitSecret != "" {
		volumes = append(volumes, gitCredentialVolume(gitSecret))
	}

	// What every phase is told, whichever credential it holds: the platform
	// contract it speaks, and where to find a registry credential if it has
	// one. Both are the *lifecycle's* own inputs, which is why they are
	// environment variables at all — what detection made of the repository is
	// the buildpacks' input and reaches them as files instead, through the
	// platform directory below.
	lifecycleEnv := func(credential string) []corev1.EnvVar {
		env := []corev1.EnvVar{{Name: "CNB_PLATFORM_API", Value: BuildpacksPlatformAPI}}
		if credential != "" {
			env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: dockerConfigDir})
		}
		return env
	}
	// The credential-holding phases mount one docker config each — two of
	// them the pushing one, since analyze validates write access to the tag
	// it is given; the two that run the repository's code mount neither, and
	// mount the platform directory instead — they are the two phases that
	// run buildpacks, and the other three neither read it nor accept the
	// flag naming it.
	readMounts := []corev1.VolumeMount{workspace, layers, readDockerConfigMount()}
	pushMounts := []corev1.VolumeMount{workspace, layers, dockerConfigMount()}
	buildpackMounts := []corev1.VolumeMount{workspace, layers, {
		Name:      volumePlatformEnv,
		MountPath: buildpacksPlatformDir + "/env",
		ReadOnly:  true,
	}}

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
			Image:        builder.Image,
			Command:      []string{"/cnb/lifecycle/" + name},
			Args:         append([]string{"-no-color"}, args...),
			Env:          lifecycleEnv(credential),
			VolumeMounts: mounts,
		}
	}
	layersArg := "-layers=" + buildpacksLayersDir
	appArg := "-app=" + appDir
	// Only detect and build define this flag: the other three phases run no
	// buildpack, so passing it to them would be an unknown flag rather than
	// a redundant one.
	platformArg := "-platform=" + buildpacksPlatformDir

	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  ptr.To(builder.UID),
				RunAsGroup: ptr.To(builder.GID),
			},
			InitContainers: []corev1.Container{
				clone,
				// What is already in the registry under this tag, and what
				// the image will be built on. It writes analyzed.toml, which
				// is where export reads the run image from.
				//
				// It holds the credential that can push because it is given
				// the output tag and validates write access to it (#534),
				// which is the fail-fast the lifecycle offers and the whole
				// reason the tag is passed here at all.
				phase("analyzer", pushMounts, credentials.Push, layersArg, plan.Tag),
				// Which buildpacks claim the repository. This is the first
				// phase that runs somebody else's code.
				phase("detector", buildpackMounts, "", appArg, layersArg, platformArg),
				// The layers the cache image still has.
				phase("restorer", readMounts, credentials.Read,
					append(cnbCacheArgs(cache), layersArg)...),
				// The repository's own build.
				phase("builder", buildpackMounts, "", appArg, layersArg, platformArg),
			},
			Containers: []corev1.Container{
				// The push. It is not the only container holding a
				// credential that can write — analyze holds one too, to
				// validate the tag it is handed — but it is the only phase
				// that puts anything in the registry. Its report carries the
				// digest of what it pushed; writing it to the termination log
				// puts it exactly where the reconciler already reads
				// BuildKit's metadata from — see digestFromTerminationMessage,
				// which reads both shapes, and imageWithDigest, which reads
				// the pod's containers rather than its init containers, so the
				// phase that pushes has to be the pod's own container.
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

// buildPlatformEnv is what detection tells the buildpacks, as the platform
// directory's contents: one entry per variable, which the reconciler writes
// as one ConfigMap key per variable and the lifecycle reads back as one file
// per variable out of `<platform>/env`.
//
// It is a map rather than the ordered slice this used to be because the
// ConfigMap is a map: the API server sorts its keys, so the same repository
// produces the same object — and the same Job pod template, which cannot be
// edited after it is created — without anything here sorting anything. The
// framework package still sorts what it hands over, for its own reasons.
//
// The heap cap is added here rather than in the framework package because it
// is not a fact about the repository at all: it is the platform's own build
// ceiling, which the framework table has no way to know and which an operator
// can move. It goes on for every framework whose build runs under Node, the
// static ones included — a Vite bundle is assembled by the same tool a Nuxt
// server is, and dies the same way.
//
// It is a floor rather than the last word: a repository whose own `build`
// script sets NODE_OPTIONS again overrides it inside the process this
// configured, which is correct — an application that knows what its build
// needs should be able to say so. What it replaces is the *absence* of a
// cap, which nothing in a repository can supply for a build it cannot
// configure at all.
//
// A platform variable replaces a buildpack layer's rather than joining it,
// because the lifecycle applies the platform directory over the accumulated
// layer environment — so NODE_OPTIONS here also replaces the
// `--use-openssl-ca` node-engine contributes as a *default*, for the
// buildpacks that run after it. That is what `pack build --env NODE_OPTIONS`
// does too, and nothing in a Kitchen build reaches a registry behind a
// private CA; adding a flag rather than replacing the list would mean a
// `.append` file in a build-config directory beside this one.
func buildPlatformEnv(detected framework.Framework, heapMiB int64) map[string]string {
	env := make(map[string]string, len(detected.BuildEnv)+1)
	if detected.RunsNode && heapMiB > 0 {
		env["NODE_OPTIONS"] = fmt.Sprintf("--max-old-space-size=%d", heapMiB)
	}
	for _, v := range detected.BuildEnv {
		env[v.Name] = v.Value
	}
	return env
}

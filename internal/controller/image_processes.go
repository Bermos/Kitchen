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

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
)

// The buildpacks image that cannot start itself, and the deploy it has to
// refuse (#440).
//
// A Cloud Native Buildpacks image starts the process type its buildpacks
// declared. Nothing guarantees they declared one: an application whose only
// launch-time buildpack was `node-run-script` has its dependencies installed,
// its build script run and no process at all, and the container says so about
// a program the person who wrote the application has never heard of:
//
//	ERROR: failed to launch: determine start command:
//	when there is no default process a command is required
//
// on repeat, for as long as the environment exists. The pod's own account of
// it is a CrashLoopBackOff, which is where that sentence stays.
//
// So the platform reads the process types the lifecycle recorded on the image,
// at the moment it is already talking to the registry about this digest, and
// refuses the Release where a workload would start nothing: no process type in
// the image and no command in the project. The refusal names the workload, the
// image, and the one field that settles it — which since #440 is a field that
// works, because a command on a buildpacks image now reaches the launcher
// rather than replacing it.

// ImageProcessTypesReader reads the process types a Cloud Native Buildpacks
// image declares, and whether the image carries the lifecycle's metadata at
// all.
//
// It is an optional half of [ImageResolver] for the reason [ImageUserReader]
// is: the resolver is faked wherever a test acquires an image, and a fake with
// no opinion about an image's processes should go on having none. A resolver
// that does not implement this reports nothing known, and nothing known is
// what stops a deploy being refused on a guess.
type ImageProcessTypesReader interface {
	ImageProcessTypes(ctx context.Context, ref string) ([]string, bool, error)
}

// imageProcessTypes is what a buildpacks image says it can start, and whether
// the platform found out at all.
//
// A read that fails is never a refusal. The image exists and the lifecycle
// wrote what it wrote; what is missing is the platform's copy of it, and
// refusing a Release over a registry having a bad minute would be worse than
// the crash loop this exists to prevent.
func (r *BuildReconciler) imageProcessTypes(
	ctx context.Context, dockerConfig []byte, registry, ref string,
) ([]string, bool) {
	log := logf.FromContext(ctx)
	factory := r.Resolvers
	if factory == nil {
		factory = defaultImageResolver
	}
	resolver, err := factory(dockerConfig, registry)
	if err != nil {
		log.V(1).Info("the image's process types could not be read", "image", ref, "cause", err.Error())
		return nil, false
	}
	reader, ok := resolver.(ImageProcessTypesReader)
	if !ok {
		return nil, false
	}
	types, found, err := reader.ImageProcessTypes(ctx, ref)
	if err != nil {
		log.V(1).Info("the image's process types could not be read", "image", ref, "cause", err.Error())
		return nil, false
	}
	return types, found
}

// buildStrategyFor is what built one workload's image within a Build: its own
// where it declared a build, and the Build's otherwise. It is the same
// question [kitchenv1alpha1.Release.StrategyFor] answers of a Release, asked
// of the build that is about to make one.
func buildStrategyFor(build *kitchenv1alpha1.Build, workload string) kitchenv1alpha1.BuildStrategy {
	if workload == "" || workload == kitchenv1alpha1.WebProcessName {
		return build.Status.Strategy
	}
	for i := range build.Status.Workloads {
		if build.Status.Workloads[i].Name == workload {
			return build.Status.Workloads[i].Strategy
		}
	}
	return build.Status.Strategy
}

// workloadStart is what one workload of a unit was told to run: the runtime's
// for the web process, and the process's own for anything else.
//
// Both halves count and a preview's arguments count too. What is being asked
// is whether the *project* supplies a command anywhere, since a workload that
// does is handed to the launcher and starts whatever the image declares or
// does not.
func workloadStart(snapshot kitchenv1alpha1.ConfigSnapshot, workload string) []string {
	if workload == "" || workload == kitchenv1alpha1.WebProcessName {
		return concat(snapshot.Runtime.Command, snapshot.Runtime.Args, snapshot.Runtime.PreviewArgs)
	}
	process := kitchenv1alpha1.FindProcess(snapshot.Processes, workload)
	if process == nil {
		return nil
	}
	return concat(process.Command, process.Args)
}

func concat(lists ...[]string) []string {
	var all []string
	for _, list := range lists {
		all = append(all, list...)
	}
	return all
}

// startlessWorkloads are the workloads of one unit that would start nothing:
// a buildpacks image whose lifecycle declared no process type, deployed by a
// workload that supplies no command of its own.
//
// It answers nothing at all in every other case, and deliberately so. A
// Dockerfile image has an entrypoint the platform never reads; an image whose
// label could not be read is not evidence of anything; and a workload with a
// command starts that command, which is the whole of what #440 fixed.
func (r *BuildReconciler) startlessWorkloads(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	target buildTarget,
	snapshot kitchenv1alpha1.ConfigSnapshot,
) []kitchenv1alpha1.BuildArtifact {
	candidates := make([]kitchenv1alpha1.BuildArtifact, 0, 1)
	for _, artifact := range build.Artifacts() {
		if buildStrategyFor(build, artifact.Workload) != kitchenv1alpha1.BuildStrategyBuildpacks {
			continue
		}
		if len(workloadStart(snapshot, artifact.Workload)) > 0 {
			continue
		}
		if artifact.Artifact == nil || artifact.Artifact.Digest == "" {
			continue
		}
		candidates = append(candidates, artifact)
	}
	if len(candidates) == 0 {
		return nil
	}

	dockerConfig, err := r.targetDockerConfig(ctx, target)
	if err != nil {
		logf.FromContext(ctx).V(1).Info("the registry credential could not be read for the image's process types",
			"build", build.Name, "cause", err.Error())
		return nil
	}
	startless := make([]kitchenv1alpha1.BuildArtifact, 0, len(candidates))
	for _, artifact := range candidates {
		ref := attestation.ArtifactRef(artifact.Artifact.Repository, artifact.Artifact.Digest)
		types, known := r.imageProcessTypes(ctx, dockerConfig, target.Registry.Server, ref)
		if !known || len(types) > 0 {
			continue
		}
		startless = append(startless, artifact)
	}
	if len(startless) == 0 {
		return nil
	}
	return startless
}

// startlessWorkloadsMessage is what such a build says, and it is written to be
// the last thing anybody needs to read about it: which workload, which image,
// what the buildpacks left behind, and the field that settles it.
//
// It names the field because the alternative — the launcher's own sentence on
// a crash-looping pod — leaves a person with an application that works
// everywhere else and no move available inside the platform.
func startlessWorkloadsMessage(artifacts []kitchenv1alpha1.BuildArtifact) string {
	named := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		image := artifact.Artifact.Repository
		if artifact.Artifact.Digest != "" {
			image += "@" + artifact.Artifact.Digest
		}
		named = append(named, fmt.Sprintf("%s runs %s", artifact.Name(), image))
	}
	return fmt.Sprintf(
		"the buildpacks that built this image set no process type, so it declares nothing to start, "+
			"and %s with no command of its own. The container would exit with \"when there is no "+
			"default process a command is required\" and go on doing so. Give the workload a `command` — "+
			"`runtime.command` for the web process, the workload's own `command` for anything else — "+
			"which is run through the buildpacks launcher with the environment the buildpacks provided, "+
			"or add a buildpack that declares a process (a `Procfile` is the shortest way) and build again",
		strings.Join(named, "; "))
}

// reasonNoDefaultProcessType is a unit one of whose images cannot start. It
// fails the Build for the reason ImageUserUnverifiable does: nothing is wrong
// with the image, no Release can be deployed from it as asked, and the refusal
// belongs where somebody is already looking rather than on a pod that restarts
// for ever.
const reasonNoDefaultProcessType = "NoDefaultProcessType"

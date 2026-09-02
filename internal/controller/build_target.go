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

	ctrl "sigs.k8s.io/controller-runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The two ways of asking for a Dockerfile stage and not getting it, and what
// the platform says about each.
//
// A multi-stage Dockerfile ships its last stage unless something says
// otherwise, and the last stage is frequently not the runtime — which makes
// the wrong artifact a *successful* build, green all the way through, found
// at runtime or never. `dockerfileTarget` is how a project says which stage
// it meant; these are what stop it being asked for and quietly not applied.
//
//   - The strategy has no stages. The buildpacks lifecycle turns one
//     application directory into one image and has no notion of a target at
//     all, so the only thing it could do with one is ignore it. The build
//     fails before a Job exists, naming both settings, because the image it
//     would otherwise push is exactly the wrong image this feature exists to
//     stop shipping.
//   - The Dockerfile has no such stage. Nothing reads the file before the
//     build — the builder fetches the commit itself — so BuildKit is what
//     discovers it, and it says so in its own terms about an option nobody
//     typed. The failure is recognised here and restated as the stage, the
//     file, and where the name was declared.

// refuseUnbuildableTarget fails a build that named a Dockerfile stage and is
// not being built from a Dockerfile. A non-nil first return is the Build
// having been failed, and the caller returns it without creating anything.
func (r *BuildReconciler) refuseUnbuildableTarget(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	strategy kitchenv1alpha1.BuildStrategy,
) (*ctrl.Result, error) {
	target := buildDockerfileTarget(project, build)
	if target == "" || strategy != kitchenv1alpha1.BuildStrategyBuildpacks {
		return nil, nil
	}
	res, err := r.fail(ctx, build, project, reasonTargetNotSupported, fmt.Sprintf(
		"%s names %q as the Dockerfile stage to ship, and this commit is built with buildpacks, "+
			"which has no stages. Nothing was built, because a target that cannot be honoured "+
			"would have pushed a different image and reported success. Either clear the target, "+
			"or build this commit with the dockerfile strategy",
		dockerfileTargetSource(build), target))
	return &res, err
}

// targetNotFound reports whether a failed build is BuildKit refusing the
// stage it was asked for.
//
// It matches on the frontend's own words, in both the forms it has used, and
// only for a build that actually asked for a stage — the phrases are common
// enough in a build's own output that recognising them on a build with no
// target would be a diagnosis invented from somebody else's log line.
func targetNotFound(build *kitchenv1alpha1.Build, failure *kitchenv1alpha1.BuildFailureStatus) bool {
	if build.Status.DockerfileTarget == "" || failure == nil {
		return false
	}
	said := strings.ToLower(failure.Message + "\n" + strings.Join(failure.Log, "\n"))
	return strings.Contains(said, "target stage") ||
		strings.Contains(said, "failed to reach build target")
}

// targetNotFoundMessage is what a build that named a stage its Dockerfile
// does not have says on the Build, on the commit and in the dashboard.
//
// It names the stage, the file it was looked for in and where the name came
// from, because those are the three things BuildKit's own sentence leaves the
// reader to work out — and because the two places a target can be declared
// are not the same place to go and fix it.
func targetNotFoundMessage(project *kitchenv1alpha1.Project, build *kitchenv1alpha1.Build) string {
	return fmt.Sprintf(
		"%s has no stage named %q, which is the target %s asks for. "+
			"Name a stage the file declares with `FROM … AS <name>`, or clear the target to ship its last stage",
		buildDockerfilePath(project, build), build.Status.DockerfileTarget, dockerfileTargetSource(build))
}

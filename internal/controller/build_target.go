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
	"github.com/Bermos/Kitchen/internal/detect"
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
//
// Both are asked of every image the commit produces rather than of the
// project alone (#271): a unit builds several files, each with its own stage
// and its own strategy, so both refusals name the workload they are about —
// "the build asked for a stage it has not got" beside four workloads that did
// not is not an answer.

// refuseUnbuildableTarget fails a build one of whose images named a Dockerfile
// stage and is not being built from a Dockerfile. A non-nil first return is
// the Build having been failed, and the caller returns it without creating
// anything.
//
// The whole unit is refused rather than the one workload, because a release
// is all of it or none: shipping three workloads and refusing the fourth is
// the half-started unit the coordinated build exists to make impossible.
func (r *BuildReconciler) refuseUnbuildableTarget(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	plans []buildPlan,
) (*ctrl.Result, error) {
	plan := unbuildableTarget(plans)
	if plan == nil {
		return nil, nil
	}
	res, err := r.fail(ctx, build, project, reasonTargetNotSupported,
		unbuildableTargetMessage(project, build, *plan))
	return &res, err
}

// unbuildableTarget is the first image of this commit that named a stage and
// is not built from a Dockerfile, or nil when every one of them can be built
// as asked.
func unbuildableTarget(plans []buildPlan) *buildPlan {
	for i := range plans {
		if plans[i].DockerfileTarget != "" && plans[i].Strategy == kitchenv1alpha1.BuildStrategyBuildpacks {
			return &plans[i]
		}
	}
	return nil
}

// unbuildableTargetMessage is what such a build says, naming both settings and
// which image of the commit they are about.
func unbuildableTargetMessage(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	plan buildPlan,
) string {
	return fmt.Sprintf(
		"%s names %q as the Dockerfile stage to ship, and %s is built with buildpacks, "+
			"which has no stages. Nothing was built, because a target that cannot be honoured "+
			"would have pushed a different image and reported success. Either clear the target, "+
			"or build %s with the dockerfile strategy",
		dockerfileTargetSource(project, build, plan), plan.DockerfileTarget,
		plan.describe(), plan.describe())
}

// targetNotFound reports whether a failed build is BuildKit refusing the
// stage it was asked for.
//
// It matches on the frontend's own words, in both the forms it has used, and
// only for a build that actually asked for a stage — the phrases are common
// enough in a build's own output that recognising them on a build with no
// target would be a diagnosis invented from somebody else's log line.
func targetNotFound(plan buildPlan, failure *kitchenv1alpha1.BuildFailureStatus) bool {
	if plan.DockerfileTarget == "" || failure == nil {
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
// reader to work out — and because the three places a target can be declared
// are not the same place to go and fix it. Which workload is in the last of
// those: one commit builds several files now, and the source clause is what
// says whose stage this was and whether the workload named it itself.
func targetNotFoundMessage(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	plan buildPlan,
) string {
	return fmt.Sprintf(
		"%s has no stage named %q, which is the target %s asks for. "+
			"Name a stage the file declares with `FROM … AS <name>`, or clear the target to ship its last stage",
		targetDockerfile(project, build, plan), plan.DockerfileTarget,
		dockerfileTargetSource(project, build, plan))
}

// targetDockerfile is the file a plan's stage was looked for in, spelled the
// way the build spells it.
//
// The web process's is the project's own; a workload's is its own, which the
// plan a running build observes does not carry — see plansUnderway — so it is
// read off the workload's declaration, falling back to what the CRD's default
// says a workload with no path of its own builds.
func targetDockerfile(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	plan buildPlan,
) string {
	switch own := workloadBuild(project, build, plan.Workload); {
	case plan.isWeb():
		return buildDockerfilePath(project, build)
	case own != nil:
		return detect.NormalizeDockerfile(own.DockerfilePath)
	default:
		return detect.NormalizeDockerfile("")
	}
}

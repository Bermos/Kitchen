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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/detect"
	"github.com/Bermos/Kitchen/internal/provider"
	"github.com/Bermos/Kitchen/internal/repoconfig"
)

// One commit, several images (#271).
//
// A project is one deployable unit, and a unit can be more than one workload:
// a monorepo ships an API, a web front end and a worker from one repository,
// and they deploy and roll back together or the preview of a pull request is
// three unrelated environments that cannot find each other. What makes that
// one thing rather than three projects is that one Build produces all of it —
// which is what this file plans.
//
// The shape is deliberately additive. The web process's build is exactly the
// build that was there before, under the Job name it always had, pushing to
// the repository it always pushed to; a workload that declares a build of its
// own is one more Job beside it. A project with no such workload plans one
// entry and behaves in every respect as it did.
//
// Three properties hold the "coordinated release" claim up:
//
//   - **They are created together.** Every plan's Job is created in the same
//     pass, so nothing can half-start a unit.
//   - **The Build is over when all of them are.** A Build succeeds only once
//     every workload pushed, and the first failure fails the whole Build
//     naming itself — three of four workloads a commit ahead of the fourth is
//     worse than a deploy that did not happen.
//   - **The digests are frozen together.** One Release records the image each
//     workload was built to, so restoring it restores that exact set.

const (
	// labelWorkload names which workload of a unit a build Job belongs to.
	// It is absent on the web process's Job, which is the build that was
	// always there — and so is the one selector that finds a unit's extra
	// builds without finding its original one.
	labelWorkload = "kitchen.bermos.dev/workload"

	// maxJobNameLength is what a Job's name has to fit in: it becomes the
	// value of the `batch.kubernetes.io/job-name` label on every pod the Job
	// makes, and a label value is capped at 63 characters.
	maxJobNameLength = 63
)

// buildPlan is one image a Build produces: where in the repository it comes
// from, how it is built, what Job runs it and where it is pushed.
type buildPlan struct {
	// Workload is the process this plan builds, empty for the web process —
	// which is the project's own image and has no entry in the process list.
	Workload string

	// Strategy is how the image is produced, after detection has had its say
	// for the web process and as declared for a workload.
	Strategy kitchenv1alpha1.BuildStrategy

	// RootDirectory is the directory of the repository this image is built
	// from, empty for the repository itself.
	RootDirectory string

	// DockerfilePath is the Dockerfile a dockerfile build reads, relative to
	// RootDirectory.
	DockerfilePath string

	// DockerfileTarget is the stage of that Dockerfile to ship, empty for
	// its last stage. It is on the plan rather than read off the project
	// where it is needed, because a stage is a fact about the file it is a
	// stage of and one commit now builds several files.
	DockerfileTarget string

	// Job is the build Job's name in the application namespace.
	Job string

	// Repository is where the image is pushed, without a tag or digest, and
	// Tag the reference the builder is told to push.
	Repository string
	Tag        string
}

// isWeb reports whether this plan is the project's own image.
func (p buildPlan) isWeb() bool { return p.Workload == "" }

// describe names the plan in a message a person reads. The web process is
// named as such rather than left blank, because "the build failed" beside
// three workloads that did not is not an answer.
func (p buildPlan) describe() string {
	if p.isWeb() {
		return "the web process"
	}
	return "workload " + p.Workload
}

// buildWorkloads is the workload list this Build builds: the project's, with
// the commit's own kitchen.json applied over it.
//
// It reads the commit's file rather than the project alone for the same
// reason the Release's snapshot does: a commit that adds a workload is a
// commit that has to build it, and a commit that removes one must not.
func buildWorkloads(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
) []kitchenv1alpha1.ProcessSpec {
	return repoconfig.Processes(project.Spec.Processes, build.Status.Config)
}

// buildPlansFor is every image this Build produces, the web process first.
//
// The web plan is passed in rather than derived, because resolving it is the
// reconciler's whole detection dance — the strategy the platform settled on,
// the framework it read out of the repository — and doing it twice would be
// two answers to one question.
func buildPlansFor(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	registry provider.RegistryTarget,
	web buildPlan,
) []buildPlan {
	plans := []buildPlan{web}
	for _, workload := range buildWorkloads(project, build) {
		if workload.Build == nil {
			continue
		}
		repository := workloadRepository(registry.Prefix, project.Name, workload.Name)
		strategy := workload.Build.EffectiveStrategy()
		// This workload's own stage where it named one, and the unit's where
		// it did not — one chain, stated once in buildDockerfileTarget. The
		// name itself is spelled by the one place that says what a stage may
		// be called, exactly as the two paths below are.
		//
		// A workload built with buildpacks inherits nothing, though: the
		// lifecycle has no stages, so the unit's stage is not a stage of
		// anything this image builds, and a unit that named one would
		// otherwise be unable to ship a single buildpacks workload. One it
		// named *itself* it keeps and is refused for — that is a mistake
		// somebody made rather than a setting standing in.
		target := detect.NormalizeTarget(workload.Build.DockerfileTarget)
		if strategy == kitchenv1alpha1.BuildStrategyDockerfile {
			target = buildDockerfileTarget(project, build, target)
		}
		plans = append(plans, buildPlan{
			Workload: workload.Name,
			Strategy: strategy,
			// A workload's build root is a build root, so it is spelled by
			// the one place that says what one is: `apps/api`, `./apps/api/`
			// and `apps/api` are one directory here exactly as they are for
			// the project's own, and this workload's Dockerfile is resolved
			// relative to this workload's root — which is the whole of what
			// its build sees.
			RootDirectory:    detect.NormalizeRoot(workload.Build.RootDirectory),
			DockerfilePath:   detect.NormalizeDockerfile(workload.Build.DockerfilePath),
			DockerfileTarget: target,
			Job:              workloadJobName(build.Name, workload.Name),
			Repository:       repository,
			Tag:              fmt.Sprintf("%s:%s", repository, shortSHA(build.Spec.Git.SHA)),
		})
	}
	return plans
}

// webPlan is the project's own image: the build that was there before any of
// this, under the name and in the repository it always had.
func webPlan(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	registry provider.RegistryTarget,
	strategy kitchenv1alpha1.BuildStrategy,
) buildPlan {
	repository := fmt.Sprintf("%s/%s", registry.Prefix, project.Name)
	return buildPlan{
		Strategy:       strategy,
		RootDirectory:  buildRootDir(project),
		DockerfilePath: buildDockerfilePath(project, build),
		// The unit's own stage: the commit's kitchen.json where the file
		// declared one, and the project's setting where it did not.
		DockerfileTarget: buildDockerfileTarget(project, build, ""),
		Job:              build.Name,
		Repository:       repository,
		Tag:              fmt.Sprintf("%s:%s", repository, shortSHA(build.Spec.Git.SHA)),
	}
}

// workloadRepository is where one workload's image is pushed: a repository
// beside the project's own, named after the workload.
//
// Beside rather than under — `shop-api`, not `shop/api` — because a registry
// path segment is where a great many registries put the account, and the
// bundled one is not the only registry a project can push to. The project's
// own name is the prefix, so everything a project pushes still sorts together
// and is covered by the one credential, the one retention rule and the one
// quota.
func workloadRepository(prefix, projectName, workload string) string {
	return fmt.Sprintf("%s/%s-%s", prefix, projectName, workload)
}

// workloadJobName is the Job one workload's build runs as.
//
// It reads as the build plus the workload, which is what somebody looking at
// the namespace needs — and it is cut with a hash of the whole when the two
// together will not fit a label value, the same trick cacheSlug uses, so that
// two workloads whose names share a prefix can never land on one Job.
func workloadJobName(buildName, workload string) string {
	return fitJobName(buildName + "-" + workload)
}

// fitJobName cuts a derived Job name down to what a Job name has to fit in,
// keeping it unique: the whole name is hashed and the digest replaces the tail
// that was cut, so two names sharing a prefix can never land on one Job.
//
// It is shared because both places that derive one have the same 63-character
// ceiling for the same reason — the name becomes the value of the
// `batch.kubernetes.io/job-name` label on every pod the Job makes — and two
// implementations of one truncation is a collision waiting for the first long
// project name.
func fitJobName(name string) string {
	if len(name) <= maxJobNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	digest := hex.EncodeToString(sum[:4])
	return strings.TrimRight(name[:maxJobNameLength-len(digest)-1], "-.") + "-" + digest
}

// plansUnderway is what this Build is actually waiting on: the plans it
// recorded when it created their Jobs, rather than the plans the project would
// produce if it were asked again.
//
// The difference matters exactly once, and it is a stall. The workload list is
// a live project setting for a project that carries no kitchen.json, so a
// workload added while a build was running would appear in a recomputed plan
// with no Job behind it — and a Build that waits for a Job nobody is ever
// going to create waits for ever, reporting Running and moving nowhere. What
// it started is a fact about the past, so it is read off the record it wrote.
//
// A Build with no recorded workloads falls back to planning, which is what a
// Build from before this existed looks like and what every single-image
// project looks like: the answer is the web plan alone either way.
func plansUnderway(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	registry provider.RegistryTarget,
	web buildPlan,
) []buildPlan {
	// The stage each image was told to produce is read back the same way,
	// and for the same reason: a diagnosis is made against what this Build
	// was told rather than against a setting that has moved since it
	// started.
	web.DockerfileTarget = build.Status.DockerfileTarget
	if len(build.Status.Workloads) == 0 {
		return buildPlansFor(project, build, registry, web)
	}
	plans := make([]buildPlan, 0, len(build.Status.Workloads)+1)
	plans = append(plans, web)
	for _, workload := range build.Status.Workloads {
		// Only what an observation needs. The strategy and the directories
		// were inputs to a pod spec that has already been written and cannot
		// be edited, so recording them would be recording something nothing
		// can act on; the stage is here because a failed build is diagnosed
		// against it.
		plans = append(plans, buildPlan{
			Workload:         workload.Name,
			Job:              workload.Job,
			DockerfileTarget: workload.DockerfileTarget,
			Repository:       workload.Repository,
			Tag:              workload.Repository + ":" + shortSHA(build.Spec.Git.SHA),
		})
	}
	return plans
}

// workloadBuild is one workload's own build declaration, or nil for the web
// process and for a workload that declares none.
//
// It is a lookup by name rather than a field on the plan because the plans a
// running Build observes carry only what an observation needs — see
// plansUnderway — and the two things a message about a target has to name,
// the file and where the stage was declared, are not among them.
func workloadBuild(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	name string,
) *kitchenv1alpha1.ProcessBuildSpec {
	if name == "" {
		return nil
	}
	for _, workload := range buildWorkloads(project, build) {
		if workload.Name == name {
			return workload.Build
		}
	}
	return nil
}

// planOutcome is what one plan's Job has done: whether it exists, and how it
// ended.
type planOutcome struct {
	Plan buildPlan
	Job  *batchv1.Job
	// Complete and Failed are the Job's own conditions; Message is what the
	// Job said about a failure.
	Complete bool
	Failed   bool
	Message  string
}

// workloadStatusFor is the row a plan's outcome writes onto the Build.
func workloadStatusFor(outcome planOutcome, image string) kitchenv1alpha1.WorkloadBuildStatus {
	status := kitchenv1alpha1.WorkloadBuildStatus{
		Name:             outcome.Plan.Workload,
		Job:              outcome.Plan.Job,
		Repository:       outcome.Plan.Repository,
		DockerfileTarget: outcome.Plan.DockerfileTarget,
		Phase:            kitchenv1alpha1.BuildRunning,
	}
	switch {
	case outcome.Failed:
		status.Phase = kitchenv1alpha1.BuildFailed
		status.Message = outcome.Message
	case outcome.Complete:
		status.Phase = kitchenv1alpha1.BuildSucceeded
		status.Image = image
	case outcome.Job == nil:
		status.Phase = kitchenv1alpha1.BuildQueued
	}
	return status
}

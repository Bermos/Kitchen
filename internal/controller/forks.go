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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// What a pull request from a fork gets (#422).
//
// A fork's head is a stranger's code. On a public repository anybody can open
// a pull request, and the head commit is reachable in the *project's* own
// repository through `refs/pull/N/head` — so before this existed, the platform
// built it with the project's registry credential and deployed it into a
// preview environment carrying the project's own secrets, with a claim in
// preview mode `shared` bound to production's. Previews are on by default, so
// that was every repo-backed project on every public repository.
//
// The gate is at the receiver rather than deep in the build, because that is
// the one place the platform holds the fact: the delivery says where the head
// lives, and by the time a build runs the head is just a SHA the base
// repository can fetch. The two readers of the decision are the receiver,
// which declines to create a Build at all, and the build controller, which
// declines to route a preview for a fork the project only asked to build.
//
// The refusal is reported in the shape the preview ceiling (#294) already
// refuses in — a commit status under the project's own preview context, plus
// the preview comment — because they are the same event to the person reading
// the pull request: the platform deliberately did not do the thing they were
// expecting, and it is saying so where they are looking.

// ForkPolicyFor resolves what a fork pull request gets on this project: the
// project's own `spec.previews.forks`, never more than the platform's
// `spec.previews.forksMax` allows.
//
// A platform the caller cannot read gives the compiled-in ceiling rather than
// an open one, for the same reason platformPreviewMax does: a limit that
// disappears when an API call fails is not a limit. That compiled-in ceiling
// is `full`, which forbids nothing — the safe default is the project's own
// `none`, and a ceiling that clamped to `none` on a failed read would refuse
// every fork build on an installation that had deliberately allowed them.
func ForkPolicyFor(
	ctx context.Context,
	reader client.Reader,
	project *kitchenv1alpha1.Project,
) kitchenv1alpha1.ForkPolicy {
	return project.Spec.Previews.ForksUnder(platformForksMax(ctx, reader))
}

// platformForksMax is the estate-wide ceiling, read off the singleton.
func platformForksMax(ctx context.Context, reader client.Reader) kitchenv1alpha1.ForkPolicy {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := reader.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return kitchenv1alpha1.ForkPolicyFull
	}
	return kitchen.Spec.Previews.EffectiveForksMax()
}

// ForkRefusalMessage is the one sentence a fork refusal is reported with — on
// the commit status, in the pull request comment and in the activity feed.
//
// It says three things, because a refusal that says fewer is a refusal
// somebody has to read the source for: what was refused, why a fork is refused
// at all, and which setting moves it. `policy` is the policy in force, so the
// sentence distinguishes "nothing was built" from "it was built and not
// published".
func ForkRefusalMessage(
	project *kitchenv1alpha1.Project,
	policy kitchenv1alpha1.ForkPolicy,
	forkRepo string,
) string {
	origin := "a fork"
	if forkRepo != "" && forkRepo != kitchenv1alpha1.UnknownForkRepo {
		origin = fmt.Sprintf("the fork %s", forkRepo)
	}
	did := "so nothing was built and no preview environment was created"
	options := "`build` compiles a fork's commit and publishes nothing, " +
		"`full` treats a fork as the project's own branch"
	if policy.BuildsForks() {
		did = "so the commit was built and no preview environment was created"
		options = "`full` treats a fork as the project's own branch, secrets included"
	}
	return fmt.Sprintf(
		"this pull request's head commit is in %s rather than in %s's own repository %s, "+
			"and %s does not publish pull requests from forks — %s. "+
			"A fork's commit would otherwise run with this project's own environment "+
			"variables, secrets and claim bindings. A project admin can change it with "+
			"spec.previews.forks on project %s: %s",
		origin, project.Name, project.Spec.Source.GitSource().Repo, project.Name, did,
		project.Name, options)
}

// ForkReporter posts a fork refusal back to the pull request that asked. It is
// the exported door onto gitReporting, for the receiver — which has to refuse
// before a Build exists and so cannot go through the build controller.
type ForkReporter struct {
	// Client reads the Project's Connection, its credential and the platform
	// singleton.
	Client client.Client
	// Factory resolves the git provider. Nil takes gitprovider.Default; tests
	// inject fakes.
	Factory gitprovider.Factory
}

// ReportForkRefused tells the pull request that its head is a fork and the
// project does not publish forks, in the two places the preview ceiling
// already speaks: a commit status under `kitchen/<project>/preview`, beside
// the build's verdict rather than over it, and the preview comment under the
// marker a preview of this pull request would have used — so a preview that
// appears later (because somebody changed the setting and pushed) rewrites it
// in place rather than leaving the refusal underneath.
//
// It is best effort, like every other thing this platform says to a forge. A
// provider that cannot be reached does not change what was refused.
func (f ForkReporter) ReportForkRefused(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	revision kitchenv1alpha1.GitRevision,
	pullRequest int32,
	reason string,
) {
	gitReporting{Client: f.Client, Factory: f.Factory}.reportPreviewBlocked(ctx, project, previewBlock{
		Revision:    revision,
		PullRequest: pullRequest,
		Environment: PreviewEnvironmentName(project.Name, pullRequest),
		Headline:    "no preview: this pull request comes from a fork",
		Reason:      reason,
		FollowUp: "Nothing is queued, and pushing again changes nothing: somebody with admin " +
			"on this project has to allow fork pull requests first.",
	})
}

// forkPreviewAllowed is the fork gate on the build controller's side (#422):
// false means this build's release is deliberately not being published, and
// the pull request has been told so.
//
// The receiver refuses a fork outright under `none` — no Build is created at
// all — so what reaches here is a fork the project asked to *build*, and the
// answer is that a build is all it gets. It is asked again rather than
// inferred from the Build's existence because the setting can move between the
// delivery and the deploy, and because a preview created before this gate
// existed must stop being deployed to as well.
func (r *BuildReconciler) forkPreviewAllowed(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	pullRequest int32,
) bool {
	if !build.Spec.Git.IsFork() {
		return true
	}
	policy := ForkPolicyFor(ctx, r.Client, project)
	if policy.PreviewsForks() {
		return true
	}
	reason := ForkRefusalMessage(project, policy, build.Spec.Git.ForkRepo)
	logf.FromContext(ctx).Info("refused a preview environment for a pull request from a fork",
		"project", project.Name, "pullRequest", pullRequest,
		"fork", build.Spec.Git.ForkRepo, "forks", policy)
	ForkReporter{Client: r.Client, Factory: r.GitProviders}.
		ReportForkRefused(ctx, project, build.Spec.Git, pullRequest, reason)
	r.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventPreviewRefused,
		Project: project.Name,
		Build:   build.Name,
		Message: reason,
	})
	return false
}

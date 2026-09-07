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
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/gitprovider"
	"github.com/Bermos/Kitchen/internal/repoconfig"
)

// prepareBuild is everything the commit itself settles before anything is
// created: what its kitchen.json declares, and whether there is anything to
// build at all.
//
// The two are one step because the second is answered partly by the first —
// `build.skipUnchanged` in the file overrides the project's setting, so that
// the commit which moves shared code out of the build root can turn the skip
// off in the same change. The configuration is read once and recorded, since
// the build Job is planned from it and the Release is merged from it on a
// later reconcile, which cannot read the repository again without spending a
// second request on a question that has an answer.
//
// Both are asked before the concurrency gate rather than after it: a build
// that is not going to happen should not wait for a slot it will never use,
// which for a monorepo's fan-out is the whole of the point (#500).
func (r *BuildReconciler) prepareBuild(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
) (*ctrl.Result, error) {
	if stop, err := r.readConfig(ctx, build, project); stop != nil {
		return stop, err
	}
	return r.skipUnchangedSource(ctx, build, project)
}

// ReasonSourceUnchanged is a build that was not run because the source at the
// project's build root is byte-identical to the one the last successful build
// of this branch used (#500).
//
// It is exported because it is not a fault and the API has to be able to say
// so: `internal/api/conditions.go` classifies it `info`, and a dashboard that
// coloured it from the condition's status alone would draw a project that did
// exactly what it was asked to do as a broken one.
const ReasonSourceUnchanged = "SourceUnchanged"

// A push to a monorepo of eight services matches eight projects, and seven of
// them build and deploy source that did not change. The compliance instinct —
// rebuild everything, so the platform is never out of step with the repository
// — does not survive contact with the record it is protecting: a release
// history where seven rows in eight are no-ops makes "when did this service
// last change" unanswerable by looking.
//
// The exact answer is neither "always build" nor a path filter, which has to
// be maintained by hand and lies the moment a shared library outside the build
// root changes. Git already holds it: the tree object at
// `<commit>:<rootDirectory>` is the identity of the source this project
// builds, so an equal tree object is byte-identical source and there is
// nothing to build. It needs no reproducible builds, costs one request, and
// cannot drift because it is derived rather than declared.
//
// **The skip is recorded, not silent.** A Build in the `Skipped` phase naming
// the commit, the tree object and the build it matched is a stronger record
// than a rebuild produces, because it asserts something about the *source* —
// this commit changed nothing here — rather than producing an artifact that
// may or may not be byte-identical for reasons nobody controls.
//
// Four things always build, and each of them is a case where the derivation
// cannot be trusted to mean what it says:
//
//   - The project has not asked for it. `spec.build.skipUnchanged` is off by
//     default, because a monorepo whose services genuinely share code outside
//     their root directories wants the fan-out and the platform cannot tell
//     which kind of repository it is looking at.
//   - There is no previous tree object to compare — the first build of a
//     project, the first build of a branch, or a project whose provider
//     cannot name the tree at this path.
//   - The build was asked for by a person. A manual rebuild is what somebody
//     reaches for when the derivation is wrong, so it may not be answered by
//     the derivation.
//   - The last build of this branch did not succeed. "There is nothing to
//     build" is only true when the artifact the last build produced exists.
func (r *BuildReconciler) skipUnchangedSource(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
) (*ctrl.Result, error) {
	if !build.FromRepository() || build.Status.SourceTree != nil {
		return nil, nil
	}
	log := logf.FromContext(ctx)
	root := buildRootDir(project)

	object, err := r.treeAt(ctx, project, build.Spec.Git.SHA, root)
	if err != nil {
		// Every failure here is the platform being unable to look, and none
		// of them is worth holding a deploy over: the comparison is an
		// optimisation, and not making it builds the commit, which is what
		// the platform did before this existed. The next build resolves it
		// again and the record catches up.
		log.V(1).Info("the source tree could not be resolved, so the commit is built",
			"build", build.Name, "project", project.Name, "root", root, "cause", err.Error())
		return nil, nil
	}

	build.Status.SourceTree = &kitchenv1alpha1.SourceTreeStatus{Object: object, Path: root}
	matched := ""
	if mayBeSkipped(build, project) {
		previous, err := r.lastBuiltTree(ctx, build, project)
		if err != nil {
			return &ctrl.Result{}, err
		}
		if previous != nil && previous.Status.SourceTree.Object == object {
			matched = previous.Name
		}
	}
	if matched == "" {
		// Recorded before anything is created, for the reason the commit's
		// own configuration is: what follows creates a Job, and a build
		// running against an identity nothing wrote down leaves the next
		// push nothing to compare.
		if err := r.Status().Update(ctx, build); err != nil {
			return &ctrl.Result{}, err
		}
		return nil, nil
	}
	return &ctrl.Result{}, r.skipBuild(ctx, build, project, matched)
}

// mayBeSkipped is the two questions that are about this build rather than
// about its source: has the project (or this commit's own kitchen.json) asked
// for the skip, and did a person ask for this build?
func mayBeSkipped(build *kitchenv1alpha1.Build, project *kitchenv1alpha1.Project) bool {
	if !repoconfig.SkipUnchanged(build.Status.Config, project.Spec.Build.SkipUnchanged) {
		return false
	}
	// A rebuild through the API carries the caller who asked for it. It is
	// the escape hatch for exactly this feature being wrong — a build whose
	// inputs are outside the repository, a registry that lost the image, a
	// commit somebody wants built again for its own sake — so it is never
	// answered with "there was nothing to build".
	return build.Annotations[audit.RequestedByAnnotation] == ""
}

// treeAt resolves the tree object at one commit and one build root through
// the project's git provider — one request, and never a clone.
func (r *BuildReconciler) treeAt(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	ref, root string,
) (string, error) {
	provider, conn, err := r.gitProviderFor(ctx, project)
	if err != nil {
		return "", err
	}
	resolver, ok := gitprovider.Trees(provider)
	if !ok {
		return "", fmt.Errorf("the %s provider cannot name a tree object", conn.Spec.Provider)
	}
	// Every failure reads the same way to the caller — ErrTreeUnavailable for
	// a provider that cannot name this tree, ErrFileNotFound for a build root
	// that is not there at this commit, and anything else for an outage —
	// because all three mean the same thing here: nothing to compare, so
	// build.
	object, err := resolver.TreeAt(ctx, project.Spec.Source.GitSource().Repo, ref, root)
	if err != nil {
		return "", err
	}
	if object == "" {
		return "", fmt.Errorf("the provider named no tree object at %s", ref)
	}
	return object, nil
}

// lastBuiltTree is the newest succeeded build of this project and this branch
// that recorded a source tree at the same build root, or nil.
//
// Three narrowings, and each is load-bearing:
//
//   - **Succeeded**, because "there is nothing to build" is only true when
//     the last build produced the artifact this one would produce. Matching a
//     failed build would answer a retry of a broken build with a skip.
//   - **The same branch**, because a pull request whose changes are all
//     outside this build root still needs an environment to be reviewed in,
//     and its first build is what creates one. A later push to that branch
//     that changes nothing here is skipped against the branch's own last
//     build, which leaves the preview serving the artifact it already had.
//   - **The same build root**, because a project whose root directory moved
//     is building different source under the same name, and an object
//     resolved at the old path says nothing about the new one.
func (r *BuildReconciler) lastBuiltTree(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
) (*kitchenv1alpha1.Build, error) {
	list := &kitchenv1alpha1.BuildList{}
	if err := r.List(ctx, list, client.InNamespace(build.Namespace),
		client.MatchingLabels{kitchenv1alpha1.ProjectLabel: project.Name}); err != nil {
		return nil, err
	}
	root := buildRootDir(project)
	candidates := make([]*kitchenv1alpha1.Build, 0, len(list.Items))
	for i := range list.Items {
		candidate := &list.Items[i]
		if candidate.Name == build.Name ||
			candidate.Spec.ProjectRef.Name != project.Name ||
			candidate.Status.Phase != kitchenv1alpha1.BuildSucceeded ||
			candidate.Spec.Git.Branch != build.Spec.Git.Branch {
			continue
		}
		tree := candidate.Status.SourceTree
		if tree == nil || tree.Object == "" || tree.Path != root {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	// Newest first, with the name as the tiebreak: two builds created in the
	// same second must not make the answer depend on the order a list came
	// back in.
	slices.SortFunc(candidates, func(left, right *kitchenv1alpha1.Build) int {
		if !left.CreationTimestamp.Equal(&right.CreationTimestamp) {
			return right.CreationTimestamp.Time.Compare(left.CreationTimestamp.Time)
		}
		return strings.Compare(right.Name, left.Name)
	})
	return candidates[0], nil
}

// skipBuild ends a Build that has nothing to build.
//
// It is terminal the moment it is written: no Job is created, so no Release is
// cut and nothing is promoted, and `isTerminal` returns true for the phase so
// the stall diagnosis — which is about a Job with no pod — can never be asked
// about a build that never had one. There is nothing to come back for, which
// is why it answers with an error alone and never a requeue.
func (r *BuildReconciler) skipBuild(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	matched string,
) error {
	build.Status.SourceTree.MatchedBuild = matched
	message := skipMessage(build.Status.SourceTree.Path, matched)
	if err := r.Audit.Record(ctx, audit.Transition{
		Object:      build,
		Kind:        audit.KindBuild,
		Controller:  actorBuildController,
		Correlation: correlationFor(build),
		From:        string(build.Status.Phase),
		To:          string(kitchenv1alpha1.BuildSkipped),
		Project:     project.Name,
		Reason:      message,
		Details: map[string]any{
			"commit":        build.Spec.Git.SHA,
			"branch":        build.Spec.Git.Branch,
			"tree":          build.Status.SourceTree.Object,
			"rootDirectory": build.Status.SourceTree.Path,
			"matchedBuild":  matched,
		},
	}); err != nil {
		return err
	}
	build.Status.Phase = kitchenv1alpha1.BuildSkipped
	build.Status.CompletedAt = ptr.To(metav1.Now())
	meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
		Type: condReady, Status: metav1.ConditionFalse, Reason: ReasonSourceUnchanged,
		Message: message, ObservedGeneration: build.Generation,
	})
	r.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventBuildSkipped,
		Project: project.Name,
		Build:   build.Name,
		Message: fmt.Sprintf("build %s was skipped: %s", build.Name, message),
	})
	// The commit is told, in the one place its author is looking. It is a
	// success rather than a neutral state because no provider's commit status
	// API has one, and because nothing went wrong: the platform read the
	// commit, understood it and had nothing to do.
	r.git().reportBuild(ctx, project, build, gitprovider.CommitSuccess, message)
	logf.FromContext(ctx).Info("build skipped: the source tree is unchanged",
		"build", build.Name, "project", project.Name, "commit", build.Spec.Git.SHA,
		"tree", build.Status.SourceTree.Object, "matched", matched)
	return r.Status().Update(ctx, build)
}

// skipMessage is the one sentence the condition, the commit status, the
// activity feed and the dashboard all carry.
func skipMessage(root, matched string) string {
	where := "the repository"
	if root != "" {
		where = root
	}
	return fmt.Sprintf("nothing was built: this commit's source at %s is byte-identical to the "+
		"source %s built, so there is no new artifact to make", where, matched)
}

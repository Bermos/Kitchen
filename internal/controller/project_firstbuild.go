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
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/detect"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// ensureInitialBuild builds the production branch as it stands the first time
// a project is reconciled.
//
// A webhook only ever says what has just changed, so a project whose
// repository is finished — which is most of them, since people connect a
// repository they already have — had nothing to deploy until somebody pushed
// a commit for the sake of pushing one. This is the push that never happened:
// the production branch's tip, resolved once, built like any other commit.
//
// It happens here rather than in the API handler for the reason every
// credential does: resolving the tip means asking the provider with the
// connection's token, and the token is the operator's. It also means the
// project's first build waits for its connections to be usable, which is
// exactly when a build could succeed.
//
// Failures are conditions rather than reconcile errors — a project whose
// first build could not be worked out is still a project, and the next push
// builds it — except for the ones worth retrying, which requeue.
func (r *ProjectReconciler) ensureInitialBuild(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	conn *kitchenv1alpha1.Connection,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	// Recorded once and never revisited: a first build somebody cancelled or
	// deleted is a decision, and seeding it again would overrule it.
	if project.Status.InitialBuildRef != nil {
		return false
	}
	// A project that has built something already has nothing to seed. That
	// covers the race where a push lands between the project being created
	// and this running, and it covers every project that existed before this
	// did.
	if built, err := r.hasBuild(ctx, project); err != nil {
		setCond(condInitialBuild, metav1.ConditionFalse, "BuildsUnreadable", err.Error())
		return true
	} else if built {
		return false
	}

	provider, err := r.resolveProvider(ctx, project, conn)
	if err != nil {
		setCond(condInitialBuild, metav1.ConditionFalse, "ProviderError", err.Error())
		return !errors.Is(err, gitprovider.ErrUnsupportedProvider)
	}
	resolver, ok := gitprovider.Revisions(provider)
	if !ok {
		setCond(condInitialBuild, metav1.ConditionFalse, "ProviderUnsupported", fmt.Sprintf(
			"the platform's %s support cannot resolve a branch, so this project builds on its first push",
			conn.Spec.Provider))
		return false
	}

	branch := project.Spec.Source.ProductionBranch
	if branch == "" {
		branch = "main"
	}
	revision, err := resolver.HeadRevision(ctx, project.Spec.Source.Repo, branch)
	if errors.Is(err, gitprovider.ErrFileNotFound) {
		// A repository the credential cannot read answers this 404 too, and
		// "push a commit" is the wrong thing to tell somebody whose
		// repository nothing can see. Asking about the repository itself is
		// what tells the two apart.
		if detect.UnreadableRepository(ctx, provider, project.Spec.Source.Repo) {
			setCond(condInitialBuild, metav1.ConditionFalse, "RepositoryUnreadable",
				detect.UnreadableRepositoryMessage(conn.Name, project.Spec.Source.Repo))
			return false
		}
		// An empty repository, or a production branch that is not there.
		// Both are the project's own configuration meeting the repository,
		// and neither improves by being asked again.
		setCond(condInitialBuild, metav1.ConditionFalse, "NoCommit", fmt.Sprintf(
			"%s has no branch %q to build: push a commit, or correct the project's production branch",
			project.Spec.Source.Repo, branch))
		return false
	}
	if err != nil {
		setCond(condInitialBuild, metav1.ConditionFalse, "RevisionUnresolved", err.Error())
		return true
	}

	build := buildForRevision(project, revision, branch)
	if r.Audit != nil {
		if err := r.Audit.Record(ctx, audit.Transition{
			Object:      build,
			Kind:        audit.KindBuild,
			Operation:   clickhouse.AuditCreate,
			Controller:  actorProjectController,
			Correlation: revision.SHA,
			To:          string(kitchenv1alpha1.BuildQueued),
			Project:     project.Name,
			Reason:      fmt.Sprintf("project %s was created, so %s is built", project.Name, branch),
			Details:     map[string]any{"commit": revision.SHA, "branch": branch},
		}); err != nil {
			setCond(condInitialBuild, metav1.ConditionFalse, "AuditFailed", err.Error())
			return true
		}
	}

	switch err := r.Create(ctx, build); {
	case apierrors.IsAlreadyExists(err):
		// A push for the same commit beat this to it, which is the whole of
		// what the deterministic name is for: the commit is being built, and
		// the seeding is done.
	case err != nil:
		setCond(condInitialBuild, metav1.ConditionFalse, "BuildNotCreated", err.Error())
		return true
	}

	project.Status.InitialBuildRef = &kitchenv1alpha1.LocalObjectReference{Name: build.Name}
	setCond(condInitialBuild, metav1.ConditionTrue, "Created", fmt.Sprintf(
		"build %s was created for %s at %s", build.Name, branch, shortSHA(revision.SHA)))
	return false
}

// hasBuild reports whether anything has built this project yet.
func (r *ProjectReconciler) hasBuild(ctx context.Context, project *kitchenv1alpha1.Project) (bool, error) {
	builds := &kitchenv1alpha1.BuildList{}
	if err := r.List(ctx, builds, client.InNamespace(project.Namespace)); err != nil {
		return false, err
	}
	for i := range builds.Items {
		if builds.Items[i].Spec.ProjectRef.Name == project.Name {
			return true, nil
		}
	}
	return false, nil
}

// buildForRevision is the Build for a commit the platform resolved itself. It
// is spelled the same way the webhook receiver spells one, deterministic name
// included, because the two are racing for the same commit whenever a project
// is created moments before somebody pushes to it.
func buildForRevision(
	project *kitchenv1alpha1.Project,
	revision gitprovider.Revision,
	branch string,
) *kitchenv1alpha1.Build {
	return &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kitchenv1alpha1.BuildNameFor(project.Name, revision.SHA),
			Namespace: project.Namespace,
			Labels:    map[string]string{kitchenv1alpha1.ProjectLabel: project.Name},
			Annotations: map[string]string{
				initialBuildAnnotation: "the project's first build, for the branch as it already stood",
			},
		},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			Git: kitchenv1alpha1.GitRevision{
				SHA:     revision.SHA,
				Branch:  branch,
				Message: revision.Message,
				Author:  revision.Author,
			},
		},
	}
}

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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/detect"
	"github.com/Bermos/Kitchen/internal/framework"
	"github.com/Bermos/Kitchen/internal/gitprovider"
	"github.com/Bermos/Kitchen/internal/repoconfig"
)

// errSourceUnreadable is the repository not being readable right now, which
// is the one detection failure the build reconciler tells apart: it keeps a
// Build queued rather than failing a commit for something the commit did not
// do. It lives in internal/detect, which is where the reading of a repository
// moved when the API grew a preflight over the same question.
var errSourceUnreadable = detect.ErrSourceUnreadable

// errRepositoryUnreadable is the repository itself not being readable — not
// there, or not visible to the credential this connection holds. It is the
// other detection failure the build reconciler tells apart, and it ends the
// other way: a repository that cannot be seen is not going to appear because
// the Build waited, so the commit fails with a sentence about the repository
// rather than one about a directory inside it.
var errRepositoryUnreadable = detect.ErrRepositoryUnreadable

// detectFramework is what `strategy: auto` means: read the repository at the
// commit under build, and decide what it is before anything is created.
//
// The Dockerfile is only looked for when the strategy is still open. A
// project that has asked for buildpacks explicitly is asking for its
// Dockerfile to be ignored, and detection then serves only to tell the
// lifecycle what it is building.
func (r *BuildReconciler) detectFramework(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	strategy kitchenv1alpha1.BuildStrategy,
) (framework.Framework, error) {
	reader, err := r.sourceReaderFor(ctx, project)
	if err != nil {
		return framework.Framework{}, err
	}
	return detect.Framework(ctx, reader, detect.Target{
		Repo:               project.Spec.Source.Repo,
		Ref:                build.Spec.Git.SHA,
		RootDirectory:      buildRootDir(project),
		DockerfilePath:     buildDockerfilePath(project, build),
		ConsiderDockerfile: strategy == kitchenv1alpha1.BuildStrategyAuto,
	})
}

// sourceReaderFor resolves the source-reading half of the Project's git
// provider. Every failure here is the platform being unable to look rather
// than the repository being unrecognisable, so they all come back as
// errSourceUnreadable — including the ones a retry will not fix, which is
// what keeps a build queued and visibly waiting instead of failing a commit
// for something the commit did not do.
func (r *BuildReconciler) sourceReaderFor(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
) (gitprovider.SourceReader, error) {
	conn := &kitchenv1alpha1.Connection{}
	key := types.NamespacedName{Namespace: project.Namespace, Name: project.Spec.Source.ConnectionRef.Name}
	if err := r.Get(ctx, key, conn); err != nil {
		return nil, fmt.Errorf("%w: %w", errSourceUnreadable, err)
	}
	if !connectionProvides(conn, kitchenv1alpha1.CapabilityGitSource) {
		return nil, fmt.Errorf("%w: connection %q does not provide %s yet",
			errSourceUnreadable, conn.Name, kitchenv1alpha1.CapabilityGitSource)
	}

	creds := &corev1.Secret{}
	credsKey := types.NamespacedName{Namespace: conn.Namespace, Name: conn.Spec.CredentialsSecretRef.Name}
	if err := r.Get(ctx, credsKey, creds); err != nil {
		return nil, fmt.Errorf("%w: %w", errSourceUnreadable, err)
	}
	token := string(creds.Data[gitCredentialsTokenKey])
	if token == "" {
		return nil, fmt.Errorf("%w: connection %q has no credential to read with",
			errSourceUnreadable, conn.Name)
	}

	factory := r.GitProviders
	if factory == nil {
		factory = gitprovider.Default
	}
	provider, err := factory(conn, token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSourceUnreadable, err)
	}
	reader, ok := gitprovider.Source(provider)
	if !ok {
		return nil, fmt.Errorf("%w: the %s provider cannot read a repository's contents",
			errSourceUnreadable, conn.Spec.Provider)
	}
	return reader, nil
}

// buildDockerfilePath is the Dockerfile this build uses, relative to the
// project's root directory: the one the commit's own kitchen.json named, and
// the project's setting where it named none.
//
// It is normalised here rather than at each use, so that detection and the
// builder are given the same string — `./Dockerfile`, `Dockerfile` and an
// absent setting are one file, and the preflight cannot report a file the
// build then fails to find.
func buildDockerfilePath(project *kitchenv1alpha1.Project, build *kitchenv1alpha1.Build) string {
	return detect.NormalizeDockerfile(
		repoconfig.DockerfilePath(build.Status.Config, project.Spec.Build.DockerfilePath))
}

// buildDockerfileTarget is the stage of that Dockerfile this build ships: the
// one the commit's own kitchen.json named, and the project's setting where it
// named none. Empty is the file's last stage.
//
// It is resolved here, beside the file it names a stage of, because the same
// answer is needed three times — the builder's `--target`, the refusal a
// buildpacks build gets, and the record written onto the Build — and a stage
// resolved twice is a stage two of them could disagree about.
func buildDockerfileTarget(project *kitchenv1alpha1.Project, build *kitchenv1alpha1.Build) string {
	return detect.NormalizeTarget(
		repoconfig.DockerfileTarget(build.Status.Config, project.Spec.Build.DockerfileTarget))
}

// dockerfileTargetSource says where a build's target was declared, so a
// refusal sends somebody to the place they can change it rather than to the
// other one.
func dockerfileTargetSource(build *kitchenv1alpha1.Build) string {
	if config := build.Status.Config; config != nil && config.Build != nil && config.Build.DockerfileTarget != "" {
		return "this commit's " + kitchenv1alpha1.RepoConfigFileName
	}
	return "this project's build settings"
}

// readConfig reads the commit's kitchen.json and records it on the Build,
// before the build Job exists.
//
// It is written to the status straight away rather than with the rest of the
// build's outcome, for the same reason the source provenance is: what follows
// creates a Job, and a status update that failed after that would leave a
// build running against a file nothing remembers reading — which the Release
// at the end would then be merged without.
//
// A non-nil first return is the Build having been parked or failed, and the
// caller returns it: a repository that cannot be read right now parks, and a
// file that is wrong fails, saying which line to fix.
func (r *BuildReconciler) readConfig(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
) (*ctrl.Result, error) {
	if build.Status.Config != nil {
		return nil, nil
	}
	reader, err := r.sourceReaderFor(ctx, project)
	if err != nil {
		res, updateErr := r.pending(ctx, build, reasonSourceUnreadable, err)
		return &res, updateErr
	}
	config, err := repoconfig.Read(ctx, reader, repoconfig.Target{
		Repo:          project.Spec.Source.Repo,
		Ref:           build.Spec.Git.SHA,
		RootDirectory: buildRootDir(project),
	})
	switch {
	case errors.Is(err, repoconfig.ErrSourceUnreadable):
		res, updateErr := r.pending(ctx, build, reasonSourceUnreadable, err)
		return &res, updateErr
	case err != nil:
		res, updateErr := r.fail(ctx, build, project, reasonConfigInvalid, err.Error())
		return &res, updateErr
	case config == nil:
		// No file, which is the ordinary case. Nothing is recorded: an
		// absent file is the project being configured entirely by the
		// dashboard, exactly as it was before this existed.
		return nil, nil
	}

	build.Status.Config = config
	logf.FromContext(ctx).Info("build read the commit's own configuration",
		"build", build.Name, "project", project.Name, "file", config.Path, "declares", config.Declares())
	if err := r.Status().Update(ctx, build); err != nil {
		return &ctrl.Result{}, err
	}
	return nil, nil
}

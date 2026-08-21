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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/detect"
	"github.com/Bermos/Kitchen/internal/framework"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// errSourceUnreadable is the repository not being readable right now, which
// is the one detection failure the build reconciler tells apart: it keeps a
// Build queued rather than failing a commit for something the commit did not
// do. It lives in internal/detect, which is where the reading of a repository
// moved when the API grew a preflight over the same question.
var errSourceUnreadable = detect.ErrSourceUnreadable

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
		DockerfilePath:     project.Spec.Build.DockerfilePath,
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

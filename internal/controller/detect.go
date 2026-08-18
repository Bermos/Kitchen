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
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/framework"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// The two ways detection does not produce an answer, kept apart because they
// end differently: one is worth trying again in fifteen seconds, the other is
// a sentence the person who pushed the commit has to read.
var (
	// errSourceUnreadable is the repository not being readable right now —
	// a provider that is down, a token that stopped working, a rate limit.
	// Nothing about the commit caused it, so the Build stays queued.
	errSourceUnreadable = errors.New("the repository could not be read")

	// errNoFrameworkDetected is the repository having been read and not
	// recognised. That is final: the same commit will not detect differently
	// on the next attempt.
	errNoFrameworkDetected = errors.New("no Dockerfile and no framework detected")
)

// detectFramework is what `strategy: auto` means: read the repository at the
// commit under build, and decide what it is before anything is created.
//
// It runs against the provider's API rather than against a clone, because the
// decision it produces is an input to the build pod — which strategy the pod
// runs, and what the builder is told — and a clone only exists once that pod
// does. Two requests answer it: one listing of the build's root directory,
// and the package manifest when there is one.
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
	signals, err := r.repositorySignals(ctx, project, build, strategy == kitchenv1alpha1.BuildStrategyAuto)
	if err != nil {
		return framework.Framework{}, err
	}

	detected, ok := framework.Detect(signals)
	if !ok {
		return framework.Framework{}, fmt.Errorf("%w in %s at %s: %s",
			errNoFrameworkDetected, project.Spec.Source.Repo, shortSHA(build.Spec.Git.SHA),
			"add a Dockerfile, or set the project's build strategy to one that suits it")
	}
	return detected, nil
}

// repositorySignals is everything detection looks at: the names in the
// build's root directory, whether the project's Dockerfile is one of them,
// and the package manifest when the repository has one.
func (r *BuildReconciler) repositorySignals(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	lookForDockerfile bool,
) (framework.Signals, error) {
	reader, err := r.sourceReaderFor(ctx, project)
	if err != nil {
		return framework.Signals{}, err
	}

	repo := project.Spec.Source.Repo
	ref := build.Spec.Git.SHA
	root := buildRootDir(project)

	entries, err := reader.ListDir(ctx, repo, ref, root)
	if err != nil {
		if errors.Is(err, gitprovider.ErrFileNotFound) {
			// A root directory that is not there is the project's
			// configuration being wrong about the repository, which no
			// amount of waiting fixes.
			return framework.Signals{}, fmt.Errorf("%w: %s has no directory %q at %s",
				errNoFrameworkDetected, repo, root, shortSHA(ref))
		}
		return framework.Signals{}, fmt.Errorf("%w: %w", errSourceUnreadable, err)
	}

	signals := framework.Signals{Files: make([]string, 0, len(entries))}
	for _, entry := range entries {
		if !entry.Dir {
			signals.Files = append(signals.Files, entry.Name)
		}
	}

	if lookForDockerfile {
		signals.Dockerfile, err = dockerfilePresent(ctx, reader, project, ref, entries)
		if err != nil {
			return framework.Signals{}, err
		}
		if signals.Dockerfile {
			// Nothing else can change the answer, so nothing else is read.
			return signals, nil
		}
	}

	if slices.Contains(signals.Files, "package.json") {
		manifest, err := reader.ReadFile(ctx, repo, ref, path.Join(root, "package.json"))
		switch {
		case errors.Is(err, gitprovider.ErrFileNotFound):
			// Listed a moment ago and gone now: another commit landed on the
			// branch mid-detection. Read it as a repository without one.
		case err != nil:
			return framework.Signals{}, fmt.Errorf("%w: %w", errSourceUnreadable, err)
		default:
			signals.PackageJSON = manifest
		}
	}
	return signals, nil
}

// dockerfilePresent reports whether the project's Dockerfile is where the
// project says it is. The usual case costs nothing — the file is in the
// listing already read — and only a path pointing into a subdirectory needs
// a second listing.
func dockerfilePresent(
	ctx context.Context,
	reader gitprovider.SourceReader,
	project *kitchenv1alpha1.Project,
	ref string,
	rootEntries []gitprovider.DirEntry,
) (bool, error) {
	dockerfile := project.Spec.Build.DockerfilePath
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	dockerfile = strings.TrimPrefix(path.Clean(dockerfile), "./")

	dir, name := path.Split(dockerfile)
	entries := rootEntries
	if dir != "" {
		var err error
		entries, err = reader.ListDir(ctx, project.Spec.Source.Repo, ref,
			path.Join(buildRootDir(project), dir))
		switch {
		case errors.Is(err, gitprovider.ErrFileNotFound):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("%w: %w", errSourceUnreadable, err)
		}
	}

	for _, entry := range entries {
		if entry.Name == name && !entry.Dir {
			return true, nil
		}
	}
	return false, nil
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

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

// Package detect reads a repository through a git provider and decides what
// it is.
//
// It is the half of detection that talks to a provider, kept apart from the
// rules in internal/framework — which look at a struct and know nothing about
// git hosting — and apart from the reconciler that used to own it, because
// two callers now ask the same question of the same repository:
//
//   - BuildReconciler, when a build with `strategy: auto` needs to know which
//     pod to run and what to tell the builder;
//   - the API, when somebody is filling in the new project form and would
//     rather find out that the root directory is wrong now than from a failed
//     build in five minutes.
//
// Both have to agree, and the only way to be sure they do is for there to be
// one implementation. A preflight that answered differently from the build
// would be worse than no preflight at all.
package detect

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/Bermos/Kitchen/internal/framework"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// The two ways detection does not produce an answer, kept apart because they
// end differently: one is worth trying again in fifteen seconds, the other is
// a sentence the person who pushed the commit has to read.
var (
	// ErrSourceUnreadable is the repository not being readable right now —
	// a provider that is down, a token that stopped working, a rate limit.
	// Nothing about the commit caused it, so a Build stays queued.
	ErrSourceUnreadable = errors.New("the repository could not be read")

	// ErrNotRecognised is the repository having been read and not
	// recognised. That is final: the same commit will not detect differently
	// on the next attempt.
	ErrNotRecognised = errors.New("no Dockerfile and no framework detected")
)

// Target is the repository, the commit and the directory within it that
// detection looks at. It is spelled out rather than taken from a Project so
// that the API can ask about a project that does not exist yet.
type Target struct {
	// Repo is the repository in the provider's own terms, owner/name for
	// every provider the platform speaks so far.
	Repo string

	// Ref is what to read the repository at: a commit, a branch, a tag.
	Ref string

	// RootDirectory is the directory within the repository that is built,
	// empty for the repository itself. It is already normalised — callers
	// pass what buildRootDir or the API's trimming produced.
	RootDirectory string

	// DockerfilePath is where the project says its Dockerfile is, relative
	// to RootDirectory, empty for the conventional "Dockerfile".
	DockerfilePath string

	// ConsiderDockerfile is false when the strategy is already decided. A
	// project that has asked for buildpacks explicitly is asking for its
	// Dockerfile to be ignored, and detection then serves only to tell the
	// lifecycle what it is building.
	ConsiderDockerfile bool
}

// Framework recognises the repository, or reports that it cannot.
//
// It runs against the provider's API rather than against a clone, because the
// decision it produces is an input to the build pod — which strategy the pod
// runs, and what the builder is told — and a clone only exists once that pod
// does. Two requests answer it: one listing of the build's root directory,
// and the package manifest when there is one.
func Framework(
	ctx context.Context,
	reader gitprovider.SourceReader,
	target Target,
) (framework.Framework, error) {
	signals, err := Signals(ctx, reader, target)
	if err != nil {
		return framework.Framework{}, err
	}

	detected, ok := framework.Detect(signals)
	if !ok {
		return framework.Framework{}, fmt.Errorf("%w in %s at %s: %s",
			ErrNotRecognised, target.Repo, ShortRef(target.Ref),
			"add a Dockerfile, or set the project's build strategy to one that suits it")
	}
	return detected, nil
}

// Signals is everything detection looks at: the names in the build's root
// directory, whether the project's Dockerfile is one of them, and the package
// manifest when the repository has one.
func Signals(
	ctx context.Context,
	reader gitprovider.SourceReader,
	target Target,
) (framework.Signals, error) {
	entries, err := reader.ListDir(ctx, target.Repo, target.Ref, target.RootDirectory)
	if err != nil {
		if errors.Is(err, gitprovider.ErrFileNotFound) {
			// A root directory that is not there is the project's
			// configuration being wrong about the repository, which no
			// amount of waiting fixes.
			return framework.Signals{}, fmt.Errorf("%w: %s has no directory %q at %s",
				ErrNotRecognised, target.Repo, rootName(target.RootDirectory), ShortRef(target.Ref))
		}
		return framework.Signals{}, fmt.Errorf("%w: %w", ErrSourceUnreadable, err)
	}

	signals := framework.Signals{Files: make([]string, 0, len(entries))}
	for _, entry := range entries {
		if !entry.Dir {
			signals.Files = append(signals.Files, entry.Name)
		}
	}

	if target.ConsiderDockerfile {
		signals.Dockerfile, err = dockerfilePresent(ctx, reader, target, entries)
		if err != nil {
			return framework.Signals{}, err
		}
		if signals.Dockerfile {
			// Nothing else can change the answer, so nothing else is read.
			return signals, nil
		}
	}

	if slices.Contains(signals.Files, "package.json") {
		manifest, err := reader.ReadFile(ctx, target.Repo, target.Ref,
			path.Join(target.RootDirectory, "package.json"))
		switch {
		case errors.Is(err, gitprovider.ErrFileNotFound):
			// Listed a moment ago and gone now: another commit landed on the
			// branch mid-detection. Read it as a repository without one.
		case err != nil:
			return framework.Signals{}, fmt.Errorf("%w: %w", ErrSourceUnreadable, err)
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
	target Target,
	rootEntries []gitprovider.DirEntry,
) (bool, error) {
	dockerfile := target.DockerfilePath
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	dockerfile = strings.TrimPrefix(path.Clean(dockerfile), "./")

	dir, name := path.Split(dockerfile)
	entries := rootEntries
	if dir != "" {
		var err error
		entries, err = reader.ListDir(ctx, target.Repo, target.Ref,
			path.Join(target.RootDirectory, dir))
		switch {
		case errors.Is(err, gitprovider.ErrFileNotFound):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("%w: %w", ErrSourceUnreadable, err)
		}
	}

	for _, entry := range entries {
		if entry.Name == name && !entry.Dir {
			return true, nil
		}
	}
	return false, nil
}

// ShortRef is a commit abbreviated for a message, and anything else left
// alone: a branch name is already the short form of itself.
func ShortRef(ref string) string {
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

// rootName is the root directory as it should read in a message about it
// being missing, since the empty string means the repository itself.
func rootName(root string) string {
	if root == "" {
		return "."
	}
	return root
}

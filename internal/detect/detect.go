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

	"github.com/Bermos/Kitchen/internal/framework"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// The three ways detection does not produce an answer, kept apart because
// they end differently: one is worth trying again in fifteen seconds, and the
// other two are sentences somebody has to read — different sentences, about
// different fields.
var (
	// ErrSourceUnreadable is the repository not being readable right now —
	// a provider that is down, a token that stopped working, a rate limit.
	// Nothing about the commit caused it, so a Build stays queued.
	ErrSourceUnreadable = errors.New("the repository could not be read")

	// ErrNotRecognised is the repository having been read and not
	// recognised. That is final: the same commit will not detect differently
	// on the next attempt.
	ErrNotRecognised = errors.New("no Dockerfile and no framework detected")

	// ErrRepositoryUnreadable is the repository itself not having been read:
	// it is not there, or the credential the connection holds cannot see it.
	//
	// It is a third answer rather than a shade of the other two because it is
	// the one they are most easily mistaken for. Every provider answers 404
	// both for a path that is not in a repository and for a repository a
	// credential may not know about, so a repository nobody can read arrives
	// here looking exactly like a root directory somebody typed wrong — and
	// the message for that sends them to correct a field that is already
	// correct, for as long as they are willing to keep trying.
	ErrRepositoryUnreadable = errors.New("the repository is not there, or the credential cannot see it")
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

	// RootDirectory is the build root: the directory within the repository
	// that is built, empty for the repository itself. It is already
	// normalised — callers pass what NormalizeRoot produced.
	RootDirectory string

	// DockerfilePath is where the project says its Dockerfile is, relative
	// to RootDirectory, empty for the conventional "Dockerfile". A path that
	// leaves the build root is not this project's file — see LeavesRoot.
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
			// Which of the two 404s this is has to be settled before
			// anything is said about a directory, because the message for a
			// missing directory is a confident instruction to fix a field.
			if err := checkRepository(ctx, reader, target.Repo); err != nil {
				return framework.Signals{}, err
			}
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

// checkRepository asks the provider about the repository itself, which is the
// one request that tells a repository the credential cannot read apart from a
// path inside one it can. It costs a request, and only on the path where
// something is already about to be reported as missing.
//
// A nil return means the repository reads, so whatever was not found is
// genuinely not in it. A provider with nothing to ask — one that reads
// contents and answers no questions about repositories — also returns nil:
// the ambiguous message it always gave is better than a confident wrong one
// invented here. It takes anything a caller holds, because the callers hold
// different halves of the same provider: a SourceReader here, the Provider
// itself where the question comes up before any reading has started.
func checkRepository(ctx context.Context, provider any, repo string) error {
	probe, ok := gitprovider.Probe(provider)
	if !ok {
		return nil
	}
	switch _, err := probe.Repository(ctx, repo); {
	case errors.Is(err, gitprovider.ErrRepositoryNotFound):
		return fmt.Errorf("%w: %s", ErrRepositoryUnreadable, repo)
	case err != nil:
		// The probe itself did not answer, so nothing has been settled. That
		// is the provider being unreachable rather than the repository being
		// anything, and it is worth another attempt.
		return fmt.Errorf("%w: %w", ErrSourceUnreadable, err)
	}
	return nil
}

// UnreadableRepository is checkRepository for a caller that is not reading
// anything and has a 404 to explain: a project's first build, where the
// production branch would not resolve and "push a commit" is the wrong thing
// to say to somebody whose repository nothing can see.
//
// False for a provider with no probe behind it, and false for a probe that
// did not answer — neither has established anything, and a verdict invented
// from silence is what this exists to stop.
func UnreadableRepository(ctx context.Context, provider any, repo string) bool {
	return errors.Is(checkRepository(ctx, provider, repo), ErrRepositoryUnreadable)
}

// dockerfilePresent reports whether the project's Dockerfile is where the
// project says it is — which is somewhere at or under the build root, since
// that is the whole of what a build sees. The usual case costs nothing — the
// file is in the listing already read — and only a path pointing into a
// subdirectory needs a second listing.
func dockerfilePresent(
	ctx context.Context,
	reader gitprovider.SourceReader,
	target Target,
	rootEntries []gitprovider.DirEntry,
) (bool, error) {
	dockerfile := NormalizeDockerfile(target.DockerfilePath)
	if LeavesRoot(dockerfile) {
		// A path out of the build root names a file no build can read: the
		// builder is handed the build root and nothing above it. Answering
		// "present" here would be detection promising a build the container
		// strategy cannot run.
		return false, nil
	}

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

// UnreadableRepositoryMessage is the one sentence to show somebody whose
// repository could not be read, wherever the platform noticed it: the
// preflight, a project's first build, a build's own detection.
//
// It names the connection, because the fix is usually the credential of the
// one that was asked and an installation may have several — and it says both
// things it can be, because no provider will say which: a 404 for a
// repository a token may not know about is how a token is kept from
// enumerating private repositories.
func UnreadableRepositoryMessage(connection, repo string) string {
	return fmt.Sprintf("connection %q cannot read %s: check the repository name, "+
		"and that the connection's credential reaches it", connection, repo)
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

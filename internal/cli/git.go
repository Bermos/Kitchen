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

package cli

import (
	"context"
	"os/exec"
	"strings"
)

// What the working directory knows about the commit somebody is deploying.
//
// The CLI shells out to `git` rather than linking a git implementation: the
// questions it asks are the four a person would type, the answers have to
// agree with what that person's `git status` says (worktrees, submodules,
// `core.hooksPath`, an `includeIf` in their config), and a second
// implementation of "which commit is this" would eventually disagree with the
// one they can see.
//
// None of it is required. `kitchen deploy --sha … --branch …` never asks git
// anything, which is what makes the command runnable from a directory that is
// not a checkout at all — a CI job that has only the metadata, or an agent
// working from a description of the change.

// gitRevision is what a working copy says about itself.
type gitRevision struct {
	// SHA is HEAD's commit, in full. The API takes a full or abbreviated sha
	// and the platform records what it was given, so the full one is what
	// makes a build identifiable months later.
	SHA string
	// Branch is HEAD's branch, or empty on a detached HEAD — which is not an
	// error: a build of a commit that has been built before needs no branch.
	Branch string
	// Dirty reports uncommitted changes. It is a warning rather than a
	// refusal: the platform builds the *commit*, so a dirty tree means the
	// build will not contain what is on the screen, which is worth saying
	// once and not worth refusing over.
	Dirty bool
	// Pushed reports whether the commit is on any remote-tracking branch.
	// False is the one that matters — the build clones from the git provider,
	// so a commit that only exists locally cannot be built.
	Pushed bool
}

// git runs one git command in dir and returns its trimmed output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitRoot is the top of the working copy dir is in, or an error if it is not
// in one.
func gitRoot(dir string) (string, error) {
	return git(context.Background(), dir, "rev-parse", "--show-toplevel")
}

// describeRevision answers what git knows about the current commit. A
// directory that is not a checkout, or a machine with no git at all, is not an
// error here: it answers an empty revision, and the caller — which is only
// ever `kitchen deploy` — says which flag would have supplied the missing
// half.
func describeRevision(ctx context.Context, dir string) gitRevision {
	sha, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return gitRevision{}
	}
	revision := gitRevision{SHA: sha}

	// `--abbrev-ref` answers "HEAD" on a detached HEAD, which is not a branch
	// name and must not be sent as one.
	if branch, err := git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && branch != "HEAD" {
		revision.Branch = branch
	}
	if status, err := git(ctx, dir, "status", "--porcelain"); err == nil {
		revision.Dirty = status != ""
	}
	// Any remote-tracking branch containing the commit will do. Which remote
	// the platform builds from is the project's own business — its Connection
	// and repository decide that — so the question here is only whether this
	// commit has left the laptop at all.
	if remote, err := git(ctx, dir, "branch", "--remotes", "--contains", sha); err == nil {
		revision.Pushed = strings.TrimSpace(remote) != ""
	}
	return revision
}

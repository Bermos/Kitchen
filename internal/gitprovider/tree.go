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

package gitprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrTreeUnavailable is what a TreeResolver returns when the provider cannot
// name the tree object at this path, as opposed to the path not being there —
// which is ErrFileNotFound, the same answer a SourceReader gives.
//
// It exists because the three providers do not expose the same corner of git.
// GitHub and Gitea both serve a commit's own root tree; GitLab's REST API
// names the id of every entry *inside* a tree and never the id of the root
// tree itself, so a GitLab project whose build root is the whole repository
// has no tree object to compare. Callers read either error the same way:
// there is nothing to compare, so build.
var ErrTreeUnavailable = errors.New("the tree object at this path cannot be resolved")

// TreeResolver is the half of a git provider that answers "what is the tree
// object at <commit>:<dir>" — the identity of the source a project builds.
//
// It is one call, and it is the whole of issue #500: two commits whose tree
// object at the build root is the same byte-for-byte have the same source
// there, whatever else the push touched. That is exact where a path filter is
// a guess, and it is derived rather than declared, so it cannot drift.
//
// Like SourceReader and RevisionResolver it is separate from Provider and
// asked for with a type assertion, so a provider can land as a source of
// webhooks first and gain this later. A provider without it never skips a
// build, which is what every project did before this existed.
type TreeResolver interface {
	// TreeAt resolves the tree object at a revision and a directory. The
	// repository is in the provider's owner/name form, ref is anything the
	// provider resolves — the caller always passes the commit under build —
	// and dir is relative to the repository root, empty for the root itself.
	//
	// A directory that is not there at that revision returns ErrFileNotFound;
	// a provider that cannot name this tree returns ErrTreeUnavailable. The
	// string is opaque and is only ever compared with another one this
	// provider produced for the same repository.
	TreeAt(ctx context.Context, repo, ref, dir string) (string, error)
}

// Trees narrows a Provider to its tree-resolving half. The second return is
// false for a provider that cannot resolve one, which callers treat as "there
// is nothing to compare" rather than as a failure.
func Trees(provider Provider) (TreeResolver, bool) {
	resolver, ok := provider.(TreeResolver)
	return resolver, ok
}

// dirEntryType is what GitHub and Gitea both call a directory in a contents
// listing. GitLab says `tree` instead, which is why its own reader spells its
// own word rather than sharing this one.
const dirEntryType = "dir"

// treeEntry is one entry of a directory listing as the tree resolvers read
// it: the name, whether it is a directory, and the object id the provider
// carries beside it. Every provider spells those three differently and maps
// onto this before the pick below.
type treeEntry struct {
	Name string
	Dir  bool
	ID   string
}

// pickTree finds the build root in its parent's listing.
//
// A name that is there but is a file rather than a directory is the same
// answer as a name that is not there at all: there is no tree object at this
// path, so there is nothing to compare and the build happens.
func pickTree(entries []treeEntry, name, dir string) (string, error) {
	for _, entry := range entries {
		if entry.Name != name || !entry.Dir {
			continue
		}
		if entry.ID == "" {
			return "", fmt.Errorf("%w: %s carried no object id", ErrTreeUnavailable, displayPath(dir))
		}
		return entry.ID, nil
	}
	return "", fmt.Errorf("%w: %s", ErrFileNotFound, displayPath(dir))
}

// splitTreePath splits a build root into the directory holding it and the
// entry to look for inside that directory. The repository root splits into
// two empty strings, which every implementation answers from the commit
// rather than from a listing.
func splitTreePath(dir string) (parent, name string) {
	trimmed := strings.Trim(strings.TrimPrefix(strings.Trim(dir, "/"), "./"), "/")
	if trimmed == "" || trimmed == "." {
		return "", ""
	}
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[:index], trimmed[index+1:]
	}
	return "", trimmed
}

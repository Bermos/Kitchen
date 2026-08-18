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
)

// ErrFileNotFound is what a SourceReader returns for a path that is not in
// the repository at that revision. It is an answer rather than a failure —
// "there is no Dockerfile" is the most common thing detection learns.
var ErrFileNotFound = errors.New("file not found")

// DirEntry is one entry of a directory listing: enough to recognise a
// repository, and nothing more.
type DirEntry struct {
	// Name of the entry within the directory, with no path in front of it.
	Name string
	// Dir distinguishes a directory from a file, since some frameworks are
	// recognised by a directory being there.
	Dir bool
}

// SourceReader is the half of a git provider that reads a repository's
// contents at a revision, which is what framework detection needs and nothing
// else in the platform does: builds fetch their own source in the build pod.
//
// Like StatusReporter it is separate from Provider and asked for with a type
// assertion, so a provider can land as a source of webhooks first and gain
// this later. A provider without it detects nothing, which the caller reports
// as such rather than guessing.
type SourceReader interface {
	// ListDir lists one directory of the repository at a revision. The
	// repository is in the provider's owner/name form, ref is anything the
	// provider resolves — detection always passes the commit under build —
	// and dir is relative to the repository root, empty for the root itself.
	//
	// A directory that is not there returns ErrFileNotFound.
	ListDir(ctx context.Context, repo, ref, dir string) ([]DirEntry, error)

	// ReadFile reads one file of the repository at a revision, returning
	// ErrFileNotFound when it is absent. Implementations cap what they read:
	// detection only ever wants small manifests, and a repository is free to
	// contain a file of any size at all.
	ReadFile(ctx context.Context, repo, ref, path string) ([]byte, error)
}

// Source narrows a Provider to its source-reading half. The second return is
// false for a provider that cannot read a repository, which callers treat as
// "detect nothing" rather than as a failure.
func Source(provider Provider) (SourceReader, bool) {
	reader, ok := provider.(SourceReader)
	return reader, ok
}

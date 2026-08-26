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

// ErrRepositoryNotFound is a repository the credential cannot read: it is not
// there, or it is there and this credential is not allowed to know that.
//
// The two are one answer on purpose. GitHub answers 404 rather than 403 for a
// repository a token cannot see, so that a token cannot enumerate private
// repositories by reading status codes, and every provider that copied its
// API copied that too. What the platform can say is which side of the
// repository the 404 came from, and that is the distinction that matters:
// a repository nobody can read is not a root directory somebody typed wrong.
var ErrRepositoryNotFound = errors.New("repository not found")

// RepositoryProbe is the half of a git provider that answers the one question
// a listing cannot: is this repository readable at all. It exists because
// every path into a repository — contents, tree, commits — answers 404 both
// for a path that is not there and for a repository that is not readable, and
// only a request addressed at the repository itself tells them apart.
//
// Like SourceReader and RepositoryLister it is separate from Provider and
// asked for with a type assertion, so a provider can land as a source of
// webhooks first and gain this later. A provider without it says what it
// always said: the caller keeps the ambiguous message rather than gaining a
// wrong one.
type RepositoryProbe interface {
	// Repository reads the repository itself, returning
	// ErrRepositoryNotFound when the credential cannot see it. Any other
	// error is the provider being unreachable or refusing, which is not an
	// answer about the repository at all.
	Repository(ctx context.Context, repo string) (Repository, error)
}

// Probe narrows what a caller already holds — a Provider, or the SourceReader
// it was narrowed to — to its repository-probing half. The second return is
// false for a provider that cannot be asked, which callers treat as "no
// second opinion" rather than as a failure.
func Probe(provider any) (RepositoryProbe, bool) {
	probe, ok := provider.(RepositoryProbe)
	return probe, ok
}

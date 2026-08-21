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

import "context"

// Revision is one commit, in the terms a Build's spec.git carries: enough to
// build it and enough to read on the build page.
type Revision struct {
	// SHA is the full commit hash.
	SHA string
	// Branch the commit was asked for on, echoed back so a caller that
	// resolved a branch has the pair without keeping it itself.
	Branch string
	// Message is the commit's subject line, empty when the provider gives
	// none.
	Message string
	// Author is the provider's identity for whoever wrote it, empty when the
	// provider cannot attribute it to an account.
	Author string
}

// RevisionResolver is the half of a git provider that answers "what is at the
// tip of this branch". Nothing in the webhook path needs it — a push says
// which commit it carries — it exists for the one moment there has been no
// push yet: a project that was just created and has never built anything.
//
// Like SourceReader and RepositoryLister it is separate from Provider and
// asked for with a type assertion, so a provider can land as a source of
// webhooks first and gain this later. A provider without it means the first
// build waits for the first push, which is what every project did before.
type RevisionResolver interface {
	// HeadRevision resolves a branch (or any ref the provider accepts) to the
	// commit at its tip. A ref that is not there returns ErrFileNotFound: an
	// empty repository and a misspelled production branch are both answers
	// rather than failures, and neither is worth retrying.
	HeadRevision(ctx context.Context, repo, ref string) (Revision, error)
}

// Revisions narrows a Provider to its revision-resolving half. The second
// return is false for a provider that cannot resolve a ref, which callers
// treat as "there is nothing to build yet" rather than as a failure.
func Revisions(provider Provider) (RevisionResolver, bool) {
	resolver, ok := provider.(RevisionResolver)
	return resolver, ok
}

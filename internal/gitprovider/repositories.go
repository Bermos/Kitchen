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

// Repository is one repository a Connection's credential can see, in the
// terms the create-a-project form asks about: the name it is addressed by,
// the branch it would deploy from, and enough context to tell two
// similarly-named repositories apart.
type Repository struct {
	// FullName is owner/name — the same string a Project's spec.source.repo
	// carries, and the only field the platform actually acts on.
	FullName string
	// DefaultBranch is what the provider considers this repository's trunk,
	// which is the production branch a new project should start with.
	DefaultBranch string
	// Private is whether the repository is visible only to the credential.
	Private bool
	// Description is the provider's own one-line description, empty for a
	// repository that has none.
	Description string
}

// RepositoryListing is what one credential can see, and whether that is all
// of it. A listing is bounded — a credential on a large organisation can see
// thousands of repositories, and a picker is not a mirror of the provider —
// so Truncated says outright that there are more, rather than leaving
// somebody to conclude a repository does not exist because it was cut off.
type RepositoryListing struct {
	// Repositories are the ones this listing carries, in the provider's own
	// order: most recently pushed to first, so the top of a truncated list is
	// the part somebody is most likely to be looking for.
	Repositories []Repository
	// Truncated is whether the provider had more to give than the cap
	// allowed.
	Truncated bool
}

// RepositoryLister is the half of a git provider that answers "what can this
// credential see". Nothing in the platform's reconcile paths needs it — a
// Project names its repository and that is that — it exists so that naming
// the repository is a choice from a list rather than a string somebody has to
// spell correctly.
//
// Like SourceReader and StatusReporter it is separate from Provider and asked
// for with a type assertion, so a provider can land as a source of webhooks
// first and gain this later. A provider without it is not an error: the
// caller falls back to asking for the name.
type RepositoryLister interface {
	// ListRepositories lists what the credential can see, newest activity
	// first, bounded by the implementation's own cap.
	ListRepositories(ctx context.Context) (RepositoryListing, error)
}

// Repositories narrows a Provider to its repository-listing half. The second
// return is false for a provider that cannot enumerate repositories, which
// callers report as "type the name" rather than as a failure.
func Repositories(provider Provider) (RepositoryLister, bool) {
	lister, ok := provider.(RepositoryLister)
	return lister, ok
}

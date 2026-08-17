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

// CommitState is a build's outcome in the vocabulary the providers share.
// GitHub, GitLab and Gitea all accept these four words for a commit status;
// a provider that spells them differently translates in its own
// implementation rather than widening this set.
type CommitState string

const (
	// CommitPending is a build that is running.
	CommitPending CommitState = "pending"
	// CommitSuccess is a build that produced an image.
	CommitSuccess CommitState = "success"
	// CommitFailure is a build the application's own code failed.
	CommitFailure CommitState = "failure"
	// CommitError is a build the platform could not run.
	CommitError CommitState = "error"
)

// CommitStatus is one status check on one commit.
type CommitStatus struct {
	// SHA of the commit the check belongs to.
	SHA string
	// State the check reports.
	State CommitState
	// Context names the check. Providers key a status by it — a second
	// status in the same context replaces the first — so it has to be
	// stable across a build's lifetime and distinct per project, since two
	// Kitchen projects can watch one repository.
	Context string
	// Description is the one line shown next to the check. Providers cap
	// its length; implementations truncate rather than fail.
	Description string
	// TargetURL is where a reader goes for detail: the build's page in the
	// dashboard. Empty when the platform does not know its own address.
	TargetURL string
}

// DeploymentState is where a deployment of one commit to one environment has
// got to.
type DeploymentState string

const (
	// DeploymentInProgress is a deployment whose workload is not serving yet.
	DeploymentInProgress DeploymentState = "in_progress"
	// DeploymentSuccess is a deployment that is serving.
	DeploymentSuccess DeploymentState = "success"
	// DeploymentFailure is a deployment that will not come up.
	DeploymentFailure DeploymentState = "failure"
	// DeploymentInactive retires a deployment whose environment is gone.
	DeploymentInactive DeploymentState = "inactive"
)

// Deployment is one commit deployed to one named environment. Providers model
// this as a deployment record with a history of statuses hanging off it, which
// is what gives a pull request its "View deployment" button.
type Deployment struct {
	// SHA of the deployed commit.
	SHA string
	// Ref is the branch the commit came from, for readability at the
	// provider.
	Ref string
	// Environment names the target. It is the identity a provider groups
	// deployments by, so it carries the Kitchen Environment's name, which is
	// already project-scoped.
	Environment string
	// State the deployment is in.
	State DeploymentState
	// Description is the one line shown with the status.
	Description string
	// URL is where the deployed application is reachable, empty when it is
	// not published (a preview whose gate is unavailable).
	URL string
	// Transient marks an environment that goes away again — every preview.
	Transient bool
	// Production marks the project's live environment.
	Production bool
}

// Comment is the single comment the platform keeps on a pull request,
// rewritten in place on every deploy rather than appended to.
type Comment struct {
	// PullRequest the comment belongs to.
	PullRequest int32
	// ID of the comment to rewrite. Empty on the first post, and after a
	// human deletes the comment: implementations fall back to finding
	// Marker, then to creating a new comment.
	ID string
	// Marker identifies the platform's own comment among everyone else's.
	// It is expected to be invisible in the rendered body — an HTML comment
	// — and distinct per environment, since two Kitchen projects can watch
	// one repository.
	Marker string
	// Body is the whole comment, Marker included.
	Body string
}

// StatusReporter is the statusChecks half of a git provider: everything the
// platform posts back to a repository about what it did with a commit.
//
// It is deliberately separate from Provider. A provider is useful as a source
// long before it can report anything back, and the operator asks for this half
// with a type assertion — so a new provider lands as a Provider first, gains
// this later, and the platform degrades to posting nothing in between.
type StatusReporter interface {
	// SetCommitStatus posts a status check on a commit, replacing whatever
	// the same context said before.
	SetCommitStatus(ctx context.Context, repo string, status CommitStatus) error
	// PublishDeployment records a deployment of a commit to an environment
	// and the state it is in, creating the deployment on first use.
	PublishDeployment(ctx context.Context, repo string, deployment Deployment) error
	// UpsertComment writes the platform's comment on a pull request and
	// returns its provider-side ID, so the next write can address it
	// directly instead of searching for it.
	UpsertComment(ctx context.Context, repo string, comment Comment) (string, error)
}

// Reporter narrows a Provider to its status-reporting half. The second return
// is false for a provider that cannot report status, which callers treat as
// "post nothing" rather than as a failure.
func Reporter(provider Provider) (StatusReporter, bool) {
	reporter, ok := provider.(StatusReporter)
	return reporter, ok
}

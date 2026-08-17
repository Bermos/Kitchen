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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// gitReporting posts what the platform did with a commit back to the
// repository it came from: a status check per build, a deployment per
// environment, and one pull request comment per preview.
//
// Everything here is best effort by construction. A revoked token is the
// Connection reconciler's business — it probes the credential and turns the
// connection red — and a build that produced an image did produce an image
// whether or not the provider would hear about it. So no method returns an
// error to a reconciler: they log, and the Environment records its last
// attempt in status.gitReport.
type gitReporting struct {
	client.Client
	// Factory resolves the git provider for a Connection. Defaults to
	// gitprovider.Default; tests inject fakes.
	Factory gitprovider.Factory
}

// reporterFor resolves the status-reporting half of the Project's source
// provider. The second return is false whenever the platform should stay
// quiet: no Connection, a Connection that does not claim statusChecks, a
// provider with no implementation, a missing credential, or a provider that
// can be a source but cannot post anything back yet.
func (g gitReporting) reporterFor(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
) (gitprovider.StatusReporter, bool) {
	conn := &kitchenv1alpha1.Connection{}
	key := types.NamespacedName{Namespace: project.Namespace, Name: project.Spec.Source.ConnectionRef.Name}
	if err := g.Get(ctx, key, conn); err != nil {
		return nil, false
	}
	if !connectionProvides(conn, kitchenv1alpha1.CapabilityStatusChecks) {
		return nil, false
	}

	creds := &corev1.Secret{}
	credsKey := types.NamespacedName{Namespace: conn.Namespace, Name: conn.Spec.CredentialsSecretRef.Name}
	if err := g.Get(ctx, credsKey, creds); err != nil {
		return nil, false
	}
	token := string(creds.Data[gitCredentialsTokenKey])
	if token == "" {
		return nil, false
	}

	factory := g.Factory
	if factory == nil {
		factory = gitprovider.Default
	}
	provider, err := factory(conn, token)
	if err != nil {
		return nil, false
	}
	return gitprovider.Reporter(provider)
}

// connectionProvides reports whether a Connection offers a capability. A
// Connection whose status has not been written yet claims nothing, which is
// the honest answer while its reconciler has not looked at it.
func connectionProvides(conn *kitchenv1alpha1.Connection, capability kitchenv1alpha1.Capability) bool {
	for _, c := range conn.Status.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// reportBuild posts the build's state as a status check on its commit.
func (g gitReporting) reportBuild(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	state gitprovider.CommitState,
	description string,
) {
	reporter, ok := g.reporterFor(ctx, project)
	if !ok {
		return
	}

	status := gitprovider.CommitStatus{
		SHA:         build.Spec.Git.SHA,
		State:       state,
		Context:     commitStatusContext(project.Name),
		Description: description,
	}
	if kitchen := g.platform(ctx); kitchen != nil {
		status.TargetURL = dashboardURL(kitchen, "builds", build.Name)
	}

	log := logf.FromContext(ctx)
	if err := reporter.SetCommitStatus(ctx, project.Spec.Source.Repo, status); err != nil {
		// The build itself is unaffected: this is the platform failing to
		// narrate it, and the Connection's own probe says why.
		log.Error(err, "failed to post the commit status", "project", project.Name,
			"repo", project.Spec.Source.Repo, "sha", build.Spec.Git.SHA)
		return
	}
	log.V(1).Info("posted commit status", "project", project.Name, "sha", build.Spec.Git.SHA, "state", state)
}

// reportEnvironment publishes the environment's deployment, and for a preview
// also the pull request comment carrying its URL. It returns the report to
// record in status — including the failure, when the provider refused it.
func (g gitReporting) reportEnvironment(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	revision kitchenv1alpha1.GitRevision,
	state gitprovider.DeploymentState,
	protected bool,
	previous *kitchenv1alpha1.GitReport,
) *kitchenv1alpha1.GitReport {
	reporter, ok := g.reporterFor(ctx, project)
	if !ok {
		// Nothing to report to. Whatever was recorded before was reported to
		// a provider the platform can no longer reach, so it stays as it is.
		return previous
	}

	now := metav1.Now()
	report := &kitchenv1alpha1.GitReport{
		Revision: revision.SHA,
		State:    string(state),
		URL:      env.Status.URL,
		At:       &now,
	}
	if previous != nil {
		report.CommentID = previous.CommentID
	}

	log := logf.FromContext(ctx)
	repo := project.Spec.Source.Repo

	err := reporter.PublishDeployment(ctx, repo, gitprovider.Deployment{
		SHA:         revision.SHA,
		Ref:         revision.Branch,
		Environment: env.Name,
		State:       state,
		Description: deploymentDescription(env, state),
		URL:         env.Status.URL,
		Transient:   env.Spec.Type == kitchenv1alpha1.EnvironmentPreview,
		Production:  env.Spec.Type == kitchenv1alpha1.EnvironmentProduction,
	})
	if err != nil {
		log.Error(err, "failed to publish the deployment", "environment", env.Name, "repo", repo)
		report.Error = err.Error()
		return report
	}

	// Only a preview belongs to a pull request, and the comment is the half
	// of this feature a reviewer actually reads.
	if env.Spec.Type != kitchenv1alpha1.EnvironmentPreview || env.Spec.Preview == nil {
		return report
	}

	comment := previewComment{
		Environment:  env.Name,
		Project:      project.Name,
		URL:          env.Status.URL,
		Phase:        env.Status.Phase,
		Release:      env.Spec.ReleaseRef.Name,
		Revision:     revision,
		Protected:    protected,
		DashboardURL: g.environmentPage(ctx, env),
	}
	id, err := reporter.UpsertComment(ctx, repo, gitprovider.Comment{
		PullRequest: env.Spec.Preview.PullRequest,
		ID:          report.CommentID,
		Marker:      comment.marker(),
		Body:        comment.body(),
	})
	if err != nil {
		log.Error(err, "failed to write the pull request comment", "environment", env.Name,
			"pullRequest", env.Spec.Preview.PullRequest)
		report.Error = err.Error()
		return report
	}
	report.CommentID = id
	log.V(1).Info("reported deploy status", "environment", env.Name, "state", state,
		"pullRequest", env.Spec.Preview.PullRequest)
	return report
}

// retireEnvironment tells the provider a preview is gone: the deployment goes
// inactive, and the comment stops advertising a URL that no longer answers.
// It runs while the Environment is being deleted, so it reports rather than
// records — there is no status left to write it to.
func (g gitReporting) retireEnvironment(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	report *kitchenv1alpha1.GitReport,
) {
	if report == nil || report.Revision == "" || env.Spec.Preview == nil {
		return
	}

	project := &kitchenv1alpha1.Project{}
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Spec.ProjectRef.Name}
	if err := g.Get(ctx, key, project); err != nil {
		return
	}
	reporter, ok := g.reporterFor(ctx, project)
	if !ok {
		return
	}

	log := logf.FromContext(ctx)
	repo := project.Spec.Source.Repo
	err := reporter.PublishDeployment(ctx, repo, gitprovider.Deployment{
		SHA:         report.Revision,
		Environment: env.Name,
		State:       gitprovider.DeploymentInactive,
		Description: "the preview environment was removed",
		Transient:   true,
	})
	if err != nil {
		log.Error(err, "failed to retire the deployment", "environment", env.Name, "repo", repo)
	}

	comment := previewComment{
		Environment: env.Name,
		Project:     project.Name,
		Removed:     true,
		Revision:    kitchenv1alpha1.GitRevision{SHA: report.Revision},
	}
	if _, err := reporter.UpsertComment(ctx, repo, gitprovider.Comment{
		PullRequest: env.Spec.Preview.PullRequest,
		ID:          report.CommentID,
		Marker:      comment.marker(),
		Body:        comment.body(),
	}); err != nil {
		log.Error(err, "failed to close out the pull request comment", "environment", env.Name)
	}
}

// platform loads the Kitchen singleton for the URLs a report links to. A
// platform that cannot be read is not a reason to skip the report: a status
// check without a target URL still says whether the build passed.
func (g gitReporting) platform(ctx context.Context) *kitchenv1alpha1.Kitchen {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := g.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return nil
	}
	return kitchen
}

// environmentPage is the environment's page in the dashboard, empty when the
// platform does not know its own address.
func (g gitReporting) environmentPage(ctx context.Context, env *kitchenv1alpha1.Environment) string {
	kitchen := g.platform(ctx)
	if kitchen == nil {
		return ""
	}
	return dashboardURL(kitchen, "environments", env.Name)
}

// commitStatusContext names the check on a commit. It carries the project,
// because a provider keys statuses by context and one repository can feed
// several Kitchen projects — sharing a context would have them overwrite each
// other's verdicts.
func commitStatusContext(projectName string) string {
	return "kitchen/" + projectName
}

// dashboardURL points at a page of the dashboard, which is served from the
// same origin as the API.
func dashboardURL(kitchen *kitchenv1alpha1.Kitchen, section, name string) string {
	return fmt.Sprintf("%s/%s/%s", strings.TrimSuffix(apiExternalURL(kitchen), "/"), section, name)
}

// deploymentDescription is the one line a provider shows next to a deployment.
func deploymentDescription(env *kitchenv1alpha1.Environment, state gitprovider.DeploymentState) string {
	switch state {
	case gitprovider.DeploymentSuccess:
		return fmt.Sprintf("%s is live", env.Name)
	case gitprovider.DeploymentFailure:
		return fmt.Sprintf("%s could not be deployed", env.Name)
	case gitprovider.DeploymentInactive:
		return fmt.Sprintf("%s was removed", env.Name)
	default:
		return fmt.Sprintf("%s is deploying", env.Name)
	}
}

// deploymentStateFor translates an Environment's phase into the deployment
// state a provider understands.
func deploymentStateFor(phase kitchenv1alpha1.EnvironmentPhase) gitprovider.DeploymentState {
	switch phase {
	case kitchenv1alpha1.EnvironmentLive:
		return gitprovider.DeploymentSuccess
	case kitchenv1alpha1.EnvironmentDegraded:
		return gitprovider.DeploymentFailure
	default:
		return gitprovider.DeploymentInProgress
	}
}

// previewComment is the pull request comment the platform keeps for a preview
// environment: written once when the preview first appears, rewritten in
// place on every deploy after that.
type previewComment struct {
	Environment  string
	Project      string
	URL          string
	Phase        kitchenv1alpha1.EnvironmentPhase
	Release      string
	Revision     kitchenv1alpha1.GitRevision
	DashboardURL string
	// Protected says the preview is gated behind platform login, which the
	// comment has to explain: a reviewer who is not a platform user meets a
	// sign-in page and would otherwise read it as a broken link.
	Protected bool
	// Removed turns the comment into the record of a preview that is gone.
	Removed bool
}

// marker identifies the platform's own comment. It is per environment rather
// than per repository, since one pull request can carry previews of several
// Kitchen projects watching the same repository.
func (c previewComment) marker() string {
	return fmt.Sprintf("<!-- kitchen-preview: %s -->", c.Environment)
}

// body renders the whole comment, marker included.
func (c previewComment) body() string {
	var b strings.Builder
	b.WriteString(c.marker())
	b.WriteString("\n### Kitchen preview\n\n")

	if c.Removed {
		fmt.Fprintf(&b, "The preview environment `%s` has been removed.\n", c.Environment)
		return b.String()
	}

	b.WriteString("| | |\n| --- | --- |\n")
	if c.URL != "" {
		fmt.Fprintf(&b, "| **Preview** | %s |\n", c.URL)
	}
	fmt.Fprintf(&b, "| **Status** | %s |\n", previewStatusWord(c.Phase))
	if c.Revision.SHA != "" {
		fmt.Fprintf(&b, "| **Commit** | `%s` |\n", shortSHA(c.Revision.SHA))
	}
	if c.Release != "" {
		fmt.Fprintf(&b, "| **Release** | `%s` |\n", c.Release)
	}
	if c.DashboardURL != "" {
		fmt.Fprintf(&b, "| **Dashboard** | [%s](%s) |\n", c.Environment, c.DashboardURL)
	}

	if c.Protected {
		b.WriteString("\nThis preview is gated behind Kitchen's login: an anonymous visitor is sent " +
			"to sign in first. That is the gate working, not a broken link.\n")
	}
	return b.String()
}

// previewStatusWord is the phase in the words a pull request reader expects,
// rather than the Environment's own vocabulary.
func previewStatusWord(phase kitchenv1alpha1.EnvironmentPhase) string {
	switch phase {
	case kitchenv1alpha1.EnvironmentLive:
		return "Ready"
	case kitchenv1alpha1.EnvironmentDegraded:
		return "Failed"
	case kitchenv1alpha1.EnvironmentTerminating:
		return "Removing"
	default:
		return "Building"
	}
}

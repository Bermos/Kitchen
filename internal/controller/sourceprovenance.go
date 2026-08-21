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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// Four eyes on a production change, recorded rather than inferred.
//
// It is the control a supervisor asks about first, and the usual way of
// answering it — going to the git provider's UI months later and looking — is
// not evidence. It is a screen that reflects the repository as it is now, on a
// pull request whose approvals may since have been dismissed, whose reviewers
// may have left, and whose branch protection may have been reconfigured twice.
//
// So the platform asks while the answer is still true, records what it was
// told, and signs it.
//
// # Refusing before the build, not after
//
// The check runs before the Job is created. That is fast feedback and no wasted
// compute, and it is what the requirement means: a commit that may not be built
// should not have been built. The **Build object still exists** and says why it
// was refused — refusing without a record would be the platform quietly
// dropping changes, which is a worse failure than the one being prevented.
//
// # Pull request builds are exempt, necessarily
//
// The requirement applies to the production branch only. A pull request's own
// builds are what produce the preview the review happens on; demanding the
// review before them would be a deadlock with itself.
//
// # The exemption is a list, and the list is the point
//
// Renovate merges its own dependency bumps. release-please merges its own
// release commits. This repository's release automation would fail this check
// on its first run. None of them will ever have an independent reviewer, and
// the realistic alternative to naming them is somebody switching the
// requirement off — so machine identities are named on the platform object and
// every use of the exemption is an audit record.
//
// # An outage is not a violation
//
// A provider that cannot be reached, a Connection with no such capability, a
// credential that has expired: none of those are findings about the commit.
// They are recorded as a check that could not be made, and they do **not**
// refuse the build. Failing closed here would mean a GitHub outage stopping
// every deployment on the platform, including the one fixing it — and the
// people affected would route around Kitchen entirely, which is the outcome
// this whole suite exists to prevent.

// PredicateSourceProvenance is minted after the build, over the artifact, out
// of what was recorded before it.
//
// The issue that asked for this spelled it `PullRequestApproval/v1`. It is
// registered here in the same shape as every other Kitchen predicate — lower
// case, hyphenated, under `attestation/` — because a URI space with one
// differently-shaped member is a URI space somebody will get wrong.
const PredicateSourceProvenance = "https://kitchen.bermos.dev/attestation/pull-request-approval/v1"

// RulePullRequest is the stable rule id an Exception names to break the glass
// on a project's pull request requirement. It is deliberately its own id
// rather than the engine's require-independent-review: that rule judges the
// artifact's attestation at promotion time and is waived through the engine's
// own exception path, while this one is the *build-time* refusal — a direct
// push to the production branch — which never reaches the engine at all. One
// id, documented in docs/api/exceptions.md and docs/COMPLIANCE.md §8.8;
// renaming it silently disconnects every standing grant.
const RulePullRequest = "require-pull-request"

// resolveSourceProvenance asks the provider how the commit arrived and decides
// whether the build may proceed.
//
// The second return is the refusal: non-nil means the build must not be
// scheduled, and carries the message to fail it with.
func (r *BuildReconciler) resolveSourceProvenance(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
) (*kitchenv1alpha1.SourceProvenanceStatus, error) {
	required := requiresPullRequest(build, project)
	status := &kitchenv1alpha1.SourceProvenanceStatus{
		Required:  required,
		CheckedAt: ptr.To(metav1.Now()),
	}

	reader, ok := r.changeReaderFor(ctx, project)
	if !ok {
		// Nothing to ask. Said plainly rather than silently: a project that
		// believes it requires review and whose provider cannot answer should
		// be able to find that out from the build rather than from an audit.
		status.Message = "this project's git connection cannot say how a commit reached the branch, " +
			"so nothing was established about its review"
		if required {
			if waived, err := r.breakGlass(ctx, build, project, status); err != nil || waived {
				return status, err
			}
			return status, fmt.Errorf(
				"this project requires commits to arrive through a reviewed pull request, and its git " +
					"connection cannot report whether this one did")
		}
		return status, nil
	}

	provenance, err := reader.CommitProvenance(ctx, project.Spec.Source.Repo, build.Spec.Git.SHA)
	if err != nil {
		// An outage is not a violation. See the note above: failing closed
		// here stops the deployment that fixes the outage.
		status.Message = "the git provider could not be asked how this commit arrived: " + err.Error()
		logf.FromContext(ctx).Info("source provenance could not be established",
			"build", build.Name, "cause", err.Error())
		return status, nil
	}

	status.Provider = provenance.Provider
	status.PullRequest = provenance.PullRequest
	status.Title = provenance.Title
	status.Author = provenance.Author
	status.MergedBy = provenance.MergedBy
	status.Approvers = provenance.Approvers()
	status.SelfApproved = provenance.SelfApproved()
	status.Independent = provenance.Independent()

	if !required {
		return status, nil
	}

	// The exemption is checked against the commit's author as the *provider*
	// named it where there is one, and against the commit author otherwise —
	// a machine's direct push has no pull request to read an author from.
	identity := provenance.Author
	if identity == "" {
		identity = build.Spec.Git.Author
	}
	if machine := r.machineIdentity(ctx, identity); machine != "" {
		status.MachineIdentity = machine
		if err := r.recordExemption(ctx, build, project, machine); err != nil {
			return status, err
		}
		return status, nil
	}

	var refusal error
	switch {
	case provenance.PullRequest == 0:
		refusal = fmt.Errorf(
			"this project requires commits to arrive through a reviewed pull request, and %s says this "+
				"one did not", provenance.Provider)
	case !provenance.Independent():
		refusal = fmt.Errorf("this project requires an independent review, and the only approval " +
			"on this change is its own author's")
	}
	if refusal != nil {
		// The break-glass path (#136): an active Exception naming
		// require-pull-request converts the refusal into an allowed,
		// privileged-audit-recorded build — never blocking the emergency
		// deployment is the design rule this whole suite stands on. The
		// exception is a specific, expiring, two-person grant; nothing else
		// gets through here.
		if waived, err := r.breakGlass(ctx, build, project, status); err != nil || waived {
			return status, err
		}
	}
	return status, refusal
}

// breakGlass consults the active break-glass exceptions for this project's
// production environment — a build-time waiver has no release yet, so only an
// environment-wide grant (no releaseRef) naming RulePullRequest applies. On a
// match the requirement is waived and loudly recorded: a privileged audit
// record first (its failure fails the build — fail-closed on the record, not
// the check, like the machine-identity exemption), then the exception's name
// on status so the signed source attestation carries it.
func (r *BuildReconciler) breakGlass(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	status *kitchenv1alpha1.SourceProvenanceStatus,
) (bool, error) {
	productionEnv := project.Name + "-production"
	active, err := ActiveExceptionsFor(ctx, r.Client, build.Namespace,
		project.Name, productionEnv, "", time.Now())
	if err != nil {
		// The listing failing is a platform fault, not a finding about the
		// commit — but unlike a provider outage it guards a refusal that is
		// about to happen, so it retries rather than deciding either way.
		return false, err
	}
	for i := range active {
		exception := &active[i]
		if !exception.WaivesRule(RulePullRequest) {
			continue
		}
		if err := r.Audit.Record(ctx, sourceBreakGlassTransition(build, project, exception)); err != nil {
			return false, err
		}
		status.Exception = exception.Name
		status.Message = fmt.Sprintf(
			"the pull request requirement was waived by break-glass exception %s, approved by %s, expiring %s: %s",
			exception.Name, exception.Spec.ApprovedBy,
			exception.Spec.ExpiresAt.UTC().Format(time.RFC3339), exception.Spec.Reason)
		logf.FromContext(ctx).Info("pull request requirement waived by break-glass exception",
			"build", build.Name, "exception", exception.Name, "approvedBy", exception.Spec.ApprovedBy)
		return true, nil
	}
	return false, nil
}

// sourceBreakGlassTransition is the privileged audit record a build-time
// break-glass appends before the build proceeds — built apart from the
// recording so a test can hold it up to the light without a store.
func sourceBreakGlassTransition(
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	exception *kitchenv1alpha1.Exception,
) audit.Transition {
	return audit.Transition{
		Object:      build,
		Kind:        audit.KindBuild,
		Controller:  actorBuildController,
		Correlation: correlationFor(build),
		Project:     project.Name,
		Reason: fmt.Sprintf(
			"break-glass: the pull request requirement was waived for commit %s by exception %s, "+
				"requested by %s, approved by %s",
			build.Spec.Git.SHA, exception.Name, exception.Spec.RequestedBy, exception.Spec.ApprovedBy),
		Details: map[string]any{
			"privileged":  true,
			"exception":   exception.Name,
			"rule":        RulePullRequest,
			"commit":      build.Spec.Git.SHA,
			"branch":      build.Spec.Git.Branch,
			"requirement": "pullRequest",
			"requestedBy": exception.Spec.RequestedBy,
			"approvedBy":  exception.Spec.ApprovedBy,
			"reason":      exception.Spec.Reason,
			"expiresAt":   exception.Spec.ExpiresAt.UTC().Format(time.RFC3339),
		},
	}
}

// requiresPullRequest reports whether this particular build has to prove it was
// reviewed.
//
// Only the production branch, and only a commit that is not itself a pull
// request build: a request's own builds are what produce the thing being
// reviewed.
func requiresPullRequest(build *kitchenv1alpha1.Build, project *kitchenv1alpha1.Project) bool {
	if !project.Spec.Source.RequirePullRequest {
		return false
	}
	if build.Spec.Git.PullRequest != nil {
		return false
	}
	return build.Spec.Git.Branch == project.Spec.Source.ProductionBranch
}

// machineIdentity answers which allowlisted identity a commit's author matches,
// or empty when none does.
//
// Matching is case-insensitive and exact. A glob would eventually exempt more
// than whoever wrote it meant, and an exemption that surprises its author is
// the one kind this must not have.
func (r *BuildReconciler) machineIdentity(ctx context.Context, author string) string {
	if author == "" {
		return ""
	}
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return ""
	}
	for _, identity := range kitchen.Spec.Compliance.MachineIdentities {
		if strings.EqualFold(identity, author) {
			return identity
		}
	}
	return ""
}

// recordExemption writes the use of the machine allowlist into the audit log.
//
// This is what makes the list auditable rather than merely configurable. A
// configured exemption says who *may* bypass review; a record says who did, for
// which commit, on which day — and the second is the one an auditor asks for.
func (r *BuildReconciler) recordExemption(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	identity string,
) error {
	return r.Audit.Record(ctx, exemptionTransition(build, project, identity))
}

// exemptionTransition is the record itself, built apart from the recording so
// that what it says can be asserted without a store behind it.
func exemptionTransition(
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	identity string,
) audit.Transition {
	return audit.Transition{
		Object:      build,
		Kind:        audit.KindBuild,
		Controller:  actorBuildController,
		Correlation: correlationFor(build),
		Project:     project.Name,
		Reason: fmt.Sprintf(
			"the pull request requirement was waived for %s, a machine identity on the platform's allowlist",
			identity),
		Details: map[string]any{
			"machineIdentity": identity,
			"commit":          build.Spec.Git.SHA,
			"branch":          build.Spec.Git.Branch,
			"requirement":     "pullRequest",
		},
	}
}

// changeReaderFor resolves the provenance half of the project's source
// provider, false when there is nothing to ask.
//
// It is a capability like the others: a provider can be a source without being
// able to answer this, and GitLab and Gitea are exactly that today — declared
// provider names with no implementation behind them. Where a Connection cannot
// answer, the platform says so rather than assuming.
func (r *BuildReconciler) changeReaderFor(
	ctx context.Context, project *kitchenv1alpha1.Project,
) (gitprovider.ChangeReader, bool) {
	connection := &kitchenv1alpha1.Connection{}
	key := types.NamespacedName{
		Namespace: project.Namespace, Name: project.Spec.Source.ConnectionRef.Name,
	}
	if err := r.Get(ctx, key, connection); err != nil {
		return nil, false
	}

	credentials := &corev1.Secret{}
	credentialsKey := types.NamespacedName{
		Namespace: connection.Namespace, Name: connection.Spec.CredentialsSecretRef.Name,
	}
	if err := r.Get(ctx, credentialsKey, credentials); err != nil {
		return nil, false
	}
	token := string(credentials.Data[gitCredentialsTokenKey])
	if token == "" {
		return nil, false
	}

	factory := r.GitProviders
	if factory == nil {
		factory = gitprovider.Default
	}
	provider, err := factory(connection, token)
	if err != nil {
		return nil, false
	}
	return gitprovider.Change(provider)
}

// sourceRecord is the predicate: who wrote the change, who agreed to it, and
// which provider said so.
//
// It records no verdict about whether the review was sufficient. Whether one
// approver is enough, whether a self-approval counts, whether a machine
// identity may deploy to production — all of those are policy, and policy is
// answered against the environment being promoted to. What is here is what
// happened.
func sourceRecord(
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	source *kitchenv1alpha1.SourceProvenanceStatus,
) map[string]any {
	record := map[string]any{
		"repository": project.Spec.Source.Repo,
		"commit":     build.Spec.Git.SHA,
		"branch":     build.Spec.Git.Branch,
		// Whose claim this is. The platform did not witness the review.
		"assertedBy": source.Provider,
		"required":   source.Required,
	}
	if source.CheckedAt != nil {
		record["checkedAt"] = source.CheckedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if source.PullRequest > 0 {
		record["pullRequest"] = source.PullRequest
		record["title"] = source.Title
		record["author"] = source.Author
		record["approvers"] = source.Approvers
		record["selfApproved"] = source.SelfApproved
		record["independentlyApproved"] = source.Independent
		if source.MergedBy != "" {
			record["mergedBy"] = source.MergedBy
		}
	} else {
		// Stated rather than left out. "No pull request" is the claim, and an
		// absent field would read as "not looked at".
		record["pullRequest"] = nil
		record["directPush"] = true
	}
	if source.MachineIdentity != "" {
		record["machineIdentity"] = source.MachineIdentity
		record["exempt"] = true
	}
	if source.Exception != "" {
		// The break-glass shape mirrors the machine exemption: the waiver is
		// named, and `exempt` says the requirement was not met but was waived
		// — a verifier reading only this attestation still sees both facts.
		record["exception"] = source.Exception
		record["exempt"] = true
	}
	if source.Message != "" {
		record["unestablished"] = source.Message
	}
	return record
}

// attestSource signs the source provenance recorded before the build and
// attaches it to the artifact.
//
// It repeats what was written down rather than asking the provider again. An
// approval can be dismissed between the build starting and finishing, and the
// evidence has to say what was true when the change was built — asking twice
// would produce an attestation about a different moment than the one the
// decision to build was made in.
func (r *BuildReconciler) attestSource(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	attester ArtifactAttester,
	signer attestation.Signer,
	repository, digest string,
	status *kitchenv1alpha1.ArtifactStatus,
) {
	source := build.Status.Source
	if source == nil {
		return
	}
	statement, err := attestation.NewStatement(
		repository, digest, PredicateSourceProvenance, sourceRecord(build, project, source))
	if err != nil {
		logf.FromContext(ctx).Info("the source provenance could not be stated",
			"build", build.Name, "cause", err.Error())
		return
	}
	if err := r.sign(ctx, attester, attestation.ArtifactRef(repository, digest),
		statement, signer, status, sourcePlatform); err != nil {
		status.Message = "the source provenance could not be attached: " + err.Error()
		logf.FromContext(ctx).Info("the source provenance was not attached",
			"build", build.Name, "cause", err.Error())
	}
}

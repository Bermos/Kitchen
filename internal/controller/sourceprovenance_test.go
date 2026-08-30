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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// What the platform does with the provider's answer.
//
// The provider's own edge cases live in internal/gitprovider. These are the
// decisions: who is refused, who is exempt, and what happens when nobody can be
// asked — which is the one the design is most opinionated about, because
// failing closed there stops the deployment that fixes the outage.

// changeProvider is a git provider that can answer how a commit arrived.
type changeProvider struct {
	provenance gitprovider.ChangeProvenance
	err        error
}

func (c *changeProvider) EnsureWebhook(
	context.Context, string, gitprovider.WebhookSpec,
) (string, error) {
	return "", nil
}

func (c *changeProvider) DeleteWebhook(context.Context, string, string) error { return nil }

func (c *changeProvider) CommitProvenance(
	context.Context, string, string,
) (gitprovider.ChangeProvenance, error) {
	if c.err != nil {
		return gitprovider.ChangeProvenance{}, c.err
	}
	return c.provenance, nil
}

// sourceFixtures is a project that requires review, and a build of its
// production branch.
func sourceFixtures(
	t *testing.T, provider *changeProvider, machines []string,
) (*BuildReconciler, *kitchenv1alpha1.Build, *kitchenv1alpha1.Project) {
	t.Helper()
	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			Compliance: kitchenv1alpha1.ComplianceSpec{MachineIdentities: machines},
		},
	}
	connection := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "github",
			CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "gh-credentials"},
		},
	}
	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-credentials", Namespace: PlatformNamespace},
		Data:       map[string][]byte{gitCredentialsTokenKey: []byte("token")},
	}
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source: kitchenv1alpha1.GitSourceSpec{
				ConnectionRef:      kitchenv1alpha1.LocalObjectReference{Name: "gh"},
				Repo:               "acme/shop",
				ProductionBranch:   "main",
				RequirePullRequest: true,
			},
		},
	}
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-1", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Git:        kitchenv1alpha1.GitRevision{SHA: "abc123def456", Branch: "main"},
		},
	}

	c := complianceClient(t, kitchen, connection, credentials, project, build)
	reconciler := &BuildReconciler{
		Client: c,
		// A recorder over a platform with no telemetry store records nothing
		// and says so, which is exactly the "there is no log" case audit
		// treats as not-an-error. What the exemption record *says* is asserted
		// against exemptionTransition instead.
		Audit: &audit.Recorder{
			Client:    c,
			Namespace: PlatformNamespace,
			Singleton: KitchenSingletonName,
		},
		GitProviders: func(*kitchenv1alpha1.Connection, string) (gitprovider.Provider, error) {
			return provider, nil
		},
	}
	return reconciler, build, project
}

func TestAReviewedCommitIsAllowedAndItsApproversAreRecorded(t *testing.T) {
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{
			Provider: "github", PullRequest: 42, Title: "Add checkout", Author: "alice",
			MergedBy: "bob",
			Approvals: []gitprovider.Approval{
				{Reviewer: "bob", SubmittedAt: "2026-08-19T09:00:00Z"},
			},
		},
	}, nil)

	status, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal != nil {
		t.Fatalf("a reviewed commit was refused: %v", refusal)
	}
	if status.PullRequest != 42 || status.Author != "alice" {
		t.Errorf("the request was not recorded: %+v", status)
	}
	if len(status.Approvers) != 1 || status.Approvers[0] != "bob" {
		t.Errorf("the approvers were not recorded: %+v", status.Approvers)
	}
	if !status.Independent || status.SelfApproved {
		t.Errorf("an independent review was recorded as %+v", status)
	}
	// Whose claim this is. The platform did not witness the review.
	if status.Provider != "github" {
		t.Errorf("the claim is not attributed: %q", status.Provider)
	}
	if !status.Required {
		t.Error("the status does not say the review was required, so a build with none " +
			"cannot be told from one nobody asked about")
	}
}

func TestADirectPushToProductionIsRefusedWhenReviewIsRequired(t *testing.T) {
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{Provider: "github"},
	}, nil)

	status, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal == nil {
		t.Fatal("a direct push to production was allowed where review is required")
	}
	if status.PullRequest != 0 {
		t.Errorf("a direct push was attributed to a request: %+v", status)
	}
}

func TestASelfApprovedChangeIsRefusedAndRecordedAsSelfApproved(t *testing.T) {
	// Refused, but recorded as what it was: an installation whose rules permit
	// self-approval has to be able to see that this is the case it is
	// permitting, and a policy engine has to be able to reject it separately.
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{
			Provider: "github", PullRequest: 7, Author: "alice",
			Approvals: []gitprovider.Approval{
				{Reviewer: "alice", SubmittedAt: "2026-08-19T09:00:00Z", SelfApproval: true},
			},
		},
	}, nil)

	status, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal == nil {
		t.Fatal("a self-approved change satisfied a requirement for independent review")
	}
	if !status.SelfApproved || status.Independent {
		t.Errorf("a self-approval was not recorded as one: %+v", status)
	}
	if len(status.Approvers) != 1 {
		t.Errorf("the self-approval was dropped rather than recorded: %+v", status.Approvers)
	}
}

func TestAMachineIdentityIsExemptAndTheExemptionIsAudited(t *testing.T) {
	// release-please merges its own release commits and will never have a
	// reviewer. The exemption exists so that the requirement is satisfiable;
	// the audit record is what stops it being a hole nobody can date.
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{Provider: "github", Author: "release-please[bot]"},
	}, []string{"renovate[bot]", "release-please[bot]"})

	status, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal != nil {
		t.Fatalf("an allowlisted machine identity was refused: %v", refusal)
	}
	if status.MachineIdentity != "release-please[bot]" {
		t.Errorf("the exemption was not recorded on the build: %+v", status)
	}

	// A configured exemption says who *may* bypass review. The record says who
	// did, for which commit — and the second is the one an auditor asks for.
	transition := exemptionTransition(build, project, status.MachineIdentity)
	if !strings.Contains(transition.Reason, "release-please[bot]") {
		t.Errorf("the audit record does not name the identity: %q", transition.Reason)
	}
	if transition.Details["machineIdentity"] != "release-please[bot]" {
		t.Errorf("the record does not carry the identity: %+v", transition.Details)
	}
	if transition.Details["commit"] != build.Spec.Git.SHA {
		t.Errorf("the record does not say which commit was waived: %+v", transition.Details)
	}
	if transition.Details["requirement"] != "pullRequest" {
		t.Errorf("the record does not say what was waived: %+v", transition.Details)
	}
	if transition.Kind != audit.KindBuild || transition.Project != project.Name {
		t.Errorf("the record is not filed against the build: %+v", transition)
	}
}

func TestAnIdentityThatIsNotOnTheListIsNotExempt(t *testing.T) {
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{Provider: "github", Author: "alice"},
	}, []string{"renovate[bot]"})

	if _, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project); refusal == nil {
		t.Fatal("a human's direct push was exempted by somebody else's allowlist entry")
	}
}

func TestAPullRequestsOwnBuildIsNotAskedToProveItWasReviewed(t *testing.T) {
	// The request's builds are what produce the thing being reviewed.
	// Requiring the review first would deadlock with itself.
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{Provider: "github"},
	}, nil)
	build.Spec.Git.PullRequest = ptr.To(int32(42))
	build.Spec.Git.Branch = "feature"

	status, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal != nil {
		t.Fatalf("a pull request's own build was refused: %v", refusal)
	}
	if status.Required {
		t.Error("a preview build was recorded as having required review")
	}
}

func TestACommitOnAnotherBranchIsNotAskedToProveItWasReviewed(t *testing.T) {
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{Provider: "github"},
	}, nil)
	build.Spec.Git.Branch = "spike"

	if _, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project); refusal != nil {
		t.Fatalf("a build of a side branch was refused: %v", refusal)
	}
}

func TestAProviderOutageDoesNotRefuseTheBuild(t *testing.T) {
	// The one the design is most opinionated about. Failing closed here means
	// a GitHub outage stops every deployment on the platform, including the
	// one fixing it — and the people affected route around Kitchen entirely,
	// which is the outcome the whole suite exists to prevent.
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		err: errors.New("502 from the provider"),
	}, nil)

	status, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal != nil {
		t.Fatalf("a provider outage refused the build: %v", refusal)
	}
	if status.Message == "" {
		t.Error("a check that could not be made left no trace")
	}
	if status.PullRequest != 0 || status.Independent {
		t.Errorf("an outage produced findings about the commit: %+v", status)
	}
}

func TestAProjectThatDoesNotRequireReviewStillRecordsWhatItFound(t *testing.T) {
	// The requirement is off, so nothing is refused — but the evidence is
	// worth having anyway, because a policy at promotion may want it even
	// where the project's own baseline did not.
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{
			Provider: "github", PullRequest: 3, Author: "alice",
			Approvals: []gitprovider.Approval{{Reviewer: "bob"}},
		},
	}, nil)
	project.Spec.Source.RequirePullRequest = false

	status, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal != nil {
		t.Fatalf("a project that requires nothing refused a build: %v", refusal)
	}
	if status.Required {
		t.Error("the status claims review was required")
	}
	if status.PullRequest != 3 || len(status.Approvers) != 1 {
		t.Errorf("the provenance was not recorded: %+v", status)
	}
}

func TestTheSourceRecordCarriesWhoAssertedItAndNoVerdict(t *testing.T) {
	_, build, project := sourceFixtures(t, &changeProvider{}, nil)
	record := sourceRecord(build, project, &kitchenv1alpha1.SourceProvenanceStatus{
		Provider: "github", PullRequest: 42, Author: "alice", Approvers: []string{"bob"},
		Independent: true, Required: true,
	})

	if record["assertedBy"] != "github" {
		t.Errorf("the record does not say whose claim it repeats: %+v", record)
	}
	// Whether one approver is enough, or a self-approval counts, is policy —
	// answered against the environment being promoted to, not here.
	for _, forbidden := range []string{"compliant", "pass", "passed", "verdict", "allowed"} {
		if _, present := record[forbidden]; present {
			t.Errorf("the source record carries a verdict field %q", forbidden)
		}
	}
}

func TestADirectPushSaysSoRatherThanOmittingTheField(t *testing.T) {
	// An absent field reads as "not looked at". The claim is that there was no
	// pull request, and it has to be stated.
	_, build, project := sourceFixtures(t, &changeProvider{}, nil)
	record := sourceRecord(build, project, &kitchenv1alpha1.SourceProvenanceStatus{
		Provider: "github", Required: true,
	})

	if record["directPush"] != true {
		t.Errorf("a direct push is not stated: %+v", record)
	}
	if _, present := record["pullRequest"]; !present {
		t.Error("the absence of a pull request is left to be inferred")
	}
}

// breakGlassGrant is the exception name the break-glass tests grant and then
// look for on the record.
const breakGlassGrant = "shop-exc-1"

// breakGlassException is an active Exception naming require-pull-request for
// shop's production environment — the shape the build-time break-glass reads.
func breakGlassException(name string, expiresIn time.Duration) *kitchenv1alpha1.Exception {
	return &kitchenv1alpha1.Exception{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.ExceptionSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: "shop-production"},
			RuleIDs:        []string{RulePullRequest},
			Reason:         "hotfix for the checkout outage",
			RequestedBy:    "grace@example.com",
			ApprovedBy:     "heidi@example.com",
			ExpiresAt:      metav1.NewTime(time.Now().Add(expiresIn)),
		},
	}
}

func TestADirectPushUnderABreakGlassExceptionIsAllowedAndLoudlyRecorded(t *testing.T) {
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{Provider: "github", PullRequest: 0},
	}, nil)
	exception := breakGlassException(breakGlassGrant, time.Hour)
	if err := reconciler.Client.Create(context.Background(), exception); err != nil {
		t.Fatal(err)
	}

	status, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal != nil {
		t.Fatalf("an emergency deployment must never be hard-blocked: %v", refusal)
	}
	if status.Exception != breakGlassGrant {
		t.Fatalf("the waiver must be named on the status, got %q", status.Exception)
	}
	if !strings.Contains(status.Message, breakGlassGrant) || !strings.Contains(status.Message, "heidi@example.com") {
		t.Fatalf("the message must say who let it through: %q", status.Message)
	}

	// The attestation carries the exemption fields, machine-identity style.
	record := sourceRecord(build, project, status)
	if record["exception"] != breakGlassGrant || record["exempt"] != true {
		t.Fatalf("the signed record must carry the waiver: %+v", record)
	}
	if record["directPush"] != true {
		t.Fatalf("the direct push is still stated — the exception changes the verdict, not the facts: %+v", record)
	}
}

func TestAStagedProjectsBreakGlassIsScopedToItsLastStage(t *testing.T) {
	// Under a staged pipeline (#133) the production target is the LAST
	// stage's environment, and that is where the build-time break-glass
	// looks: a grant scoped to the name the register actually shows must
	// break the glass.
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{Provider: "github", PullRequest: 0},
	}, nil)
	project.Spec.Promotion = &kitchenv1alpha1.PromotionPolicySpec{Stages: []kitchenv1alpha1.PromotionStage{
		{Name: "staging", Environment: "shop-staging"},
		{Name: "live", Environment: "shop-live"},
	}}
	exception := breakGlassException(breakGlassGrant, time.Hour)
	exception.Spec.EnvironmentRef = kitchenv1alpha1.LocalObjectReference{Name: "shop-live"}
	if err := reconciler.Client.Create(context.Background(), exception); err != nil {
		t.Fatal(err)
	}

	status, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal != nil {
		t.Fatalf("an exception scoped to the pipeline's last stage must break the glass: %v", refusal)
	}
	if status.Exception != breakGlassGrant {
		t.Fatalf("the waiver must be named on the status, got %q", status.Exception)
	}
}

func TestAStagedProjectIgnoresAGrantOnTheDefaultProductionName(t *testing.T) {
	// The other half of the scoping: with a pipeline declared,
	// `<project>-production` is not the production target unless a stage
	// names it, so a grant there covers nothing.
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{Provider: "github", PullRequest: 0},
	}, nil)
	project.Spec.Promotion = &kitchenv1alpha1.PromotionPolicySpec{Stages: []kitchenv1alpha1.PromotionStage{
		{Name: "live", Environment: "shop-live"},
	}}
	if err := reconciler.Client.Create(context.Background(),
		breakGlassException("shop-exc-misplaced", time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project); refusal == nil {
		t.Fatal("a grant on an environment that is not the staged project's production target waives nothing")
	}
}

func TestAnExpiredExceptionNoLongerBreaksTheGlass(t *testing.T) {
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{Provider: "github", PullRequest: 0},
	}, nil)
	if err := reconciler.Client.Create(context.Background(), breakGlassException("shop-exc-old", -time.Minute)); err != nil {
		t.Fatal(err)
	}

	_, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal == nil {
		t.Fatal("an expired grant waives nothing; the refusal must stand")
	}
}

func TestAReleaseScopedExceptionDoesNotCoverABuild(t *testing.T) {
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{Provider: "github", PullRequest: 0},
	}, nil)
	scoped := breakGlassException("shop-exc-scoped", time.Hour)
	scoped.Spec.ReleaseRef = &kitchenv1alpha1.LocalObjectReference{Name: "shop-rel-9"}
	if err := reconciler.Client.Create(context.Background(), scoped); err != nil {
		t.Fatal(err)
	}

	_, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal == nil {
		t.Fatal("a build has no release yet, so a release-scoped grant cannot cover it")
	}
}

func TestAnExceptionForAnotherRuleDoesNotCoverThePullRequestRequirement(t *testing.T) {
	reconciler, build, project := sourceFixtures(t, &changeProvider{
		provenance: gitprovider.ChangeProvenance{Provider: "github", PullRequest: 0},
	}, nil)
	other := breakGlassException("shop-exc-other", time.Hour)
	other.Spec.RuleIDs = []string{"max-severity"}
	if err := reconciler.Client.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}

	_, refusal := reconciler.resolveSourceProvenance(context.Background(), build, project)
	if refusal == nil {
		t.Fatal("a waiver is per-rule; one for max-severity says nothing about review")
	}
}

func TestTheBreakGlassTransitionSaysEverythingAnAuditorAsks(t *testing.T) {
	reconciler, build, project := sourceFixtures(t, &changeProvider{}, nil)
	_ = reconciler
	exception := breakGlassException(breakGlassGrant, time.Hour)
	transition := sourceBreakGlassTransition(build, project, exception)
	if transition.Kind != "Build" || transition.Project != "shop" {
		t.Fatalf("unexpected transition: %+v", transition)
	}
	if transition.Privileged != audit.PrivilegeBreakGlass {
		t.Fatalf("a break-glass use is a privileged break-glass record, got %q", transition.Privileged)
	}
	if transition.Details["rule"] != RulePullRequest || transition.Details["exception"] != breakGlassGrant {
		t.Fatalf("the record names the rule and the grant: %+v", transition.Details)
	}
	for _, key := range []string{"commit", "branch", "requestedBy", "approvedBy", "reason", "expiresAt"} {
		if _, ok := transition.Details[key]; !ok {
			t.Fatalf("the record must carry %q: %+v", key, transition.Details)
		}
	}
}

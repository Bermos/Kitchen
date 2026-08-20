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

package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// An environment's requirements: who may set the bar, and how a release
// measures up against it.
//
// The write here is the one place the enforcement table's project role is not
// the whole answer. The table admits anyone who may see the environment; the
// handler then applies the rule the table cannot spell: only the environment's
// owners — spec.owners, matched the way every access entry is — or a platform
// operator may change what it demands. That split is the point of the feature:
// deploying into an environment is a project role, deciding what it demands is
// ownership, and holding one must never confer the other.

// bundleDigestPattern is the CRD's own rule for spec.requirements.bundleDigest,
// checked here first so the refusal can say how to fix it rather than echoing
// an admission error.
var bundleDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// patchEnvironmentRequirementsRequest changes an environment's bar, its
// parameters, or who owns it. Every field is optional and absent means
// untouched, so one call can change any of the three without restating the
// rest.
type patchEnvironmentRequirementsRequest struct {
	// BundleDigest replaces the policy bundle the environment requires.
	// An empty string removes the requirements altogether — the environment
	// goes back to declaring no bar.
	BundleDigest *string `json:"bundleDigest,omitempty"`
	// Parameters replaces the whole parameter list. Absent leaves it alone;
	// an empty object clears it. (JSON tells the two apart: an absent map
	// decodes to nil, a present-but-empty one does not.)
	Parameters map[string]string `json:"parameters,omitempty"`
	// Owners replaces the owners list. An empty list is a lock, not an open
	// door: it leaves requirement changes to the platform's operators alone.
	Owners *[]string `json:"owners,omitempty"`
}

// environmentOwner reports whether the caller is named in the environment's
// owners list, by `sub` or by verified address — the same matching every
// access entry gets, through the same implementation.
func environmentOwner(env *kitchenv1alpha1.Environment, caller Caller) bool {
	for _, owner := range env.Spec.Owners {
		if access.SubjectMatches(owner, caller.access()) {
			return true
		}
	}
	return false
}

// requirementsRefusal is the 403 for a caller who may see the environment and
// may not set its bar. It says who may, because the whole point of the rule is
// that the answer is written on the environment rather than implied by a role.
func requirementsRefusal(env *kitchenv1alpha1.Environment) string {
	if len(env.Spec.Owners) == 0 {
		return fmt.Sprintf("environment %s names no owners, so only a platform operator may change its requirements",
			env.Name)
	}
	return fmt.Sprintf("changing the requirements of %s is for its owners (%s) or a platform operator; "+
		"deploying into an environment does not grant a say in what it demands",
		env.Name, strings.Join(env.Spec.Owners, ", "))
}

// changedParameterNames is which parameters a replacement list touches —
// added, removed or changed — by name and never by value. Values are the
// owner's policy tuning, not a secret, but the audit log records field names
// for every other write and a second convention would be a second thing to
// get wrong.
func changedParameterNames(before, after map[string]string) []string {
	names := map[string]struct{}{}
	for name, value := range before {
		if newValue, ok := after[name]; !ok || newValue != value {
			names[name] = struct{}{}
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			names[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// requirementsTransition is the audit record a requirements change appends
// before it is made. It is built apart from the recording so a test can hold
// it up to the light without a store: the previous digest is what makes the
// change reversible on paper, the parameter names say what moved without
// copying values, and privileged marks it as a segregation-of-duties write.
func requirementsTransition(
	env *kitchenv1alpha1.Environment,
	previousDigest, nextDigest string,
	changedParameters []string,
	owners *[]string,
) audit.Transition {
	details := map[string]any{
		"privileged":           true,
		"previousBundleDigest": previousDigest,
		"bundleDigest":         nextDigest,
	}
	if len(changedParameters) > 0 {
		details["changedParameters"] = changedParameters
	}
	if owners != nil {
		details["owners"] = *owners
	}
	return audit.Transition{
		Object:    env,
		Kind:      audit.KindEnvironment,
		Operation: clickhouse.AuditUpdate,
		From:      previousDigest,
		To:        nextDigest,
		Project:   env.Spec.ProjectRef.Name,
		Reason:    fmt.Sprintf("environment %s requirements changed", env.Name),
		Details:   details,
	}
}

// patchEnvironmentRequirements changes what an environment demands of an
// artifact, and who may change that. The table let the caller in on a project
// role; the ownership rule is enforced here, before the body is even read,
// because it does not depend on what the change is.
func (s *Server) patchEnvironmentRequirements(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	if !platformRoleFrom(ctx).AtLeast(access.PlatformOperator) && !environmentOwner(env, caller) {
		forbidden(w, requirementsRefusal(env))
		return
	}

	body := patchEnvironmentRequirementsRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if body.BundleDigest == nil && body.Parameters == nil && body.Owners == nil {
		badRequest(w, "nothing to change: send bundleDigest, parameters or owners")
		return
	}
	if body.Owners != nil {
		for _, owner := range *body.Owners {
			if strings.TrimSpace(owner) == "" {
				badRequest(w, "owners must name accounts: an issuer subject, or an email address containing %q", "@")
				return
			}
		}
	}

	// Work out the requirements this patch leaves behind before anything is
	// recorded, so the record describes the change that is actually made.
	previousDigest, previousParameters := "", map[string]string(nil)
	if env.Spec.Requirements != nil {
		previousDigest = env.Spec.Requirements.BundleDigest
		previousParameters = env.Spec.Requirements.Parameters
	}
	nextDigest, nextParameters := previousDigest, previousParameters
	if body.BundleDigest != nil {
		nextDigest = strings.TrimSpace(*body.BundleDigest)
		if nextDigest != "" && !bundleDigestPattern.MatchString(nextDigest) {
			badRequest(w, "bundleDigest %q is not a bundle digest: it has the form sha256:<64 hex characters>",
				nextDigest)
			return
		}
	}
	if body.Parameters != nil {
		nextParameters = body.Parameters
	}
	if nextDigest == "" {
		if body.Parameters != nil {
			if body.BundleDigest == nil {
				badRequest(w, "environment %q declares no requirements, so there are no parameters to change: "+
					"set bundleDigest first", env.Name)
				return
			}
			badRequest(w, "parameters have nothing to parameterize: an empty bundleDigest removes the "+
				"requirements, and an environment without requirements takes none")
			return
		}
		nextParameters = nil
	}

	changed := changedParameterNames(previousParameters, nextParameters)
	if !s.recorded(w, req, requirementsTransition(env, previousDigest, nextDigest, changed, body.Owners)) {
		return
	}

	patch := client.MergeFrom(env.DeepCopy())
	if body.Owners != nil {
		env.Spec.Owners = *body.Owners
	}
	switch {
	case nextDigest == "":
		env.Spec.Requirements = nil
	default:
		env.Spec.Requirements = &kitchenv1alpha1.EnvironmentRequirements{
			BundleDigest: nextDigest,
			Parameters:   nextParameters,
		}
	}
	if err := s.Client.Patch(ctx, env, patch); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("environment requirements changed through the api",
		"environment", env.Name, "project", env.Spec.ProjectRef.Name, "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newEnvironmentView(env))
}

// eligibilityBody is what GET .../eligibility answers: the environment's bar,
// the evidence the release's artifact carries, and — once the policy engine
// exists — the verdict. The shape already has room for the verdict so the
// engine can fill it in without breaking a reader: `eligible` is null until
// something has actually evaluated, never a guess.
type eligibilityBody struct {
	Environment  string                    `json:"environment"`
	Project      string                    `json:"project"`
	Release      string                    `json:"release"`
	Requirements *requirementsView         `json:"requirements"`
	Evidence     []eligibilityEvidenceView `json:"evidence"`
	// Eligible is true or false only after an evaluation; null says nothing
	// has judged this pair yet.
	Eligible  *bool `json:"eligible"`
	Evaluated bool  `json:"evaluated"`
	// UnmetRules names the rules that fired, as stable rule ids — never a
	// generic failure. Empty until the policy engine evaluates.
	UnmetRules []string `json:"unmetRules"`
	Message    string   `json:"message,omitempty"`
}

// evaluateRequirements is the evaluation seam the policy engine (#132) fills
// in. Until it exists there are exactly two honest answers: an environment
// that declares no requirements is a bar of height zero, cleared by
// everything; one that declares any is not evaluated yet, and saying
// "eligible: null, evaluated: false" is the difference between "not judged"
// and "passed" that a promotion pipeline will stand on.
//
// Whatever replaces this stays a pure function of the environment's declared
// requirements and the stored evidence handed to it — no live checks, no
// network, so a decision can be replayed from what was recorded.
func evaluateRequirements(
	requirements *kitchenv1alpha1.EnvironmentRequirements,
) (eligible *bool, evaluated bool, unmetRules []string, message string) {
	if requirements == nil {
		yes := true
		return &yes, true, []string{},
			"this environment declares no requirements, so every release is eligible"
	}
	return nil, false, []string{},
		"requirements are declared but not evaluated: policy engine evaluation lands with the promotion pipeline"
}

// environmentEligibility answers how a release measures up against an
// environment's requirements — a pure function of the stored evidence, which
// is what makes the answer the same however often and however late it is
// asked. It stores no decision and says nothing about whether a promotion
// would be allowed to *happen*; it reads the bar and reads the evidence, and
// the promotion pipeline is what will act on the comparison.
func (s *Server) environmentEligibility(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}

	// The release defaults to the one the environment runs, because "is what
	// is deployed here still eligible" is the question the screen asks.
	releaseName := strings.TrimSpace(req.URL.Query().Get("release"))
	if releaseName == "" {
		releaseName = env.Spec.ReleaseRef.Name
	}
	if releaseName == "" {
		badRequest(w, "environment %q runs no release; name one with ?release=<release name>", env.Name)
		return
	}
	release := &kitchenv1alpha1.Release{}
	if err := s.get(ctx, releaseName, release); err != nil {
		s.writeError(w, err)
		return
	}
	if release.Spec.ProjectRef.Name != env.Spec.ProjectRef.Name {
		badRequest(w, "release %q belongs to project %q, but environment %q belongs to project %q",
			release.Name, release.Spec.ProjectRef.Name, env.Name, env.Spec.ProjectRef.Name)
		return
	}

	body := eligibilityBody{
		Environment:  env.Name,
		Project:      env.Spec.ProjectRef.Name,
		Release:      release.Name,
		Requirements: newRequirementsView(env.Spec.Requirements),
		Evidence:     []eligibilityEvidenceView{},
	}
	var verdict string
	body.Eligible, body.Evaluated, body.UnmetRules, verdict = evaluateRequirements(env.Spec.Requirements)

	evidence, caveat := s.materializeEvidence(ctx, release)
	body.Evidence = evidence
	body.Message = verdict
	if caveat != "" {
		body.Message = verdict + ". " + caveat
	}
	writeJSON(w, http.StatusOK, body)
}

// materializeEvidence is the evidence half of an eligibility answer: what the
// release's artifact carries, summarized to what a bar is judged against.
//
// The index on the Build says what was attached and by whom; the registry is
// the source of truth and the only place a signature can be checked. So the
// index is the skeleton, the registry fills in `verified` — and a registry
// that cannot be asked degrades to the index with a sentence saying so,
// because an unverifiable list shown as verified would be the one dishonest
// answer this endpoint could give.
func (s *Server) materializeEvidence(
	ctx context.Context,
	release *kitchenv1alpha1.Release,
) ([]eligibilityEvidenceView, string) {
	views := []eligibilityEvidenceView{}

	build := &kitchenv1alpha1.Build{}
	if err := s.get(ctx, release.Spec.BuildRef.Name, build); err != nil {
		if apierrors.IsNotFound(err) {
			return views, "The release's build is gone, so no evidence index remains to read"
		}
		return views, "The release's build could not be read: " + err.Error()
	}
	artifact := build.Status.Artifact
	if artifact == nil || artifact.Digest == "" {
		return views, "The release's build recorded no artifact digest, so nothing carries evidence"
	}
	for _, entry := range artifact.Evidence {
		views = append(views, eligibilityEvidenceView{
			PredicateType: entry.PredicateType,
			Source:        entry.Source,
		})
	}

	set, err := s.artifactEvidence(ctx, build, artifact)
	if err != nil {
		return views, "The registry could not be asked to verify the evidence, so it is listed unverified: " +
			err.Error()
	}
	verified := map[string]bool{}
	indexed := map[string]bool{}
	for i := range views {
		indexed[views[i].PredicateType] = true
	}
	for _, evidence := range set.Attestations {
		if evidence.Verified {
			verified[evidence.PredicateType] = true
		}
		// Evidence in the registry the index never heard of — attached by
		// something other than the platform — is still evidence the artifact
		// carries, listed with no source claimed for it.
		if !indexed[evidence.PredicateType] {
			indexed[evidence.PredicateType] = true
			views = append(views, eligibilityEvidenceView{
				PredicateType: evidence.PredicateType,
				Verified:      evidence.Verified,
			})
		}
	}
	for i := range views {
		if verified[views[i].PredicateType] {
			views[i].Verified = true
		}
	}
	if !set.Verified {
		return views, "The platform holds no signing key, so the evidence is listed without verification"
	}
	return views, ""
}

// artifactEvidence reads what is attached to a build's artifact, verified
// against the platform's key when there is one — the same read
// buildAttestations serves, minus the HTTP framing.
func (s *Server) artifactEvidence(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	artifact *kitchenv1alpha1.ArtifactStatus,
) (attestation.EvidenceSet, error) {
	reader, err := s.evidenceFor(ctx, build)
	if err != nil {
		return attestation.EvidenceSet{}, err
	}
	verifiers := []attestation.Verifier{}
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err == nil {
		if key, err := controller.SigningKeyFor(ctx, s.Client, kitchen); err == nil && key != nil {
			verifiers = append(verifiers, key)
		}
	}
	return reader.Evidence(ctx, artifact.Repository+"@"+artifact.Digest, verifiers...)
}

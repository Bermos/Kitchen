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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/policy"
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
	// DataClass rates the environment: the highest sensitivity class it may
	// hold, which the promotion rule dataclass-le-environment compares the
	// project's class against. Empty removes the rating. It travels on this
	// endpoint because it is the same kind of declaration as the bundle —
	// the owners' bar, not the deploying team's — and it is guarded and
	// audit-logged the same way.
	DataClass *string `json:"dataClass,omitempty"`
	// Residency declares where this environment's data is located. Empty
	// falls back to the platform default; the value is declared, not
	// observed.
	Residency *string `json:"residency,omitempty"`
	// Criticality designates how much it matters that *this environment*
	// keeps working, and RTO/RPO are its disruption tolerances. Empty removes
	// each. They travel on this endpoint for the same reason the data class
	// does: what an environment is worth is its owners' declaration, not the
	// deploying team's, and a preview of a critical project is not itself a
	// critical function.
	//
	// Kitchen does not decide any of the three, and refuses no deployment
	// because of them. Setting an RTO changes when env.rto-at-risk fires;
	// designating an environment critical raises this environment's warnings
	// to critical findings.
	Criticality *string `json:"criticality,omitempty"`
	RTO         *string `json:"rto,omitempty"`
	RPO         *string `json:"rpo,omitempty"`
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

// dataClassChange and residencyChange carry a declaration's before and after
// for the audit record — a classification change without its previous value
// is a record nobody can reverse on paper.
type dataClassChange struct{ previous, next string }

type residencyChange struct{ previous, next string }

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
	dataClass *dataClassChange,
	residency *residencyChange,
	continuity continuityChange,
	before kitchenv1alpha1.Continuity,
) audit.Transition {
	details := map[string]any{
		"previousBundleDigest": previousDigest,
		"bundleDigest":         nextDigest,
	}
	if len(changedParameters) > 0 {
		details["changedParameters"] = changedParameters
	}
	if owners != nil {
		details["owners"] = *owners
	}
	if dataClass != nil {
		details["previousDataClass"] = dataClass.previous
		details["dataClass"] = dataClass.next
	}
	if residency != nil {
		details["previousResidency"] = residency.previous
		details["residency"] = residency.next
	}
	continuity.recordInto(details, before)
	return audit.Transition{
		Object:     env,
		Kind:       audit.KindEnvironment,
		Operation:  clickhouse.AuditUpdate,
		Privileged: audit.PrivilegeRequirements,
		From:       previousDigest,
		To:         nextDigest,
		Project:    env.Spec.ProjectRef.Name,
		Reason:     fmt.Sprintf("environment %s requirements changed", env.Name),
		Details:    details,
	}
}

// barChange is the bundle and parameters a PATCH leaves behind, worked out
// before anything is recorded so that the audit record describes the change
// that is actually made.
type barChange struct {
	previousDigest     string
	previousParameters map[string]string
	nextDigest         string
	nextParameters     map[string]string
}

// resolveBar works that out, or says what is wrong with the request. It is a
// function of its own rather than a block in the handler because the three
// refusals below are the fiddly part of this endpoint and are worth reading
// without the ownership check, the designation and the recording around them.
func resolveBar(
	env *kitchenv1alpha1.Environment, body patchEnvironmentRequirementsRequest,
) (barChange, string) {
	bar := barChange{}
	if env.Spec.Requirements != nil {
		bar.previousDigest = env.Spec.Requirements.BundleDigest
		bar.previousParameters = env.Spec.Requirements.Parameters
	}
	bar.nextDigest, bar.nextParameters = bar.previousDigest, bar.previousParameters

	if body.BundleDigest != nil {
		bar.nextDigest = strings.TrimSpace(*body.BundleDigest)
		if bar.nextDigest != "" && !bundleDigestPattern.MatchString(bar.nextDigest) {
			return barChange{}, fmt.Sprintf(
				"bundleDigest %q is not a bundle digest: it has the form sha256:<64 hex characters>",
				bar.nextDigest)
		}
	}
	if body.Parameters != nil {
		bar.nextParameters = body.Parameters
	}
	if bar.nextDigest != "" {
		return bar, ""
	}
	if body.Parameters == nil {
		bar.nextParameters = nil
		return bar, ""
	}
	if body.BundleDigest == nil {
		return barChange{}, fmt.Sprintf(
			"environment %q declares no requirements, so there are no parameters to change: "+
				"set bundleDigest first", env.Name)
	}
	return barChange{}, "parameters have nothing to parameterize: an empty bundleDigest removes the " +
		"requirements, and an environment without requirements takes none"
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
	continuity, err := continuityFromRequest(body.Criticality, body.RTO, body.RPO)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if body.BundleDigest == nil && body.Parameters == nil && body.Owners == nil &&
		body.DataClass == nil && body.Residency == nil && !continuity.touched() {
		badRequest(w, "nothing to change: send bundleDigest, parameters, owners, dataClass, "+
			"residency, criticality, rto or rpo")
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

	bar, problem := resolveBar(env, body)
	if problem != "" {
		badRequest(w, "%s", problem)
		return
	}
	previousDigest, previousParameters := bar.previousDigest, bar.previousParameters
	nextDigest, nextParameters := bar.nextDigest, bar.nextParameters

	var classChange *dataClassChange
	if body.DataClass != nil {
		class, err := dataClassFromRequest(*body.DataClass)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		classChange = &dataClassChange{previous: string(env.Spec.DataClass), next: string(class)}
	}
	var residency *residencyChange
	if body.Residency != nil {
		residency = &residencyChange{previous: env.Spec.Residency, next: strings.TrimSpace(*body.Residency)}
	}

	changed := changedParameterNames(previousParameters, nextParameters)
	// The designation as it stands, for the record: a change without its
	// previous value is a change nobody can reverse on paper.
	before := kitchenv1alpha1.Continuity{
		Criticality: env.Spec.Criticality, RTO: env.Spec.RTO, RPO: env.Spec.RPO,
	}
	if !s.recorded(w, req, requirementsTransition(env, previousDigest, nextDigest, changed, body.Owners,
		classChange, residency, continuity, before)) {
		return
	}

	patch := client.MergeFrom(env.DeepCopy())
	if body.Owners != nil {
		env.Spec.Owners = *body.Owners
	}
	if classChange != nil {
		env.Spec.DataClass = kitchenv1alpha1.DataClass(classChange.next)
	}
	if residency != nil {
		env.Spec.Residency = residency.next
	}
	continuity.apply(&env.Spec.Criticality, &env.Spec.RTO, &env.Spec.RPO)
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

// evaluateRequirements answers how a release measures up, through the same
// engine every stored decision comes from (internal/policy). It is a pure
// function of the environment's declared requirements and the materialized
// evidence handed to it — no live checks, no network; the engine's compile
// step enforces that rather than trusting it — and it is a read: the input's
// kind is "eligibility" and no decision is stored, because nothing was
// decided.
//
// Three honest answers remain. No requirements is a bar of height zero,
// cleared by everything. A bundle that resolves is evaluated for real, and
// eligible says what a promotion's verdict would say. A bundle that cannot be
// resolved is *not evaluated*, eligible null — the difference between "not
// judged" and "passed" that a promotion pipeline stands on.
func (s *Server) evaluateRequirements(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	build *kitchenv1alpha1.Build,
	evidence []policy.Evidence,
) (eligible *bool, evaluated bool, unmetRules []string, message string) {
	requirements := env.Spec.Requirements
	if requirements == nil {
		yes := true
		return &yes, true, []string{},
			"this environment declares no requirements, so every release is eligible"
	}

	resolver := &policy.Resolver{Client: s.Client, Namespace: s.Namespace}
	info, err := resolver.Resolve(ctx, requirements.BundleDigest)
	if err != nil {
		return nil, false, []string{},
			"requirements are declared but not evaluated: " + err.Error()
	}

	input := s.eligibilityInput(ctx, env, release, build, evidence)
	result, err := policy.Evaluate(ctx, info.Bundle, input)
	if err != nil {
		return nil, false, []string{},
			"requirements are declared but not evaluated: " + err.Error()
	}

	unmetRules = []string{}
	for _, rule := range result.Fired {
		if !rule.Waived {
			unmetRules = append(unmetRules, rule.Rule)
		}
	}
	allowed := result.Verdict != policy.VerdictBlocked
	switch result.Verdict {
	case policy.VerdictAllowed:
		message = fmt.Sprintf("the release clears bundle %s", requirements.BundleDigest)
	case policy.VerdictAllowedWithException:
		message = fmt.Sprintf("the release clears bundle %s, with every fired rule waived by an exception",
			requirements.BundleDigest)
	default:
		message = fmt.Sprintf("the release does not clear bundle %s: %d rule(s) fired",
			requirements.BundleDigest, len(unmetRules))
	}
	return &allowed, true, unmetRules, message
}

// eligibilityInput materializes the engine's input for the read-only
// eligibility question, through the one materializer every evaluation uses
// (policy.MaterializeInput) — which is what keeps the preview and the
// promotion decision the same evaluation: same bundle, same evidence, same
// claims, same answer.
func (s *Server) eligibilityInput(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	build *kitchenv1alpha1.Build,
	evidence []policy.Evidence,
) policy.Input {
	// The project and the claims are context rather than prerequisites: a
	// project that cannot be read is judged unclassified, and claims that
	// cannot be listed are judged absent — the same degraded honesty the
	// evidence read keeps.
	var project *kitchenv1alpha1.Project
	found := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, env.Spec.ProjectRef.Name, found); err == nil {
		project = found
	}
	claims := []policy.Claim{}
	list := &kitchenv1alpha1.ResourceClaimList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err == nil {
		claims = policy.ClaimFacts(env, list.Items)
	}
	at := time.Now().UTC()
	input := policy.MaterializeInput(policy.KindEligibility, at, project, env, release, build, evidence, claims)
	// The caller is the requester the preview is about. A promotion this
	// person asked for would carry their name, so a preview that left it out
	// would answer the four-eyes question over a vendored digest for nobody
	// — and tell them a move was allowed that their own request would be
	// refused for (#309).
	caller, _ := CallerFrom(ctx)
	input.RequestedBy = callerName(caller)
	// Active exceptions join the input through the one listing the promotion
	// reconciler consults (ActiveExceptionsFor), judged at the same clock the
	// input carries — which is what keeps the preview's verdict the verdict a
	// promotion would act on: a pair waived into allowed-with-exception must
	// not read as blocked on the screen. A listing that fails degrades like
	// the project and the claims do — judged absent, which can only make the
	// preview stricter than the promotion, never looser.
	if active, err := controller.ActiveExceptionsFor(ctx, s.Client, s.Namespace,
		env.Spec.ProjectRef.Name, env.Name, release.Name, at); err == nil {
		input.Exceptions = controller.PolicyExceptions(active)
	}
	return input
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

	materialized := s.materializeEvidence(ctx, release)
	body.Evidence = materialized.views

	var verdict string
	body.Eligible, body.Evaluated, body.UnmetRules, verdict =
		s.evaluateRequirements(ctx, env, release, materialized.build, materialized.input)
	body.Message = verdict
	if materialized.caveat != "" {
		body.Message = verdict + ". " + materialized.caveat
	}
	writeJSON(w, http.StatusOK, body)
}

// materializedEvidence is the evidence half of an eligibility answer, in both
// of its shapes: the summary the screen renders, and the engine's input.
type materializedEvidence struct {
	views []eligibilityEvidenceView
	// input is the same evidence materialized for the policy engine, with
	// predicates verbatim. When the registry could not be asked it degrades
	// to the index — predicate types without predicates, unverified — and the
	// caveat says so; an evaluation over it is honest about what it saw.
	input []policy.Evidence
	// build is the release's build, when it still exists.
	build  *kitchenv1alpha1.Build
	caveat string
}

// materializeEvidence gathers what the release's artifact carries, summarized
// for the screen and materialized for the engine.
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
) materializedEvidence {
	out := materializedEvidence{views: []eligibilityEvidenceView{}, input: []policy.Evidence{}}

	build := &kitchenv1alpha1.Build{}
	if err := s.get(ctx, release.Spec.BuildRef.Name, build); err != nil {
		if apierrors.IsNotFound(err) {
			out.caveat = "The release's build is gone, so no evidence index remains to read"
			return out
		}
		out.caveat = "The release's build could not be read: " + err.Error()
		return out
	}
	out.build = build

	// Every image the release deploys, not the project's own alone. A unit
	// ships one image per workload that declares a build, each attested in
	// its own right, and an eligibility answer over the first of them would
	// preview a promotion that judges all of them (#300).
	artifacts := []kitchenv1alpha1.BuildArtifact{}
	for _, artifact := range build.Artifacts() {
		if artifact.Artifact.Digest != "" && artifact.Artifact.Repository != "" {
			artifacts = append(artifacts, artifact)
		}
	}
	if len(artifacts) == 0 {
		out.caveat = "The release's build recorded no artifact digest, so nothing carries evidence"
		return out
	}
	// Which images of the unit carry no signed evidence at all. It is said
	// here rather than left to be inferred from a short list: an eligibility
	// answer that showed the API's provenance and said nothing about the
	// worker would read as covering the release.
	//
	// The notes accumulate rather than overwrite one another, because they
	// are answers to different questions — what the artifacts carry, and how
	// well this read could see it — and the second silently replacing the
	// first is how a caveat comes to hide the thing it was written for.
	notes := []string{}
	if missing := build.ArtifactsWithoutEvidence(); len(missing) > 0 {
		notes = append(notes, "No signed evidence describes "+artifactSentence(missing)+
			", so a rule requiring evidence will fire for "+
			pluralArtifacts(len(missing), "it", "them"))
	}

	registryFailed, unsigned := "", false
	for _, subject := range artifacts {
		sources := policy.EvidenceSources(subject.Artifact)
		indexed := map[string]bool{}
		for _, entry := range subject.Artifact.Evidence {
			indexed[entry.PredicateType] = true
			out.views = append(out.views, eligibilityEvidenceView{
				PredicateType: entry.PredicateType,
				Source:        entry.Source,
				Workload:      subject.Name(),
			})
		}

		set, err := s.artifactEvidence(ctx, build, subject.Workload, subject.Artifact)
		if err != nil {
			// The index without the registry: types and sources, no
			// predicates, nothing verified.
			registryFailed = err.Error()
			continue
		}
		out.input = append(out.input,
			policy.EvidenceFrom(subject.Workload, set, sources)...)

		verified := map[string]bool{}
		for _, evidence := range set.Attestations {
			if evidence.Verified {
				verified[evidence.PredicateType] = true
			}
			// Evidence in the registry the index never heard of — attached by
			// something other than the platform — is still evidence the
			// artifact carries, listed with no source claimed for it.
			if !indexed[evidence.PredicateType] {
				indexed[evidence.PredicateType] = true
				out.views = append(out.views, eligibilityEvidenceView{
					PredicateType: evidence.PredicateType,
					Verified:      evidence.Verified,
					Workload:      subject.Name(),
				})
			}
		}
		for i := range out.views {
			if out.views[i].Workload == subject.Name() && verified[out.views[i].PredicateType] {
				out.views[i].Verified = true
			}
		}
		if !set.Verified && !unsigned {
			unsigned = true
			notes = append(notes,
				"The platform holds no signing key, so the evidence is listed without verification")
		}
	}
	if registryFailed != "" {
		// One artifact the registry would not answer for makes the whole
		// answer a degraded one: an input half read from the registry and
		// half from the index would be an evaluation nobody could reproduce.
		out.input = policy.IndexedEvidence(build)
		notes = append(notes,
			"The registry could not be asked to verify the evidence, so it is listed unverified: "+
				registryFailed)
	}
	out.caveat = strings.Join(notes, ". ")
	return out
}

// artifactSentence names images of a unit in a sentence a person reads.
func artifactSentence(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return "workload " + names[0]
	default:
		return "workloads " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// pluralArtifacts picks the word for one artifact or several.
func pluralArtifacts(count int, one, many string) string {
	if count == 1 {
		return one
	}
	return many
}

// artifactEvidence reads what is attached to a build's artifact, verified
// against the platform's key when there is one — the same read
// buildAttestations serves, minus the HTTP framing.
func (s *Server) artifactEvidence(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	workload string,
	artifact *kitchenv1alpha1.ArtifactStatus,
) (attestation.EvidenceSet, error) {
	reader, err := s.evidenceFor(ctx, build, workload)
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

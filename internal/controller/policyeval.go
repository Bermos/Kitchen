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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/policy"
	"github.com/Bermos/Kitchen/internal/provider"
)

// Asking an environment's bar about a release: one implementation, two
// callers.
//
// The promotion reconciler (#133) asks it before it moves an environment onto
// a release. The rescan sweep (#134) asks the identical question of the
// release the environment is *already* running, on an interval, with today's
// vulnerability evidence attached — and "the same code path as promotion" in
// issue #134 is this file rather than a resemblance between two files.
//
// That matters more than it saves. A rescan that judged with a second
// materializer, a second evidence read or a second exception listing would
// eventually disagree with the promotion that let the release in, and the
// disagreement would read as drift rather than as a bug — which is the one
// failure mode a compliance control must not have. Everything that could
// differ is therefore a parameter: the kind, the clock, the data snapshot,
// and where the evidence and the exceptions are read from.
//
// What stays with each caller is what is genuinely theirs: the promotion's
// expired-exception note (worded from the Promotion's own triple) and the
// rescan's drift bookkeeping.

// PolicyEvaluation is one evaluation's whole answer: what was asked, what came
// back, and the sentence a status line reads.
type PolicyEvaluation struct {
	// Input is the fully materialized input the engine saw, exceptions
	// included. It is what the decision store keeps.
	Input policy.Input
	// Result is the verdict and every rule that fired.
	Result policy.Result
	// Bundle is the bundle the result came from — empty for an environment
	// that declares no requirements, which is a real evaluation of no rules
	// rather than a missing one.
	Bundle policy.Bundle
	// Message is the sentence for a status field.
	Message string
	// Refusal is a permanent fault of the *question* rather than an answer to
	// it: references that do not line up, a bundle that cannot be resolved, a
	// bundle that will not evaluate. A caller that gets one must not read the
	// verdict, because there is not one — nothing judged the artifact.
	Refusal string
}

// PolicyEvaluator asks one environment's bar about one release.
//
// Its seams are the two places an evaluation reaches outside the objects it
// was handed: the registry the artifact's evidence is read from, and the
// listing of break-glass grants in scope. Both default to the real thing and
// both are injected by tests, which is what lets the promotion reconciler and
// the rescan sweep be tested against the same evaluation without a registry.
type PolicyEvaluator struct {
	Client client.Client

	// EvidenceReaders resolves how the artifact's attached evidence is read
	// back. Nil talks to the real registry with the project's own credential.
	EvidenceReaders EvidenceSetReaderFactory

	// Exceptions lists the active break-glass grants in scope for a triple at
	// a moment. Nil means the one real listing, ActiveExceptionsFor.
	Exceptions func(ctx context.Context, project, environment, release string, at time.Time) ([]policy.Exception, error)
}

// EvaluationRequest is one question for the evaluator.
type EvaluationRequest struct {
	// Kind is why the engine is being asked: policy.KindPromotion,
	// policy.KindRescan. It reaches the stored decision and the input digest,
	// so it is never guessed.
	Kind string
	// At is the evaluation's clock. Exception expiry is judged against it,
	// which is what makes a replay waive exactly what the original waived.
	At time.Time
	// DataSnapshot identifies the dataset the evidence was produced against —
	// a scanner's vulnerability database identifier. The rescan sets it; a
	// promotion leaves it empty, because a promotion judges evidence produced
	// at build time by gates that each name their own version, and claiming
	// one snapshot for the lot of them would be a claim the platform cannot
	// stand behind. (The eligibility preview leaves it empty for the same
	// reason: it previews a promotion, and an input digest that differed from
	// the promotion's by a field the preview invented would make the preview
	// a second opinion rather than the same evaluation.)
	DataSnapshot string

	// Kitchen is the platform singleton, for the signing key the evidence is
	// verified against. May be nil.
	Kitchen *kitchenv1alpha1.Kitchen
	// Project may be nil: a project that could not be read is judged
	// unclassified, honestly, rather than refused a judgement.
	Project     *kitchenv1alpha1.Project
	Environment *kitchenv1alpha1.Environment
	Release     *kitchenv1alpha1.Release
}

// Evaluate materializes the input and asks the engine.
//
// A non-nil error is transient — the caller requeues. A PolicyEvaluation
// carrying a Refusal is permanent for this question. Everything else is an
// answer, blocked included.
func (e *PolicyEvaluator) Evaluate(
	ctx context.Context, req EvaluationRequest,
) (PolicyEvaluation, error) {
	env, release := req.Environment, req.Release

	build := &kitchenv1alpha1.Build{}
	if err := e.Client.Get(ctx, types.NamespacedName{
		Namespace: env.Namespace, Name: release.Spec.BuildRef.Name,
	}, build); err != nil {
		if !apierrors.IsNotFound(err) {
			return PolicyEvaluation{}, err
		}
		// A pruned build leaves the release judged on what the registry still
		// carries — honestly nothing, if the index is gone too.
		build = nil
	}

	claims, err := e.claimFacts(ctx, env)
	if err != nil {
		return PolicyEvaluation{}, err
	}

	requirements := env.Spec.Requirements
	if requirements == nil {
		// The hard check behind issue #137 guards this ungated door too: a
		// classified project's release does not land on an environment rated
		// below it, and with no bundle pinned there is no engine to say so —
		// the refusal is the caller's own, permanent for this question,
		// naming both classes and the fix.
		if refusal := DataClassRefusal(req.Project, env); refusal != "" {
			return PolicyEvaluation{Refusal: refusal}, nil
		}
		// An environment that declares no bar accepts anything — exactly
		// today's behaviour, stated rather than implied: the decision is
		// still recorded, with an empty bundle and no rules evaluated.
		input := policy.MaterializeInput(req.Kind, req.At, req.Project, env, release, build, nil, claims)
		input.DataSnapshot = req.DataSnapshot
		return PolicyEvaluation{
			Input:  input,
			Result: policy.Result{Verdict: policy.VerdictAllowed, Fired: []policy.FiredRule{}},
			Bundle: policy.Bundle{},
			Message: fmt.Sprintf(
				"environment %s declares no requirements, so no rules were evaluated and the release is allowed",
				env.Name),
		}, nil
	}

	resolver := &policy.Resolver{Client: e.Client, Namespace: PlatformNamespace}
	info, err := resolver.Resolve(ctx, requirements.BundleDigest)
	if err != nil {
		// A bar that cannot be read is not a bar that is cleared: the caller
		// fails, naming the digest, rather than guessing in either direction.
		// (Blocked would claim rules fired; none were evaluated.)
		return PolicyEvaluation{
			Refusal: "the environment's requirements could not be evaluated: " + err.Error(),
		}, nil
	}

	evidence, err := e.materializeEvidence(ctx, req.Kitchen, req.Project, build)
	if err != nil {
		return PolicyEvaluation{}, err
	}

	exceptions, err := e.activeExceptions(ctx, env, release, req.At)
	if err != nil {
		return PolicyEvaluation{}, err
	}

	input := policy.MaterializeInput(req.Kind, req.At, req.Project, env, release, build, evidence, claims)
	input.Exceptions = exceptions
	input.DataSnapshot = req.DataSnapshot
	// The ingested OpenVEX statements (#135) are already on input.VEX: they
	// are set by MaterializeInput, because they are a projection of the
	// evidence set it was handed rather than a listing it would have to go
	// and fetch. The seam this file used to reserve for them turned out to be
	// the wrong shape — a second place to remember is how a preview and a
	// promotion come to disagree — so the reason Exceptions is set here does
	// not carry over, and policy.VEXFrom is the one implementation instead.
	result, err := policy.Evaluate(ctx, info.Bundle, input)
	if err != nil {
		// The bundle resolved and would not evaluate: a broken bundle, not a
		// broken platform. Permanent for this question.
		return PolicyEvaluation{
			Refusal: "the environment's requirements could not be evaluated: " + err.Error(),
		}, nil
	}

	message := ""
	switch result.Verdict {
	case policy.VerdictAllowed:
		message = fmt.Sprintf("the release clears bundle %s", requirements.BundleDigest)
	case policy.VerdictAllowedWithException:
		message = fmt.Sprintf("the release clears bundle %s, with every fired rule waived by an exception",
			requirements.BundleDigest)
	default:
		message = fmt.Sprintf("blocked by bundle %s: %s", requirements.BundleDigest, firedSentence(result))
	}
	return PolicyEvaluation{
		Input: input, Result: result, Bundle: info.Bundle, Message: message,
	}, nil
}

// activeExceptions is the Exceptions seam with its default behind it: the one
// shared listing (ActiveExceptionsFor), scoped to this triple, judged at `at`
// — the same clock the engine's input carries, so what is listed is exactly
// what ApplyExceptions will honour.
func (e *PolicyEvaluator) activeExceptions(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	at time.Time,
) ([]policy.Exception, error) {
	if e.Exceptions != nil {
		return e.Exceptions(ctx, env.Spec.ProjectRef.Name, env.Name, release.Name, at)
	}
	active, err := ActiveExceptionsFor(ctx, e.Client, env.Namespace,
		env.Spec.ProjectRef.Name, env.Name, release.Name, at)
	if err != nil {
		return nil, err
	}
	return PolicyExceptions(active), nil
}

// claimFacts materializes the project's resource claims for the environment —
// the data facts the dataclass and provenance rules judge.
func (e *PolicyEvaluator) claimFacts(
	ctx context.Context, env *kitchenv1alpha1.Environment,
) ([]policy.Claim, error) {
	list := &kitchenv1alpha1.ResourceClaimList{}
	if err := e.Client.List(ctx, list, client.InNamespace(env.Namespace)); err != nil {
		return nil, err
	}
	return policy.ClaimFacts(env, list.Items), nil
}

// materializeEvidence reads what the release's artifacts carry, verified
// against the platform's key — the registry is the source of truth, and a
// registry that cannot be asked is a requeue rather than a judgement over a
// guess. A release whose build or artifact is gone is judged on nothing.
//
// **Every image the release deploys, not the project's own alone.** A unit
// ships one image per workload that declares a build (#271) and each carries
// its own evidence against its own digest (#300), so an evaluation over the
// first of them would judge a five-image release on one image's SBOM and one
// image's exploitability assertions — and would say `allowed` while four of
// them had never been looked at. Each artifact's set is materialized in its
// own right, which is also what keeps "the newest vulnerability scan" a
// question asked within an image rather than across a unit.
func (e *PolicyEvaluator) materializeEvidence(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
) ([]policy.Evidence, error) {
	if build == nil {
		return policy.IndexedEvidence(nil), nil
	}
	artifacts := []kitchenv1alpha1.BuildArtifact{}
	for _, artifact := range build.Artifacts() {
		if artifact.Artifact.Digest != "" && artifact.Artifact.Repository != "" {
			artifacts = append(artifacts, artifact)
		}
	}
	if len(artifacts) == 0 {
		return policy.IndexedEvidence(build), nil
	}

	reader, err := e.evidenceReader(ctx, project)
	if err != nil {
		return nil, err
	}
	verifiers := []attestation.Verifier{}
	if key, err := SigningKeyFor(ctx, e.Client, kitchen); err == nil && key != nil {
		verifiers = append(verifiers, key)
	}
	evidence := []policy.Evidence{}
	for _, artifact := range artifacts {
		ref := artifact.Artifact.Repository + "@" + artifact.Artifact.Digest
		set, err := reader.Evidence(ctx, ref, verifiers...)
		if err != nil {
			return nil, fmt.Errorf("the artifact's evidence could not be read from the registry: %w", err)
		}
		evidence = append(evidence,
			policy.EvidenceFrom(artifact.Workload, set, policy.EvidenceSources(artifact.Artifact))...)
	}
	return evidence, nil
}

// evidenceReader resolves the registry the project's artifacts live in — the
// same resolution the DecisionRecorder's attester makes, for the read.
func (e *PolicyEvaluator) evidenceReader(
	ctx context.Context, project *kitchenv1alpha1.Project,
) (EvidenceSetReader, error) {
	if project == nil {
		return nil, fmt.Errorf("the artifact's registry cannot be resolved without the project")
	}
	connection := &kitchenv1alpha1.Connection{}
	if err := e.Client.Get(ctx, types.NamespacedName{
		Namespace: PlatformNamespace, Name: project.Spec.RegistryConnection(),
	}, connection); err != nil {
		return nil, err
	}
	registry, err := provider.Registry(connection)
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: connection.Spec.CredentialsSecretRef.Name}
	if err := e.Client.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("the registry credential could not be read: %w", err)
	}
	factory := e.EvidenceReaders
	if factory == nil {
		factory = defaultEvidenceSetReader
	}
	return factory(secret.Data[corev1.DockerConfigJsonKey], registry.Server)
}

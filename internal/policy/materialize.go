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

package policy

import (
	"strings"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Materializing the input is half of what makes a decision reproducible, so
// there is exactly one implementation of it. The API's eligibility preview,
// the promotion reconciler and the scheduled rescan (#134) all assemble their
// input here — same objects in, same facts out — which is what makes a
// preview's answer the answer a promotion will act on, rather than a second
// opinion from a second code path.
//
// What differs between the callers is only where the evidence set comes from
// (the API and the reconciler each hold their own registry seam) and the
// kind, which is why both stay parameters.

// MaterializeInput builds the engine's input from the typed objects. The
// project, build, evidence and claims may be nil/empty — a release whose
// build is gone is judged on nothing, honestly, rather than refused a
// judgement, and a project that could not be read is judged unclassified.
//
// Exceptions are deliberately absent here: which grants are in scope is a
// listing (controller.ActiveExceptionsFor — the one implementation), not a
// materialization, and the callers that evaluate for real set them on the
// input after materializing — the promotion reconciler and the eligibility
// preview both do — so every kind of evaluation waives the same grants the
// same way.
//
// The artifact's OpenVEX statements (#135) are the other way round and are
// set here, because they are a projection of the `evidence` argument rather
// than a second thing to fetch: VEXFrom needs nothing this function does not
// already hold. Setting them anywhere else would be a second materialization
// site, and the two would drift the moment somebody added a third caller.
func MaterializeInput(
	kind string,
	at time.Time,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	build *kitchenv1alpha1.Build,
	evidence []Evidence,
	claims []Claim,
) Input {
	input := Input{
		Kind: kind,
		At:   at,
		Project: ProjectFacts{
			Name: env.Spec.ProjectRef.Name,
		},
		Environment: EnvironmentFacts{
			Name:      env.Name,
			Type:      string(env.Spec.Type),
			DataClass: string(env.Spec.DataClass),
			Residency: env.Spec.Residency,
		},
		Release: ReleaseFacts{
			Name:  release.Name,
			Image: release.Spec.Image,
		},
		Evidence: evidence,
		Claims:   claims,
		VEX:      VEXFrom(evidence, at),
	}
	if project != nil {
		input.Project.DataClass = string(project.Spec.DataClass)
		input.Project.Criticality = string(project.Spec.Criticality)
		input.Project.RTO = string(project.Spec.RTO)
		input.Project.RPO = string(project.Spec.RPO)
	}
	// The environment's designation is the *effective* one — its own where it
	// declares one, its project's where production declares none — because
	// that is the designation the platform acts on everywhere else, and a
	// rule reading a different number from the screen beside it would be the
	// worst kind of disagreement to debug.
	continuity := kitchenv1alpha1.EffectiveContinuity(project, env)
	input.Environment.Criticality = string(continuity.Criticality)
	input.Environment.RTO = string(continuity.RTO)
	input.Environment.RPO = string(continuity.RPO)
	if requirements := env.Spec.Requirements; requirements != nil {
		input.Parameters = requirements.Parameters
	}
	if _, digest, found := strings.Cut(release.Spec.Image, "@"); found {
		input.Release.Digest = digest
	}
	if build != nil {
		input.Release.Build = BuildFacts{
			Name:   build.Name,
			Commit: build.Spec.Git.SHA,
			Branch: build.Spec.Git.Branch,
		}
	}
	return input
}

// ClaimFacts materializes a project's resource claims for one environment:
// the data facts issues #137/#138 record about each — its class, what its
// data derives from, and where the provider actually put it. Claims of other
// projects are skipped, so callers can hand the whole namespace's list over.
//
// The facts are the environment's view of the claim: a preview backed by its
// own database branch is judged on that branch's declared provenance, not the
// primary's, because the branch is what its workload reads. An environment
// with no branch of its own reads the claim's primary declaration.
func ClaimFacts(env *kitchenv1alpha1.Environment, claims []kitchenv1alpha1.ResourceClaim) []Claim {
	out := []Claim{}
	for i := range claims {
		claim := &claims[i]
		if claim.Spec.ProjectRef.Name != env.Spec.ProjectRef.Name {
			continue
		}
		fact := Claim{
			Name:       claim.Name,
			Type:       claim.Spec.Type,
			DataClass:  string(claim.Spec.DataClass),
			Provenance: claim.Status.DataProvenance,
			Residency:  claim.Status.Residency,
		}
		for _, branch := range claim.Status.Branches {
			if branch.Environment == env.Name && branch.Provenance != "" {
				fact.Provenance = branch.Provenance
			}
		}
		out = append(out, fact)
	}
	return out
}

// EvidenceSources indexes whose claim each attached predicate type was,
// from the build's own evidence index — the registry knows what is attached,
// the index knows who attached it.
func EvidenceSources(build *kitchenv1alpha1.Build) map[string]string {
	sources := map[string]string{}
	if build == nil || build.Status.Artifact == nil {
		return sources
	}
	for _, entry := range build.Status.Artifact.Evidence {
		sources[entry.PredicateType] = entry.Source
	}
	return sources
}

// IndexedEvidence is the degraded materialization for when the registry
// cannot be asked: the build's index alone — predicate types and sources,
// no predicates, nothing verified. An evaluation over it is honest about
// what it saw; a rule that wants the predicate's content will fire.
func IndexedEvidence(build *kitchenv1alpha1.Build) []Evidence {
	out := []Evidence{}
	if build == nil || build.Status.Artifact == nil {
		return out
	}
	for _, entry := range build.Status.Artifact.Evidence {
		out = append(out, Evidence{
			PredicateType: entry.PredicateType,
			Source:        entry.Source,
		})
	}
	return out
}

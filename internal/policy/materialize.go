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
// build and evidence may be nil/empty — a release whose build is gone is
// judged on nothing, honestly, rather than refused a judgement.
//
// Exceptions are deliberately absent here: until #136 lands there are no
// Exception objects to list, and the callers pass none. When it lands, the
// listing joins this one materializer so every kind of evaluation waives the
// same grants the same way.
func MaterializeInput(
	kind string,
	at time.Time,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	build *kitchenv1alpha1.Build,
	evidence []Evidence,
) Input {
	input := Input{
		Kind: kind,
		At:   at,
		Project: ProjectFacts{
			Name: env.Spec.ProjectRef.Name,
		},
		Environment: EnvironmentFacts{
			Name: env.Name,
			Type: string(env.Spec.Type),
		},
		Release: ReleaseFacts{
			Name:  release.Name,
			Image: release.Spec.Image,
		},
		Evidence: evidence,
	}
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

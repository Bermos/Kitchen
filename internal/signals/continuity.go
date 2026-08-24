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

package signals

import (
	"fmt"
	"strings"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Where the institution's own numbers become thresholds (issue #141).
//
// Every other number in this package is a constant in thresholds.go with a
// paragraph saying why it is that number, and that is right for a rule the
// platform is entitled to have an opinion about: nobody has to be asked
// whether five restarts in half an hour is a loop. A recovery time objective
// is the opposite kind of number. Kitchen has no business deciding it — the
// institution sets it, on the Project or the Environment — and the platform's
// job is to hold the estate to it.
//
// So there are exactly two things here, and both change what wakes somebody:
//
//   - environment.rto-at-risk fires against the declared RTO rather than
//     against a constant. Change the RTO and you have changed when the pager
//     goes off, which is the difference between a tolerance that drives
//     alerting and one that decorates a screen.
//   - a warning about an environment designated critical is raised to a
//     critical finding, in one place rather than in every rule. See
//     [Findings.escalate].
//
// The RPO is carried through the mapping and reaches the policy input, and
// nothing here fires on it. That is not an oversight and it is not going to be
// papered over with a rule that always passes: measuring a recovery *point*
// needs a recovery point to measure, and the platform observes none — no
// provider on it declares one. docs/OBSERVABILITY.md says what would close it.

// SignalRTOAtRisk is the one rule whose threshold comes from outside the
// catalogue.
const SignalRTOAtRisk ID = "env.rto-at-risk"

// ContinuityFor is one environment's resolved designation as the rules read
// it. It is [kitchenv1alpha1.Continuity] with the project name kept beside it,
// because a finding that says an environment inherited its RTO has to be able
// to say from where.
type ContinuityFor struct {
	kitchenv1alpha1.Continuity
	// Project is where an inherited value came from, and is set whether or
	// not anything was inherited.
	Project string
}

// inheritedNote words where a value came from, for a detail clause.
func (c ContinuityFor) inheritedNote(field string) string {
	for _, inherited := range c.Inherited {
		if inherited == field {
			return "inherited from project " + c.Project
		}
	}
	return "declared on the environment"
}

// ContinuityFacts resolves the designation of every environment in a snapshot,
// once, the way [Gather] does it — exported so the API's forward mapping and
// the catalogue cannot resolve it two subtly different ways.
func ContinuityFacts(
	projects []kitchenv1alpha1.Project, environments []kitchenv1alpha1.Environment,
) map[EnvKey]ContinuityFor {
	byName := make(map[string]*kitchenv1alpha1.Project, len(projects))
	for i := range projects {
		byName[projects[i].Name] = &projects[i]
	}
	facts := make(map[EnvKey]ContinuityFor, len(environments))
	for i := range environments {
		env := &environments[i]
		project := env.Spec.ProjectRef.Name
		facts[EnvKey{Project: project, Environment: env.Name}] = ContinuityFor{
			Continuity: kitchenv1alpha1.EffectiveContinuity(byName[project], env),
			Project:    project,
		}
	}
	return facts
}

func continuitySignals() []Signal {
	return []Signal{{
		ID:       SignalRTOAtRisk,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary: "an environment is serving nothing and is eating into the recovery time " +
			"objective the institution declared for it",
		Requires: []Input{InputWorkloads, InputEnvironments, InputProjects},
		Evaluate: evaluateRTOAtRisk,
	}}
}

// evaluateRTOAtRisk measures an outage against the environment's own tolerance.
//
// "Not serving" is the workload wanting pods and having none available, which
// is the same reading workload.notready takes — and this rule deliberately
// does not suppress that one. They are two different statements: notready says
// the pods are gone, this says how much of the institution's tolerance has
// been spent, and an operator holding a fifteen-minute RTO wants the second
// one at minute eight rather than the first one at minute ten. An environment
// idled to zero on purpose wants no pods at all and is skipped by the same
// test.
func evaluateRTOAtRisk(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for _, workload := range snapshotWorkloads(snapshot) {
		if workload.project == "" || workload.environment == "" {
			continue
		}
		if workload.desired == 0 || workload.available > 0 {
			continue
		}
		facts, ok := snapshot.Continuity[EnvKey{
			Project: workload.project, Environment: workload.environment,
		}]
		if !ok {
			continue
		}
		objective, declared := facts.RTO.Duration()
		if !declared || objective <= 0 {
			// No tolerance, or a tolerance of none at all. Neither is a
			// threshold: zero would fire the instant a rollout blinked, and
			// an environment nobody has given an RTO is workload.notready's.
			continue
		}
		outage := snapshot.Now.Sub(workload.changedAt)
		if outage < time.Duration(float64(objective)*RTOWarnFraction) {
			continue
		}

		severity, headline := SeverityWarning, fmt.Sprintf("%s into a %s recovery objective",
			duration(outage), facts.RTO)
		if outage >= objective {
			severity, headline = SeverityCritical, fmt.Sprintf("past its %s recovery objective",
				facts.RTO)
		}
		scope := Scope{
			Kind: ScopeEnvironment, Project: workload.project, Environment: workload.environment,
		}
		findings = append(findings, fire(SignalRTOAtRisk, severity, scope, workload.changedAt,
			headline,
			sentence(
				fmt.Sprintf("nothing has served here for %s against an RTO of %s",
					duration(outage), facts.RTO),
				facts.inheritedNote("rto"),
				"the objective is the institution's, not the platform's — Kitchen holds the "+
					"estate to it and does not set it",
			),
			scopeEvidence(scope, sectionWorkload)))
	}
	return findings
}

// escalate raises a warning about a critical environment to a critical
// finding, and says in the detail that it did.
//
// It lives here, applied once by [Registry.Evaluate], rather than inside the
// rules, for the same reason the availability check does: there are thirty-odd
// rules and one right answer, and a designation honoured by some of them and
// forgotten by the rest would be worse than one honoured by none. It does not
// move any rule's [Signal.Version] — no rule's meaning changed — and it does
// not touch a [Finding.Fingerprint], so a condition that opened as a warning
// and escalated when somebody designated the environment stays the same
// condition.
//
// Only `critical` escalates. `important` deliberately changes nothing about
// severity: if the middle rung escalated as well there would be nothing left
// at warning, and a list where everything is critical is a list nobody reads.
// What `important` does is appear in the mapping and reach the policy input,
// which is where a designation short of critical belongs.
func (f Findings) escalate(continuity map[EnvKey]ContinuityFor) {
	if len(continuity) == 0 {
		return
	}
	for i := range f {
		if f[i].Severity != SeverityWarning || f[i].Scope.Kind != ScopeEnvironment {
			continue
		}
		facts, ok := continuity[EnvKey{
			Project: f[i].Scope.Project, Environment: f[i].Scope.Environment,
		}]
		if !ok || facts.Criticality != kitchenv1alpha1.CriticalityCritical {
			continue
		}
		f[i].Severity = SeverityCritical
		f[i].Detail = sentence(f[i].Detail, strings.TrimSpace(
			"raised from warning: this environment is designated critical ("+
				facts.inheritedNote("criticality")+")"))
	}
}

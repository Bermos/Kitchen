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
	"sort"
	"strings"
	"time"
)

// ID is a signal's name. It is part of every fingerprint and so of every
// transition record a later release writes, which makes it an interface: a
// renamed signal is a signal that resolved and a different one that opened.
type ID string

// Severity is how much of a hurry the reader is in.
type Severity string

const (
	// SeverityCritical is serving broken or about to be: the platform is not
	// doing the job it exists to do.
	SeverityCritical Severity = "critical"
	// SeverityWarning is heading for critical, or degraded in a way somebody
	// will notice — the memory climbing towards the limit rather than the OOM
	// kill that follows.
	SeverityWarning Severity = "warning"
	// SeverityUnknown is a rule that could not be evaluated because an input
	// was unreadable. It is deliberately not "info": a rule that cannot see is
	// closer to a problem than to a note, and silently reporting health it did
	// not measure is the failure this whole package exists to avoid.
	SeverityUnknown Severity = "unknown"
	// SeverityInfo is worth showing on a screen and worth nobody's night.
	SeverityInfo Severity = "info"
)

// severityRank orders the problems list. Unknown sits below a real warning and
// above info, which is where "I could not tell you" belongs.
var severityRank = map[Severity]int{
	SeverityCritical: 3,
	SeverityWarning:  2,
	SeverityUnknown:  1,
	SeverityInfo:     0,
}

// Rank orders severities against each other, highest first.
func (s Severity) Rank() int { return severityRank[s] }

// ScopeKind is what sort of thing a finding is about. It decides which screen
// the finding belongs on and which fields of [Scope] are populated.
type ScopeKind string

const (
	// ScopePlatform is the whole installation: the store, the edge, the
	// tunnel, a platform component.
	ScopePlatform ScopeKind = "platform"
	// ScopeProject is one project across all of its environments.
	ScopeProject ScopeKind = "project"
	// ScopeEnvironment is one running environment, which is what the
	// developer's diagnostics strip renders.
	ScopeEnvironment ScopeKind = "environment"
	// ScopeWorkload is a Deployment, StatefulSet or DaemonSet addressed
	// directly, for the platform's own workloads that belong to no project.
	ScopeWorkload ScopeKind = "workload"
	// ScopeNode is one cluster node.
	ScopeNode ScopeKind = "node"
	// ScopeVolume is one PersistentVolumeClaim.
	ScopeVolume ScopeKind = "volume"
	// ScopeDomain is one hostname: a custom domain, a generated URL, or a host
	// that reached the edge which the platform never published.
	ScopeDomain ScopeKind = "domain"
	// ScopeBuild is one Build.
	ScopeBuild ScopeKind = "build"
)

// Scope names the thing a finding is about.
//
// A scope sets the fields that identify its subject and no more: an
// application's workload is (project, environment, name) and its namespace is
// derivable from the project, while a platform workload is (namespace, name)
// and has no project. That restraint is not tidiness — [Scope.Path] joins
// exactly the fields that are set, and the join is the fingerprint, so a field
// populated "for completeness" changes the identity of every finding at that
// scope.
type Scope struct {
	Kind ScopeKind `json:"kind"`

	// Project and Environment attribute the finding to an application. Both
	// are empty for anything the platform owns itself.
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`

	// Namespace is set only where it is not derivable from Project — the
	// platform namespace, in practice.
	Namespace string `json:"namespace,omitempty"`

	// Node is the cluster node, for node-scoped findings.
	Node string `json:"node,omitempty"`

	// Name is the subject within the scope: a container, a claim, a hostname,
	// a filesystem mount point, a component, a build.
	Name string `json:"name,omitempty"`
}

// Path is the scope's identity, and the tail of every fingerprint at it.
//
// The field order is fixed and must stay fixed: it is the difference between
// `workload.crashloop/shop/pr-41/web` meaning the same container next week and
// meaning a different one.
func (s Scope) Path() string {
	parts := make([]string, 0, 5)
	for _, part := range []string{s.Project, s.Environment, s.Namespace, s.Node, s.Name} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "/")
}

// unevaluableMarker separates a signal's id from the note that it could not be
// evaluated at all. It is a character no signal id, Kubernetes name or hostname
// can contain, so the two fingerprints can never collide — which matters,
// because "this platform-scoped rule is firing" and "this platform-scoped rule
// could not be read" are different conditions with the same scope, and a
// transition log that conflated them would resolve one by opening the other.
const unevaluableMarker = "#unevaluable"

// Fingerprint composes a signal's identity at a scope. Same condition, same
// string, every evaluation.
func Fingerprint(id ID, scope Scope) string {
	if path := scope.Path(); path != "" {
		return string(id) + "/" + path
	}
	return string(id)
}

// Finding is one firing condition: what is wrong, where, since when, and where
// to go and look.
type Finding struct {
	// Signal is the rule that produced it.
	Signal ID `json:"signal"`

	Severity Severity `json:"severity"`
	Scope    Scope    `json:"scope"`

	// Audience is the producing signal's, stamped on by the registry because a
	// rule cannot see its own catalogue entry. It is the other half of what
	// [Findings.ForEnvironment] narrows on: scope says which environment a
	// finding is about, and audience says whether that environment's developer
	// is the person who should be reading it.
	Audience Audience `json:"audience"`

	// Fingerprint identifies the condition across evaluations. Two rounds that
	// both find the same container crash-looping produce the same string, which
	// is what lets a later release diff rounds and record transitions instead
	// of re-announcing the same problem every interval.
	Fingerprint string `json:"fingerprint"`

	// Title is the short human sentence — "crash-looping", "memory at 96% of
	// limit" — which is what the environment page's diagnostics strip
	// concatenates. It carries no numbers beyond the one that names the
	// condition.
	Title string `json:"title"`

	// Detail carries the numbers and, where a rule exists to catch a specific
	// misconfiguration, says what is suspected. It is what the reader acts on.
	//
	// Its first clause is the headline number, because the diagnostics strip
	// renders `title (first clause)` — which is how "crash-looping" and "12
	// restarts in 30m" become one line without the strip having to know
	// anything about the rule that produced them.
	Detail string `json:"detail"`

	// Since is the earliest instant the snapshot can prove the condition held:
	// a condition's last transition, an event's first occurrence, the start of
	// the run of buckets that made it sustained. Where nothing in the snapshot
	// dates the condition it is the evaluation time, which is honest for a
	// stateless evaluator and is the field a background loop replaces with the
	// real opening time once it keeps history.
	Since time.Time `json:"since"`

	// Evidence is a dashboard path to the screen that shows the numbers behind
	// the finding. Relative, because the dashboard is served from the same
	// origin as the API that answers with it.
	Evidence string `json:"evidence"`
}

// Findings is one evaluated round.
type Findings []Finding

// Sort puts the round in the order the problems list renders: worst first,
// then by signal and fingerprint so that two evaluations of an unchanged
// cluster produce byte-identical output.
func (f Findings) Sort() {
	sort.SliceStable(f, func(i, j int) bool {
		if left, right := f[i].Severity.Rank(), f[j].Severity.Rank(); left != right {
			return left > right
		}
		if f[i].Signal != f[j].Signal {
			return f[i].Signal < f[j].Signal
		}
		return f[i].Fingerprint < f[j].Fingerprint
	})
}

// ForEnvironment keeps the findings one environment's diagnostics strip
// renders: the developer-audience ones about it, and those about the project it
// belongs to.
//
// Audience narrows the scope filter rather than replacing it, because the two
// answer different questions and both have to hold. Scope alone would put
// `pvc.pending` and `volume.attach-failed` on a developer's strip — both are
// scoped to a claim in the project's namespace, and both are the operator's
// problem: a claim that will not bind is a cluster with no default
// StorageClass, and a volume that will not attach is a CSI driver
// misbehaving. Neither is anything the developer whose preview is down can do
// a thing about.
func (f Findings) ForEnvironment(project, environment string) Findings {
	kept := make(Findings, 0, len(f))
	for _, finding := range f {
		if finding.Audience != AudienceDeveloper {
			continue
		}
		if finding.Scope.Project != project {
			continue
		}
		if finding.Scope.Environment != "" && finding.Scope.Environment != environment {
			continue
		}
		kept = append(kept, finding)
	}
	return kept
}

// Firing drops the rules that could not be evaluated, for a caller that wants
// only what is known to be wrong. The dropped ones are not nothing — see
// [Snapshot.Unreadable] for saying so once rather than per rule.
func (f Findings) Firing() Findings {
	kept := make(Findings, 0, len(f))
	for _, finding := range f {
		if finding.Severity != SeverityUnknown {
			kept = append(kept, finding)
		}
	}
	return kept
}

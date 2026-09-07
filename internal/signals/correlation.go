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
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Bermos/Kitchen/internal/controller"
)

// The confidence ladder.
//
// The operator's question is not "which projects are unhealthy", it is "is
// this one problem". The answer has three rungs, and the rule that matters
// most is that **a correlation is never withheld for being unexplained**:
// *everything blipped at 04:05 for two minutes and nothing explains it* is
// among the most valuable lines an operator can be handed, precisely because
// nobody was awake for it.
//
//  1. **Coincidence** — time only. Several projects started failing inside one
//     window, and the finding says what it does *not* know: no shared node, no
//     shared dependency, no change of ours.
//  2. **Shared dependency** — the affected set intersected against what those
//     projects have in common: node placement, the Gateway in front of them,
//     the StorageClass underneath them, a claimed resource they all attach.
//     It names the intersection, not a culprit.
//  3. **Shared change** — the same, joined against the platform's own
//     timeline: a release, an addon upgrade, a node reboot, a Gateway
//     reprogramming, a privileged operator action. The only rung that can
//     offer an action on the cause rather than on the symptom.
//
// The rung is raised at the highest confidence reachable and the finding says
// which one that was, which is the whole of #472.

// Confidence is which rung a correlation was raised at.
type Confidence string

const (
	// ConfidenceCoincidence is time and nothing else.
	ConfidenceCoincidence Confidence = "coincidence"
	// ConfidenceDependency is time plus something the affected projects share.
	ConfidenceDependency Confidence = "dependency"
	// ConfidenceChange is time plus something the platform did to itself.
	ConfidenceChange Confidence = "change"
)

// confidenceRank orders the three, highest first. An unknown value ranks below
// coincidence, which is where a rung nobody declared belongs.
var confidenceRank = map[Confidence]int{
	ConfidenceChange:      2,
	ConfidenceDependency:  1,
	ConfidenceCoincidence: 0,
}

// Rank orders confidences against each other, highest first.
func (c Confidence) Rank() int {
	if rank, ok := confidenceRank[c]; ok {
		return rank
	}
	return -1
}

// PlatformChange is one thing the platform did to itself, on a timeline.
//
// It is a flattening of four sources that already exist and are not joined —
// PlatformUpdate objects, AddonUpgrade objects, the cluster's own Warning
// events, and the hash-chained audit log — into the one shape rung 3 needs:
// when, what sort, and a sentence. The join is here rather than in a new store
// because nothing about it is worth persisting: it is a question asked at
// evaluation time about the last hour.
type PlatformChange struct {
	// At is when the change happened, not when the platform noticed it.
	At time.Time
	// Kind is which of the four sources it came from, in the operator's own
	// vocabulary: `release`, `addon`, `node`, `gateway`, `operator`.
	Kind string
	// Summary is the sentence a finding quotes.
	Summary string
}

// SignalCorrelated is rung 1 widened past HTTP: any condition in the
// catalogue, firing across projects at once.
const SignalCorrelated ID = "platform.correlated"

// correlationExcluded are the signal ids [evaluateCorrelated] leaves alone,
// because a dedicated detector already reads the same degradation from a
// better input.
//
// platform.latency-correlated and platform.error-correlated compare per-project
// traffic aggregates across two windows, which is a stronger statement than
// counting how many projects happen to carry the environment-scoped finding —
// p95 does not average, and a project whose one busy environment slowed is not
// the same claim as a project with one finding on it. Raising both would put
// two rows on the screen for one incident.
var correlationExcluded = map[ID]bool{
	SignalErrorRate:         true,
	SignalLatencyRegressed:  true,
	SignalLatencyCorrelated: true,
	SignalErrorCorrelated:   true,
	SignalCorrelated:        true,
}

func correlationSignals() []Signal {
	return []Signal{{
		ID:      SignalCorrelated,
		Version: 1,
		// It reads the round rather than the snapshot, which is what makes it
		// the widening: every rule in the catalogue dates its findings and
		// fingerprints them, so every rule can be correlated without any of
		// them knowing it.
		Correlates: true,
		Audience:   AudienceOperator,
		Tiers:      Tiers{Operator: TierPage},
		Summary:    "one condition is firing across several projects at once",
		Evaluate:   evaluateCorrelated,
	}}
}

// evaluateCorrelated is the widened rung 1: the round grouped by what is
// wrong, and raised where enough projects carry the same condition inside the
// correlation window.
//
// Grouping by the *signal* rather than by the projects is the same decision
// the two traffic detectors already made about their scope, and for the same
// reason: which projects are caught up in a platform problem changes minute to
// minute as traffic and pods move, and a fingerprint that listed them would
// resolve and reopen on every evaluation. The condition is the dimension; the
// projects are the detail.
func evaluateCorrelated(snapshot *Snapshot) []Finding {
	bySignal := map[ID][]Finding{}
	for _, finding := range snapshot.Round {
		switch {
		case correlationExcluded[finding.Signal],
			finding.Severity == SeverityUnknown,
			finding.Severity == SeverityInfo,
			finding.Scope.Project == "":
			continue
		}
		bySignal[finding.Signal] = append(bySignal[finding.Signal], finding)
	}

	ids := make([]ID, 0, len(bySignal))
	for id := range bySignal {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	findings := make([]Finding, 0, 1)
	for _, id := range ids {
		coincidence, ok := coincident(bySignal[id], snapshot.Now, snapshot.Policy)
		if !ok {
			continue
		}
		rung := snapshot.Ladder(coincidence.projects, coincidence.since)
		scope := Scope{Kind: ScopePlatform, Name: string(id)}
		findings = append(findings, rung.fire(SignalCorrelated, scope, coincidence.since, []ID{id},
			fmt.Sprintf("%s is firing in %d projects at once",
				coincidence.title, len(coincidence.projects)),
			sentence(
				fmt.Sprintf("%s across %s", coincidence.title,
					strings.Join(coincidence.projects, ", ")),
				"one condition in this many projects at once is usually one cause",
			)))
	}
	return findings
}

// coincidence is what one group of findings amounts to: which projects are
// caught up in it, when the earliest of them began, and the words for it.
type coincidence struct {
	projects []string
	since    time.Time
	// title is the headline of one of the *affected* findings rather than of
	// the group. A finding that fell outside the window is not part of this
	// correlation, and quoting its numbers in the headline would describe a
	// row the correlation does not cover.
	title string
}

// coincident narrows a group of findings to the projects whose condition began
// inside the correlation window, and answers with the earliest of them.
//
// The window is measured back from the newest of the group rather than from
// the evaluation's instant, which is what makes this widen *over time* rather
// than within one round: a condition recorded as open an hour ago carries the
// instant it opened, and two failures four minutes apart at 04:05 are still
// four minutes apart when the round that notices them runs at 05:12.
func coincident(group []Finding, now time.Time, policy Policy) (coincidence, bool) {
	newest := time.Time{}
	for _, finding := range group {
		at := findingAt(finding, now)
		if at.After(newest) {
			newest = at
		}
	}
	from := newest.Add(-policy.CorrelationWindow)

	answer := coincidence{}
	seen := map[string]bool{}
	for _, finding := range group {
		at := findingAt(finding, now)
		if at.Before(from) || seen[finding.Scope.Project] {
			continue
		}
		seen[finding.Scope.Project] = true
		answer.projects = append(answer.projects, finding.Scope.Project)
		if answer.title == "" {
			answer.title = finding.Title
		}
		if answer.since.IsZero() || at.Before(answer.since) {
			answer.since = at
		}
	}
	if len(answer.projects) < policy.CorrelatedProjects {
		return coincidence{}, false
	}
	sort.Strings(answer.projects)
	return answer, true
}

// findingAt is when a finding's condition began, as far as anything can say. A
// finding whose Since is in the future — a clock that moved, a status written
// ahead — is read as now rather than trusted, because a condition that has not
// happened yet cannot coincide with one that has.
func findingAt(finding Finding, now time.Time) time.Time {
	if finding.Since.IsZero() || finding.Since.After(now) {
		return now
	}
	return finding.Since
}

// Rung is one correlation's answer: how confident it is, what the affected set
// shares, and what the platform did to itself just before.
//
// It is a value rather than three returns because every correlated finding
// needs all three — the badge, the intersection and the change are one
// sentence — and because the two traffic detectors and the widened one build
// their findings identically once they have it.
type Rung struct {
	Confidence Confidence

	// Projects is the affected set, sorted.
	Projects []string

	// Shared is what they have in common, in the operator's words: `node
	// worker-2`, `storage class local-path`. Empty at rung 1, which is the
	// point of rung 1.
	Shared []string

	// Change is the platform's own change that preceded it, at rung 3.
	Change *PlatformChange
}

// Ladder climbs as far as the snapshot allows for one affected set.
//
// The order is the ladder's: a shared change is the strongest claim, a shared
// dependency the next, and time alone is still a claim. Nothing here can
// *refuse* a correlation — the affected set has already cleared the threshold
// by the time this is called — so the worst it can answer is coincidence.
func (s *Snapshot) Ladder(projects []string, since time.Time) Rung {
	rung := Rung{
		Confidence: ConfidenceCoincidence,
		Projects:   projects,
		Shared:     s.sharedDependencies(projects),
	}
	if len(rung.Shared) > 0 {
		rung.Confidence = ConfidenceDependency
	}
	if change := s.changeBefore(since); change != nil {
		rung.Confidence = ConfidenceChange
		rung.Change = change
	}
	return rung
}

// fire builds the finding, in the words the rung earns.
//
// The explanation is not decoration. At rung 1 it says what is *not* known,
// because an operator handed "three projects degraded" with no qualifier will
// go looking for the shared cause this evaluation already failed to find; at
// rungs 2 and 3 it names the intersection or the change, and stops short of
// naming a culprit — three projects on one node is not proof the node did it.
func (r Rung) fire(id ID, scope Scope, since time.Time, covers []ID, title, detail string) Finding {
	finding := fire(id, SeverityCritical, scope, since, title,
		sentence(detail, r.explanation()), EvidencePlatform)
	finding.Confidence = r.Confidence
	finding.Projects = r.Projects
	finding.Correlates = covers
	return finding
}

func (r Rung) explanation() string {
	switch r.Confidence {
	case ConfidenceChange:
		return fmt.Sprintf("the platform changed just before it: %s — and these share %s",
			r.Change.Summary, joinShared(r.Shared))
	case ConfidenceDependency:
		return "these share " + joinShared(r.Shared) + ", which is where to look before anyone " +
			"debugs an application"
	default:
		return "nothing explains it yet: no shared node, no shared dependency, and no change of " +
			"ours in the window — which is worth knowing on its own"
	}
}

func joinShared(shared []string) string {
	if len(shared) == 0 {
		return "nothing this evaluation can see"
	}
	return strings.Join(shared, " and ")
}

// sharedDependencies is rung 2: what every affected project has in common.
//
// Every one of these is read off the snapshot the catalogue already gathers,
// and each is an intersection over the *whole* affected set rather than a
// majority. A dependency two of three projects share explains two of three
// failures, which is a different and much weaker sentence than the one this
// rung makes — and the rung below it is not a punishment, it is the honest
// answer.
//
// Two of the six candidates the issue lists are deliberately absent. The
// addons an installation runs are installed once for the whole cluster, so
// every project shares every one of them and naming them would name them for
// every correlation ever raised; an addon reaches this ladder as a *change*
// instead, at rung 3, where it says something. The forge Connection a project
// builds from is the other: a Connection degrading stops builds, and a build
// that does not start is not a running application degrading.
func (s *Snapshot) sharedDependencies(projects []string) []string {
	shared := make([]string, 0, 4)
	if node := s.sharedNode(projects); node != "" {
		shared = append(shared, "node "+node)
	}
	if class := s.sharedStorageClass(projects); class != "" {
		shared = append(shared, "storage class "+class)
	}
	if gateway := s.sharedGateway(projects); gateway != "" {
		shared = append(shared, "the "+gateway+" gateway")
	}
	if claim := s.sharedClaim(projects); claim != "" {
		shared = append(shared, "the "+claim+" they all attach")
	}
	return shared
}

// sharedNode is the one node every affected project has a pod on, if there is
// exactly one.
//
// "Exactly one" rather than "at least one" is what makes this worth printing.
// On a two-node cluster every pair of projects shares both nodes, and a
// finding that said so would be describing the cluster rather than the
// incident.
func (s *Snapshot) sharedNode(projects []string) string {
	return oneCommon(projects, func(project string) map[string]bool {
		nodes := map[string]bool{}
		for i := range s.Pods {
			pod := &s.Pods[i]
			if pod.Labels[controller.LabelProject] != project || pod.Spec.NodeName == "" {
				continue
			}
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			nodes[pod.Spec.NodeName] = true
		}
		return nodes
	})
}

// sharedStorageClass is the one StorageClass every affected project's claims
// are on, which is how "three PVCs filling" and "three applications that all
// write to the same local-path volume" become one row rather than three.
func (s *Snapshot) sharedStorageClass(projects []string) string {
	return oneCommon(projects, func(project string) map[string]bool {
		classes := map[string]bool{}
		namespace := controller.AppNamespace(project)
		for i := range s.Claims {
			claim := &s.Claims[i]
			if claim.Namespace != namespace || claim.Spec.StorageClassName == nil {
				continue
			}
			classes[*claim.Spec.StorageClassName] = true
		}
		return classes
	})
}

// sharedGateway is the Gateway every affected project's routes hang off.
//
// On an installation with one shared Gateway this is always true and always
// says the same thing, which is why it is reported only where the cluster has
// more than one: on a platform with a second Gateway, "these three are all
// behind the internal gateway" is the whole answer.
func (s *Snapshot) sharedGateway(projects []string) string {
	if len(s.Gateways) < 2 {
		return ""
	}
	return oneCommon(projects, func(project string) map[string]bool {
		gateways := map[string]bool{}
		namespace := controller.AppNamespace(project)
		for i := range s.Routes {
			route := &s.Routes[i]
			if route.Namespace != namespace {
				continue
			}
			for _, parent := range route.Spec.ParentRefs {
				gateways[string(parent.Name)] = true
			}
		}
		return gateways
	})
}

// sharedClaim is the resource every affected project attaches — the claimed
// database at the bottom of the issue's own example.
//
// It is matched on the claim's type and the connection behind it rather than
// on the object's name, because two projects never share a ResourceClaim
// object: they each hold one, and what they share is what those resolve to. A
// claim the platform provisions itself carries no connection, and two of those
// are two databases, so it contributes nothing.
func (s *Snapshot) sharedClaim(projects []string) string {
	return oneCommon(projects, func(project string) map[string]bool {
		attached := map[string]bool{}
		for i := range s.ResourceClaims {
			claim := &s.ResourceClaims[i]
			if claim.Spec.ProjectRef.Name != project || claim.Spec.ConnectionRef == nil {
				continue
			}
			attached[claim.Spec.Type+" on "+claim.Spec.ConnectionRef.Name] = true
		}
		return attached
	})
}

// oneCommon intersects a per-project set across the whole affected set and
// answers only when exactly one member survives. Anything else — nothing in
// common, or several things — is not a sentence worth putting on a screen.
func oneCommon(projects []string, of func(string) map[string]bool) string {
	if len(projects) == 0 {
		return ""
	}
	var common map[string]bool
	for _, project := range projects {
		values := of(project)
		if len(values) == 0 {
			return ""
		}
		if common == nil {
			common = values
			continue
		}
		for value := range common {
			if !values[value] {
				delete(common, value)
			}
		}
		if len(common) == 0 {
			return ""
		}
	}
	if len(common) != 1 {
		return ""
	}
	for value := range common {
		return value
	}
	return ""
}

// changeBefore is rung 3: the platform's own most recent change inside the
// correlation window before the correlation began.
//
// "Before" and not "around": a change that landed after the failures started
// did not cause them, and a ladder that ignored the direction of time would
// promote every correlation raised during a rollout to its highest rung. The
// most recent of them is answered because a join across four sources routinely
// finds several — a release is a node drain is a Gateway reprogramming — and
// the nearest one is the one an operator looks at first.
func (s *Snapshot) changeBefore(since time.Time) *PlatformChange {
	from := since.Add(-s.Policy.CorrelationWindow)
	var nearest *PlatformChange
	for i := range s.PlatformChanges {
		change := &s.PlatformChanges[i]
		if change.At.Before(from) || change.At.After(since) {
			continue
		}
		if nearest == nil || change.At.After(nearest.At) {
			nearest = change
		}
	}
	if nearest == nil {
		return nil
	}
	copied := *nearest
	return &copied
}

// SortPlatformChanges puts a timeline newest first, so that two gathers of an
// unchanged platform produce the same snapshot.
func SortPlatformChanges(changes []PlatformChange) {
	sort.SliceStable(changes, func(i, j int) bool {
		if !changes[i].At.Equal(changes[j].At) {
			return changes[i].At.After(changes[j].At)
		}
		if changes[i].Kind != changes[j].Kind {
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].Summary < changes[j].Summary
	})
}

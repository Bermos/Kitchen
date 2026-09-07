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

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The cross-project detectors of §7, which exist because of one observation:
// several projects degrading at the same moment is a platform problem wearing
// project costumes.
//
// Every other rule in the catalogue answers "what is wrong with this thing".
// These three answer "is the thing that is wrong with all of these the same
// thing", which no project-scoped endpoint can ever be asked — and which is
// the difference between three teams each debugging their application for an
// afternoon and one operator looking at a saturated node.

const (
	SignalLatencyCorrelated  ID = "platform.latency-correlated"
	SignalErrorCorrelated    ID = "platform.error-correlated"
	SignalComponentUnhealthy ID = "platform.component-unhealthy"
)

func platformSignals() []Signal {
	return []Signal{{
		ID:       SignalLatencyCorrelated,
		Version:  3,
		Audience: AudienceOperator,
		Tiers:    Tiers{Operator: TierPage},
		Summary:  "p95 is up against baseline in as many projects at once as the policy calls a correlation",
		Requires: []Input{InputRequests},
		Evaluate: evaluateLatencyCorrelated,
	}, {
		ID:       SignalErrorCorrelated,
		Version:  3,
		Audience: AudienceOperator,
		Tiers:    Tiers{Operator: TierPage},
		Summary:  "5xx is up against baseline in as many projects at once as the policy calls a correlation",
		Requires: []Input{InputRequests},
		Evaluate: evaluateErrorCorrelated,
	}, {
		ID:       SignalComponentUnhealthy,
		Version:  2,
		Audience: AudienceOperator,
		Tiers:    Tiers{Operator: TierPage},
		Summary:  "a platform component the survey watches is not running",
		Requires: []Input{InputKitchen},
		Evaluate: evaluateComponentUnhealthy,
	}}
}

func evaluateLatencyCorrelated(snapshot *Snapshot) []Finding {
	return correlated(snapshot, correlationRule{
		id:     SignalLatencyCorrelated,
		covers: []ID{SignalLatencyRegressed},
		name:   "latency",
		scope:  "p95",
		value:  func(traffic clickhouse.ProjectTraffic) float64 { return traffic.P95Ms },
		degraded: func(comparison Regression) bool {
			return comparison.Regressed(LatencyRegressionFactor, LatencyFloorMs)
		},
		render: milliseconds,
		explain: "several projects slowing together is the node, the store or the edge — not three " +
			"applications that each got slower on the same afternoon",
	})
}

func evaluateErrorCorrelated(snapshot *Snapshot) []Finding {
	return correlated(snapshot, correlationRule{
		id:     SignalErrorCorrelated,
		covers: []ID{SignalErrorRate},
		name:   "errors",
		scope:  "5xx rate",
		value:  func(traffic clickhouse.ProjectTraffic) float64 { return traffic.ErrorRate },
		degraded: func(comparison Regression) bool {
			return comparison.Elevated(ErrorRateFactor, ErrorRateFiring)
		},
		render: percent,
		explain: "several projects failing together is usually something they share — a claimed " +
			"database, the store behind their sessions, or the edge itself",
	})
}

// correlationRule is the shape both detectors have. They differ in which
// number they read and what counts as degraded; the counting, the threshold on
// how many projects, and the words are one implementation.
type correlationRule struct {
	id ID
	// covers is the environment-scoped rule whose findings this detector's row
	// stands in front of, so that the overview can fold those rows into it
	// rather than showing one incident twice. It is declared rather than
	// derived because these two read the request rollup and not the round:
	// nothing in what they compute names the rule they correspond to.
	covers   []ID
	name     string
	scope    string
	value    func(clickhouse.ProjectTraffic) float64
	degraded func(Regression) bool
	render   func(float64) string
	explain  string
}

func correlated(snapshot *Snapshot, rule correlationRule) []Finding {
	baseline := map[string]clickhouse.ProjectTraffic{}
	for _, traffic := range snapshot.ProjectTrafficBaseline {
		baseline[traffic.Project] = traffic
	}

	type degraded struct {
		project  string
		recent   float64
		baseline float64
	}
	affected := make([]degraded, 0, len(snapshot.ProjectTrafficRecent))
	for _, traffic := range snapshot.ProjectTrafficRecent {
		if traffic.Requests < MinRequestsToJudge {
			// A project that served nine requests cannot corroborate anything.
			continue
		}
		before, known := baseline[traffic.Project]
		if !known {
			continue
		}
		// Support is the count the Regression guards on, and a project
		// aggregate is one merged answer rather than a series of buckets. It
		// is reported as established because the window behind it is the whole
		// baseline window — which is more support, not less, than
		// MinBaselineBuckets asks for.
		comparison := Regression{
			Recent:   rule.value(traffic),
			Baseline: rule.value(before),
			Support:  MinBaselineBuckets,
		}
		if !rule.degraded(comparison) {
			continue
		}
		affected = append(affected, degraded{
			project:  traffic.Project,
			recent:   comparison.Recent,
			baseline: comparison.Baseline,
		})
	}
	if len(affected) < snapshot.Policy.CorrelatedProjects {
		return nil
	}
	sort.Slice(affected, func(i, j int) bool { return affected[i].project < affected[j].project })

	names := make([]string, 0, len(affected))
	numbers := make([]string, 0, len(affected))
	for _, entry := range affected {
		names = append(names, entry.project)
		numbers = append(numbers, fmt.Sprintf("%s %s (was %s)", entry.project,
			rule.render(entry.recent), rule.render(entry.baseline)))
	}

	// The scope carries the dimension rather than the projects. Which projects
	// are caught up in a platform problem changes minute to minute as traffic
	// moves; the problem does not, and a fingerprint that listed them would
	// resolve and reopen on every evaluation.
	scope := Scope{Kind: ScopePlatform, Name: rule.name}
	// The window the *data* covers, which is not the correlation window: these
	// two rules compare one merge of the recent window against one merge of
	// the baseline, so the earliest instant either can prove anything about is
	// where the recent window began. The correlation window decides how far
	// back the ladder looks for a change, and nothing about what the numbers
	// mean.
	since := snapshot.Now.Add(-RecentWindow)
	rung := snapshot.Ladder(names, since)
	// Critical, unlike the widened detector's answer: these two do not stand
	// in front of other rows, they *are* the measurement, and p95 or 5xx up
	// against baseline across the estate is the condition the platform exists
	// to notice.
	return []Finding{rung.fire(rule.id, SeverityCritical, scope, since, rule.covers,
		fmt.Sprintf("%s degraded across %d projects", rule.scope, len(affected)),
		sentence(
			strings.Join(numbers, ", "),
			rule.explain,
		))}
}

// evaluateComponentUnhealthy folds the component survey into the same feed.
//
// It deliberately re-derives nothing. The survey already compares what each
// platform workload wants against what it has and goes and finds the reason
// where they differ — including the case that has no pods to read a reason
// from — and it writes the answer to the Kitchen singleton's status. Computing
// it a second time here would be a second implementation to keep in step with
// the first, and the failure mode of that is two screens disagreeing about
// whether the platform is up.
func evaluateComponentUnhealthy(snapshot *Snapshot) []Finding {
	components := snapshot.Platform.Components
	findings := make([]Finding, 0, 1)
	for _, component := range components {
		if component.Healthy {
			continue
		}
		scope := Scope{Kind: ScopePlatform, Name: component.Name}
		findings = append(findings, fire(SignalComponentUnhealthy, SeverityCritical, scope, snapshot.Now,
			fmt.Sprintf("%s is not running", component.Name),
			sentence(
				fmt.Sprintf("%d of %d pods available", component.Available, component.Desired),
				component.Message,
				fmt.Sprintf("%s %s in the platform namespace", component.Kind, component.Name),
			),
			workloadEvidence(controller.PlatformNamespace, component.Name)))
	}
	return findings
}

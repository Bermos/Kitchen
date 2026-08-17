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
	"time"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The builds table of §7. Builds are where a developer notices the platform
// most, because a build that will not start is indistinguishable from a
// platform that is ignoring them.

const (
	SignalBuildQueueBackedUp   ID = "build.queue-backed-up"
	SignalBuildPodPending      ID = "build.pod-pending"
	SignalBuildFailingRepeated ID = "build.failing-repeatedly"
)

func buildSignals() []Signal {
	return []Signal{{
		ID:       SignalBuildQueueBackedUp,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "builds are waiting far longer than a build normally takes",
		Requires: []Input{InputBuilds},
		Evaluate: evaluateBuildQueue,
	}, {
		ID:       SignalBuildPodPending,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "a running build's pod cannot be scheduled",
		Requires: []Input{InputBuilds, InputPods},
		Evaluate: evaluateBuildPodPending,
	}, {
		ID:       SignalBuildFailingRepeated,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "a project's builds have failed several times in a row",
		Requires: []Input{InputBuilds},
		Evaluate: evaluateBuildFailing,
	}}
}

// evaluateBuildQueue measures the wait against what a build of this platform
// normally takes, rather than against a fixed number of minutes.
//
// A Rust build that queues for eight minutes on a platform where builds take
// six is fine; a static site that queues for eight minutes on a platform where
// builds take forty seconds is a concurrency limit doing its job badly. The
// floor is what stops a fresh platform, whose median is zero, from reporting
// every queued build as stuck.
func evaluateBuildQueue(snapshot *Snapshot) []Finding {
	median := medianBuildDuration(snapshot)
	threshold := time.Duration(float64(median) * BuildQueueFactor)
	if threshold < BuildQueueFloor {
		threshold = BuildQueueFloor
	}

	waiting := make([]*kitchenv1alpha1.Build, 0, 4)
	oldestWait := time.Duration(0)
	for i := range snapshot.Builds {
		build := &snapshot.Builds[i]
		if build.Status.Phase != kitchenv1alpha1.BuildQueued {
			continue
		}
		wait := snapshot.Now.Sub(build.CreationTimestamp.Time)
		if wait < threshold {
			continue
		}
		waiting = append(waiting, build)
		if wait > oldestWait {
			oldestWait = wait
		}
	}
	if len(waiting) == 0 {
		return nil
	}

	scope := Scope{Kind: ScopePlatform, Name: "builds"}
	return []Finding{fire(SignalBuildQueueBackedUp, SeverityWarning, scope,
		snapshot.Now.Add(-oldestWait),
		fmt.Sprintf("%s waiting to start", plural(len(waiting), "build", "builds")),
		sentence(
			fmt.Sprintf("the oldest has waited %s", duration(oldestWait)),
			medianClause(median, threshold),
			"builds run against a concurrency limit; until one finishes, the queue does not move",
		),
		EvidenceBuilds)}
}

func medianClause(median, threshold time.Duration) string {
	if median <= 0 {
		return fmt.Sprintf("no completed build to compare against, so the floor of %s applies",
			duration(threshold))
	}
	return fmt.Sprintf("a build here normally takes %s", duration(median))
}

// medianBuildDuration is the median of the builds that succeeded in the
// lookback. Successful ones only: a build that failed after four seconds
// because the Dockerfile was missing says nothing about how long building
// takes.
func medianBuildDuration(snapshot *Snapshot) time.Duration {
	durations := make([]time.Duration, 0, len(snapshot.Builds))
	for i := range snapshot.Builds {
		build := &snapshot.Builds[i]
		if build.Status.Phase != kitchenv1alpha1.BuildSucceeded ||
			build.Status.StartedAt == nil || build.Status.CompletedAt == nil {
			continue
		}
		if snapshot.Now.Sub(build.Status.CompletedAt.Time) > BuildLookback {
			continue
		}
		if span := build.Status.CompletedAt.Sub(build.Status.StartedAt.Time); span > 0 {
			durations = append(durations, span)
		}
	}
	if len(durations) == 0 {
		return 0
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return durations[len(durations)/2]
}

// evaluateBuildPodPending reports a build whose pod the scheduler will not
// place.
//
// It is separate from workload.unschedulable because a build pod belongs to no
// environment and no Deployment: it is a Job's pod, it lives in the platform
// namespace, and it would otherwise be reported as an anonymous platform
// workload with a name nobody recognises.
func evaluateBuildPodPending(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for i := range snapshot.Builds {
		build := &snapshot.Builds[i]
		if build.Status.Phase != kitchenv1alpha1.BuildRunning {
			continue
		}
		pod := buildPod(snapshot, build.Name)
		if pod == nil || pod.Status.Phase != corev1.PodPending {
			continue
		}
		condition := podCondition(pod, corev1.PodScheduled)
		if condition == nil || condition.Status != corev1.ConditionFalse {
			continue
		}
		if snapshot.Now.Sub(condition.LastTransitionTime.Time) < PendingGrace {
			continue
		}
		scope := Scope{
			Kind:    ScopeBuild,
			Project: build.Spec.ProjectRef.Name,
			Name:    build.Name,
		}
		findings = append(findings, fire(SignalBuildPodPending, SeverityWarning, scope,
			condition.LastTransitionTime.Time,
			"the build cannot be scheduled",
			sentence(
				fmt.Sprintf("pending for %s",
					duration(snapshot.Now.Sub(condition.LastTransitionTime.Time))),
				withReason(condition.Reason, condition.Message),
				"the build reports Running because its Job exists; nothing is executing",
			),
			buildEvidence(build.Name)))
	}
	return findings
}

// buildPod finds the pod a Build's Job created. The join is the Job
// controller's own `job-name` label rather than anything Kitchen writes: the
// pod is created by the Job controller, so the only labels on it beyond the
// template's are that controller's.
func buildPod(snapshot *Snapshot, build string) *corev1.Pod {
	for i := range snapshot.Pods {
		pod := &snapshot.Pods[i]
		if pod.Labels[labelJobName] == build {
			return pod
		}
	}
	return nil
}

// labelJobName is the Job controller's own label, which is how a build's pod
// is found. It is batch/v1's, not Kitchen's.
const labelJobName = "job-name"

// failureStreak counts the consecutive failures at the newest end of a
// project's builds, and dates the streak from its oldest member.
//
// A build that has not finished neither breaks the streak nor extends it: a
// queued build after three failures is a fourth attempt, not a recovery. The
// first success ends it, which is what makes the count mean "right now".
func failureStreak(builds []*kitchenv1alpha1.Build) (int, time.Time) {
	streak := 0
	var since time.Time
	for _, build := range builds {
		switch build.Status.Phase {
		case kitchenv1alpha1.BuildFailed:
			streak++
			since = build.CreationTimestamp.Time
		case kitchenv1alpha1.BuildSucceeded:
			return streak, since
		default:
			// Queued, Running, Cancelled: not an outcome either way.
		}
	}
	return streak, since
}

// evaluateBuildFailing reports a project whose builds keep failing, which is
// the difference between one bad commit and something wrong with the project's
// configuration — a missing secret, a base image that moved, a registry that
// stopped accepting the credentials.
func evaluateBuildFailing(snapshot *Snapshot) []Finding {
	byProject := map[string][]*kitchenv1alpha1.Build{}
	for i := range snapshot.Builds {
		build := &snapshot.Builds[i]
		if snapshot.Now.Sub(build.CreationTimestamp.Time) > BuildLookback {
			continue
		}
		project := build.Spec.ProjectRef.Name
		byProject[project] = append(byProject[project], build)
	}

	projects := make([]string, 0, len(byProject))
	for project := range byProject {
		projects = append(projects, project)
	}
	sort.Strings(projects)

	findings := make([]Finding, 0, 1)
	for _, project := range projects {
		builds := byProject[project]
		// Newest first, so that "consecutive" is counted from the present: a
		// project whose last three builds failed is broken now, one whose
		// first three did and has since recovered is not.
		sort.Slice(builds, func(i, j int) bool {
			return builds[j].CreationTimestamp.Before(&builds[i].CreationTimestamp)
		})
		streak, since := failureStreak(builds)
		if streak < BuildFailureStreak {
			continue
		}
		scope := Scope{Kind: ScopeProject, Project: project}
		findings = append(findings, fire(SignalBuildFailingRepeated, SeverityWarning, scope, since,
			fmt.Sprintf("%s in a row failed", plural(streak, "build", "builds")),
			sentence(
				fmt.Sprintf("%d consecutive failures in %s", streak, duration(BuildLookback)),
				"a run this long is usually the project's configuration rather than its commits — "+
					"a missing secret, a base image that moved, a registry that stopped accepting "+
					"the credentials",
			),
			projectEvidence(project)))
	}
	return findings
}

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

	corev1 "k8s.io/api/core/v1"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The workload table of docs/OBSERVABILITY.md §7: everything that is wrong with
// one application, from the two sides the platform can see it from — what the
// API server decided about its pods, and what the kubelet and the edge
// measured of them.
//
// Audience is developer throughout, which means each of these also reaches the
// environment page's diagnostics strip. That is why the titles are written as
// half-sentences a person would say — "crash-looping", "memory at 96% of
// limit" — rather than as the condition names they are derived from.

// Signal ids, as constants because each one is spelled twice: once in the
// catalogue entry and once in every finding the rule produces.
const (
	SignalCrashLoop        ID = "workload.crashloop"
	SignalOOMKilled        ID = "workload.oomkilled"
	SignalNearMemoryLimit  ID = "workload.near-memory-limit"
	SignalAtCPULimit       ID = "workload.at-cpu-limit"
	SignalImagePull        ID = "workload.imagepull"
	SignalUnschedulable    ID = "workload.unschedulable"
	SignalAdmissionRefused ID = "workload.admission-refused"
	SignalNotReady         ID = "workload.notready"

	SignalErrorRate        ID = "env.error-rate"
	SignalLatencyRegressed ID = "env.latency-regressed"
	SignalTrafficVanished  ID = "env.traffic-vanished"
	SignalNoBackend        ID = "env.no-backend"
)

// The waiting reasons the rules key on. They are the kubelet's own strings and
// there is no constant for them in client-go.
const (
	reasonCrashLoopBackOff = "CrashLoopBackOff"
	reasonImagePullBackOff = "ImagePullBackOff"
	reasonErrImagePull     = "ErrImagePull"
	reasonFailedCreate     = "FailedCreate"
	reasonOOMKilled        = "OOMKilled"
)

func workloadSignals() []Signal {
	return []Signal{{
		ID:       SignalCrashLoop,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "a container is in CrashLoopBackOff, or an environment restarted more than a rollout explains",
		Requires: []Input{InputPods},
		Evaluate: evaluateCrashLoop,
	}, {
		ID:       SignalOOMKilled,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "the kernel killed a container for exceeding its memory limit",
		Requires: []Input{InputResources},
		Evaluate: evaluateOOMKilled,
	}, {
		ID:       SignalNearMemoryLimit,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "working set sustained near the memory limit — the OOM kill, before it happens",
		Requires: []Input{InputResources},
		Evaluate: evaluateNearMemoryLimit,
	}, {
		ID:       SignalAtCPULimit,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "CPU pinned at the limit, which is throttling by another name",
		Requires: []Input{InputResources},
		Evaluate: evaluateAtCPULimit,
	}, {
		ID:       SignalImagePull,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "a container's image cannot be pulled",
		Requires: []Input{InputPods},
		Evaluate: evaluateImagePull,
	}, {
		ID:       SignalUnschedulable,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "a pod is Pending because the scheduler can place it nowhere",
		Requires: []Input{InputPods},
		Evaluate: evaluateUnschedulable,
	}, {
		ID:       SignalAdmissionRefused,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "a workload wants pods, has none at all, and a FailedCreate event says why",
		Requires: []Input{InputWorkloads, InputPods, InputClusterEvents},
		Evaluate: evaluateAdmissionRefused,
	}, {
		ID:       SignalNotReady,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "fewer pods available than wanted, for longer than a rollout takes",
		Requires: []Input{InputWorkloads},
		Evaluate: evaluateNotReady,
	}, {
		ID:       SignalErrorRate,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "the edge is answering 5xx for an environment, above threshold and above its own baseline",
		Requires: []Input{InputRequests},
		Evaluate: evaluateErrorRate,
	}, {
		ID:       SignalLatencyRegressed,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "p95 sustained above the trailing baseline",
		Requires: []Input{InputRequests},
		Evaluate: evaluateLatencyRegressed,
	}, {
		ID:       SignalTrafficVanished,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "traffic stopped where the trailing window had some",
		Requires: []Input{InputRequests},
		Evaluate: evaluateTrafficVanished,
	}, {
		ID:       SignalNoBackend,
		Version:  1,
		Audience: AudienceDeveloper,
		Summary:  "the edge is failing requests for an environment with no ready pod behind it",
		Requires: []Input{InputRequests, InputPods},
		Evaluate: evaluateNoBackend,
	}}
}

// evaluateCrashLoop reads the crash loop from both ends.
//
// Pod status names the container, which is what the reader wants, and it is
// authoritative while the loop is backing off. The restart count from
// metrics_5m is the number that makes the finding actionable — "12 restarts in
// 30m" is the line the diagnostics strip renders — and it is also the only way
// to catch the loop that never backs off: a container that exits and is
// restarted every two minutes never enters CrashLoopBackOff at all, because
// the kubelet's back-off resets after ten minutes of running.
//
// So both fire, at different scopes, and the environment-wide one steps aside
// where a container-scoped one already said it: the same restarts reported
// twice would be two rows on the strip for one problem.
func evaluateCrashLoop(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 2)
	looping := map[EnvKey]bool{}

	for i := range snapshot.Pods {
		pod := &snapshot.Pods[i]
		for _, status := range containerStatuses(pod) {
			if waitingReason(status) != reasonCrashLoopBackOff {
				continue
			}
			scope := podScope(pod, status.Name)
			looping[EnvKey{Project: scope.Project, Environment: scope.Environment}] = true
			findings = append(findings, fire(SignalCrashLoop, SeverityCritical, scope,
				crashLoopSince(snapshot, status),
				"crash-looping",
				sentence(
					crashLoopHeadline(snapshot, scope, status),
					terminationDetail(status),
					fmt.Sprintf("pod %s in namespace %s", pod.Name, pod.Namespace),
				),
				scopeEvidence(scope, sectionWorkload)))
		}
	}

	// The restart-delta path, for the environments no container reported.
	if snapshot.Available(InputResources) {
		findings = append(findings, restartStormFindings(snapshot, looping)...)
	}
	return findings
}

// crashLoopHeadline is the restart count from the rollup where there is one,
// and the pod's own cumulative count where there is not. The rollup's number
// is the better one — it is windowed, so it distinguishes a container that
// restarted twelve times this half hour from one that restarted twelve times
// last month and has been up since.
func crashLoopHeadline(snapshot *Snapshot, scope Scope, status *corev1.ContainerStatus) string {
	key := EnvKey{Project: scope.Project, Environment: scope.Environment}
	if restarts, ok := restartsInWindow(snapshot, key); ok {
		return fmt.Sprintf("%s in %s", plural(restarts, "restart", "restarts"), duration(RestartWindow))
	}
	return fmt.Sprintf("%s so far", plural(int(status.RestartCount), "restart", "restarts"))
}

// restartsInWindow totals an environment's restarts over the restart window,
// reporting false when the rollup holds nothing for it.
func restartsInWindow(snapshot *Snapshot, key EnvKey) (int, bool) {
	series, ok := snapshot.Resources[key]
	if !ok {
		return 0, false
	}
	buckets := bucketsSince(restartBuckets(series), snapshot.Now.Add(-RestartWindow))
	if len(buckets) == 0 {
		return 0, false
	}
	return int(Sum(buckets)), true
}

// restartStormFindings reports environments whose restart count is past the
// threshold without any container being in backoff — the loop that is fast
// enough to look healthy.
func restartStormFindings(snapshot *Snapshot, looping map[EnvKey]bool) []Finding {
	findings := make([]Finding, 0, 1)
	for _, key := range snapshot.EnvKeys() {
		if looping[key] {
			continue
		}
		restarts, ok := restartsInWindow(snapshot, key)
		if !ok || restarts < RestartsFiring {
			continue
		}
		scope := Scope{Kind: ScopeEnvironment, Project: key.Project, Environment: key.Environment}
		findings = append(findings, fire(SignalCrashLoop, SeverityWarning, scope,
			snapshot.Now.Add(-RestartWindow),
			"restarting repeatedly",
			sentence(
				fmt.Sprintf("%s in %s", plural(restarts, "restart", "restarts"), duration(RestartWindow)),
				"no container is in CrashLoopBackOff, so the kubelet's back-off keeps resetting: "+
					"the containers are running long enough between exits to look healthy",
			),
			scopeEvidence(scope, sectionResources)))
	}
	return findings
}

// crashLoopSince dates the loop from the container's most recent death, which
// is the last thing about it the snapshot can prove.
func crashLoopSince(snapshot *Snapshot, status *corev1.ContainerStatus) time.Time {
	if terminated := status.LastTerminationState.Terminated; terminated != nil &&
		!terminated.FinishedAt.IsZero() {
		return terminated.FinishedAt.Time
	}
	return snapshot.Now
}

// terminationDetail is what the container died of, which is the difference
// between a program that is broken and a limit that is too small.
func terminationDetail(status *corev1.ContainerStatus) string {
	terminated := status.LastTerminationState.Terminated
	if terminated == nil || terminated.Reason == "" {
		return ""
	}
	detail := fmt.Sprintf("last exit %s (code %d)", terminated.Reason, terminated.ExitCode)
	if terminated.Reason == reasonOOMKilled {
		detail += " — the memory limit, not the program"
	}
	return detail
}

func evaluateOOMKilled(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for _, key := range snapshot.EnvKeys() {
		series, ok := snapshot.Resources[key]
		if !ok {
			continue
		}
		buckets := bucketsSince(oomBuckets(series), snapshot.Now.Add(-OOMWindow))
		kills := int(Sum(buckets))
		if kills == 0 {
			continue
		}
		scope := Scope{Kind: ScopeEnvironment, Project: key.Project, Environment: key.Environment}
		findings = append(findings, fire(SignalOOMKilled, SeverityCritical, scope,
			firstNonZero(buckets, snapshot.Now),
			"killed for using too much memory",
			sentence(
				fmt.Sprintf("%s in %s", plural(kills, "OOM kill", "OOM kills"), duration(OOMWindow)),
				oomLimitClause(series),
				"the kernel enforces the limit without warning: either the limit is too small or the "+
					"application is leaking",
			),
			scopeEvidence(scope, sectionResources)))
	}
	return findings
}

// oomLimitClause names the limit that was enforced, which is the number the
// reader has to change.
func oomLimitClause(series clickhouse.ResourceSeries) string {
	if series.MemoryLimitBytes == 0 {
		return ""
	}
	return "memory limit " + bytes(float64(series.MemoryLimitBytes))
}

func evaluateNearMemoryLimit(snapshot *Snapshot) []Finding {
	return limitFindings(snapshot, limitRule{
		id:       SignalNearMemoryLimit,
		severity: SeverityWarning,
		fraction: MemoryLimitFraction,
		buckets:  memoryFractionBuckets,
		title: func(run Run) string {
			return "memory at " + percent(run.Latest) + " of limit"
		},
		detail: func(run Run, series clickhouse.ResourceSeries) string {
			return sentence(
				fmt.Sprintf("peak working set %s of the %s limit",
					bytes(run.Latest*float64(series.MemoryLimitBytes)),
					bytes(float64(series.MemoryLimitBytes))),
				"sustained for "+duration(run.Duration),
				"the kernel kills at 100% with no grace period",
			)
		},
	})
}

func evaluateAtCPULimit(snapshot *Snapshot) []Finding {
	return limitFindings(snapshot, limitRule{
		id:       SignalAtCPULimit,
		severity: SeverityWarning,
		fraction: CPULimitFraction,
		buckets:  cpuFractionBuckets,
		title: func(run Run) string {
			return "CPU pinned at " + percent(run.Latest) + " of limit"
		},
		detail: func(run Run, series clickhouse.ResourceSeries) string {
			return sentence(
				fmt.Sprintf("peak %.2f of the %.2f core limit", run.Latest*series.CPULimitCores,
					series.CPULimitCores),
				"sustained for "+duration(run.Duration),
				"a container at its CPU quota is being throttled, which shows up as latency rather "+
					"than as an error",
			)
		},
	})
}

// limitRule is the shared shape of the two "sustained against a limit" rules.
// They differ in the fraction, the series and the words; everything else —
// which environments to walk, what sustained means, where the evidence is — is
// the same, and writing it twice is how the two would come to disagree about
// it.
type limitRule struct {
	id       ID
	severity Severity
	fraction float64
	buckets  func(clickhouse.ResourceSeries) []Bucket
	title    func(Run) string
	detail   func(Run, clickhouse.ResourceSeries) string
}

func limitFindings(snapshot *Snapshot, rule limitRule) []Finding {
	findings := make([]Finding, 0, 1)
	for _, key := range snapshot.EnvKeys() {
		series, ok := snapshot.Resources[key]
		if !ok {
			continue
		}
		buckets := rule.buckets(series)
		if len(buckets) == 0 {
			// No limit configured. An environment cannot be near a limit it
			// does not have, and inventing one to compare against would be
			// reporting the node's capacity as the application's ceiling.
			continue
		}
		run := TrailingRun(buckets, resourceWidth(series), snapshot.Now, func(value float64) bool {
			return value >= rule.fraction
		})
		if !run.Sustained() {
			continue
		}
		scope := Scope{Kind: ScopeEnvironment, Project: key.Project, Environment: key.Environment}
		findings = append(findings, fire(rule.id, rule.severity, scope, run.Since,
			rule.title(run), rule.detail(run, series), scopeEvidence(scope, sectionResources)))
	}
	return findings
}

func evaluateImagePull(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for i := range snapshot.Pods {
		pod := &snapshot.Pods[i]
		for _, status := range containerStatuses(pod) {
			reason := waitingReason(status)
			if reason != reasonImagePullBackOff && reason != reasonErrImagePull {
				continue
			}
			scope := podScope(pod, status.Name)
			findings = append(findings, fire(SignalImagePull, SeverityCritical, scope, snapshot.Now,
				"image cannot be pulled",
				sentence(reason, status.State.Waiting.Message,
					fmt.Sprintf("image %s", status.Image)),
				scopeEvidence(scope, sectionWorkload)))
		}
	}
	return findings
}

func evaluateUnschedulable(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for i := range snapshot.Pods {
		pod := &snapshot.Pods[i]
		if pod.Status.Phase != corev1.PodPending || pod.DeletionTimestamp != nil {
			continue
		}
		condition := podCondition(pod, corev1.PodScheduled)
		if condition == nil || condition.Status != corev1.ConditionFalse {
			continue
		}
		// A pod is briefly unscheduled on the way to being scheduled. Only a
		// pod that has stayed that way is stuck.
		if snapshot.Now.Sub(condition.LastTransitionTime.Time) < PendingGrace {
			continue
		}
		scope := podScope(pod, "")
		findings = append(findings, fire(SignalUnschedulable, SeverityCritical, scope,
			condition.LastTransitionTime.Time,
			"cannot be scheduled",
			sentence(
				fmt.Sprintf("pending for %s", duration(snapshot.Now.Sub(condition.LastTransitionTime.Time))),
				withReason(condition.Reason, condition.Message),
				fmt.Sprintf("pod %s in namespace %s", pod.Name, pod.Namespace),
			),
			scopeEvidence(scope, sectionWorkload)))
	}
	return findings
}

// evaluateAdmissionRefused is the component survey's lesson applied to every
// namespace: a workload whose pods are refused at admission has no pods at
// all, so every screen that counts failing pods shows nothing wrong.
//
// The rejection is not pod state — there is no pod — it is a FailedCreate
// Warning on the workload, which the k8s_events recorder now keeps. This is
// how the log collector sat dead for hours on a cluster whose every condition
// read True, and the detail names the suspect out loud because the reader has
// no reason to think of Pod Security on their own.
func evaluateAdmissionRefused(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for _, workload := range snapshotWorkloads(snapshot) {
		if workload.desired == 0 || workload.hasPods {
			continue
		}
		events := snapshot.EventsFor(workload.namespace, workload.kind, workload.name, reasonFailedCreate)
		if len(events) == 0 {
			continue
		}
		newest := events[0]
		for _, event := range events {
			if event.Timestamp.After(newest.Timestamp) {
				newest = event
			}
		}
		scope := workload.scope()
		findings = append(findings, fire(SignalAdmissionRefused, SeverityCritical, scope, oldest(events),
			"pods are being refused, so there are none",
			sentence(
				fmt.Sprintf("%s wants %d and has none", workload.kind, workload.desired),
				newest.Message,
				admissionSuspect(newest.Message, workload.namespace),
			),
			eventsEvidence(workload.namespace, workload.kind, workload.name)))
	}
	return findings
}

// admissionSuspect names the cause where the message betrays it. Pod Security
// is called out by name because it is the one that has actually happened here:
// the chart sets the platform namespace to privileged precisely because the
// log collector mounts the node's /var/log, and a cluster that defaults to
// baseline refuses those pods silently.
func admissionSuspect(message, namespace string) string {
	if strings.Contains(message, "violates PodSecurity") || strings.Contains(message, "pod-security") {
		return fmt.Sprintf(
			"the Pod Security level enforced on namespace %s is refusing these pods — check its "+
				"pod-security.kubernetes.io/enforce label", namespace)
	}
	if strings.Contains(message, "exceeded quota") {
		return fmt.Sprintf("a ResourceQuota in namespace %s is refusing them", namespace)
	}
	return "the workload's controller could not create the pods at all, so nothing shows in the pod list"
}

func evaluateNotReady(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for _, workload := range snapshotWorkloads(snapshot) {
		if workload.desired == 0 || workload.available >= workload.desired {
			continue
		}
		// A workload short of pods with none at all is admission-refused's
		// story, told with the reason attached. Reporting both would put two
		// rows on the list for one cause.
		if !workload.hasPods && len(snapshot.EventsFor(
			workload.namespace, workload.kind, workload.name, reasonFailedCreate)) > 0 {
			continue
		}
		if snapshot.Now.Sub(workload.changedAt) < NotReadyGrace {
			continue
		}
		scope := workload.scope()
		severity := SeverityWarning
		if workload.available == 0 {
			// Nothing is serving. Whatever the cause, that is not degradation.
			severity = SeverityCritical
		}
		findings = append(findings, fire(SignalNotReady, severity, scope, workload.changedAt,
			fmt.Sprintf("%d of %d pods available", workload.available, workload.desired),
			sentence(
				fmt.Sprintf("short of pods for %s", duration(snapshot.Now.Sub(workload.changedAt))),
				workload.reason,
				fmt.Sprintf("%s %s in namespace %s", workload.kind, workload.name, workload.namespace),
			),
			scopeEvidence(scope, sectionWorkload)))
	}
	return findings
}

func evaluateErrorRate(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for _, key := range snapshot.EnvKeys() {
		series, ok := snapshot.Traffic[key]
		if !ok {
			continue
		}
		recent, baseline := Split(ratioBuckets(series, errorRateOf), snapshot.Now.Add(-RecentWindow))
		recentRequests, _ := Split(requestBuckets(series, requestsOf), snapshot.Now.Add(-RecentWindow))
		requests := Sum(recentRequests)
		if requests < MinRequestsToJudge {
			// Too little traffic for a ratio to mean anything: with nineteen
			// requests, one failure is already past the threshold.
			continue
		}
		recentRate, _ := Mean(recent)
		baselineRate, support := Mean(baseline)
		comparison := Regression{Recent: recentRate, Baseline: baselineRate, Support: support}
		if !comparison.Elevated(ErrorRateFactor, ErrorRateFiring) {
			continue
		}
		scope := Scope{Kind: ScopeEnvironment, Project: key.Project, Environment: key.Environment}
		findings = append(findings, fire(SignalErrorRate, SeverityCritical, scope,
			snapshot.Now.Add(-RecentWindow),
			"failing "+percent(recentRate)+" of requests",
			sentence(
				fmt.Sprintf("%s of %.0f requests in %s answered 5xx",
					percent(recentRate), requests, duration(RecentWindow)),
				baselineClause(percent(baselineRate), support),
			),
			scopeEvidence(scope, sectionRequests)))
	}
	return findings
}

func evaluateLatencyRegressed(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for _, key := range snapshot.EnvKeys() {
		series, ok := snapshot.Traffic[key]
		if !ok {
			continue
		}
		latency := ratioBuckets(series, func(point clickhouse.RequestPoint) float64 { return point.P95Ms })
		recent, baseline := Split(latency, snapshot.Now.Add(-RecentWindow))
		recentP95, _ := Mean(recent)
		baselineP95, support := Mean(baseline)
		comparison := Regression{Recent: recentP95, Baseline: baselineP95, Support: support}
		if !comparison.Regressed(LatencyRegressionFactor, LatencyFloorMs) {
			continue
		}
		// Regressed says it is worse on average; sustained says it has stayed
		// worse. A single slow minute inside a fifteen-minute window can carry
		// the mean, and that is a spike rather than a regression.
		run := TrailingRun(recent, requestWidth(series), snapshot.Now, func(value float64) bool {
			return value >= baselineP95*LatencyRegressionFactor
		})
		if !run.Sustained() {
			continue
		}
		scope := Scope{Kind: ScopeEnvironment, Project: key.Project, Environment: key.Environment}
		findings = append(findings, fire(SignalLatencyRegressed, SeverityWarning, scope, run.Since,
			"p95 latency "+milliseconds(recentP95),
			sentence(
				fmt.Sprintf("p95 %s against a %s baseline", milliseconds(recentP95),
					milliseconds(baselineP95)),
				"sustained for "+duration(run.Duration),
				"the edge measures the whole request, so a cold start or a slow dependency both "+
					"look like this",
			),
			scopeEvidence(scope, sectionRequests)))
	}
	return findings
}

func evaluateTrafficVanished(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for _, key := range snapshot.EnvKeys() {
		series, ok := snapshot.Traffic[key]
		if !ok {
			continue
		}
		requests := requestBuckets(series, requestsOf)
		recent, baseline := Split(requests, snapshot.Now.Add(-RecentWindow))
		if len(recent) == 0 || Sum(recent) > 0 {
			continue
		}
		baselineTotal := Sum(baseline)
		width := requestWidth(series)
		baselineSeconds := float64(len(baseline)) * width.Seconds()
		if baselineSeconds <= 0 || baselineTotal/baselineSeconds < TrafficVanishedBaselineRPS {
			continue
		}
		scope := Scope{Kind: ScopeEnvironment, Project: key.Project, Environment: key.Environment}
		findings = append(findings, fire(SignalTrafficVanished, SeverityWarning, scope,
			snapshot.Now.Add(-RecentWindow),
			"no traffic reaching it",
			sentence(
				fmt.Sprintf("nothing in %s, against %.2f requests/s before it",
					duration(RecentWindow), baselineTotal/baselineSeconds),
				"the edge sees only what arrives: a DNS record, a certificate or a route can stop "+
					"traffic without anything here failing",
			),
			scopeEvidence(scope, sectionRequests)))
	}
	return findings
}

func evaluateNoBackend(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for _, key := range snapshot.EnvKeys() {
		series, ok := snapshot.Traffic[key]
		if !ok {
			continue
		}
		recent, _ := Split(requestBuckets(series, func(point clickhouse.RequestPoint) float64 {
			return float64(point.Errors)
		}), snapshot.Now.Add(-RecentWindow))
		errors := Sum(recent)
		if errors < NoBackendMinErrors || snapshot.ReadyPods(key) > 0 {
			continue
		}
		scope := Scope{Kind: ScopeEnvironment, Project: key.Project, Environment: key.Environment}
		findings = append(findings, fire(SignalNoBackend, SeverityCritical, scope,
			snapshot.Now.Add(-RecentWindow),
			"nothing behind the route",
			sentence(
				fmt.Sprintf("%.0f failed requests in %s with no ready pod", errors, duration(RecentWindow)),
				"the route is published and the edge is answering it, so visitors are getting the "+
					"proxy's error rather than the application's",
			),
			scopeEvidence(scope, sectionRequests)))
	}
	return findings
}

// requestsOf and errorRateOf are the two projections the traffic rules share.
func requestsOf(point clickhouse.RequestPoint) float64  { return float64(point.Requests) }
func errorRateOf(point clickhouse.RequestPoint) float64 { return point.ErrorRate }

// baselineClause says what the recent number is being compared against, or
// says that there was nothing to compare against — which is a materially
// different finding and should not read like a comparison that happened.
func baselineClause(baseline string, support int) string {
	if support < MinBaselineBuckets {
		return "no trailing baseline to compare against yet"
	}
	return "baseline " + baseline
}

// firstNonZero dates a counted condition from the oldest bucket that counted
// anything, which is as close to "since" as a bucketed series gets.
func firstNonZero(buckets []Bucket, fallback time.Time) time.Time {
	for _, bucket := range buckets {
		if bucket.Observed && bucket.Value > 0 {
			return bucket.Start
		}
	}
	return fallback
}

// oldest is the earliest timestamp in a set of events, which is when the
// condition they describe started.
func oldest(events []clickhouse.K8sEvent) time.Time {
	oldest := events[0].Timestamp
	for _, event := range events {
		if event.Timestamp.Before(oldest) {
			oldest = event.Timestamp
		}
	}
	return oldest
}

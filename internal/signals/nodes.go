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
)

// The nodes-and-capacity table of §7: the machine underneath, which every
// application on it shares and none of them can see.

const (
	SignalNodeNotReady    ID = "node.notready"
	SignalNodePressure    ID = "node.pressure"
	SignalNodeSaturated   ID = "node.saturated"
	SignalNodeDiskFilling ID = "node.disk-filling"
	SignalNodeSilent      ID = "node.silent"
	SignalOvercommitted   ID = "cluster.overcommitted"
)

func nodeSignals() []Signal {
	return []Signal{{
		ID:       SignalNodeNotReady,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "a node's Ready condition is not true",
		Requires: []Input{InputNodes},
		Evaluate: evaluateNodeNotReady,
	}, {
		ID:       SignalNodePressure,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "a node reports memory, disk or PID pressure",
		Requires: []Input{InputNodes},
		Evaluate: evaluateNodePressure,
	}, {
		ID:       SignalNodeSaturated,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "a node's CPU or memory has been at or above 90% for long enough to matter",
		Requires: []Input{InputHostMetrics},
		Evaluate: evaluateNodeSaturated,
	}, {
		ID:       SignalNodeDiskFilling,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "a filesystem is projected to fill within a week",
		Requires: []Input{InputHostMetrics},
		Evaluate: evaluateNodeDiskFilling,
	}, {
		ID:       SignalNodeSilent,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "a node exists in the API server but its collector has shipped nothing",
		Requires: []Input{InputNodes, InputFreshness},
		Evaluate: evaluateNodeSilent,
	}, {
		ID:       SignalOvercommitted,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "scheduled requests exceed what remains if one node is lost",
		Requires: []Input{InputNodes, InputPods},
		Evaluate: evaluateOvercommitted,
	}}
}

func evaluateNodeNotReady(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for i := range snapshot.Nodes {
		node := &snapshot.Nodes[i]
		condition := nodeCondition(node, corev1.NodeReady)
		if condition == nil || condition.Status == corev1.ConditionTrue {
			continue
		}
		scope := Scope{Kind: ScopeNode, Node: node.Name}
		findings = append(findings, fire(SignalNodeNotReady, SeverityCritical, scope,
			condition.LastTransitionTime.Time,
			"node is not ready",
			sentence(
				fmt.Sprintf("Ready=%s for %s", condition.Status,
					duration(snapshot.Now.Sub(condition.LastTransitionTime.Time))),
				withReason(condition.Reason, condition.Message),
				fmt.Sprintf("%s pods were scheduled here",
					plural(podsOnNode(snapshot, node.Name), "pod", "pods")),
			),
			nodeEvidence(node.Name)))
	}
	return findings
}

// pressureConditions are the three the kubelet raises when the node is out of
// something. Each one stops scheduling and starts eviction, so each one is a
// finding rather than a note.
var pressureConditions = []corev1.NodeConditionType{
	corev1.NodeMemoryPressure,
	corev1.NodeDiskPressure,
	corev1.NodePIDPressure,
}

func evaluateNodePressure(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for i := range snapshot.Nodes {
		node := &snapshot.Nodes[i]
		for _, conditionType := range pressureConditions {
			condition := nodeCondition(node, conditionType)
			if condition == nil || condition.Status != corev1.ConditionTrue {
				continue
			}
			// The condition type is part of the scope, because a node under
			// memory pressure and the same node under disk pressure are two
			// conditions that open and resolve independently.
			scope := Scope{Kind: ScopeNode, Node: node.Name, Name: string(conditionType)}
			findings = append(findings, fire(SignalNodePressure, SeverityWarning, scope,
				condition.LastTransitionTime.Time,
				"node under "+pressureName(conditionType)+" pressure",
				sentence(
					fmt.Sprintf("%s for %s", conditionType,
						duration(snapshot.Now.Sub(condition.LastTransitionTime.Time))),
					withReason(condition.Reason, condition.Message),
					"the kubelet stops scheduling here and starts evicting pods",
				),
				nodeEvidence(node.Name)))
		}
	}
	return findings
}

// pressureName turns MemoryPressure into "memory", for a title a person reads
// rather than a condition type they decode.
func pressureName(conditionType corev1.NodeConditionType) string {
	return strings.ToLower(strings.TrimSuffix(string(conditionType), "Pressure"))
}

func evaluateNodeSaturated(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for _, name := range sortedNodeUsage(snapshot) {
		usage := snapshot.NodeUsage[name]
		for _, dimension := range []struct {
			label   string
			buckets []Bucket
		}{
			{label: "CPU", buckets: usage.CPU},
			{label: "memory", buckets: usage.Memory},
		} {
			run := TrailingRun(dimension.buckets, usage.BucketWidth, snapshot.Now, func(value float64) bool {
				return value >= NodeSaturationFraction
			})
			if !run.Sustained() {
				continue
			}
			// The dimension is part of the scope: a node saturated on CPU and
			// the same node saturated on memory are two conditions that open
			// and resolve independently.
			scope := Scope{Kind: ScopeNode, Node: name, Name: dimension.label}
			findings = append(findings, fire(SignalNodeSaturated, SeverityWarning, scope, run.Since,
				fmt.Sprintf("node %s at %s", dimension.label, percent(run.Latest)),
				sentence(
					fmt.Sprintf("%s at %s, peaking at %s", dimension.label, percent(run.Latest),
						percent(run.Peak)),
					"sustained for "+duration(run.Duration),
					"latency on every application scheduled here comes from the node before it comes "+
						"from the application",
				),
				nodeEvidence(name)))
		}
	}
	return findings
}

func evaluateNodeDiskFilling(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for _, name := range sortedNodeUsage(snapshot) {
		usage := snapshot.NodeUsage[name]
		for _, filesystem := range usage.Filesystems {
			full, ok := projectedFull(filesystem.Used)
			if !ok {
				continue
			}
			scope := Scope{Kind: ScopeNode, Node: name, Name: filesystem.MountPoint}
			latest := latestValue(filesystem.Used)
			findings = append(findings, fire(SignalNodeDiskFilling, SeverityWarning, scope, snapshot.Now,
				fmt.Sprintf("%s full in %s", filesystem.MountPoint, duration(full)),
				sentence(
					fmt.Sprintf("%s used of %s, filling at the rate of the last %s",
						percent(latest), bytes(float64(filesystem.CapacityBytes)),
						duration(usage.BucketWidth*time.Duration(len(filesystem.Used)))),
					fmt.Sprintf("projected full in %s on a straight-line fit", duration(full)),
					"a full node filesystem stops the kubelet, not just the application that filled it",
				),
				nodeEvidence(name)))
		}
	}
	return findings
}

// projectedFull fits a line through a filesystem's fill and reports how long
// until it reaches 100%, or false when there is nothing worth projecting.
//
// The guards are what keep this from being a random-number generator. A
// filesystem still half empty has too much room for a slope measured over
// hours to say anything about; a flat or shrinking one has no date at all; and
// a projection further out than the window it is worried about is not news.
func projectedFull(used []Bucket) (time.Duration, bool) {
	latest := latestValue(used)
	if latest < DiskFillMinFraction || latest >= 1 {
		return 0, false
	}
	perSecond, points := Slope(used)
	if points < MinBaselineBuckets || perSecond <= 0 {
		return 0, false
	}
	full := time.Duration((1-latest)/perSecond) * time.Second
	if full > DiskFillDays*24*time.Hour {
		return 0, false
	}
	return full, true
}

// evaluateNodeSilent is the only rule that reads an absence.
//
// A node whose collector is dead, or whose collector was never admitted,
// looks *clean* everywhere else: the API server is happy, the conditions are
// true, and every screen that reads telemetry simply shows less of it. This is
// the one place that looks broken — and it catches the namespace Pod Security
// failure as a side effect, because a DaemonSet whose pods are refused at
// admission has no pods at all and so ships nothing.
//
// The freshness query's contract is that a node silent for longer than the
// lookback is *absent* from the answer rather than present with an old
// timestamp. So absence is the strongest signal here, not a missing input.
func evaluateNodeSilent(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for i := range snapshot.Nodes {
		node := &snapshot.Nodes[i]
		lastSeen, reported := snapshot.Freshness[node.Name]
		silence := snapshot.Now.Sub(lastSeen)
		if reported && silence < NodeSilentAfter {
			continue
		}
		scope := Scope{Kind: ScopeNode, Node: node.Name}
		findings = append(findings, fire(SignalNodeSilent, SeverityCritical, scope,
			nodeSilentSince(snapshot, lastSeen, reported),
			"no telemetry from this node",
			sentence(
				nodeSilenceClause(silence, reported),
				"the node is in the API server and its conditions may all read true — a collector "+
					"that is dead, or whose pods were refused at admission, is invisible everywhere "+
					"else",
				"check the collector DaemonSet's events for FailedCreate, and the namespace's "+
					"pod-security.kubernetes.io/enforce label",
			),
			nodeEvidence(node.Name)))
	}
	return findings
}

func nodeSilenceClause(silence time.Duration, reported bool) string {
	if !reported {
		return fmt.Sprintf("nothing in the store from this node within the last %s",
			duration(FreshnessLookback))
	}
	return "silent for " + duration(silence)
}

// nodeSilentSince dates the silence from the last thing that arrived, or from
// the edge of the lookback when nothing did — which is the earliest the
// snapshot can prove, not the earliest it happened.
func nodeSilentSince(snapshot *Snapshot, lastSeen time.Time, reported bool) time.Time {
	if reported {
		return lastSeen
	}
	return snapshot.Now.Add(-FreshnessLookback)
}

// evaluateOvercommitted answers "can the next node loss be rescheduled".
//
// It compares what pods have *requested* — not what they are using — against
// what remains when the largest node is removed, because requests are what the
// scheduler will have to satisfy and the largest node is the worst one to
// lose.
func evaluateOvercommitted(snapshot *Snapshot) []Finding {
	if len(snapshot.Nodes) <= OvercommitHeadroomNodes {
		// A single-node cluster cannot lose a node and stay a cluster. Saying
		// so on every evaluation would be true and useless.
		return nil
	}

	var cpuAllocatable, memoryAllocatable, largestCPU, largestMemory int64
	for i := range snapshot.Nodes {
		node := &snapshot.Nodes[i]
		cpu := node.Status.Allocatable.Cpu().MilliValue()
		memory := node.Status.Allocatable.Memory().Value()
		cpuAllocatable += cpu
		memoryAllocatable += memory
		if cpu > largestCPU {
			largestCPU = cpu
		}
		if memory > largestMemory {
			largestMemory = memory
		}
	}

	var cpuRequested, memoryRequested int64
	for i := range snapshot.Pods {
		pod := &snapshot.Pods[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		cpu, memory := podRequests(pod)
		cpuRequested += cpu
		memoryRequested += memory
	}

	cpuHeadroom := cpuAllocatable - largestCPU
	memoryHeadroom := memoryAllocatable - largestMemory
	overCPU := cpuHeadroom > 0 && cpuRequested > cpuHeadroom
	overMemory := memoryHeadroom > 0 && memoryRequested > memoryHeadroom
	if !overCPU && !overMemory {
		return nil
	}

	scope := Scope{Kind: ScopePlatform, Name: "capacity"}
	return []Finding{fire(SignalOvercommitted, SeverityWarning, scope, snapshot.Now,
		"no headroom to lose a node",
		sentence(
			overcommitHeadline(overCPU, overMemory, cpuRequested, cpuHeadroom,
				memoryRequested, memoryHeadroom),
			fmt.Sprintf("%d nodes, %s CPU and %s memory allocatable in total",
				len(snapshot.Nodes), formatCores(cpuAllocatable), bytes(float64(memoryAllocatable))),
			"if the largest node goes, the scheduler has nowhere to put what was on it",
		),
		EvidencePlatformNodes)}
}

func overcommitHeadline(overCPU, overMemory bool, cpuRequested, cpuHeadroom,
	memoryRequested, memoryHeadroom int64) string {
	clauses := make([]string, 0, 2)
	if overCPU {
		clauses = append(clauses, fmt.Sprintf("%s of CPU requested against %s without the largest node",
			formatCores(cpuRequested), formatCores(cpuHeadroom)))
	}
	if overMemory {
		clauses = append(clauses, fmt.Sprintf("%s of memory requested against %s",
			bytes(float64(memoryRequested)), bytes(float64(memoryHeadroom))))
	}
	return strings.Join(clauses, " and ")
}

// podRequests totals one pod's requests, taking init containers at their
// maximum rather than their sum — which is how the scheduler reads them, since
// init containers run one at a time.
func podRequests(pod *corev1.Pod) (cpu, memory int64) {
	for i := range pod.Spec.Containers {
		requests := pod.Spec.Containers[i].Resources.Requests
		cpu += requests.Cpu().MilliValue()
		memory += requests.Memory().Value()
	}
	var initCPU, initMemory int64
	for i := range pod.Spec.InitContainers {
		requests := pod.Spec.InitContainers[i].Resources.Requests
		if value := requests.Cpu().MilliValue(); value > initCPU {
			initCPU = value
		}
		if value := requests.Memory().Value(); value > initMemory {
			initMemory = value
		}
	}
	if initCPU > cpu {
		cpu = initCPU
	}
	if initMemory > memory {
		memory = initMemory
	}
	return cpu, memory
}

// formatCores renders millicores as the cores an operator thinks in.
func formatCores(milli int64) string {
	return fmt.Sprintf("%.1f cores", float64(milli)/1000)
}

func nodeCondition(node *corev1.Node, conditionType corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == conditionType {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

func podsOnNode(snapshot *Snapshot, node string) int {
	count := 0
	for i := range snapshot.Pods {
		if snapshot.Pods[i].Spec.NodeName == node {
			count++
		}
	}
	return count
}

// sortedNodeUsage walks the usage map in a fixed order, so that two
// evaluations of an unchanged cluster produce the same round.
func sortedNodeUsage(snapshot *Snapshot) []string {
	names := make([]string, 0, len(snapshot.NodeUsage))
	for name := range snapshot.NodeUsage {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// latestValue is the newest observed value of a series, or zero.
func latestValue(buckets []Bucket) float64 {
	for i := len(buckets) - 1; i >= 0; i-- {
		if buckets[i].Observed {
			return buckets[i].Value
		}
	}
	return 0
}

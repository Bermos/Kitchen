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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// node builds a node that is ready and has room, which is what each test here
// departs from.
func node(name string, cpu, memory string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			},
			Conditions: []corev1.NodeCondition{{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(testNow.Add(-24 * time.Hour)),
			}},
		},
	}
}

// fresh marks every node as having reported recently, so that node.silent does
// not fire in tests about something else.
func fresh(snapshot *Snapshot) {
	for i := range snapshot.Nodes {
		snapshot.Freshness[snapshot.Nodes[i].Name] = testNow.Add(-time.Minute)
	}
}

func TestNodeNotReadyFires(t *testing.T) {
	snapshot := newSnapshot()
	broken := node(testNode, "4", "16Gi")
	broken.Status.Conditions[0].Status = corev1.ConditionUnknown
	broken.Status.Conditions[0].Reason = "NodeStatusUnknown"
	broken.Status.Conditions[0].Message = "Kubelet stopped posting node status."
	broken.Status.Conditions[0].LastTransitionTime = metav1.NewTime(testNow.Add(-30 * time.Minute))
	snapshot.Nodes = []corev1.Node{broken}

	finding := expectOne(t, evaluate(t, SignalNodeNotReady, snapshot))
	expectDetail(t, finding, "Kubelet stopped posting node status")
	if finding.Fingerprint != "node.notready/"+testNode {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
}

func TestNodeNotReadyStaysQuietOnAHealthyNode(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Nodes = []corev1.Node{node(testNode, "4", "16Gi")}
	expectNone(t, evaluate(t, SignalNodeNotReady, snapshot))
}

// Each pressure kind opens and resolves independently, so each gets its own
// fingerprint.
func TestNodePressureFiresPerCondition(t *testing.T) {
	snapshot := newSnapshot()
	pressured := node(testNode, "4", "16Gi")
	for _, conditionType := range []corev1.NodeConditionType{
		corev1.NodeMemoryPressure, corev1.NodeDiskPressure,
	} {
		pressured.Status.Conditions = append(pressured.Status.Conditions, corev1.NodeCondition{
			Type:               conditionType,
			Status:             corev1.ConditionTrue,
			Reason:             "KubeletHasPressure",
			LastTransitionTime: metav1.NewTime(testNow.Add(-time.Hour)),
		})
	}
	snapshot.Nodes = []corev1.Node{pressured}

	findings := evaluate(t, SignalNodePressure, snapshot)
	if len(findings) != 2 {
		t.Fatalf("expected two findings, got %s", describe(findings))
	}
	if findings[0].Fingerprint == findings[1].Fingerprint {
		t.Fatalf("both pressures share a fingerprint: %q", findings[0].Fingerprint)
	}
}

func TestNodeSaturatedFiresWhenSustained(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.NodeUsage[testNode] = usageOf(testNode, func(i int) float64 {
		if i >= nodeBuckets-4 {
			return 0.97
		}
		return 0.30
	}, flat(0.40))

	finding := expectOne(t, evaluate(t, SignalNodeSaturated, snapshot))
	if finding.Scope.Name != "CPU" {
		t.Fatalf("scope name = %q, want the dimension", finding.Scope.Name)
	}
	expectDetail(t, finding, "latency on every application scheduled here")
}

func TestNodeSaturatedIgnoresABurst(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.NodeUsage[testNode] = usageOf(testNode, func(i int) float64 {
		if i == nodeBuckets-1 {
			return 0.99
		}
		return 0.30
	}, flat(0.40))
	expectNone(t, evaluate(t, SignalNodeSaturated, snapshot))
}

func TestDiskFillingProjectsForward(t *testing.T) {
	snapshot := newSnapshot()
	usage := usageOf(testNode, flat(0.2), flat(0.2))
	// From 60% to 84% across an hour: a straight line through that reaches
	// 100% inside the week the rule is worried about.
	usage.Filesystems = []NodeFilesystem{{
		MountPoint:    "/var/lib/containerd",
		CapacityBytes: 500 << 30,
		Used:          fillingSeries(0.60, 0.02),
	}}
	snapshot.NodeUsage[testNode] = usage

	finding := expectOne(t, evaluate(t, SignalNodeDiskFilling, snapshot))
	expectDetail(t, finding, "projected full in")
	if finding.Scope.Name != "/var/lib/containerd" {
		t.Fatalf("scope name = %q, want the mount point", finding.Scope.Name)
	}
}

// A disk that is not growing has no date, however full it is.
func TestDiskFillingIgnoresAFlatFilesystem(t *testing.T) {
	snapshot := newSnapshot()
	usage := usageOf(testNode, flat(0.2), flat(0.2))
	usage.Filesystems = []NodeFilesystem{{
		MountPoint: "/", CapacityBytes: 500 << 30, Used: fillingSeries(0.70, 0),
	}}
	snapshot.NodeUsage[testNode] = usage
	expectNone(t, evaluate(t, SignalNodeDiskFilling, snapshot))
}

// A straight line through a disk at 4% predicts a date that means nothing.
func TestDiskFillingIgnoresAnEmptyFilesystem(t *testing.T) {
	snapshot := newSnapshot()
	usage := usageOf(testNode, flat(0.2), flat(0.2))
	usage.Filesystems = []NodeFilesystem{{
		MountPoint: "/", CapacityBytes: 500 << 30, Used: fillingSeries(0.04, 0.001),
	}}
	snapshot.NodeUsage[testNode] = usage
	expectNone(t, evaluate(t, SignalNodeDiskFilling, snapshot))
}

// fillingSeries is a filesystem's fill starting at `from` and growing by
// `perBucket` each bucket.
func fillingSeries(from, perBucket float64) []Bucket {
	start := testNow.Add(-nodeBuckets * ResourceBucket)
	buckets := make([]Bucket, nodeBuckets)
	for i := range buckets {
		buckets[i] = Bucket{
			Start:    start.Add(time.Duration(i) * ResourceBucket),
			Value:    from + perBucket*float64(i),
			Observed: true,
		}
	}
	return buckets
}

// A node absent from the freshness answer reported nothing within the
// lookback, which is the whole subject of the rule.
func TestNodeSilentFiresOnAnAbsentNode(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Nodes = []corev1.Node{node(testNode, "4", "16Gi")}

	finding := expectOne(t, evaluate(t, SignalNodeSilent, snapshot))
	expectDetail(t, finding, "pod-security.kubernetes.io/enforce")
	expectDetail(t, finding, "refused at admission")
}

func TestNodeSilentFiresOnAStaleTimestamp(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Nodes = []corev1.Node{node(testNode, "4", "16Gi")}
	snapshot.Freshness[testNode] = testNow.Add(-30 * time.Minute)

	finding := expectOne(t, evaluate(t, SignalNodeSilent, snapshot))
	expectDetail(t, finding, "silent for 30m")
}

func TestNodeSilentStaysQuietOnAReportingNode(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Nodes = []corev1.Node{node(testNode, "4", "16Gi")}
	fresh(snapshot)
	expectNone(t, evaluate(t, SignalNodeSilent, snapshot))
}

func TestOvercommittedFires(t *testing.T) {
	snapshot := newSnapshot()
	// Deliberately asymmetric: the rule removes the *largest* node, which is
	// the worst one to lose, so equal nodes would not prove it does.
	snapshot.Nodes = []corev1.Node{
		node(testNode, "4", "16Gi"),
		node(testOtherNode, "8", "32Gi"),
	}
	// Seven cores requested against the four that remain without the larger
	// node: whatever is running there cannot be rescheduled.
	snapshot.Pods = []corev1.Pod{requestingPod("3500m", "6Gi"), requestingPod("3500m", "6Gi")}

	finding := expectOne(t, evaluate(t, SignalOvercommitted, snapshot))
	expectDetail(t, finding, "the scheduler has nowhere to put what was on it")
	expectDetail(t, finding, "of CPU requested against 4.0 cores")
}

func TestOvercommittedStaysQuietWithHeadroom(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Nodes = []corev1.Node{
		node(testNode, "4", "16Gi"),
		node(testOtherNode, "4", "16Gi"),
	}
	snapshot.Pods = []corev1.Pod{requestingPod("500m", "1Gi")}
	expectNone(t, evaluate(t, SignalOvercommitted, snapshot))
}

// A single-node cluster cannot lose a node and stay a cluster; saying so every
// evaluation would be true and useless.
func TestOvercommittedIgnoresASingleNodeCluster(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Nodes = []corev1.Node{node(testNode, "4", "16Gi")}
	snapshot.Pods = []corev1.Pod{requestingPod("3500m", "12Gi")}
	expectNone(t, evaluate(t, SignalOvercommitted, snapshot))
}

func requestingPod(cpu, memory string) corev1.Pod {
	pod := appPod(testPodName)
	pod.Spec.Containers = []corev1.Container{{
		Name: testContainer,
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		}},
	}}
	return *pod
}

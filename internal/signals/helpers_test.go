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
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The vocabulary every test in this package shares. They are constants because
// each of them appears in a dozen assertions, and a repeated literal is both a
// lint failure here and a rename waiting to go half-done.
const (
	testProject     = "shop"
	testEnvironment = "pr-41"
	testContainer   = "web"
	testNode        = "node-1"
	testOtherNode   = "node-2"
	testClaim       = "data"
	testHost        = "shop-pr-41.example.com"
	testGatewayIP   = "203.0.113.10"
)

// testNow is a fixed instant. Nothing in this package reads the wall clock, so
// every test places its conditions relative to this and no test is flaky at a
// bucket boundary.
var testNow = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

var testKey = EnvKey{Project: testProject, Environment: testEnvironment}

// newSnapshot is a snapshot of a platform with one project and one
// environment, and nothing wrong with it. Each test breaks the one thing it is
// about.
func newSnapshot() *Snapshot {
	return &Snapshot{
		Now:       testNow,
		Traffic:   map[EnvKey]clickhouse.RequestSeries{},
		Resources: map[EnvKey]clickhouse.ResourceSeries{},
		Freshness: map[string]time.Time{},
		NodeUsage: map[string]NodeUsage{},
		Environments: []kitchenv1alpha1.Environment{{
			ObjectMeta: metav1.ObjectMeta{Name: testEnvironment},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: testProject},
			},
			Status: kitchenv1alpha1.EnvironmentStatus{URL: "https://" + testHost},
		}},
	}
}

// appPod is one of an environment's pods, labelled the way the environment
// reconciler labels them.
func appPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: controller.AppNamespace(testProject),
			Labels: map[string]string{
				controller.LabelProject:     testProject,
				controller.LabelEnvironment: testEnvironment,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// testPodName is the one app pod most tests need; appPod takes a name for the
// cases that need two.
const testPodName = "shop-pr-41-abc"

// readyPod is an app pod that is serving, which several rules check for before
// they judge the traffic in front of it.
func readyPod() corev1.Pod {
	pod := appPod(testPodName)
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	return *pod
}

// waitingPod is an app pod whose container is stuck in one waiting reason.
func waitingPod(reason, message string) corev1.Pod {
	pod := appPod(testPodName)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  testContainer,
		Image: "registry.example.com/shop:sha-1234",
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message},
		},
	}}
	return *pod
}

// deployment is a workload of the environment, with the counts a rule reads.
func deployment(desired, available int32) appsv1.Deployment {
	replicas := desired
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              testEnvironment,
			Namespace:         controller.AppNamespace(testProject),
			CreationTimestamp: metav1.NewTime(testNow.Add(-time.Hour)),
			Labels: map[string]string{
				controller.LabelProject:     testProject,
				controller.LabelEnvironment: testEnvironment,
			},
		},
		Spec:   appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{AvailableReplicas: available},
	}
}

// trafficBuckets is how many buckets of the request series one gather covers.
const trafficBuckets = 128

// trafficOf builds a request series ending at testNow, each bucket filled by
// `fill`. Index 0 is the oldest, which is the order every reader answers in.
func trafficOf(fill func(i int) clickhouse.RequestPoint) clickhouse.RequestSeries {
	start := testNow.Add(-trafficBuckets * TrafficBucket)
	series := clickhouse.RequestSeries{
		Start:         start,
		End:           testNow,
		BucketSeconds: int(TrafficBucket / time.Second),
		Points:        make([]clickhouse.RequestPoint, trafficBuckets),
		Rollup:        clickhouse.RequestRollupMinute,
	}
	for i := range series.Points {
		point := fill(i)
		point.Start = start.Add(time.Duration(i) * TrafficBucket)
		series.Points[i] = point
	}
	return series
}

// recentFrom is the first index of a trafficOf series that falls inside
// RecentWindow, found by asking the series rather than by arithmetic — the
// window does not divide the bucket width evenly, and an off-by-one here would
// silently weaken every traffic test by mixing one baseline bucket into the
// recent mean.
var recentFrom = func() int {
	series := trafficOf(func(int) clickhouse.RequestPoint { return clickhouse.RequestPoint{} })
	boundary := testNow.Add(-RecentWindow)
	for i, point := range series.Points {
		if !point.Start.Before(boundary) {
			return i
		}
	}
	return len(series.Points)
}()

// resourceBuckets is how many buckets of the resource series one gather covers.
const resourceBuckets = 12

// resourcesOf builds a resource series ending at testNow.
func resourcesOf(memoryLimit uint64, cpuLimit float64, fill func(i int) clickhouse.ResourcePoint) clickhouse.ResourceSeries {
	start := testNow.Add(-resourceBuckets * ResourceBucket)
	series := clickhouse.ResourceSeries{
		Start:            start,
		End:              testNow,
		BucketSeconds:    int(ResourceBucket / time.Second),
		Points:           make([]clickhouse.ResourcePoint, resourceBuckets),
		MemoryLimitBytes: memoryLimit,
		CPULimitCores:    cpuLimit,
		Rollup:           true,
	}
	for i := range series.Points {
		point := fill(i)
		point.Start = start.Add(time.Duration(i) * ResourceBucket)
		series.Points[i] = point
	}
	return series
}

// nodeBuckets is how many buckets a node usage series carries here.
const nodeBuckets = 12

// usageOf builds one node's saturation series.
func usageOf(node string, cpu, memory func(i int) float64) NodeUsage {
	start := testNow.Add(-nodeBuckets * ResourceBucket)
	usage := NodeUsage{Node: node, BucketWidth: ResourceBucket}
	for i := 0; i < nodeBuckets; i++ {
		at := start.Add(time.Duration(i) * ResourceBucket)
		usage.CPU = append(usage.CPU, Bucket{Start: at, Value: cpu(i), Observed: true})
		usage.Memory = append(usage.Memory, Bucket{Start: at, Value: memory(i), Observed: true})
	}
	return usage
}

// flat is a series generator that never changes, for the dimension a test is
// not about.
func flat(value float64) func(int) float64 {
	return func(int) float64 { return value }
}

// evaluate runs one signal by id over a snapshot, through the registry, so
// that every test exercises the same availability handling the platform does.
func evaluate(t *testing.T, id ID, snapshot *Snapshot) Findings {
	t.Helper()
	signal, ok := Catalogue().Lookup(id)
	if !ok {
		t.Fatalf("no signal %q in the catalogue", id)
	}
	registry, err := NewRegistry(signal)
	if err != nil {
		t.Fatalf("building a one-signal registry: %v", err)
	}
	return registry.Evaluate(snapshot)
}

// expectOne asserts that exactly one finding fired and returns it.
func expectOne(t *testing.T, findings Findings) Finding {
	t.Helper()
	if len(findings) != 1 {
		t.Fatalf("expected one finding, got %d: %s", len(findings), describe(findings))
	}
	return findings[0]
}

// expectNone asserts that a rule stayed quiet.
func expectNone(t *testing.T, findings Findings) {
	t.Helper()
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %d: %s", len(findings), describe(findings))
	}
}

// expectDetail asserts that a finding's detail names something the rule exists
// to say — the suspected cause, usually, which is the whole point of the rules
// that were written for a specific misconfiguration.
func expectDetail(t *testing.T, finding Finding, want string) {
	t.Helper()
	if !strings.Contains(finding.Detail, want) {
		t.Fatalf("detail does not mention %q: %s", want, finding.Detail)
	}
}

func describe(findings Findings) string {
	described := make([]string, 0, len(findings))
	for _, finding := range findings {
		described = append(described, string(finding.Severity)+" "+finding.Fingerprint+" "+finding.Title)
	}
	return strings.Join(described, " | ")
}

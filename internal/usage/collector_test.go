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

package usage

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

var sampledAt = time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)

// The names the fixtures use, spelled once: an assertion that disagrees with
// the pod it is about is the one bug these tests cannot catch.
const (
	appProject     = "shop"
	appEnvironment = "production"
	appNamespace   = "kitchen-app-shop"
	appContainer   = "app"
	appPodName     = "production-abc-1"
	appNode        = "node-1"
)

type podOption func(*corev1.Pod)

func withRestarts(count int32, lastTermination *corev1.ContainerStateTerminated) podOption {
	return func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses[0].RestartCount = count
		if lastTermination != nil {
			pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = lastTermination
		}
	}
}

func withLimits(cpu, memory string) podOption {
	return func(pod *corev1.Pod) {
		pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		}
	}
}

func startedAt(at time.Time) podOption {
	return func(pod *corev1.Pod) {
		pod.Status.StartTime = &metav1.Time{Time: at}
	}
}

func appPod(name string, options ...podOption) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: appNamespace,
			// Derived from the name rather than fixed: the uid is what the
			// collector associates a sample with, so two pods sharing one
			// would hide exactly the mix-up it exists to prevent.
			UID: types.UID("uid-" + name),
			Labels: map[string]string{
				"kitchen.bermos.dev/project":     appProject,
				"kitchen.bermos.dev/environment": appEnvironment,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   appNode,
			Containers: []corev1.Container{{Name: appContainer}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{Name: appContainer}},
		},
	}
	for _, option := range options {
		option(pod)
	}
	return pod
}

func collector(pods ...client.Object) *Collector {
	reader := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pods...).Build()
	return &Collector{Client: reader, Reader: reader}
}

// withPods swaps what the collector can see, keeping what it remembers.
func withPods(from *Collector, pods ...client.Object) *Collector {
	reader := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pods...).Build()
	return &Collector{Client: reader, Reader: reader, seen: from.seen, last: from.last}
}

func byPod(sweep Sweep) map[string]ContainerSample {
	out := map[string]ContainerSample{}
	for _, sample := range sweep.Containers {
		out[sample.Pod] = sample
	}
	return out
}

// The join no scrape of the kubelet can make: a container's sample carries the
// project and environment that own the pod, straight off its labels.
func TestASampleCarriesTheProjectAndEnvironment(t *testing.T) {
	collector := collector(appPod(appPodName, withLimits("500m", "512Mi")))

	sweep, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(sweep.Containers) != 1 {
		t.Fatalf("want one sample, got %d", len(sweep.Containers))
	}
	sample := sweep.Containers[0]
	if sample.Project != appProject || sample.Environment != appEnvironment {
		t.Fatalf("the sample should say whose it is: %+v", sample)
	}
	if sample.CPULimitCores != 0.5 || sample.MemoryLimitBytes != 512*1024*1024 {
		t.Fatalf("the limits should ride along: %+v", sample)
	}
}

// The attribute names are the schema's, not this package's: they are what the
// materialized columns read, and what the node collector stamps on the
// kubelet's own numbers so that both halves land on the same rows.
func TestTheResourceAttributesAreTheOnesTheSchemaReads(t *testing.T) {
	collector := collector(appPod(appPodName))

	sweep, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	attributes := attributesOf(t, sweep, appPodName, appContainer)

	for key, want := range map[string]string{
		"kitchen.project":             appProject,
		"deployment.environment.name": appEnvironment,
		"kitchen.source":              clickhouse.SourceRuntime,
		"k8s.namespace.name":          appNamespace,
		"k8s.pod.name":                appPodName,
		"k8s.container.name":          appContainer,
		"k8s.node.name":               appNode,
	} {
		if attributes[key] != want {
			t.Fatalf("resource attribute %s: want %q, got %q", key, want, attributes[key])
		}
	}
	// A runtime pod has no build, and an empty attribute is not the same as
	// no attribute to anything reading the map.
	if _, ok := attributes["kitchen.build"]; ok {
		t.Fatalf("a runtime sample should carry no build attribute: %+v", attributes)
	}
}

// The limits are gauges and the restarts a counter, because that is what the
// read path asks for by name.
func TestTheMetricsAreShapedTheWayTheReadPathExpects(t *testing.T) {
	collector := collector(appPod(appPodName,
		withLimits("500m", "512Mi"), startedAt(sampledAt.Add(-time.Hour)), withRestarts(3, nil)))

	sweep, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	batch := batchFor(t, sweep, appPodName, appContainer)

	restarts, ok := metricOf(t, batch, metricRestarts).Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s should be a sum", metricRestarts)
	}
	if !restarts.IsMonotonic || restarts.Temporality != metricdata.CumulativeTemporality {
		t.Fatalf("%s is a lifetime counter: %+v", metricRestarts, restarts)
	}
	if restarts.DataPoints[0].Value != 3 {
		t.Fatalf("want the counter as the API server reports it, got %d", restarts.DataPoints[0].Value)
	}
	// A cumulative point counts from the pod's start, not from the operator's.
	if !restarts.DataPoints[0].StartTime.Equal(sampledAt.Add(-time.Hour)) {
		t.Fatalf("want the pod's start time, got %v", restarts.DataPoints[0].StartTime)
	}

	delta, ok := metricOf(t, batch, metricRestartsDelta).Data.(metricdata.Sum[int64])
	if !ok || delta.Temporality != metricdata.DeltaTemporality {
		t.Fatalf("%s is a delta: %+v", metricRestartsDelta, metricOf(t, batch, metricRestartsDelta).Data)
	}

	cpu, ok := metricOf(t, batch, metricCPULimit).Data.(metricdata.Gauge[float64])
	if !ok || cpu.DataPoints[0].Value != 0.5 {
		t.Fatalf("%s should be half a core: %+v", metricCPULimit, metricOf(t, batch, metricCPULimit).Data)
	}
	memory, ok := metricOf(t, batch, metricMemoryLimit).Data.(metricdata.Gauge[int64])
	if !ok || memory.DataPoints[0].Value != 512*1024*1024 {
		t.Fatalf("%s should be the limit in bytes: %+v", metricMemoryLimit, metricOf(t, batch, metricMemoryLimit).Data)
	}
}

// A pod nothing on the node can say anything about — crash-looping, waiting on
// an image — is still a replica the environment is running, and is usually the
// reason someone is on the page.
func TestAPodThatIsNotRunningIsStillAReplica(t *testing.T) {
	collector := collector(appPod(appPodName), appPod("production-abc-2"))

	sweep, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(sweep.Environments) != 1 {
		t.Fatalf("want one environment, got %+v", sweep.Environments)
	}
	if sweep.Environments[0].Pods != 2 {
		t.Fatalf("want both pods counted, got %d", sweep.Environments[0].Pods)
	}

	replicas := batchFor(t, sweep, "", "")
	gauge, ok := metricOf(t, replicas, metricReplicas).Data.(metricdata.Gauge[int64])
	if !ok || gauge.DataPoints[0].Value != 2 {
		t.Fatalf("%s should be the pod count: %+v", metricReplicas, metricOf(t, replicas, metricReplicas).Data)
	}
	// The replica count belongs to the environment, so its resource names no
	// pod or container to be attributed to.
	attributes := attributesOf(t, sweep, "", "")
	if attributes["kitchen.project"] != appProject || attributes["deployment.environment.name"] != appEnvironment {
		t.Fatalf("the replica count should say whose it is: %+v", attributes)
	}
}

// The whole reason the sampler does the differencing: a lifetime counter
// bucketed for a chart loses every transition that lands on a boundary, so
// what is exported beside it is the change since the last sweep.
func TestARestartIsAnEventNotACounter(t *testing.T) {
	collector := collector(appPod(appPodName, withRestarts(3, nil)))

	// The first sweep has nothing to compare against, so it attributes
	// nothing: three restarts that happened before the collector was looking
	// are not three restarts in this window.
	first, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if first.Containers[0].Restarts != 3 || first.Containers[0].Restarted != 0 {
		t.Fatalf("the baseline should record the counter and no event: %+v", first.Containers[0])
	}

	// Nothing has changed, so nothing happened.
	collector = withPods(collector, appPod(appPodName, withRestarts(3, nil)))
	second, err := collector.Sample(context.Background(), sampledAt.Add(30*time.Second))
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if second.Containers[0].Restarted != 0 {
		t.Fatalf("an unchanged counter is not a restart: %+v", second.Containers[0])
	}

	// Two restarts between one sweep and the next are two events, not one: a
	// crash loop must not read as a single restart because the collector only
	// looked twice.
	collector = withPods(collector, appPod(appPodName, withRestarts(5, nil)))
	third, err := collector.Sample(context.Background(), sampledAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if third.Containers[0].Restarted != 2 {
		t.Fatalf("want the two restarts since the last sweep, got %d", third.Containers[0].Restarted)
	}

	batch := batchFor(t, third, appPodName, appContainer)
	delta, _ := metricOf(t, batch, metricRestartsDelta).Data.(metricdata.Sum[int64])
	if delta.DataPoints[0].Value != 2 {
		t.Fatalf("the delta metric should carry the change, got %d", delta.DataPoints[0].Value)
	}
	// A delta point says which window it covers, and the window is the gap
	// between the two sweeps rather than anything the exporter invents.
	if !delta.DataPoints[0].StartTime.Equal(sampledAt.Add(30 * time.Second)) {
		t.Fatalf("want the previous sweep as the window's start, got %v", delta.DataPoints[0].StartTime)
	}
	if !delta.DataPoints[0].Time.Equal(sampledAt.Add(time.Minute)) {
		t.Fatalf("want this sweep as the window's end, got %v", delta.DataPoints[0].Time)
	}
}

// The first sweep's delta window is empty, so it cannot claim anything
// happened in it.
func TestTheFirstSweepCoversNoWindow(t *testing.T) {
	collector := collector(appPod(appPodName, withRestarts(3, nil)))

	sweep, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if !sweep.Since.Equal(sweep.At) {
		t.Fatalf("want an empty window, got %v to %v", sweep.Since, sweep.At)
	}
}

// A container that OOMed once keeps that last state until it terminates again,
// so the reason alone would mark every later restart an OOM kill.
func TestOnlyAFreshOOMKillIsRecorded(t *testing.T) {
	recent := &corev1.ContainerStateTerminated{
		Reason:     oomKilledReason,
		FinishedAt: metav1.NewTime(sampledAt.Add(-time.Minute)),
	}
	ancient := &corev1.ContainerStateTerminated{
		Reason:     oomKilledReason,
		FinishedAt: metav1.NewTime(sampledAt.Add(-72 * time.Hour)),
	}

	for name, test := range map[string]struct {
		termination *corev1.ContainerStateTerminated
		want        bool
	}{
		"the kill that caused this restart":     {recent, true},
		"a kill from days ago, restarted since": {ancient, false},
		"no termination at all":                 {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			collector := collector(appPod(appPodName, withRestarts(1, test.termination)))
			// A baseline so that the sweep sees the restart.
			collector.seen = map[ContainerKey]uint32{
				{Namespace: appNamespace, Pod: appPodName, Container: appContainer}: 0,
			}
			collector.last = sampledAt.Add(-30 * time.Second)

			sweep, err := collector.Sample(context.Background(), sampledAt)
			if err != nil {
				t.Fatalf("Sample: %v", err)
			}
			if sweep.Containers[0].Restarted != 1 {
				t.Fatalf("the restart itself should have been noticed: %+v", sweep.Containers[0])
			}
			if sweep.Containers[0].OOMKilled != test.want {
				t.Fatalf("want OOMKilled=%v, got %+v", test.want, sweep.Containers[0])
			}

			// Summing the metric over a window has to count kills, not
			// sampling intervals, so it is 1 for the sweep that noticed one
			// and 0 for every other.
			batch := batchFor(t, sweep, appPodName, appContainer)
			killed, _ := metricOf(t, batch, metricOOMKilled).Data.(metricdata.Sum[int64])
			want := int64(0)
			if test.want {
				want = 1
			}
			if killed.DataPoints[0].Value != want {
				t.Fatalf("want %s=%d, got %d", metricOOMKilled, want, killed.DataPoints[0].Value)
			}
		})
	}
}

// A container that has gone away takes its entry with it, so the map does not
// grow with every preview the platform has ever run.
func TestTheRestartBaselineForgetsWhatIsGone(t *testing.T) {
	collector := collector(
		appPod(appPodName),
		appPod("preview-pr-7-xyz-1"),
	)
	if _, err := collector.Sample(context.Background(), sampledAt); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(collector.seen) != 2 {
		t.Fatalf("want both containers remembered, got %d", len(collector.seen))
	}

	collector = withPods(collector, appPod(appPodName))
	if _, err := collector.Sample(context.Background(), sampledAt.Add(time.Minute)); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(collector.seen) != 1 {
		t.Fatalf("the removed preview should have been forgotten, got %d entries", len(collector.seen))
	}
}

// Pods without the environment label are the cluster's, not the platform's:
// sampling them would fill the store with series no page can attribute — and
// the node collector is already shipping the kubelet's numbers for them.
func TestOnlyApplicationPodsAreSampled(t *testing.T) {
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns-1", Namespace: "kube-system"},
		Spec:       corev1.PodSpec{NodeName: appNode, Containers: []corev1.Container{{Name: "coredns"}}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "coredns"}}},
	}
	collector := collector(appPod(appPodName), other)

	sweep, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(sweep.Containers) != 1 || sweep.Containers[0].Pod != appPodName {
		t.Fatalf("want only the application pod, got %+v", sweep.Containers)
	}
	if len(byPod(sweep)) != 1 {
		t.Fatalf("want one pod's worth of samples, got %+v", sweep.Containers)
	}
}

// attributesOf reads one batch's resource attributes back as a map, which is
// how the schema reads them too.
func attributesOf(t *testing.T, sweep Sweep, pod, container string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, attribute := range batchFor(t, sweep, pod, container).Resource.Attributes() {
		out[string(attribute.Key)] = attribute.Value.String()
	}
	return out
}

// batchFor is the batch describing one pod's container, or — for the empty
// pod and container — the one describing an environment's replica count.
func batchFor(t *testing.T, sweep Sweep, pod, container string) *metricdata.ResourceMetrics {
	t.Helper()
	for _, batch := range sweep.ResourceMetrics() {
		found := map[string]string{}
		for _, attribute := range batch.Resource.Attributes() {
			found[string(attribute.Key)] = attribute.Value.String()
		}
		if found[attrPod] == pod && found[attrContainer] == container {
			return batch
		}
	}
	t.Fatalf("no batch for pod %q container %q", pod, container)
	return nil
}

// metricOf is one named metric out of a batch.
func metricOf(t *testing.T, batch *metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, scope := range batch.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == name {
				return metric
			}
		}
	}
	t.Fatalf("the batch carries no %s", name)
	return metricdata.Metrics{}
}

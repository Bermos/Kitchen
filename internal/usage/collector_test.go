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
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

var sampledAt = time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)

// stubSampler answers for the nodes it was given and fails for any other, so a
// test can be explicit about which kubelets are reachable.
type stubSampler struct {
	usage map[string]map[ContainerKey]Usage
	asked []string
}

func (s *stubSampler) NodeUsage(_ context.Context, node string) (map[ContainerKey]Usage, error) {
	s.asked = append(s.asked, node)
	if answer, ok := s.usage[node]; ok {
		return answer, nil
	}
	return nil, errors.New("kubelet unreachable")
}

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

func appPod(name, node string, options ...podOption) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kitchen-app-shop",
			Labels: map[string]string{
				"kitchen.bermos.dev/project":     "shop",
				"kitchen.bermos.dev/environment": "production",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app"}},
		},
	}
	for _, option := range options {
		option(pod)
	}
	return pod
}

func collector(sampler Sampler, pods ...client.Object) *Collector {
	reader := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pods...).Build()
	return &Collector{Client: reader, Reader: reader, Sampler: sampler}
}

func byPod(samples []clickhouse.ResourceSample) map[string]clickhouse.ResourceSample {
	out := map[string]clickhouse.ResourceSample{}
	for _, sample := range samples {
		out[sample.Pod] = sample
	}
	return out
}

// The join a metrics scrape cannot make: a container's usage carries the
// project and environment that own the pod, straight off its labels.
func TestASampleCarriesTheProjectAndEnvironment(t *testing.T) {
	sampler := &stubSampler{usage: map[string]map[ContainerKey]Usage{
		"node-1": {
			{Namespace: "kitchen-app-shop", Pod: "production-abc-1", Container: "app"}: {
				CPUCores: 0.25, MemoryBytes: 200_000_000,
			},
		},
	}}
	collector := collector(sampler, appPod("production-abc-1", "node-1", withLimits("500m", "512Mi")))

	samples, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("want one sample, got %d", len(samples))
	}
	sample := samples[0]
	if sample.Project != "shop" || sample.Environment != "production" {
		t.Fatalf("the sample should say whose it is: %+v", sample)
	}
	if sample.CPUCores != 0.25 || sample.MemoryBytes != 200_000_000 {
		t.Fatalf("the kubelet's numbers did not land: %+v", sample)
	}
	if sample.CPULimitCores != 0.5 || sample.MemoryLimitBytes != 512*1024*1024 {
		t.Fatalf("the limits should ride along: %+v", sample)
	}
}

// A crash-looping container has no usage to report, and leaving it out would
// make the replica count — how many pods reported — say zero for an
// environment that exists and is the reason someone is on the page.
func TestAPodWithNoUsageIsStillSampled(t *testing.T) {
	collector := collector(&stubSampler{}, appPod("production-abc-1", "node-1"))

	samples, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("a pod the kubelet said nothing about is still a replica, got %d samples", len(samples))
	}
	if samples[0].CPUCores != 0 || samples[0].MemoryBytes != 0 {
		t.Fatalf("unknown usage is zero, not invented: %+v", samples[0])
	}
}

// One sick node must not blank the whole platform's series.
func TestAnUnreachableKubeletDoesNotFailTheSweep(t *testing.T) {
	sampler := &stubSampler{usage: map[string]map[ContainerKey]Usage{
		"node-1": {
			{Namespace: "kitchen-app-shop", Pod: "production-abc-1", Container: "app"}: {CPUCores: 0.25},
		},
	}}
	collector := collector(sampler,
		appPod("production-abc-1", "node-1"),
		appPod("production-abc-2", "node-2"), // node-2 refuses
	)

	samples, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("both pods should be sampled, got %d", len(samples))
	}
	got := byPod(samples)
	if got["production-abc-1"].CPUCores != 0.25 {
		t.Fatalf("the healthy node's numbers should survive: %+v", got["production-abc-1"])
	}
	if got["production-abc-2"].CPUCores != 0 {
		t.Fatalf("the sick node contributes no usage, not wrong usage: %+v", got["production-abc-2"])
	}
}

// Each kubelet is asked once however many pods it runs, and never for a node
// that runs nothing of interest.
func TestEachNodeIsAskedOnce(t *testing.T) {
	sampler := &stubSampler{}
	collector := collector(sampler,
		appPod("production-abc-1", "node-1"),
		appPod("production-abc-2", "node-1"),
		appPod("production-abc-3", "node-2"),
	)

	if _, err := collector.Sample(context.Background(), sampledAt); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(sampler.asked) != 2 {
		t.Fatalf("want one call per node, got %v", sampler.asked)
	}
}

// The whole reason the collector does the differencing: a lifetime counter
// bucketed in the store loses every transition that lands on a boundary, so
// what is written is the change since the last sample.
func TestARestartIsAnEventNotACounter(t *testing.T) {
	collector := collector(&stubSampler{}, appPod("production-abc-1", "node-1", withRestarts(3, nil)))

	// The first sample has nothing to compare against, so it attributes
	// nothing: three restarts that happened before the collector was looking
	// are not three restarts in this bucket.
	first, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if first[0].Restarts != 3 || first[0].Restarted != 0 {
		t.Fatalf("the baseline should record the counter and no event: %+v", first[0])
	}

	// Nothing has changed, so nothing happened.
	second, err := collector.Sample(context.Background(), sampledAt.Add(30*time.Second))
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if second[0].Restarted != 0 {
		t.Fatalf("an unchanged counter is not a restart: %+v", second[0])
	}

	// Two restarts between one sample and the next are two events, not one:
	// a crash loop must not read as a single restart because the collector
	// only looked twice.
	collector = withPods(collector, appPod("production-abc-1", "node-1", withRestarts(5, nil)))
	third, err := collector.Sample(context.Background(), sampledAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if third[0].Restarted != 2 {
		t.Fatalf("want the two restarts since the last sample, got %d", third[0].Restarted)
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
			collector := collector(&stubSampler{},
				appPod("production-abc-1", "node-1", withRestarts(1, test.termination)))
			// A baseline so that the second sample sees the restart.
			collector.seen = map[ContainerKey]uint32{
				{Namespace: "kitchen-app-shop", Pod: "production-abc-1", Container: "app"}: 0,
			}

			samples, err := collector.Sample(context.Background(), sampledAt)
			if err != nil {
				t.Fatalf("Sample: %v", err)
			}
			if samples[0].Restarted != 1 {
				t.Fatalf("the restart itself should have been noticed: %+v", samples[0])
			}
			if samples[0].OOMKilled != test.want {
				t.Fatalf("want OOMKilled=%v, got %+v", test.want, samples[0])
			}
		})
	}
}

// A container that has gone away takes its entry with it, so the map does not
// grow with every preview the platform has ever run.
func TestTheRestartBaselineForgetsWhatIsGone(t *testing.T) {
	collector := collector(&stubSampler{},
		appPod("production-abc-1", "node-1"),
		appPod("preview-pr-7-xyz-1", "node-1"),
	)
	if _, err := collector.Sample(context.Background(), sampledAt); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(collector.seen) != 2 {
		t.Fatalf("want both containers remembered, got %d", len(collector.seen))
	}

	collector = withPods(collector, appPod("production-abc-1", "node-1"))
	if _, err := collector.Sample(context.Background(), sampledAt.Add(time.Minute)); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(collector.seen) != 1 {
		t.Fatalf("the removed preview should have been forgotten, got %d entries", len(collector.seen))
	}
}

// Pods without the environment label are the cluster's, not the platform's:
// sampling them would fill the store with rows no page can attribute.
func TestOnlyApplicationPodsAreSampled(t *testing.T) {
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns-1", Namespace: "kube-system"},
		Spec:       corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "coredns"}}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "coredns"}}},
	}
	collector := collector(&stubSampler{}, appPod("production-abc-1", "node-1"), other)

	samples, err := collector.Sample(context.Background(), sampledAt)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(samples) != 1 || samples[0].Pod != "production-abc-1" {
		t.Fatalf("want only the application pod, got %+v", samples)
	}
}

// withPods swaps what the collector can see, keeping what it remembers.
func withPods(from *Collector, pods ...client.Object) *Collector {
	reader := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pods...).Build()
	return &Collector{Client: reader, Reader: reader, Sampler: from.Sampler, seen: from.seen}
}

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

package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The fixtures every node test reads: one node, one disk, and the window the
// signals gatherer asks for — an hour at the rollup's resolution.
const (
	testNode       = "node-1"
	testMountPoint = "/"
	testDevice     = "/dev/vda"
	testNodeWindow = time.Hour
)

// nodeAnswers routes the two statements NodeUsage sends to their own rows. The
// filesystem one is the one that names the filesystem metric.
func nodeAnswers(saturation, filesystems string) func(string) string {
	return func(query string) string {
		if strings.Contains(query, quoteLiteral(MetricHostFilesystemUsage)) {
			return filesystems
		}
		return saturation
	}
}

// nodeWindow is the window and bucket the gatherer asks for, which is what
// these tests read against.
func nodeWindow() (time.Time, NodeUsageQuery) {
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	return start, NodeUsageQuery{
		Since:  start,
		Until:  start.Add(testNodeWindow),
		Bucket: 5 * time.Minute,
	}
}

// The busy fraction is what is not idle. There is no utilisation metric to read
// — the scrapers ship counters and leave the arithmetic to whoever asks — so
// this is the arithmetic, and a bucket that could not carry it out is a bucket
// nobody observed rather than an idle node.
func TestNodeUsageComputesTheBusyFractionOutOfTheCPUTime(t *testing.T) {
	store := newFakeLogStore(t)
	start, query := nodeWindow()
	store.answer = nodeAnswers(strings.Join([]string{
		// 90% busy, 91% of memory used.
		`{"node":"` + testNode + `","bucket":"` + stamp(start) + `","cpuTotal":"200","cpuIdle":"20",` +
			`"memoryTotal":"1000","memoryUsed":"910"}`,
		// A bucket with a single sample in it: no delta to divide, so nothing
		// was measured.
		`{"node":"` + testNode + `","bucket":"` + stamp(start.Add(10*time.Minute)) + `","cpuTotal":"0",` +
			`"cpuIdle":"0","memoryTotal":"0","memoryUsed":"0"}`,
		// Entirely idle, which is an observation.
		`{"node":"` + testNode + `","bucket":"` + stamp(start.Add(15*time.Minute)) + `","cpuTotal":"100",` +
			`"cpuIdle":"100","memoryTotal":"1000","memoryUsed":"100"}`,
		// And a bucket outside the window, which is not this window's.
		`{"node":"` + testNode + `","bucket":"` + stamp(start.Add(4*time.Hour)) + `","cpuTotal":"100",` +
			`"cpuIdle":"0","memoryTotal":"1000","memoryUsed":"1000"}`,
	}, "\n"), "")

	usage, err := store.client(t).NodeUsage(context.Background(), query)
	if err != nil {
		t.Fatalf("NodeUsage: %v", err)
	}
	if len(usage) != 1 {
		t.Fatalf("want one node, got %d", len(usage))
	}
	node := usage[0]
	if node.Node != testNode || node.BucketSeconds != 300 {
		t.Fatalf("unexpected node: %+v", node)
	}
	// Every bucket of the window is present, so that a gap reads as a gap.
	if len(node.CPU) != 13 || len(node.Memory) != 13 {
		t.Fatalf("want thirteen buckets, got %d cpu and %d memory", len(node.CPU), len(node.Memory))
	}
	if node.CPU[0].Value != 0.9 || !node.CPU[0].Observed {
		t.Errorf("the first bucket should be 90%% busy: %+v", node.CPU[0])
	}
	if node.Memory[0].Value != 0.91 || !node.Memory[0].Observed {
		t.Errorf("the first bucket should be 91%% of memory: %+v", node.Memory[0])
	}
	if node.CPU[1].Observed || node.CPU[2].Observed {
		t.Errorf("a bucket nothing reported is not an idle node: %+v, %+v", node.CPU[1], node.CPU[2])
	}
	if node.CPU[3].Value != 0 || !node.CPU[3].Observed {
		t.Errorf("an idle node is an observation of nothing, not an absence: %+v", node.CPU[3])
	}
	if !node.CPU[3].Start.Equal(start.Add(15 * time.Minute)) {
		t.Errorf("the buckets should be stamped from the window's start, got %s", node.CPU[3].Start)
	}
	for i, bucket := range node.CPU[4:] {
		if bucket.Observed {
			t.Errorf("bucket %d is past everything that reported: %+v", i+4, bucket)
		}
	}
}

// The metrics asked for by name are the ones the scrapers actually emit. The
// three obvious ones are disabled by default upstream, and a reader that asked
// for them would find nothing and call every node healthy forever.
func TestNodeUsageAsksForTheMetricsTheScrapersEmitByDefault(t *testing.T) {
	store := newFakeLogStore(t)
	_, query := nodeWindow()

	if _, err := store.client(t).NodeUsage(context.Background(), query); err != nil {
		t.Fatalf("NodeUsage: %v", err)
	}
	for _, metric := range []string{MetricHostCPUTime, MetricHostMemoryUsage, MetricHostFilesystemUsage} {
		if !store.sawQuery(quoteLiteral(metric)) {
			t.Errorf("the read never asks for %s:\n%s", metric, store.transcript())
		}
	}
	if store.sawQuery("utilization") {
		t.Errorf("the utilisation metrics are off by default and are never written:\n%s", store.transcript())
	}
	// Host metrics are cumulative sums, and none of them reach the container
	// rollup: its two views filter on the container metric names.
	if !store.sawQuery(qualified(MetricsSumTable)) {
		t.Errorf("the host metrics live in the sum table:\n%s", store.transcript())
	}
	for _, table := range []string{MetricsGaugeTable, MetricsRollupTable} {
		if store.sawQuery(qualified(table)) {
			t.Errorf("%s holds nothing a node scraper wrote:\n%s", table, store.transcript())
		}
	}
	// The CPU counter is cumulative seconds since boot, so a bucket's work is
	// the difference across it and never the value in it.
	if !store.sawQuery("max(h.Value) - min(h.Value)") {
		t.Errorf("a cumulative counter is read as a delta:\n%s", store.transcript())
	}
	// `slot` is deliberately not `bucket`: a GROUP BY resolves against the
	// SELECT aliases first, and the outer bucket is a rendered string.
	if store.sawQuery("GROUP BY node, bucket") {
		t.Errorf("grouping by the rendered string would bucket by text:\n%s", store.transcript())
	}
	// A row that names no node says nothing about one.
	if !store.sawQuery("h.node != ''") || !store.sawQuery("f.node != ''") {
		t.Errorf("both reads should skip the rows with no node:\n%s", store.transcript())
	}
}

// A node's filesystems are one series each, and the capacity they are read
// against is what a writer can use — used plus free, which leaves out the root
// reservation nothing can write into.
func TestNodeUsageKeepsASeriesPerFilesystem(t *testing.T) {
	store := newFakeLogStore(t)
	start, query := nodeWindow()
	store.answer = nodeAnswers("", strings.Join([]string{
		`{"node":"` + testNode + `","bucket":"` + stamp(start) + `","mountpoint":"` + testMountPoint +
			`","device":"` + testDevice + `","used":"800","free":"200"}`,
		`{"node":"` + testNode + `","bucket":"` + stamp(start.Add(5*time.Minute)) + `","mountpoint":"` +
			testMountPoint + `","device":"` + testDevice + `","used":"900","free":"100"}`,
		// A second mount on the same node, and one the store has nothing else
		// about — a node that reported only its disks still gets its series.
		`{"node":"` + testNode + `","bucket":"` + stamp(start) + `","mountpoint":"/var","device":"/dev/vdb",` +
			`"used":"100","free":"900"}`,
	}, "\n"))

	usage, err := store.client(t).NodeUsage(context.Background(), query)
	if err != nil {
		t.Fatalf("NodeUsage: %v", err)
	}
	if len(usage) != 1 {
		t.Fatalf("want one node, got %d", len(usage))
	}
	node := usage[0]
	if len(node.Filesystems) != 2 {
		t.Fatalf("want two filesystems, got %d", len(node.Filesystems))
	}
	root := node.Filesystems[0]
	if root.MountPoint != testMountPoint || root.Device != testDevice {
		t.Errorf("a filesystem is named by what it is mounted as: %+v", root)
	}
	if root.Used[0].Value != 0.8 || root.Used[1].Value != 0.9 {
		t.Errorf("the fill should be used over used-plus-free: %+v", root.Used[:2])
	}
	// The capacity comes off the newest bucket that reported, so a volume grown
	// mid-window is read against the size it has now.
	if root.CapacityBytes != 1000 {
		t.Errorf("capacity should exclude the reserved bytes, got %d", root.CapacityBytes)
	}
	if node.Filesystems[1].MountPoint != "/var" || node.Filesystems[1].Used[0].Value != 0.1 {
		t.Errorf("the second mount is its own series: %+v", node.Filesystems[1])
	}
	// The node reported no saturation at all, which is a node whose series are
	// present and empty rather than a node that is missing.
	if len(node.CPU) != 13 {
		t.Fatalf("want thirteen cpu buckets, got %d", len(node.CPU))
	}
	for i, bucket := range node.CPU {
		if bucket.Observed {
			t.Fatalf("cpu bucket %d should be unobserved: %+v", i, bucket)
		}
	}
}

// Every node comes back in name order, so that two evaluations of an unchanged
// cluster produce the same round.
func TestNodeUsageAnswersInNodeOrder(t *testing.T) {
	store := newFakeLogStore(t)
	start, query := nodeWindow()
	store.answer = nodeAnswers(strings.Join([]string{
		`{"node":"node-2","bucket":"` + stamp(start) + `","cpuTotal":"100","cpuIdle":"50",` +
			`"memoryTotal":"100","memoryUsed":"50"}`,
		`{"node":"` + testNode + `","bucket":"` + stamp(start) + `","cpuTotal":"100","cpuIdle":"10",` +
			`"memoryTotal":"100","memoryUsed":"10"}`,
	}, "\n"), "")

	usage, err := store.client(t).NodeUsage(context.Background(), query)
	if err != nil {
		t.Fatalf("NodeUsage: %v", err)
	}
	if len(usage) != 2 {
		t.Fatalf("want two nodes, got %d", len(usage))
	}
	if usage[0].Node != testNode || usage[1].Node != "node-2" {
		t.Errorf("the nodes should be in name order, got %s then %s", usage[0].Node, usage[1].Node)
	}
}

// The fractions are bounded by construction as long as the samples are. A
// source that contradicts itself renders as a full node, not as a node at
// 200%.
func TestNodeUsageHoldsAContradictorySourceInsideTheScale(t *testing.T) {
	store := newFakeLogStore(t)
	start, query := nodeWindow()
	store.answer = nodeAnswers(
		`{"node":"`+testNode+`","bucket":"`+stamp(start)+`","cpuTotal":"100","cpuIdle":"-100",`+
			`"memoryTotal":"100","memoryUsed":"-10"}`, "")

	usage, err := store.client(t).NodeUsage(context.Background(), query)
	if err != nil {
		t.Fatalf("NodeUsage: %v", err)
	}
	if usage[0].CPU[0].Value != 1 {
		t.Errorf("cpu should be held at 100%%, got %v", usage[0].CPU[0].Value)
	}
	if usage[0].Memory[0].Value != 0 {
		t.Errorf("memory should be held at 0, got %v", usage[0].Memory[0].Value)
	}
}

// The bucket width is rounded up to a rung of the same ladder an environment's
// series uses, so a node and the workloads on it are read at the same
// resolution rather than at two.
func TestNodeUsageQuantisesTheBucketToTheLadder(t *testing.T) {
	store := newFakeLogStore(t)
	_, query := nodeWindow()

	for _, width := range []struct {
		asked time.Duration
		want  string
	}{
		{asked: 0, want: "300"},
		{asked: 45 * time.Second, want: "60"},
		{asked: 5 * time.Minute, want: "300"},
		{asked: 400 * time.Hour, want: "86400"},
	} {
		query.Bucket = width.asked
		if _, err := store.client(t).NodeUsage(context.Background(), query); err != nil {
			t.Fatalf("NodeUsage: %v", err)
		}
		if got := store.params.Get("param_width"); got != width.want {
			t.Errorf("a %s bucket should be answered at %ss, got %ss", width.asked, width.want, got)
		}
	}
}

func TestANodeWindowMustEndAfterItStarts(t *testing.T) {
	store := newFakeLogStore(t)
	start, _ := nodeWindow()

	_, err := store.client(t).NodeUsage(context.Background(), NodeUsageQuery{
		Since: start,
		Until: start.Add(-time.Hour),
	})
	if err == nil {
		t.Fatal("a backwards window should be refused before it reaches the store")
	}
	if len(store.queries) != 0 {
		t.Errorf("expected nothing to reach the store, got:\n%s", store.transcript())
	}
}

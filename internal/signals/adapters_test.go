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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// fakeStoreReader stands in for the telemetry store. It records the query it
// was handed — the adapters' other job is asking for the right window — and can
// fail, which is the path that separates "could not be read" from "nothing to
// report".
type fakeStoreReader struct {
	nodes   []clickhouse.NodeUsage
	volumes []clickhouse.VolumeUsage
	err     error

	nodeQuery   clickhouse.NodeUsageQuery
	volumeQuery clickhouse.VolumeUsageQuery
}

func (f *fakeStoreReader) NodeUsage(
	_ context.Context, query clickhouse.NodeUsageQuery,
) ([]clickhouse.NodeUsage, error) {
	f.nodeQuery = query
	return f.nodes, f.err
}

func (f *fakeStoreReader) VolumeUsage(
	_ context.Context, query clickhouse.VolumeUsageQuery,
) ([]clickhouse.VolumeUsage, error) {
	f.volumeQuery = query
	return f.volumes, f.err
}

// errStoreDown is what a store that cannot be reached answers with.
var errStoreDown = errors.New("clickhouse: connection refused")

// storeBuckets builds one of the store's series over the window the gatherer
// asks for, at the resolution it asks for.
func storeBuckets(value func(i int) float64, observed bool) []clickhouse.NodeUsagePoint {
	start := testNow.Add(-ResourceWindow)
	points := make([]clickhouse.NodeUsagePoint, storeBucketCount)
	for i := range points {
		points[i] = clickhouse.NodeUsagePoint{
			Start:    start.Add(time.Duration(i) * ResourceBucket),
			Value:    value(i),
			Observed: observed,
		}
	}
	return points
}

// storeBucketCount is how many buckets an hour at the rollup's width holds,
// both boundaries included — the shape [Gather] asks the store for.
const storeBucketCount = int(ResourceWindow/ResourceBucket) + 1

// storeCapacityBytes is the size of the disk in these fixtures.
const storeCapacityBytes = 100 << 30

func TestStoreHostMetricsProjectsWhatTheStoreAnswered(t *testing.T) {
	reader := &fakeStoreReader{nodes: []clickhouse.NodeUsage{{
		Node:          testNode,
		BucketSeconds: int(ResourceBucket / time.Second),
		CPU:           storeBuckets(flat(0.95), true),
		Memory:        storeBuckets(flat(0.20), false),
		Filesystems: []clickhouse.NodeFilesystemUsage{{
			MountPoint:    "/var/lib/containerd",
			Device:        "/dev/vda",
			CapacityBytes: storeCapacityBytes,
			Used:          storeBuckets(flat(0.80), true),
		}},
	}}}

	usage, err := StoreHostMetrics(reader).NodeUsage(context.Background(),
		testNow.Add(-ResourceWindow), testNow, ResourceBucket)
	if err != nil {
		t.Fatalf("NodeUsage: %v", err)
	}
	// The window the gatherer wants is the window the store is asked for.
	if !reader.nodeQuery.Until.Equal(testNow) || reader.nodeQuery.Bucket != ResourceBucket {
		t.Fatalf("unexpected query: %+v", reader.nodeQuery)
	}

	if len(usage) != 1 {
		t.Fatalf("want one node, got %d", len(usage))
	}
	node := usage[0]
	if node.Node != testNode || node.BucketWidth != ResourceBucket {
		t.Fatalf("the width answered at is what turns a run into a duration: %+v", node)
	}
	if len(node.CPU) != storeBucketCount || node.CPU[0].Value != 0.95 || !node.CPU[0].Observed {
		t.Errorf("the CPU series did not survive the projection: %+v", node.CPU[:1])
	}
	// Observed is carried rather than derived: a bucket the collector missed
	// and a bucket in which nothing happened are different facts, and only the
	// reader can tell them apart.
	if node.Memory[0].Observed {
		t.Errorf("an unobserved bucket should stay unobserved: %+v", node.Memory[0])
	}
	if len(node.Filesystems) != 1 {
		t.Fatalf("want one filesystem, got %d", len(node.Filesystems))
	}
	filesystem := node.Filesystems[0]
	if filesystem.MountPoint != "/var/lib/containerd" || filesystem.Device != "/dev/vda" {
		t.Errorf("a filesystem is named by where it is mounted: %+v", filesystem)
	}
	if filesystem.CapacityBytes != storeCapacityBytes || len(filesystem.Used) != storeBucketCount {
		t.Errorf("the filesystem lost its size or its series: %+v", filesystem)
	}
}

func TestStoreVolumeUsageProjectsWhatTheStoreAnswered(t *testing.T) {
	reader := &fakeStoreReader{volumes: []clickhouse.VolumeUsage{{
		Namespace:     "kitchen-app-shop",
		Claim:         testClaim,
		Project:       testProject,
		Environment:   testEnvironment,
		Pod:           testPodName,
		CapacityBytes: storeCapacityBytes,
		UsedBytes:     storeCapacityBytes / 10 * 9,
		UsedFraction:  0.9,
	}}}

	volumes, err := StoreVolumeUsage(reader).VolumeUsage(context.Background(), testNow)
	if err != nil {
		t.Fatalf("VolumeUsage: %v", err)
	}
	if !reader.volumeQuery.At.Equal(testNow) {
		t.Fatalf("the answer should be about the snapshot's instant, got %s", reader.volumeQuery.At)
	}
	if len(volumes) != 1 {
		t.Fatalf("want one claim, got %d", len(volumes))
	}
	volume := volumes[0]
	if volume.Claim != testClaim || volume.Project != testProject {
		t.Errorf("a claim carries what it belongs to: %+v", volume)
	}
	// The fraction is carried rather than recomputed, because the store may
	// know it more precisely than the two byte counts do.
	if volume.UsedFraction != 0.9 || volume.CapacityBytes != storeCapacityBytes {
		t.Errorf("the fill did not survive the projection: %+v", volume)
	}
}

// A store that cannot be reached is not a platform with nothing wrong with it.
// The error has to reach the gatherer, which is what marks the input unreadable
// and makes the round say so.
func TestTheAdaptersReportAFailedRead(t *testing.T) {
	reader := &fakeStoreReader{err: errStoreDown}

	if _, err := StoreHostMetrics(reader).NodeUsage(context.Background(),
		testNow.Add(-ResourceWindow), testNow, ResourceBucket); !errors.Is(err, errStoreDown) {
		t.Errorf("NodeUsage should have reported the store's error, got %v", err)
	}
	if _, err := StoreVolumeUsage(reader).VolumeUsage(context.Background(), testNow); !errors.Is(err, errStoreDown) {
		t.Errorf("VolumeUsage should have reported the store's error, got %v", err)
	}
}

// The three rules §7 leaves dark without a source: with one, a gather over the
// store's own shapes has to reach them. This is the whole change in one test —
// store rows in, findings out, through [Gather] rather than around it.
func TestTheStoreSourcesLightTheThreeDarkSignals(t *testing.T) {
	filling := func(i int) float64 {
		// Six tenths to nine over the hour, which projects full inside the week
		// the rule is worried about.
		return 0.60 + 0.30*float64(i)/float64(storeBucketCount-1)
	}
	reader := &fakeStoreReader{
		nodes: []clickhouse.NodeUsage{{
			Node:          testNode,
			BucketSeconds: int(ResourceBucket / time.Second),
			CPU:           storeBuckets(flat(0.95), true),
			Memory:        storeBuckets(flat(0.20), true),
			Filesystems: []clickhouse.NodeFilesystemUsage{{
				MountPoint:    "/",
				Device:        "/dev/vda",
				CapacityBytes: storeCapacityBytes,
				Used:          storeBuckets(filling, true),
			}},
		}},
		volumes: []clickhouse.VolumeUsage{{
			Namespace:     "kitchen-app-shop",
			Claim:         testClaim,
			Project:       testProject,
			Environment:   testEnvironment,
			CapacityBytes: storeCapacityBytes,
			UsedBytes:     storeCapacityBytes / 100 * 92,
			UsedFraction:  0.92,
		}},
	}

	snapshot := Gather(context.Background(), Sources{
		HostMetrics: StoreHostMetrics(reader),
		VolumeUsage: StoreVolumeUsage(reader),
		Now:         func() time.Time { return testNow },
	}, Options{})

	if !snapshot.Available(InputHostMetrics) || !snapshot.Available(InputVolumeStats) {
		t.Fatalf("both inputs should have been read: %+v", snapshot.Unreadable())
	}
	if _, ok := snapshot.NodeUsage[testNode]; !ok {
		t.Fatalf("the node's usage should be keyed by its name, got %+v", snapshot.NodeUsage)
	}

	saturated := expectOne(t, evaluate(t, SignalNodeSaturated, snapshot))
	if saturated.Scope.Node != testNode || saturated.Scope.Name != "CPU" {
		t.Errorf("unexpected scope: %+v", saturated.Scope)
	}
	filled := expectOne(t, evaluate(t, SignalNodeDiskFilling, snapshot))
	if filled.Scope.Name != "/" {
		t.Errorf("the filesystem is named by its mount point: %+v", filled.Scope)
	}
	claim := expectOne(t, evaluate(t, SignalPVCFilling, snapshot))
	if claim.Scope.Project != testProject || claim.Scope.Name != testClaim {
		t.Errorf("unexpected scope: %+v", claim.Scope)
	}
}

// And without them, the same gather says nothing about the same three — which
// is the state this change moved away from, and the one an installation with no
// store is still in.
func TestTheThreeSignalsStayDarkWithoutASource(t *testing.T) {
	snapshot := Gather(context.Background(), Sources{Now: func() time.Time { return testNow }}, Options{})

	if snapshot.Available(InputHostMetrics) || snapshot.Available(InputVolumeStats) {
		t.Fatal("an absent source is not a read")
	}
	for _, id := range []ID{SignalNodeSaturated, SignalNodeDiskFilling, SignalPVCFilling} {
		expectNone(t, evaluate(t, id, snapshot))
	}
}

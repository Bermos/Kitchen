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
	"time"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The two optional sources of [Sources], satisfied from the telemetry store.
//
// They are adapters rather than methods on the store for the reason the whole
// seam exists: this package depends on the shape of the reads, not on the
// store's implementation, and the store must not depend on this package at all
// — it is imported by it. So the store returns its own row types, and the
// mapping into the snapshot's lives here, where the rules that read it do.
//
// The mapping is deliberately dull. Everything with a judgement in it — what
// counts as an observation, what a fraction is measured against, which sample
// wins — has already been decided in the reader, which is the only place that
// can see the samples.

// NodeUsageReader and VolumeUsageReader are the store reads behind
// [HostMetricsSource] and [VolumeUsageSource]. They are interfaces so the
// adapters can be tested against a fake, including one that fails: an
// unreadable source and an empty one are different answers, and both paths are
// exercised.
type NodeUsageReader interface {
	NodeUsage(ctx context.Context, query clickhouse.NodeUsageQuery) ([]clickhouse.NodeUsage, error)
}

type VolumeUsageReader interface {
	VolumeUsage(ctx context.Context, query clickhouse.VolumeUsageQuery) ([]clickhouse.VolumeUsage, error)
}

// StoreHostMetrics adapts a telemetry store to [HostMetricsSource], which is
// what makes node.saturated and node.disk-filling fire.
//
// Pass it a store that exists. An installation without one leaves
// [Sources.HostMetrics] nil, which the gatherer marks not-applicable — an
// adapter wrapped around nothing would be a source that fails every read, and
// "the store is broken" is not the same sentence as "there is no store".
func StoreHostMetrics(reader NodeUsageReader) HostMetricsSource {
	return hostMetrics{reader: reader}
}

// StoreVolumeUsage adapts a telemetry store to [VolumeUsageSource], which is
// what makes pvc.filling fire. Same caveat about nil as [StoreHostMetrics].
func StoreVolumeUsage(reader VolumeUsageReader) VolumeUsageSource {
	return volumeUsage{reader: reader}
}

type hostMetrics struct{ reader NodeUsageReader }

// NodeUsage asks the store for the window the gatherer wants and projects the
// answer into the snapshot's shape.
func (h hostMetrics) NodeUsage(
	ctx context.Context, since, until time.Time, bucket time.Duration,
) ([]NodeUsage, error) {
	rows, err := h.reader.NodeUsage(ctx, clickhouse.NodeUsageQuery{
		Since:  since,
		Until:  until,
		Bucket: bucket,
	})
	if err != nil {
		return nil, err
	}

	usage := make([]NodeUsage, 0, len(rows))
	for _, row := range rows {
		node := NodeUsage{
			Node:   row.Node,
			CPU:    usageBuckets(row.CPU),
			Memory: usageBuckets(row.Memory),
			// The width the store answered at, which is not always the one that
			// was asked for: the reader rounds it up to a rung of its ladder,
			// and a run of buckets is only a duration when the width is the
			// real one.
			BucketWidth: time.Duration(row.BucketSeconds) * time.Second,
			Filesystems: make([]NodeFilesystem, 0, len(row.Filesystems)),
		}
		for _, filesystem := range row.Filesystems {
			node.Filesystems = append(node.Filesystems, NodeFilesystem{
				MountPoint:    filesystem.MountPoint,
				Device:        filesystem.Device,
				CapacityBytes: filesystem.CapacityBytes,
				Used:          usageBuckets(filesystem.Used),
			})
		}
		usage = append(usage, node)
	}
	return usage, nil
}

type volumeUsage struct{ reader VolumeUsageReader }

// VolumeUsage asks the store how full each claim is as of the snapshot's
// instant.
func (v volumeUsage) VolumeUsage(ctx context.Context, at time.Time) ([]VolumeUsage, error) {
	rows, err := v.reader.VolumeUsage(ctx, clickhouse.VolumeUsageQuery{At: at})
	if err != nil {
		return nil, err
	}

	volumes := make([]VolumeUsage, 0, len(rows))
	for _, row := range rows {
		volumes = append(volumes, VolumeUsage{
			Namespace:     row.Namespace,
			Claim:         row.Claim,
			Project:       row.Project,
			Environment:   row.Environment,
			CapacityBytes: row.CapacityBytes,
			UsedBytes:     row.UsedBytes,
			UsedFraction:  row.UsedFraction,
		})
	}
	return volumes, nil
}

// usageBuckets projects a node series into [Bucket], which is what the sustained
// and trailing-run machinery in window.go reads.
//
// It carries Observed across rather than deriving one, which is the same
// judgement series.go makes about the other two shapes: whether a bucket was an
// observation is a fact about the source, and the reader is where the samples
// were counted.
func usageBuckets(points []clickhouse.NodeUsagePoint) []Bucket {
	buckets := make([]Bucket, 0, len(points))
	for _, point := range points {
		buckets = append(buckets, Bucket{
			Start:    point.Start,
			Value:    point.Value,
			Observed: point.Observed,
		})
	}
	return buckets
}

// The store satisfies both reads, and the adapters satisfy both sources. A
// signature that moves breaks the build here rather than at whichever call site
// happened to wire it.
var (
	_ NodeUsageReader   = (*clickhouse.Client)(nil)
	_ VolumeUsageReader = (*clickhouse.Client)(nil)
	_ HostMetricsSource = hostMetrics{}
	_ VolumeUsageSource = volumeUsage{}
)

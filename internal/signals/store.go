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

// What the gatherer asks the telemetry store for.
//
// These are interfaces rather than a *clickhouse.Client so that a test can
// gather a snapshot from a fake — including a fake that fails, which is the
// only way to exercise the degradation path — and so that this package depends
// on the shape of the reads rather than on the store's implementation. The
// assertion below keeps the shapes honest: if a reader's signature moves, this
// fails to compile rather than silently ceasing to be satisfiable.

// Store is the set of reads that exist today and that the catalogue depends
// on. *clickhouse.Client satisfies it.
type Store interface {
	// RequestSeries is the golden signals bucketed, per environment. The
	// gatherer asks for the recent window and the baseline in one read.
	RequestSeries(ctx context.Context, query clickhouse.RequestSeriesQuery) (clickhouse.RequestSeries, error)
	// ResourceSeries is CPU and memory against their limits, plus restarts and
	// OOM kills, per environment, from metrics_5m.
	ResourceSeries(ctx context.Context, query clickhouse.ResourceSeriesQuery) (clickhouse.ResourceSeries, error)
	// ProjectTraffic is per-project traffic for one window, which the
	// cross-project detectors compare between two windows.
	ProjectTraffic(ctx context.Context, query clickhouse.ProjectTrafficQuery) ([]clickhouse.ProjectTraffic, error)
	// UnroutedHosts is the edge's bucket of hosts nobody published.
	UnroutedHosts(ctx context.Context, query clickhouse.PlatformRequestsQuery) ([]clickhouse.UnroutedHost, error)
	// QueryK8sEvents is the cluster's Warning history.
	QueryK8sEvents(ctx context.Context, query clickhouse.K8sEventQuery) ([]clickhouse.K8sEvent, error)
	// TelemetryFreshness is when each node's collector last shipped anything.
	// A node absent from the answer reported nothing within the lookback,
	// which is node.silent's whole subject.
	TelemetryFreshness(ctx context.Context, within time.Duration) ([]clickhouse.NodeFreshness, error)
	// StoreStats is the store's own size and ingest rate. It is deliberately
	// this read rather than the dashboard's overview: the two numbers store.disk
	// and store.ingest-stalled need are all the gatherer wants, and every
	// evaluation of the catalogue pays for whatever it asks for here.
	StoreStats(ctx context.Context) (clickhouse.StoreStats, error)
}

// HostMetricsSource reads node saturation and filesystem fill out of the
// host_metrics the collector already ships.
//
// It is a separate, optional interface because an installation can run without
// a store at all: node.saturated and node.disk-filling are computed from
// nothing else, so a nil source has the gatherer mark [InputHostMetrics]
// not-applicable and those two rules stay quiet rather than claiming the nodes
// are fine.
//
// The shape is a window and a bucket width in, per-node utilisation fractions
// and per-filesystem fill out — and it was the finished shape before anything
// satisfied it, which is why the rules needed no change when something did.
// [StoreHostMetrics] satisfies it from the telemetry store.
type HostMetricsSource interface {
	NodeUsage(ctx context.Context, since, until time.Time, bucket time.Duration) ([]NodeUsage, error)
}

// VolumeUsageSource reads how full each mounted claim is, from the kubelet's
// volume metric group.
//
// Same situation as [HostMetricsSource], and the same reason it stays optional:
// pvc.filling is computed from this and nothing else, so without a source it
// says nothing — because "no claim is over 85%" and "nobody looked" must not
// render the same. [StoreVolumeUsage] satisfies it from the telemetry store.
type VolumeUsageSource interface {
	VolumeUsage(ctx context.Context, at time.Time) ([]VolumeUsage, error)
}

// IngestAccounting reports what the flow follower lost.
//
// The counts live in the follower's own memory — Relay reports LostEvent
// notices in-stream, and the follower is the only thing that sees them — so
// this is satisfied by the follower rather than by the store. ingest.flows-lost
// stays quiet until something does.
type IngestAccounting interface {
	IngestHealth(ctx context.Context) (IngestHealth, error)
}

// Resolver is how dns.mismatch resolves a name. It is an interface so that the
// rule's tests need no network and the gatherer's tests can make the resolver
// itself misbehave, which is the case that must not look like broken DNS.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// The store package satisfies the reads the catalogue needs today. A signature
// that moves breaks the build here rather than at the one call site that
// happened to notice.
var _ Store = (*clickhouse.Client)(nil)

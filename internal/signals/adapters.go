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
	"net"
	"strings"
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

// SystemResolver is the standard library's resolver with a deadline per
// lookup, which is what both callers of the catalogue hand [Sources.Resolver].
//
// The deadline is what makes name resolution safe to do from a request handler
// and from a loop alike: dns.mismatch probes a handful of published names every
// evaluation, and a resolver that is itself unreachable would otherwise hold
// each one open for the resolv.conf timeout multiplied by the probe limit.
//
// It lives here, beside the interface it satisfies, because the API and the
// background evaluation loop must probe DNS the same way. Two copies of it
// would be two rounds that can disagree about whether a name resolves, which is
// exactly the disagreement between "the screen says" and "the history recorded"
// that this package exists to prevent.
//
// The distinction the rule rests on survives the deadline: a lookup that timed
// out comes back as a *net.DNSError whose IsNotFound is false, which the
// gatherer reads as an input it could not read — and an error carrying no
// DNSError at all fails that test the same way. Only a name the resolver
// positively says does not exist becomes a finding.
func SystemResolver(timeout time.Duration) Resolver {
	return boundedResolver{resolver: net.DefaultResolver, timeout: timeout}
}

type boundedResolver struct {
	resolver *net.Resolver
	timeout  time.Duration
}

func (r boundedResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.resolver.LookupHost(ctx, host)
}

// The signal history's two mappings: a round's transitions as rows, and rows
// as transitions.
//
// They are here, beside the store adapters, for the same reason those are: the
// store returns its own row types and knows nothing about severities,
// audiences or scope kinds, and this is the one place that translates. Both
// callers of the history — the loop that writes it and the API that reads it —
// go through them, so a column can never mean one thing on the way in and
// another on the way out.

// TransitionRows is a round's transitions in the shape the store writes.
func TransitionRows(transitions []Transition) []clickhouse.SignalTransition {
	rows := make([]clickhouse.SignalTransition, 0, len(transitions))
	for _, transition := range transitions {
		rows = append(rows, clickhouse.SignalTransition{
			At:          transition.At,
			State:       string(transition.State),
			Signal:      string(transition.Signal),
			Fingerprint: transition.Fingerprint,
			Audience:    string(transition.Audience),
			Tier:        string(transition.Tier),
			Version:     transition.Version,
			Severity:    string(transition.Severity),
			Scope:       string(transition.Scope.Kind),
			Project:     transition.Scope.Project,
			Environment: transition.Scope.Environment,
			Namespace:   transition.Scope.Namespace,
			Node:        transition.Scope.Node,
			Name:        transition.Scope.Name,
			Title:       transition.Title,
			Detail:      transition.Detail,
			Evidence:    transition.Evidence,
			Confidence:  string(transition.Confidence),
			Projects:    strings.Join(transition.Projects, listSeparator),
			Correlates:  joinIDs(transition.Correlates),
			Policy:      transition.Policy,
			Since:       transition.Since,
			OpenedAt:    transition.OpenedAt,
		})
	}
	return rows
}

// TransitionsFrom is recorded rows read back as what they were about.
func TransitionsFrom(rows []clickhouse.SignalTransition) []Transition {
	transitions := make([]Transition, 0, len(rows))
	for _, row := range rows {
		transitions = append(transitions, Transition{
			At:          row.At,
			State:       TransitionState(row.State),
			Signal:      ID(row.Signal),
			Fingerprint: row.Fingerprint,
			Audience:    Audience(row.Audience),
			Tier:        Tier(row.Tier),
			Version:     row.Version,
			Severity:    Severity(row.Severity),
			Scope: Scope{
				Kind:        ScopeKind(row.Scope),
				Project:     row.Project,
				Environment: row.Environment,
				Namespace:   row.Namespace,
				Node:        row.Node,
				Name:        row.Name,
			},
			Title:      row.Title,
			Detail:     row.Detail,
			Evidence:   row.Evidence,
			Confidence: Confidence(row.Confidence),
			Projects:   splitList(row.Projects),
			Correlates: splitIDs(row.Correlates),
			Policy:     row.Policy,
			Since:      row.Since,
			OpenedAt:   row.OpenedAt,
		})
	}
	return transitions
}

// listSeparator joins the two list-shaped columns a correlation carries. The
// transitions table is flat — one column per field, no nesting — so a list is
// a joined string, and a comma is safe here because both lists hold Kubernetes
// names and signal ids, neither of which may contain one.
const listSeparator = ","

func joinIDs(ids []ID) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, string(id))
	}
	return strings.Join(parts, listSeparator)
}

func splitList(joined string) []string {
	if joined == "" {
		return nil
	}
	return strings.Split(joined, listSeparator)
}

func splitIDs(joined string) []ID {
	parts := splitList(joined)
	if parts == nil {
		return nil
	}
	ids := make([]ID, 0, len(parts))
	for _, part := range parts {
		ids = append(ids, ID(part))
	}
	return ids
}

// The mitigation records' two mappings, for the reason the transitions' are
// here: the store returns rows and knows nothing about what an acknowledgement
// does to a tier, and this is the one place that translates.

// MitigationRow is one record in the shape the store writes.
func MitigationRow(mitigation Mitigation) clickhouse.SignalMitigation {
	return clickhouse.SignalMitigation{
		At:          mitigation.At,
		Kind:        string(mitigation.Kind),
		Fingerprint: mitigation.Fingerprint,
		Audience:    string(mitigation.Audience),
		Project:     mitigation.Project,
		Actor:       mitigation.Actor,
		Reason:      mitigation.Reason,
		Until:       mitigation.Until,
		Source:      mitigation.Source,
	}
}

// MitigationsFrom is recorded rows read back as what they were about.
func MitigationsFrom(rows []clickhouse.SignalMitigation) []Mitigation {
	mitigations := make([]Mitigation, 0, len(rows))
	for _, row := range rows {
		mitigations = append(mitigations, Mitigation{
			At:          row.At,
			Kind:        MitigationKind(row.Kind),
			Fingerprint: row.Fingerprint,
			Audience:    Audience(row.Audience),
			Project:     row.Project,
			Actor:       row.Actor,
			Reason:      row.Reason,
			Until:       row.Until,
			Source:      row.Source,
		})
	}
	return mitigations
}

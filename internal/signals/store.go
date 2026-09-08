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
	// QueryAuditRecords is what people did to the platform, and is read for
	// one purpose: the privileged records are the fourth leg of the
	// correlation ladder's timeline. See gatherAuditChanges.
	QueryAuditRecords(ctx context.Context, query clickhouse.AuditQuery) ([]clickhouse.AuditRecord, error)
	// OpenSignalTransitions is the history's answer to "when did this
	// platform first see each of these", which is the correlation ladder's
	// clock. It is a read of the catalogue's *own* output, which is unusual
	// enough to be worth saying: the ladder is the one rule whose subject is
	// the history, because "did these begin together" is a question no
	// snapshot of the estate can answer. See [Snapshot.OpenedAt].
	//
	// It is the expensive way to get that answer and the fallback rather than
	// the path — see [StartsSource].
	OpenSignalTransitions(ctx context.Context) ([]clickhouse.SignalTransition, error)
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

// StartsSource is where the correlation ladder's clock comes from: when this
// platform first saw each condition that is currently open.
//
// It is an interface with two implementations for one reason, and it is a cost
// reason. The background loop already holds every open condition and its
// opening instant in memory — that is what [Tracker] *is* — so asking it costs
// nothing, and asking the store instead would add an unbounded `GROUP BY` over
// the whole transitions table to every round, for ever, to learn something the
// process had already. [StoreStarts] is the answer for a caller with no
// tracker: the API's evaluate-on-request path, which runs only when the loop is
// off or behind, and pays the query once per screen rather than once a minute.
type StartsSource interface {
	SignalStarts(ctx context.Context) (map[string]time.Time, error)
}

// SignalStarts makes a [Tracker] a [StartsSource]. It reads process memory, so
// it never fails and never queries anything.
// A nil tracker answers "nothing known" rather than panicking: it reaches this
// as a typed nil through the [StartsSource] interface, where a nil check at the
// call site cannot see it, and a round is not worth an operator restart.
func (t *Tracker) SignalStarts(context.Context) (map[string]time.Time, error) {
	if t == nil {
		return nil, nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	starts := make(map[string]time.Time, len(t.open))
	for key, episode := range t.open {
		if episode.openedAt.IsZero() {
			continue
		}
		// One condition is up to two deliveries and they open together; the
		// earliest is the condition's own start either way.
		if was, seen := starts[key.Fingerprint]; !seen || episode.openedAt.Before(was) {
			starts[key.Fingerprint] = episode.openedAt.UTC()
		}
	}
	return starts, nil
}

// CurrentReading is one open condition as the platform last saw it: the
// finding a round evaluated, and when that round read it.
//
// It is not what the history holds. A transition is written once, at the
// instant the condition opened, and it is the record of that moment on
// purpose; this is the answer to the other question, which is what the
// condition says *now*.
type CurrentReading struct {
	// Finding is the condition as the most recent round that could evaluate
	// it found it.
	Finding Finding
	// At is when that round was evaluated. It is per reading rather than one
	// instant for the whole set, because a rule whose input went unreadable
	// is carried forward with the last reading anybody took of it — which is
	// older than the round, and saying otherwise would be dating a number
	// that has not moved.
	At time.Time
}

// CurrentSource is a read of the conditions a running detection loop holds
// open, as of its most recent round.
//
// It exists for one screen and one problem. The alerts list reads the durable
// `open` transitions, whose title and detail are the ones recorded when the
// condition fired; for every rule whose whole content is a moving number that
// is the least alarming value the condition ever had, shown indefinitely and
// beside an age that belongs to the condition rather than to the reading. The
// loop already holds the current answer in memory — [Tracker.Observe]
// refreshes the episode every round — so this is a map lookup rather than a
// second evaluation or a write per round per open finding.
//
// It answers what a process *knows*, so an empty answer is not an error and
// must not be read as one: a replica that is not the leader runs no loop, and
// a leader that has just restarted has seeded itself from the history and not
// yet evaluated. Both mean "no reading of my own", and the caller shows the
// opening's text and says so.
type CurrentSource interface {
	CurrentReadings(ctx context.Context) (map[TransitionKey]CurrentReading, error)
}

// CurrentReadings makes a [Tracker] a [CurrentSource]. Like [SignalStarts] it
// reads process memory, so it never fails and never queries anything, and a
// nil tracker answers "nothing known" rather than panicking — it reaches this
// as a typed nil through the interface, where a nil check at the call site
// cannot see it.
//
// An episode this process has not evaluated is left out. [Tracker.Restore]
// seeds the open set from the history, and what those episodes carry is the
// opening's own text: returning it here would answer "this is the current
// reading" with the very row the caller is trying to get away from.
func (t *Tracker) CurrentReadings(context.Context) (map[TransitionKey]CurrentReading, error) {
	if t == nil {
		return nil, nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	readings := make(map[TransitionKey]CurrentReading, len(t.open))
	for key, episode := range t.open {
		if episode.readAt.IsZero() {
			continue
		}
		readings[key] = CurrentReading{Finding: episode.finding, At: episode.readAt.UTC()}
	}
	return readings, nil
}

// StoreStarts is the same answer read back out of the history, for a caller
// that holds no tracker.
func StoreStarts(store Store) StartsSource {
	if store == nil {
		return nil
	}
	return storeStarts{store: store}
}

type storeStarts struct {
	store Store
}

func (s storeStarts) SignalStarts(ctx context.Context) (map[string]time.Time, error) {
	open, err := s.store.OpenSignalTransitions(ctx)
	if err != nil {
		return nil, err
	}
	starts := make(map[string]time.Time, len(open))
	for _, row := range open {
		opened := row.OpenedAt
		if opened.IsZero() {
			opened = row.At
		}
		if opened.IsZero() {
			continue
		}
		if was, seen := starts[row.Fingerprint]; !seen || opened.Before(was) {
			starts[row.Fingerprint] = opened.UTC()
		}
	}
	return starts, nil
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

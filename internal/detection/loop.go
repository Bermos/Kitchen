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

// Package detection evaluates the signal catalogue on a timer and records what
// changed.
//
// It is the detection half of stage 5 of docs/OBSERVABILITY.md, and it is
// deliberately the only new caller of anything: [signals.Registry.Evaluate] is
// unchanged and still pure, [signals.Gather] is the same gather the API does,
// and the diffing is [signals.Tracker]'s. What this package adds is the loop
// around them and the write at the end of it.
//
// # Why a round has to be recorded at all
//
// A finding evaluated for a screen is thrown away with the response, which
// makes three things unrepresentable: how long a condition has been true (the
// finding's `since` is the *object's* age, not when the platform first saw it),
// anything a person did about it, and any correlation across evaluations. All
// three are questions about history, and history is the one thing an evaluator
// that runs when somebody is looking cannot have.
//
// # Where it runs
//
// In the operator's process, as a leader-elected Runnable, beside the retention
// sweep and the event recorder. Two replicas each recording that the same
// condition opened would produce a history that reads as two conditions, which
// is the same small lie every other sweep here declares NeedLeaderElection to
// avoid.
package detection

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/notify"
	"github.com/Bermos/Kitchen/internal/signals"
)

const (
	// idleInterval is how often a loop with nothing to do re-reads the
	// Kitchen object: waiting for a telemetry store to appear, or for
	// somebody to turn detection back on. It is not the evaluation interval
	// — an installation that has just been given a store should start
	// recording within half a minute rather than within an hour.
	idleInterval = 30 * time.Second

	// dnsLookupTimeout bounds one name resolution, and is the API's own
	// number for the same probe. See [signals.SystemResolver].
	dnsLookupTimeout = 2 * time.Second
)

// Store is everything one round needs of the telemetry store: every read the
// catalogue makes, the two optional readers behind the node and volume rules,
// and the two calls the history is written and read back through.
//
// It is one interface rather than a *clickhouse.Client so that a test can run
// a round against a fake — and so that the integration test can run one
// against a real server without the loop knowing the difference.
type Store interface {
	signals.Store
	signals.NodeUsageReader
	signals.VolumeUsageReader

	// OpenSignalTransitions is on signals.Store already — the correlation
	// ladder reads the history as an input — and this loop wants it for a
	// different reason: seeding the tracker so a restart does not re-announce
	// everything the last leader recorded.
	InsertSignalTransitions(ctx context.Context, transitions []clickhouse.SignalTransition) error
}

// The store satisfies it. A signature that moves breaks the build here rather
// than at the one call site that happened to notice.
var _ Store = (*clickhouse.Client)(nil)

// Loop evaluates the catalogue on an interval and records the transitions.
type Loop struct {
	// Client reads the Kitchen singleton, the store's credential, and — this
	// is the expensive one — every object the catalogue reads.
	//
	// It is the manager's *cached* client, which is the opposite of what the
	// API does with the same gather and is the right answer for the opposite
	// reason. The API's readers are open dashboards: a warm informer over
	// every pod in the cluster is a permanent cost for a question only
	// somebody looking asks. This reader asks every minute forever, which is
	// exactly what a cache is for — see [signals.Sources.Client].
	Client client.Client

	// Ingest is the flow follower's loss ledger, the one input to the
	// catalogue that comes from neither the API server nor the store. Nil
	// leaves ingest.flows-lost unevaluated rather than quiet, which is the
	// gatherer's business and not this loop's.
	Ingest signals.IngestAccounting

	// Now is the clock. Nil is time.Now; tests move it so that a round can be
	// dated without waiting for one.
	Now func() time.Time

	// Interval overrides the configured evaluation interval. Tests alone set
	// it — an installation sets spec.observability.signals.intervalSeconds.
	Interval time.Duration

	// Resolver overrides how dns.mismatch resolves a name, so a test needs no
	// network. Nil is the bounded system resolver the API also uses.
	Resolver signals.Resolver

	// Notifier is where a recorded transition goes out, for the
	// subscriptions that asked for it. Nil notifies nothing, which is what a
	// caller wired without one gets — and what the tests use, since a
	// delivery object is the notification path's business rather than this
	// loop's.
	//
	// It is fed from here rather than from the activity feed on purpose. The
	// feed is prose for a person catching up, and a condition that opens and
	// resolves forty times while a node flaps would fill it with forty lines
	// nobody reads. The history is the right stream for this, and it is
	// written one row per change — which is what makes "once per transition"
	// true by construction rather than by de-duplication.
	Notifier *notify.Notifier

	// store resolves the telemetry store. It is a field for the reason the
	// API's logStore is one: a test must be able to run a round against a
	// fake without a ClickHouse to point at.
	store func(ctx context.Context) (Store, error)

	// tracker is the previous round, in memory. Nil means this process has
	// not evaluated yet and must seed itself from the store before it can
	// tell a condition that just opened from one the last leader already
	// recorded.
	tracker *signals.Tracker
}

// NeedLeaderElection makes the loop a singleton across replicas.
func (l *Loop) NeedLeaderElection() bool { return true }

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

// Start implements manager.Runnable. Like every other sweep in this operator
// it never returns an error before the context ends: detection is an
// observability capability, and a store that is down must not take the
// operator with it.
func (l *Loop) Start(ctx context.Context) error {
	for {
		round, err := l.RoundOnce(ctx)
		switch {
		case err != nil:
			logf.FromContext(ctx).V(1).Info("a signal evaluation round did not complete",
				"reason", err.Error())
		case round.Recorded > 0:
			logf.FromContext(ctx).V(1).Info("recorded signal transitions",
				"transitions", round.Recorded, "open", round.Open)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(l.wait(round, err)):
		}
	}
}

// wait is how long until the next attempt: the configured interval after a
// round that evaluated, and the idle interval after one that could not.
func (l *Loop) wait(round Round, err error) time.Duration {
	if err != nil || !round.Evaluated {
		return idleInterval
	}
	if l.Interval > 0 {
		return l.Interval
	}
	return round.Interval
}

// Round is what one pass did, which is what a test asserts on.
type Round struct {
	// Evaluated is whether the catalogue ran at all. False is a loop that is
	// switched off or has no store to record into, and Message says which.
	Evaluated bool

	// Off is the first of those two: switched off rather than unable. It is
	// the one case where the last round is not carried forward on the status,
	// because a loop nobody has turned on has no last round to report.
	Off bool

	// Findings is how many conditions the round found, Open how many
	// deliveries are open after it, and Recorded how many transitions it
	// wrote.
	Findings int
	Open     int
	Recorded int

	// Transitions is what it wrote, for a test that wants to read it rather
	// than the store.
	Transitions []signals.Transition

	// Interval is the interval this round was evaluated on.
	Interval time.Duration

	// Message explains a round that evaluated nothing.
	Message string
}

// RoundOnce evaluates the catalogue once and records what changed.
//
// It is exported because it is the unit of the loop: the tests drive it
// directly rather than waiting on a timer.
func (l *Loop) RoundOnce(ctx context.Context) (Round, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := l.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		return Round{}, err
	}

	spec := kitchen.Spec.Observability.Signals
	round := Round{Interval: spec.Interval()}
	if l.Interval > 0 {
		round.Interval = l.Interval
	}
	if !spec.SignalsEnabled() {
		// Published with no instant against it, and that is the point: a
		// status that went on reporting the last round of a loop somebody
		// switched off would be the platform claiming to be watching. It is
		// also what makes the screens fall back to evaluating on request
		// within one round rather than serving a history nothing is
		// maintaining.
		round.Off = true
		round.Message = "background evaluation is switched off"
		l.publish(ctx, round, nil)
		return round, nil
	}

	store, err := l.resolveStore(ctx, kitchen)
	if err != nil {
		round.Message = err.Error()
		l.publish(ctx, round, nil)
		return round, nil
	}

	// Seeding before the first round is what stops a restart, or a replica
	// that has just won the lease, re-announcing every condition the previous
	// leader already recorded. A seed that fails is a round that does not
	// happen: opening everything again would be worse than recording nothing.
	if l.tracker == nil {
		open, err := store.OpenSignalTransitions(ctx)
		// A table that does not exist yet is not a failed seed: it is an
		// installation whose telemetry schema the Kitchen reconcile has not
		// created yet, and there is nothing open in a history nobody has
		// written. Anything else is a store that might be holding open
		// conditions this round cannot see, and going on would re-announce
		// every one of them.
		if err != nil && !clickhouse.IsUnknownTable(err) {
			return round, fmt.Errorf("cannot read back what is already open: %w", err)
		}
		tracker := signals.NewTracker(signals.Catalogue())
		tracker.Restore(signals.TransitionsFrom(open))
		l.tracker = tracker
	}

	snapshot := signals.Gather(ctx, l.sources(store), signals.Options{})
	findings := signals.Catalogue().Evaluate(snapshot)
	transitions := l.tracker.Observe(findings, snapshot.Now)

	rows := signals.TransitionRows(transitions)
	if err := store.InsertSignalTransitions(ctx, rows); err != nil {
		// The tracker has already moved on, so these transitions would
		// otherwise be lost between one round and the next — the condition
		// would be open in memory and absent from the history forever.
		// Dropping the tracker makes the next round seed from the store
		// again, which is the state the store is actually in.
		l.tracker = nil
		return round, fmt.Errorf("the transitions could not be recorded: %w", err)
	}

	// Only once the row is in the history, so that a receiver told about a
	// condition can always find it — and never before, because a
	// notification about a transition the store refused would be the
	// platform saying something it has no record of.
	l.notify(ctx, rows)

	round.Evaluated = true
	round.Findings = len(findings.Firing())
	round.Open = l.tracker.Open()
	round.Recorded = len(transitions)
	round.Transitions = transitions
	l.publish(ctx, round, snapshot)
	return round, nil
}

// notify hands each recorded transition to the subscriptions that asked for
// it. Best-effort and quiet, like every other outbound path here: the round's
// job was to record what changed, and it has.
func (l *Loop) notify(ctx context.Context, rows []clickhouse.SignalTransition) {
	if l.Notifier == nil {
		return
	}
	for _, row := range rows {
		if _, err := l.Notifier.QueueSignal(ctx, row); err != nil {
			logf.FromContext(ctx).V(1).Info("a signal notification was not queued",
				"fingerprint", row.Fingerprint, "audience", row.Audience, "reason", err.Error())
		}
	}
}

// sources is where a round's snapshot comes from. It is the API's own wiring,
// with one difference: the client is cached. See [Loop.Client].
func (l *Loop) sources(store Store) signals.Sources {
	sources := signals.Sources{
		Client:      l.Client,
		Store:       store,
		HostMetrics: signals.StoreHostMetrics(store),
		VolumeUsage: signals.StoreVolumeUsage(store),
		Resolver:    l.Resolver,
		Ingest:      l.Ingest,
		Now:         l.Now,
	}
	if sources.Resolver == nil {
		sources.Resolver = signals.SystemResolver(dnsLookupTimeout)
	}
	return sources
}

// resolveStore opens the telemetry store, or says why there is none.
//
// Unlike the API's evaluation, a round with no store does not happen at all.
// The API can answer a screen from the cluster alone and mark the store-backed
// rules not-applicable; this loop's whole output is a write to that store, so
// an installation without one has nothing for it to do — and recording a
// partial round would be recording that half the catalogue found nothing when
// it was never asked.
func (l *Loop) resolveStore(ctx context.Context, kitchen *kitchenv1alpha1.Kitchen) (Store, error) {
	if l.store != nil {
		return l.store(ctx)
	}
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		return nil, fmt.Errorf("this installation has no telemetry store, so there is nowhere to " +
			"record what the catalogue finds: the screens still evaluate it on request. " +
			"Set spec.observability.clickhouse.secretRef")
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: controller.PlatformNamespace, Name: ref.Name}
	if err := l.Client.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	cfg, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		return nil, err
	}
	return clickhouse.New(cfg), nil
}

// publish keeps the singleton honest about whether detection is running.
//
// Best effort and quiet, like every other sweep's: the round is the job. What
// it carries is read by an operator wondering whether anything is watching,
// and by the API deciding whether the recorded history is current enough to
// answer from — which is why the interval is on it as well as the instant.
func (l *Loop) publish(ctx context.Context, round Round, snapshot *signals.Snapshot) {
	current := &kitchenv1alpha1.Kitchen{}
	if err := l.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, current); err != nil {
		return
	}
	status := &kitchenv1alpha1.SignalEvaluationStatus{
		Open:            int32(round.Open),
		IntervalSeconds: int32(round.Interval / time.Second),
		Message:         round.Message,
	}
	if round.Evaluated {
		status.LastEvaluated = ptr.To(metav1.NewTime(l.now()))
	} else if existing := current.Status.Signals; existing != nil && !round.Off {
		// A round that could not evaluate does not erase when the last one
		// did: "it last ran an hour ago and here is why it has not since" is
		// the useful sentence, and it needs both halves.
		status.LastEvaluated = existing.LastEvaluated
	}
	if snapshot != nil {
		for _, failure := range snapshot.Unreadable() {
			status.Unreadable = append(status.Unreadable, kitchenv1alpha1.SignalInputStatus{
				Input:  string(failure.Input),
				Reason: failure.Reason,
			})
		}
	}
	current.Status.Signals = status
	if err := l.Client.Status().Update(ctx, current); err != nil {
		logf.FromContext(ctx).V(1).Info("the signal evaluation status was not published",
			"reason", err.Error())
	}
}

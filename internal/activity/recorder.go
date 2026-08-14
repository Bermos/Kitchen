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

// Package activity records what the platform did — releases moving, builds
// finishing, previews coming and going — into the telemetry store, where the
// dashboard's activity feed reads it back.
//
// Kubernetes Events were considered as the source of truth and rejected: they
// are ephemeral (an hour by default), per-object rather than per-platform, and
// noisy with machinery the feed would have to filter back out. The platform
// already has one telemetry store with one retention policy; its own activity
// belongs there too.
package activity

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// DefaultActor is recorded on events the reconcilers decided on their own,
// as opposed to something an authenticated caller asked for.
const DefaultActor = "operator"

// storeCacheTTL is how long a resolved store connection is trusted before the
// singleton and its secret are read again. Recording is frequent and the
// connection almost never changes; a minute keeps the common path free of
// API-server reads without holding on to a stale credential for long.
const storeCacheTTL = time.Minute

// Recorder writes activity events, asynchronously and best-effort. A nil
// Recorder is valid and records nothing, so tests and callers that were wired
// without one do not have to care.
//
// Best-effort is deliberate: the feed is an account of what happened, not a
// ledger anything depends on. A reconcile must never fail — or even slow down —
// because the telemetry store is down, so Record returns before the insert and
// a failed insert is a log line, not an error.
type Recorder struct {
	// Client reads the Kitchen singleton and the connection secret.
	Client client.Client
	// Namespace is where the platform (and the connection secret) lives.
	Namespace string
	// Singleton is the Kitchen object's name.
	Singleton string

	mu       sync.Mutex
	store    *clickhouse.Client
	resolved time.Time
}

// Record writes one event. It is safe on a nil Recorder and returns without
// waiting for the store.
func (r *Recorder) Record(ctx context.Context, event clickhouse.Event) {
	if r == nil {
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.Actor == "" {
		event.Actor = DefaultActor
	}

	// The reconcile that produced the event should not wait for the store,
	// and its cancellation (a requeue, a shutdown race) should not lose the
	// entry that was already decided on.
	background := context.WithoutCancel(ctx)
	go func() {
		insertCtx, cancel := context.WithTimeout(background, 10*time.Second)
		defer cancel()

		store, err := r.resolveStore(insertCtx)
		if err != nil {
			logf.Log.WithName("activity").V(1).Info("activity event dropped: no store", "type", event.Type, "reason", err.Error())
			return
		}
		if store == nil {
			// Installed without a telemetry store: there is no feed, by choice.
			return
		}
		if err := store.InsertEvent(insertCtx, event); err != nil {
			logf.Log.WithName("activity").V(1).Info("activity event dropped", "type", event.Type, "reason", err.Error())
		}
	}()
}

// resolveStore connects to the telemetry store the way the Kitchen reconciler
// does — off the singleton's secret reference — caching the answer briefly.
// A nil client with a nil error means the installation has no store.
func (r *Recorder) resolveStore(ctx context.Context) (*clickhouse.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.resolved) < storeCacheTTL {
		return r.store, nil
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: r.Singleton}, kitchen); err != nil {
		return nil, err
	}
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		r.store, r.resolved = nil, time.Now()
		return nil, nil
	}
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: ref.Name}, secret); err != nil {
		return nil, err
	}
	cfg, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		return nil, err
	}
	r.store, r.resolved = clickhouse.New(cfg), time.Now()
	return r.store, nil
}

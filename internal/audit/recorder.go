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

package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// RequestedByAnnotation records which authenticated caller asked for an
// object. The REST API writes it on everything it creates; the audit log reads
// it back so that a Project created from the dashboard is attributed to the
// person who clicked, not to the reconciler that noticed.
//
// It is the same key the API spells in internal/api, and it is duplicated here
// on purpose rather than imported: internal/api imports the controllers, so
// the arrow only goes one way.
const RequestedByAnnotation = "kitchen.bermos.dev/requested-by"

// configCacheTTL is how long the platform's compliance configuration and store
// connection are trusted before the singleton and its secret are read again.
// It matches the activity recorder's for the same reason: recording is
// frequent, the configuration almost never changes.
const configCacheTTL = time.Minute

// UnknownActor is what an actor resolves to when nothing said who. It is a
// value rather than an empty column so that a record which lost its
// attribution reads as one, instead of looking like a record about nobody.
const UnknownActor = "unknown"

// ControllerActor names a reconciler as an actor. The prefix is Kubernetes'
// own for a non-human identity, which is what anyone reading the log will
// already recognise.
func ControllerActor(controller string) string {
	if controller == "" {
		return UnknownActor
	}
	return "system:controller/" + controller
}

// Transition is one thing that happened to one object, as a reconciler sees
// it. It is deliberately wider than the record it becomes: Controller and
// Details are inputs the recorder turns into columns, so that a caller writes
// what it knows rather than what the table wants.
type Transition struct {
	// Object is what changed. Kind, name, namespace and UID are read off it,
	// so a caller cannot record a transition against an object it does not
	// have.
	Object client.Object

	// Kind of the object. Reconcilers work with typed objects whose TypeMeta
	// is empty by the time the client is done with them, so this is passed
	// rather than read.
	Kind string

	// Operation is what happened: clickhouse.AuditCreate, AuditUpdate,
	// AuditTransition or AuditDelete. Empty means AuditTransition.
	Operation string

	// From and To are the states either side of the move. Both are optional:
	// a create has no from, a delete has no to.
	From string
	To   string

	// Project the object belongs to, where it belongs to one.
	Project string

	// Reason is the one line a person reads.
	Reason string

	// Controller names the reconciler recording this, and becomes the actor
	// for anything the platform decided on its own.
	Controller string

	// Actor overrides the resolution below, for a transition whose cause is
	// known to be a particular person.
	Actor string

	// Correlation ties this record to the others that came out of the same
	// cause. Empty falls back to the object's UID, which at least ties the
	// object's own history together.
	Correlation string

	// Details is whatever else the record is incomplete without. It is
	// marshalled to JSON with sorted keys and hashed verbatim, so a value
	// that cannot be marshalled fails the transition rather than being
	// dropped quietly.
	Details map[string]any

	// Privileged classifies a transition that moved a control rather than a
	// workload — see privilege.go. Empty is the ordinary case. The recorder
	// materializes it into Details, so the classification is inside what the
	// chain hashes rather than beside it.
	Privileged Privilege
}

// Recorder appends transitions to the chain. A nil Recorder is valid and
// records nothing, so tests and callers wired without one do not have to care.
//
// Appends are serialized twice over: on this Recorder's mutex, which keeps a
// process from contending with itself, and on the head object, which keeps
// replicas from contending with each other. The second is the one that makes
// the chain sound — see head.go.
type Recorder struct {
	// Client reads the Kitchen singleton and the store's secret, and writes
	// the chain's head object.
	Client client.Client
	// Reader reads the head object straight from the API server, bypassing
	// the manager's cache. It has to: the head is written and read again on
	// the very next append, and a cached read would hand back the version
	// before the write — whose resourceVersion is stale, so the update that
	// follows conflicts, retries, and reads the same stale version again.
	// Nil falls back to Client, which is what tests with a fake client do.
	Reader client.Reader
	// Namespace is where the platform (and the connection secret) lives.
	Namespace string
	// Singleton is the Kitchen object's name.
	Singleton string

	mu sync.Mutex
	// store and enabled are the resolved configuration, refreshed on
	// configCacheTTL.
	store    *clickhouse.Client
	enabled  bool
	resolved time.Time
	// sequence is the last number this process appended, for the status the
	// platform publishes. It is this replica's view and not the chain's: the
	// head object is the chain's.
	sequence int64
	// unavailable explains a recorder that is not recording, for the status
	// the Kitchen reconciler publishes.
	unavailable string
}

// Status is what the platform reports about its own audit log.
type Status struct {
	// Recording is true when the recorder has somewhere to append to.
	Recording bool
	// Sequence is the last sequence number appended, and it is the anchor
	// the package comment describes: published outside the table so that a
	// log truncated from the end is visible without reading the log.
	Sequence int64
	// Message explains a recorder that is not recording.
	Message string
}

// Status reports what the recorder is doing, without touching the store.
func (r *Recorder) Status() Status {
	if r == nil {
		return Status{Message: "this build records no audit log"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !r.enabled && r.resolved.IsZero():
		return Status{Message: "the audit log has not been configured yet"}
	case !r.enabled:
		return Status{Message: "the audit log is turned off in spec.compliance.audit"}
	case r.store == nil:
		return Status{Message: r.unavailableMessage()}
	}
	return Status{Recording: true, Sequence: r.sequence}
}

func (r *Recorder) unavailableMessage() string {
	if r.unavailable != "" {
		return r.unavailable
	}
	return "this installation has no telemetry store, so there is nowhere to append to"
}

// Record appends one transition and returns only once it is durable.
//
// The contract is the opposite of the activity feed's, and callers must treat
// it that way: a non-nil error means the transition was not recorded, and the
// caller must not make it. Returning the error requeues the reconcile, which
// retries both. Over-recording — a transition recorded whose mutation then
// failed and was retried — is the acceptable direction to fail in; a mutation
// nothing recorded is not.
//
// The two cases that are not errors are both "there is no log": the feature is
// turned off, or the installation has no telemetry store. Both are reported by
// Status rather than by failing every reconcile on the platform, and both are
// visible on the Kitchen object.
func (r *Recorder) Record(ctx context.Context, transition Transition) error {
	if r == nil {
		return nil
	}

	record, err := transition.record()
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	store, err := r.resolve(ctx)
	if err != nil {
		return fmt.Errorf("the audit log could not be reached: %w", err)
	}
	if store == nil {
		return nil
	}

	sealed, previous, err := r.claim(ctx, record, store.AuditHead)
	if err != nil {
		return err
	}
	if err := store.InsertAuditRecord(ctx, sealed); err != nil {
		// Give the number back, so that a store which was briefly unreachable
		// costs a retry rather than a hole in the chain. It only comes back
		// if nothing has appended on top in the meantime; if something has,
		// the gap stands and the verifier reports it, which is the honest
		// outcome.
		r.release(context.WithoutCancel(ctx), sealed, previous)
		return fmt.Errorf("the audit record could not be appended: %w", err)
	}
	r.sequence = sealed.Sequence
	return nil
}

// record turns a Transition into the row it becomes, resolving the actor and
// marshalling the details.
func (t Transition) record() (clickhouse.AuditRecord, error) {
	if t.Object == nil {
		return clickhouse.AuditRecord{}, fmt.Errorf("an audit transition must name the object it is about")
	}
	operation := t.Operation
	if operation == "" {
		operation = clickhouse.AuditTransition
	}

	// The classification goes into the details rather than beside them: it is
	// then covered by the chain, so a privileged marking cannot be added or
	// taken off a stored record without the hash saying so. Writing it here
	// rather than at each site is what keeps the spelling single.
	fields := t.Details
	if t.Privileged != "" {
		fields = make(map[string]any, len(t.Details)+2)
		for key, value := range t.Details {
			fields[key] = value
		}
		fields[PrivilegedDetail] = true
		fields[PrivilegeClassDetail] = string(t.Privileged)
	}

	details := ""
	if len(fields) > 0 {
		encoded, err := json.Marshal(fields)
		if err != nil {
			return clickhouse.AuditRecord{}, fmt.Errorf("the audit record's details could not be encoded: %w", err)
		}
		details = string(encoded)
	}

	correlation := t.Correlation
	if correlation == "" {
		correlation = string(t.Object.GetUID())
	}

	actor, actorKind := t.actor(operation)
	return clickhouse.AuditRecord{
		Actor:       actor,
		ActorKind:   actorKind,
		Correlation: correlation,
		Operation:   operation,
		Kind:        t.Kind,
		Namespace:   t.Object.GetNamespace(),
		Name:        t.Object.GetName(),
		UID:         string(t.Object.GetUID()),
		Project:     t.Project,
		FromState:   t.From,
		ToState:     t.To,
		Reason:      t.Reason,
		Details:     details,
	}, nil
}

// actor decides who a transition is attributed to.
//
// An explicit actor always wins. Otherwise the object's own lifecycle —
// created, edited, deleted — is attributed to whoever asked for it, which the
// REST API left on the object; anything else is a decision a reconciler made
// and is attributed to that reconciler.
//
// The split matters because the annotation outlives the request that wrote it.
// A Build that a person started goes Running and then Succeeded on the
// platform's own account, minutes later, and attributing those to the person
// would be a record of something they did not do.
func (t Transition) actor(operation string) (string, string) {
	if t.Actor != "" {
		return t.Actor, clickhouse.ActorUser
	}
	if operation != clickhouse.AuditTransition {
		if requester := t.Object.GetAnnotations()[RequestedByAnnotation]; requester != "" {
			return requester, clickhouse.ActorUser
		}
	}
	return ControllerActor(t.Controller), clickhouse.ActorService
}

// resolve reads the compliance configuration and the store connection off the
// Kitchen singleton, caching the answer briefly. A nil client with a nil error
// means there is no log to append to, for a reason Status explains.
//
// The caller holds r.mu.
func (r *Recorder) resolve(ctx context.Context) (*clickhouse.Client, error) {
	if time.Since(r.resolved) < configCacheTTL {
		if !r.enabled {
			return nil, nil
		}
		return r.store, nil
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: r.Singleton}, kitchen); err != nil {
		return nil, err
	}
	r.enabled = kitchen.Spec.Compliance.Audit.Enabled
	if !r.enabled {
		r.store, r.resolved, r.unavailable = nil, time.Now(), ""
		return nil, nil
	}

	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		r.store, r.resolved = nil, time.Now()
		r.unavailable = "spec.compliance.audit is on but this installation has no telemetry store to append to: " +
			"set spec.observability.clickhouse.secretRef"
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
	r.store, r.resolved, r.unavailable = clickhouse.New(cfg), time.Now(), ""
	return r.store, nil
}

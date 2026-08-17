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

// Package k8sevents keeps the cluster's Warning events, which Kubernetes
// itself throws away.
//
// The API server expires an event about an hour after it happens, which makes
// its copy useless for the question people actually ask — "what happened at
// 03:00". The operator already reads these events one at a time for the
// component survey; recording them turns that watch into a history the Events
// screen and the crash report can read.
//
// This is not the activity feed. That table is the platform's own story —
// builds finishing, releases moving — written by the reconcilers about things
// Kitchen did. This one is the cluster's, written about things that happened
// to it: FailedScheduling, FailedCreate, FailedMount, OOMKilling.
//
// # Why it is built rather than deployed
//
// The off-the-shelf answer is a second, Deployment-mode collector running the
// k8s_objects receiver: a new workload whose output would still need reshaping
// into rows, and which would know nothing about which project an object belongs
// to. The operator is an existing watcher that already holds that knowledge, so
// the recorder is a runnable beside the other two rather than a component.
package k8sevents

import (
	"context"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

const (
	// configPollInterval is how often an idle recorder re-reads the Kitchen
	// object waiting for a telemetry store to appear.
	configPollInterval = 30 * time.Second
	// sweepInterval is how often the watch is read for what has changed on it.
	// It is not a sampling rate: the API server aggregates repeats of one event
	// into one object at a few per minute at most, so a shorter interval would
	// mostly re-read counts that have not moved.
	sweepInterval = 30 * time.Second
	// flushBatch bounds how large one insert grows. A quiet cluster produces a
	// handful of rows per sweep; a cluster in trouble produces thousands, and
	// they should not arrive as one statement.
	flushBatch = 500
	// seedWindow is how far back the recorder reads its own history on start.
	// An hour is the whole of it, because that is how long the API server keeps
	// an event: nothing older can still be on the watch to be recorded twice.
	seedWindow = time.Hour
	// maxMessage bounds a stored message at the same length the component
	// survey bounds the ones it puts on a status condition. The number is
	// spelled again here rather than shared, because the survey's bound is
	// about how much explanation belongs in a status field and this one is
	// about how much belongs in a stored row: they agree today, and neither
	// should have to move because the other did.
	maxMessage = 220
	// kubeletComponent is the one reporter whose identity is a node name.
	kubeletComponent = "kubelet"
	// nodeKind is the cluster-scoped kind that is its own node.
	nodeKind = "Node"
)

// Recorder is a manager Runnable. It idles until the Kitchen singleton names a
// telemetry store, then records every Warning the cluster raises.
type Recorder struct {
	// Client reads the Kitchen singleton, the events themselves, and the
	// objects those events are about.
	//
	// It is the manager's cached client throughout, which is a trade taken
	// deliberately. The watch this recorder is built on is the event informer
	// the first List starts; attribution then reads the object each event is
	// about, and reading it from the cache costs one informer per kind, where
	// reading it from the API server would cost a Get per recorded occurrence
	// — arriving fastest exactly when the cluster is unhappy enough to be
	// producing events. The kinds that price is paid for are the ones
	// labelledObject names, and no others.
	Client client.Client

	// seen is the count each event carried when it was last recorded, which is
	// what makes an occurrence an occurrence rather than a watch update. It is
	// rebuilt whole every sweep, so an event the API server has expired takes
	// its entry with it, and an identical event raised later starts again from
	// its own first occurrence.
	seen map[eventKey]uint32
}

// NeedLeaderElection makes the recorder a singleton: this is a cluster-wide
// watch, and every replica writing the same events would show up as several
// times the trouble.
func (r *Recorder) NeedLeaderElection() bool { return true }

func (r *Recorder) log() logr.Logger { return logf.Log.WithName("k8sevents") }

// config is one resolved reading of the Kitchen object.
type config struct {
	store    clickhouse.Config
	hasStore bool
}

// Start implements manager.Runnable. Like the flow collector it never returns
// an error before the context ends: an event history is an observability
// capability, and a store being down must not take the operator down with it.
func (r *Recorder) Start(ctx context.Context) error {
	for {
		cfg, err := r.resolve(ctx)
		switch {
		case err != nil:
			r.log().V(1).Info("cannot read the telemetry configuration", "reason", err.Error())
		case cfg.hasStore:
			r.run(ctx, cfg)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(configPollInterval):
		}
	}
}

// resolve reads the store connection off the Kitchen singleton, the same way
// the API and the reconcilers do.
func (r *Recorder) resolve(ctx context.Context) (config, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		return config{}, err
	}
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		return config{}, nil
	}
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: controller.PlatformNamespace, Name: ref.Name}, secret); err != nil {
		return config{}, err
	}
	store, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		return config{}, err
	}
	return config{store: store, hasStore: true}, nil
}

// run sweeps on the interval until the configuration moves away or the context
// ends.
func (r *Recorder) run(ctx context.Context, cfg config) {
	store := clickhouse.New(cfg.store)
	if err := r.seed(ctx, store); err != nil {
		// A store that cannot be read is a store that cannot be written
		// either, so there is nothing to do but wait for it and try again.
		r.log().V(1).Info("cannot read back what is already recorded", "reason", err.Error())
		return
	}
	r.log().Info("recording the cluster's warning events", "interval", sweepInterval.String())

	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	r.sweepOnce(ctx, store)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := r.resolve(ctx)
			if err == nil && (!current.hasStore || current.store != cfg.store) {
				r.log().Info("the telemetry store changed, restarting")
				return
			}
			r.sweepOnce(ctx, store)
		}
	}
}

// seed reads back what is already recorded, so that a restart neither
// re-ingests history nor loses what happened while the operator was down.
//
// Which way that was traded, and why: an event's count is cumulative and the
// store keeps it that way, so the only thing a new leader has to know is which
// counts are already written. Reading the store's own last hour gives exactly
// that — an event whose count has not moved since is skipped, an event that
// grew while nobody was watching is recorded at its new total, and an event the
// store has never seen is recorded whole. The two simpler starts each pick one
// error and keep it: starting from nothing writes a duplicate row for every
// event the API server still holds, and starting from "everything so far is
// history" silently drops the restart window itself, which is the window
// someone restarting the operator is most likely to be asking about.
//
// Rows past the query's ceiling are not seeded, so a cluster loud enough to
// have filled it repeats at most those events, once, at their current count.
func (r *Recorder) seed(ctx context.Context, store *clickhouse.Client) error {
	recorded, err := store.QueryK8sEvents(ctx, clickhouse.K8sEventQuery{
		Since: time.Now().Add(-seedWindow),
		Limit: clickhouse.MaxK8sEventLimit,
	})
	if err != nil {
		return err
	}
	seen := make(map[eventKey]uint32, len(recorded))
	for _, row := range recorded {
		if key := rowKey(row); row.Count > seen[key] {
			seen[key] = row.Count
		}
	}
	r.seen = seen
	return nil
}

// sweepOnce records what one sweep found.
func (r *Recorder) sweepOnce(ctx context.Context, store *clickhouse.Client) {
	events, err := r.Sweep(ctx, time.Now().UTC())
	if err != nil {
		r.log().V(1).Info("cannot read the cluster's events", "reason", err.Error())
		return
	}
	for start := 0; start < len(events); start += flushBatch {
		end := min(start+flushBatch, len(events))
		if err := store.InsertK8sEvents(ctx, events[start:end]); err != nil {
			// Dropped rows, not a broken recorder: what did not land goes back
			// on the pile for the next sweep to write again, and the rest of
			// this sweep goes with it, because a store that refuses one insert
			// refuses the next four hundred and would say so as many times.
			r.forget(events[start:])
			r.log().V(1).Info("event batch dropped", "events", len(events)-start, "reason", err.Error())
			return
		}
	}
}

// forget puts rows the store refused back where the next sweep will find them.
// Without it a failed insert would be indistinguishable from a written one, and
// those occurrences would be lost for good.
func (r *Recorder) forget(events []clickhouse.K8sEvent) {
	for _, event := range events {
		delete(r.seen, rowKey(event))
	}
}

// The events are the watch itself; the namespaces, pods, replicasets and
// deployments are what an event is attributed through. Only replicasets is a
// grant the operator did not already hold — the others repeat what the
// reconcilers and the component survey ask for, spelled here so that reading
// this package says what it needs rather than what it happens to inherit.
//
// +kubebuilder:rbac:groups="",resources=events;namespaces;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets;deployments,verbs=get;list;watch

// Sweep reads the watch and returns a row per occurrence that is not recorded
// yet, remembering what it saw.
//
// It reads the watch on an interval rather than handling every update on it,
// and that is the point rather than a shortcut: a Kubernetes Event is not one
// occurrence but one object the API server keeps updating, with a cumulative
// count on it. A sweep that slept through three increments still records the
// total, and an event lives about an hour — far longer than one interval — so
// nothing appears and expires between two passes.
func (r *Recorder) Sweep(ctx context.Context, at time.Time) ([]clickhouse.K8sEvent, error) {
	events := &corev1.EventList{}
	if err := r.Client.List(ctx, events); err != nil {
		return nil, err
	}

	seen := make(map[eventKey]uint32, len(r.seen))
	// Room for every warning the cluster is holding, which is what the first
	// sweep after a fresh install actually records; every later one records the
	// few that moved.
	recorded := make([]clickhouse.K8sEvent, 0, len(events.Items))
	for i := range events.Items {
		event := &events.Items[i]
		// Warning only. A Normal event is the cluster narrating itself —
		// pulling an image, starting a container — and is volume without
		// signal in a table that exists to answer what went wrong.
		if event.Type != corev1.EventTypeWarning {
			continue
		}
		row := r.row(ctx, event, at)
		key := rowKey(row)
		previous, known := r.seen[key]
		seen[key] = row.Count
		// A count that has not moved is the same occurrences over again. A
		// count that has moved either way is news: down means the event object
		// expired and an identical one has started counting from the top.
		if known && previous == row.Count {
			continue
		}
		recorded = append(recorded, row)
	}
	r.seen = seen
	return recorded, nil
}

// row turns one Warning into the row the store keeps.
func (r *Recorder) row(ctx context.Context, event *corev1.Event, at time.Time) clickhouse.K8sEvent {
	involved := event.InvolvedObject
	row := clickhouse.K8sEvent{
		Timestamp: observedAt(event, at),
		Namespace: involved.Namespace,
		Kind:      involved.Kind,
		Name:      involved.Name,
		Reason:    event.Reason,
		Message:   truncateMessage(event.Message),
		Count:     occurrences(event),
		Node:      reportingNode(event),
	}
	r.attribute(ctx, &row, involved)
	return row
}

// attribute answers whose event this is, and — where the answer is a pod —
// which node it happened on.
//
// The namespace is asked first and the object second, because the namespace is
// the half that survives: an event about a pod that has already been deleted is
// often the most interesting one on the screen, and it still has to be recorded
// with whatever attribution is left. A namespace carrying no project label is
// the platform's own or the cluster's, which the schema records as an empty
// project on purpose — the events that explain an install that never came up
// belong to no project at all.
func (r *Recorder) attribute(ctx context.Context, row *clickhouse.K8sEvent, involved corev1.ObjectReference) {
	if involved.Namespace == "" {
		return
	}
	namespace := &corev1.Namespace{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: involved.Namespace}, namespace); err != nil {
		return
	}
	row.Project = namespace.Labels[controller.LabelProject]

	object := labelledObject(involved)
	if object == nil {
		return
	}
	key := types.NamespacedName{Namespace: involved.Namespace, Name: involved.Name}
	if err := r.Client.Get(ctx, key, object); err != nil {
		return
	}
	labels := object.GetLabels()
	if project := labels[controller.LabelProject]; project != "" {
		row.Project = project
	}
	row.Environment = labels[controller.LabelEnvironment]
	if pod, ok := object.(*corev1.Pod); ok && row.Node == "" {
		// The last place the node is written down: a kubelet stamps its own
		// name on what it reports, but the scheduler and the attach/detach
		// controller do not, and their events are about a placed pod.
		row.Node = pod.Spec.NodeName
	}
}

// labelledObject is an empty object of the kind an event involves, for the
// kinds worth reading back.
//
// It is a fixed list rather than an unstructured lookup for two reasons. Only
// what an Environment materializes carries the project and environment labels,
// so reading anything else would cost a lookup with a known answer; and every
// kind read through the manager's cache becomes an informer over that kind
// cluster-wide, which for a kind the operator has no permission to list would
// be an informer that never syncs and never goes away.
func labelledObject(involved corev1.ObjectReference) client.Object {
	// A core object's apiVersion is "v1", and an emitter that left it out
	// meant the core group too.
	apiVersion := involved.APIVersion
	if apiVersion == "" {
		apiVersion = "v1"
	}
	switch apiVersion + "/" + involved.Kind {
	case "v1/Pod":
		// Nearly every Warning worth reading is about a pod, and the pod is
		// also where the node is written down.
		return &corev1.Pod{}
	case "apps/v1/ReplicaSet":
		// Where an admission refusal lands for anything a Deployment owns: the
		// pods do not exist to carry the complaint, so the ReplicaSet does.
		return &appsv1.ReplicaSet{}
	case "apps/v1/Deployment":
		return &appsv1.Deployment{}
	}
	return nil
}

// occurrences is how many times the cluster has reported this event, which is
// the number the store keeps: a Kubernetes Event is not one occurrence but one
// object the API server increments.
func occurrences(event *corev1.Event) uint32 {
	if event.Series != nil && event.Series.Count > 0 {
		return uint32(event.Series.Count) // #nosec G115 -- guarded positive above
	}
	if event.Count > 0 {
		return uint32(event.Count) // #nosec G115 -- guarded positive above
	}
	// An event the newer API wrote once carries neither counter: it happened
	// once, which is not the same as having happened no times.
	return 1
}

// observedAt is when the event last happened, preferring the time an emitter
// set on the series over the legacy timestamps the newer events API leaves
// zeroed. An event carrying none of them is stamped with the sweep that found
// it, because an event with no time on it still happened.
func observedAt(event *corev1.Event, at time.Time) time.Time {
	switch {
	case event.Series != nil && !event.Series.LastObservedTime.IsZero():
		return event.Series.LastObservedTime.Time
	case !event.LastTimestamp.IsZero():
		return event.LastTimestamp.Time
	case !event.EventTime.IsZero():
		return event.EventTime.Time
	case !event.FirstTimestamp.IsZero():
		return event.FirstTimestamp.Time
	}
	return at
}

// reportingNode is the node an event is about, where that is knowable. The
// node-scoped signals read this column, and a Warning with no node on it is one
// they cannot place.
//
// source.host is a node name whenever it is set at all, because only node-local
// components set it. reportingInstance is the newer API's spelling of the same
// field, but only when the kubelet is what reported: for a controller it
// identifies the controller, and writing that into the node column would put a
// name that is not a node's on the row. An event about a Node is about that
// node, whoever raised it.
func reportingNode(event *corev1.Event) string {
	switch {
	case event.Source.Host != "":
		return event.Source.Host
	case event.ReportingController == kubeletComponent && event.ReportingInstance != "":
		return event.ReportingInstance
	case event.InvolvedObject.Kind == nodeKind && event.InvolvedObject.Namespace == "":
		return event.InvolvedObject.Name
	}
	return ""
}

// truncateMessage bounds a stored message. The reason is at the front of every
// one of these — an admission rejection, a scheduler complaint, a container's
// exit — so the tail is what can be spared.
func truncateMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > maxMessage {
		return message[:maxMessage] + "…"
	}
	return message
}

// eventKey identifies one event object across sweeps, and across a restart.
//
// Every field of it is a column the store keeps, which is what lets a new
// leader pick up where the last one left off: the key of a row read back out of
// the store is the key of the same event still on the watch. It is the involved
// object, the reason and the message rather than the event's own name because
// that is what Kubernetes aggregates on — one Event object exists per object,
// reason and message, and the count on it is how many times that combination
// has happened.
type eventKey struct {
	namespace string
	kind      string
	name      string
	node      string
	reason    string
	message   string
}

// rowKey is the identity of a row, whether it was just built from an event or
// just read back out of the store.
func rowKey(row clickhouse.K8sEvent) eventKey {
	return eventKey{
		namespace: row.Namespace,
		kind:      row.Kind,
		name:      row.Name,
		node:      row.Node,
		reason:    row.Reason,
		message:   row.Message,
	}
}

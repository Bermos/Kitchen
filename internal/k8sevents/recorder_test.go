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

package k8sevents

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

var sweptAt = time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)

// The names the fixtures use, spelled once: an assertion that disagrees with
// the object it is about is the one bug these tests cannot catch.
const (
	appProject     = "shop"
	appEnvironment = "production"
	appNamespace   = "kitchen-shop"
	appPodName     = "production-abc-1"
	appNode        = "node-1"
	failedMount    = "FailedMount"
)

// appNS is the namespace an Environment materializes into, labelled the way the
// environment reconciler labels it.
func appNS() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   appNamespace,
		Labels: map[string]string{controller.LabelProject: appProject},
	}}
}

// platformNS is the platform's own namespace, which belongs to no project.
func platformNS() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controller.PlatformNamespace}}
}

func appPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appPodName,
			Namespace: appNamespace,
			Labels: map[string]string{
				controller.LabelProject:     appProject,
				controller.LabelEnvironment: appEnvironment,
			},
		},
		Spec: corev1.PodSpec{NodeName: appNode},
	}
}

type eventOption func(*corev1.Event)

func about(kind, namespace, name string) eventOption {
	return func(event *corev1.Event) {
		event.InvolvedObject = corev1.ObjectReference{Kind: kind, Namespace: namespace, Name: name}
		event.Namespace = namespace
	}
}

func aboutIn(apiVersion, kind, namespace, name string) eventOption {
	return func(event *corev1.Event) {
		about(kind, namespace, name)(event)
		event.InvolvedObject.APIVersion = apiVersion
	}
}

func counted(count int32) eventOption {
	return func(event *corev1.Event) { event.Count = count }
}

func serialised(count int32, last time.Time) eventOption {
	return func(event *corev1.Event) {
		event.Series = &corev1.EventSeries{Count: count, LastObservedTime: metav1.NewMicroTime(last)}
	}
}

func normal() eventOption {
	return func(event *corev1.Event) { event.Type = corev1.EventTypeNormal }
}

func from(component, host string) eventOption {
	return func(event *corev1.Event) { event.Source = corev1.EventSource{Component: component, Host: host} }
}

func reportedBy(controllerName, instance string) eventOption {
	return func(event *corev1.Event) {
		event.ReportingController, event.ReportingInstance = controllerName, instance
	}
}

func saying(message string) eventOption {
	return func(event *corev1.Event) { event.Message = message }
}

// warning is an event as the API server holds it: a Warning about the
// application's pod, one occurrence, unless an option says otherwise.
func warning(name, reason string, options ...eventOption) *corev1.Event {
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: appNamespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", APIVersion: "v1", Namespace: appNamespace, Name: appPodName},
		Reason:         reason,
		Message:        reason + " happened",
		Type:           corev1.EventTypeWarning,
		Count:          1,
		LastTimestamp:  metav1.NewTime(sweptAt.Add(-time.Minute)),
	}
	for _, option := range options {
		option(event)
	}
	return event
}

func recorder(objects ...client.Object) *Recorder {
	return &Recorder{Client: fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()}
}

// swap is what the cluster looks like now, keeping what the recorder remembers.
func swap(from *Recorder, objects ...client.Object) *Recorder {
	next := recorder(objects...)
	next.seen = from.seen
	return next
}

func sweep(t *testing.T, r *Recorder, at time.Time) []clickhouse.K8sEvent {
	t.Helper()
	rows, err := r.Sweep(context.Background(), at)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return rows
}

func only(t *testing.T, rows []clickhouse.K8sEvent) clickhouse.K8sEvent {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d: %+v", len(rows), rows)
	}
	return rows[0]
}

// The join the store cannot make for itself: a Warning about a pod carries the
// project and environment that own it, straight off the pod's labels.
func TestAWarningIsAttributedToWhatItIsAbout(t *testing.T) {
	r := recorder(appNS(), appPod(), warning("e1", failedMount))

	row := only(t, sweep(t, r, sweptAt))
	if row.Project != appProject || row.Environment != appEnvironment {
		t.Fatalf("the row should say whose event it is: %+v", row)
	}
	if row.Namespace != appNamespace || row.Kind != "Pod" || row.Name != appPodName {
		t.Fatalf("the row should name the involved object: %+v", row)
	}
	if row.Reason != failedMount || row.Count != 1 {
		t.Fatalf("unexpected row %+v", row)
	}
	// The scheduler and the attach/detach controller stamp no host on what they
	// report, so the placed pod is where the node is read from.
	if row.Node != appNode {
		t.Fatalf("want the pod's node, got %q", row.Node)
	}
}

// An event about a pod that is already gone is often the most interesting one
// on the screen, so it is still recorded — with whatever attribution survives.
func TestAnEventOutlivesTheObjectItIsAbout(t *testing.T) {
	r := recorder(appNS(), warning("e1", failedMount))

	row := only(t, sweep(t, r, sweptAt))
	if row.Project != appProject {
		t.Fatalf("the namespace still knows whose it is: %+v", row)
	}
	if row.Environment != "" {
		t.Fatalf("nothing left knows the environment, so it should be empty: %+v", row)
	}
	if row.Name != appPodName || row.Reason != failedMount {
		t.Fatalf("the event itself should be recorded regardless: %+v", row)
	}
}

// Platform and cluster-scoped objects get an empty project — which is not a
// gap, it is the bucket the events that explain a broken install land in.
func TestPlatformAndClusterObjectsBelongToNoProject(t *testing.T) {
	collector := warning("e1", "FailedCreate",
		aboutIn("apps/v1", "DaemonSet", controller.PlatformNamespace, "kitchen-collector"))
	node := warning("e2", "NodeNotReady", about(nodeKind, "", appNode))
	r := recorder(appNS(), platformNS(), collector, node)

	rows := sweep(t, r, sweptAt)
	if len(rows) != 2 {
		t.Fatalf("want both rows, got %+v", rows)
	}
	byName := map[string]clickhouse.K8sEvent{}
	for _, row := range rows {
		byName[row.Name] = row
	}
	if got := byName["kitchen-collector"]; got.Project != "" || got.Namespace != controller.PlatformNamespace {
		t.Fatalf("the platform's own DaemonSet belongs to no project: %+v", got)
	}
	// A node's event is about that node, whoever raised it, and the node-scoped
	// signals read that column.
	if got := byName[appNode]; got.Project != "" || got.Node != appNode {
		t.Fatalf("a Node event should place itself: %+v", got)
	}
}

// Normal events are the cluster narrating itself; this table exists to answer
// what went wrong.
func TestOnlyWarningsAreRecorded(t *testing.T) {
	r := recorder(appNS(), appPod(),
		warning("e1", "Pulled", normal()),
		warning("e2", failedMount))

	row := only(t, sweep(t, r, sweptAt))
	if row.Reason != failedMount {
		t.Fatalf("want only the warning, got %+v", row)
	}
}

// The whole reason the recorder keeps a count: an Event is one object the API
// server increments, not one occurrence, and re-reading it must not re-record
// what is already stored.
func TestAnEventIsRecordedWhenItsCountMovesAndNotOtherwise(t *testing.T) {
	r := recorder(appNS(), appPod(), warning("e1", failedMount, counted(3)))

	// First sight is news, and the count is what the cluster reported, not one.
	first := only(t, sweep(t, r, sweptAt))
	if first.Count != 3 {
		t.Fatalf("want the cluster's count, got %d", first.Count)
	}

	// The same object, unchanged, is the same three occurrences over again.
	r = swap(r, appNS(), appPod(), warning("e1", failedMount, counted(3)))
	if rows := sweep(t, r, sweptAt.Add(sweepInterval)); len(rows) != 0 {
		t.Fatalf("an unchanged count is not an occurrence: %+v", rows)
	}

	// Two more occurrences are news, and the row carries the total, because
	// "this happened five times" is the question the column answers.
	r = swap(r, appNS(), appPod(), warning("e1", failedMount, counted(5)))
	grown := only(t, sweep(t, r, sweptAt.Add(2*sweepInterval)))
	if grown.Count != 5 {
		t.Fatalf("want the new total, got %d", grown.Count)
	}

	// An event that expired and came back starts counting from the top, and a
	// count that moved backwards is exactly how that looks.
	r = swap(r, appNS(), appPod(), warning("e2", failedMount, counted(1)))
	fresh := only(t, sweep(t, r, sweptAt.Add(3*sweepInterval)))
	if fresh.Count != 1 {
		t.Fatalf("want the new event's own count, got %d", fresh.Count)
	}
}

// An event the API server has expired takes its entry with it, so the map does
// not grow with every warning the cluster has ever raised.
func TestWhatTheClusterHasForgottenIsForgotten(t *testing.T) {
	r := recorder(appNS(), appPod(), warning("e1", failedMount), warning("e2", "BackOff"))
	if rows := sweep(t, r, sweptAt); len(rows) != 2 {
		t.Fatalf("want both events, got %+v", rows)
	}
	if len(r.seen) != 2 {
		t.Fatalf("want both remembered, got %d", len(r.seen))
	}

	r = swap(r, appNS(), appPod(), warning("e1", failedMount))
	if rows := sweep(t, r, sweptAt.Add(sweepInterval)); len(rows) != 0 {
		t.Fatalf("nothing new happened: %+v", rows)
	}
	if len(r.seen) != 1 {
		t.Fatalf("the expired event should have been forgotten, got %d entries", len(r.seen))
	}
}

// The restart trade-off, tested from the store's side: what seeding reads back
// keys the same way as what the watch still holds, so a new leader re-ingests
// nothing and misses nothing.
func TestARestartPicksUpWhereTheStoreLeftOff(t *testing.T) {
	unchanged := warning("e1", failedMount, counted(3))
	grew := warning("e2", "BackOff", counted(7))
	r := recorder(appNS(), appPod(), unchanged, grew)

	// What the previous leader wrote, as the store hands it back: the rows are
	// clickhouse.K8sEvent either way, which is what makes the keys agree.
	before := recorder(appNS(), appPod(),
		warning("e1", failedMount, counted(3)),
		warning("e2", "BackOff", counted(4)))
	stored := sweep(t, before, sweptAt.Add(-time.Hour))
	if len(stored) != 2 {
		t.Fatalf("want two stored rows, got %+v", stored)
	}
	r.seen = map[eventKey]uint32{}
	for _, row := range stored {
		r.seen[rowKey(row)] = row.Count
	}

	rows := sweep(t, r, sweptAt)
	row := only(t, rows)
	if row.Reason != "BackOff" || row.Count != 7 {
		t.Fatalf("want only the event that grew, at its new total: %+v", rows)
	}
}

// A store that refused the insert has not stored anything, and the recorder
// must not mistake that for a written row.
func TestRowsTheStoreRefusedComeBack(t *testing.T) {
	r := recorder(appNS(), appPod(), warning("e1", failedMount, counted(3)))
	rows := sweep(t, r, sweptAt)
	if len(rows) != 1 {
		t.Fatalf("want one row, got %+v", rows)
	}

	r.forget(rows)

	r = swap(r, appNS(), appPod(), warning("e1", failedMount, counted(3)))
	retried := only(t, sweep(t, r, sweptAt.Add(sweepInterval)))
	if retried.Count != 3 {
		t.Fatalf("the dropped occurrences should be offered again: %+v", retried)
	}
}

// The node column is what the node-scoped signals place an event by, and only
// some of the things that report an event know a node's name.
func TestTheNodeComesFromWhoeverActuallyKnowsIt(t *testing.T) {
	for name, test := range map[string]struct {
		event *corev1.Event
		want  string
	}{
		"a kubelet stamps its own host on what it reports": {
			warning("e", failedMount, from(kubeletComponent, appNode)), appNode,
		},
		"the newer API spells the same field differently": {
			warning("e", failedMount, reportedBy(kubeletComponent, appNode)), appNode,
		},
		"a controller's instance is not a node name": {
			warning("e", failedMount, reportedBy("attachdetach-controller", "kube-controller-manager-x")), appNode,
		},
		"a node's own event places itself": {
			warning("e", "NodeHasDiskPressure", about(nodeKind, "", appNode)), appNode,
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := recorder(appNS(), appPod(), test.event)
			if row := only(t, sweep(t, r, sweptAt)); row.Node != test.want {
				t.Fatalf("want node %q, got %q", test.want, row.Node)
			}
		})
	}

	// Nothing knows: no host, no kubelet, and the pod is gone with its
	// placement. The event is still worth recording without a node.
	r := recorder(appNS(), warning("e", failedMount, reportedBy("attachdetach-controller", "kube-controller-manager-x")))
	if row := only(t, sweep(t, r, sweptAt)); row.Node != "" {
		t.Fatalf("want no node rather than a guess, got %q", row.Node)
	}
}

// An admission refusal for anything a Deployment owns lands on the ReplicaSet,
// because the pods it is refusing do not exist to carry it.
func TestAnAdmissionRefusalIsAttributedThroughTheReplicaSet(t *testing.T) {
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name:      "production-6d5f9",
		Namespace: appNamespace,
		Labels: map[string]string{
			controller.LabelProject:     appProject,
			controller.LabelEnvironment: appEnvironment,
		},
	}}
	refusal := warning("e1", "FailedCreate",
		aboutIn("apps/v1", "ReplicaSet", appNamespace, rs.Name),
		saying(`pods "production-6d5f9-" is forbidden: violates PodSecurity "baseline:latest"`))
	r := recorder(appNS(), rs, refusal)

	row := only(t, sweep(t, r, sweptAt))
	if row.Project != appProject || row.Environment != appEnvironment {
		t.Fatalf("the refusal should be attributable: %+v", row)
	}
	if !strings.Contains(row.Message, "PodSecurity") {
		t.Fatalf("the message is what names the reason: %+v", row)
	}
}

// The count the store keeps is the one the cluster reports, whichever of the
// two events APIs reported it.
func TestTheCountIsWhicheverCounterTheClusterUsed(t *testing.T) {
	for name, test := range map[string]struct {
		event *corev1.Event
		want  uint32
	}{
		"the legacy counter":                 {warning("e", failedMount, counted(12)), 12},
		"the newer API's series":             {warning("e", failedMount, serialised(9, sweptAt)), 9},
		"an event that carries no counter":   {warning("e", failedMount, counted(0)), 1},
		"a series wins over a stale counter": {warning("e", failedMount, counted(2), serialised(40, sweptAt)), 40},
	} {
		t.Run(name, func(t *testing.T) {
			r := recorder(appNS(), appPod(), test.event)
			if row := only(t, sweep(t, r, sweptAt)); row.Count != test.want {
				t.Fatalf("want count %d, got %d", test.want, row.Count)
			}
		})
	}
}

// A row is stamped with when the event last happened, not with when the sweep
// noticed it — the two differ by up to an interval, and the crash report joins
// on the first.
func TestARowIsStampedWithWhenItHappened(t *testing.T) {
	happened := sweptAt.Add(-3 * time.Minute)
	r := recorder(appNS(), appPod(), warning("e1", failedMount, serialised(4, happened)))
	if row := only(t, sweep(t, r, sweptAt)); !row.Timestamp.Equal(happened) {
		t.Fatalf("want %v, got %v", happened, row.Timestamp)
	}

	// An event with no time on it still happened, so the sweep that found it
	// stamps it rather than dropping it.
	untimed := warning("e2", failedMount)
	untimed.LastTimestamp = metav1.Time{}
	r = recorder(appNS(), appPod(), untimed)
	if row := only(t, sweep(t, r, sweptAt)); !row.Timestamp.Equal(sweptAt) {
		t.Fatalf("want the sweep's own time, got %v", row.Timestamp)
	}
}

// One explanation should not dominate the table, and the reason leads every one
// of these messages, so the tail is what goes.
func TestALongMessageIsBounded(t *testing.T) {
	r := recorder(appNS(), appPod(), warning("e1", "FailedScheduling", saying(strings.Repeat("a", 500))))

	row := only(t, sweep(t, r, sweptAt))
	if len([]rune(row.Message)) != maxMessage+1 || !strings.HasSuffix(row.Message, "…") {
		t.Fatalf("want a bounded message ending in an ellipsis, got %d characters", len([]rune(row.Message)))
	}
}

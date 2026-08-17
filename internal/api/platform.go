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

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/flows"
	"github.com/Bermos/Kitchen/internal/signals"
)

// The operator's estate, docs/OBSERVABILITY.md §6.2: the platform seen across
// every project rather than from inside one.
//
// Two rules shape all of it. The first is the path: every endpoint here lives
// under `/platform/`, and nothing project-scoped does. Today the API
// authenticates and does not authorize — an open item in docs/AUTH.md — so the
// prefix enforces nothing yet; the day it does, telling an operator's screen
// from a developer's is a prefix match in a middleware rather than an audit of
// every handler. Which is why a platform-wide question never appears as a query
// parameter on a project-scoped endpoint, however convenient that would be.
//
// The second is that none of these adds a watch. They read the API server
// through the same uncached reader the introspection endpoints use, and the
// store through the same client the logs do, so a screen nobody has open costs
// the platform nothing.
//
// Nodes and pods are declared where the endpoint that first needed them is —
// nodes on /status, pods on the workload endpoint. These are the rest of what
// the platform screens and the signals gatherer walk:
//
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch

// The platform's own labels, as the component survey selects on them. Every
// name the chart generates is release-name prefixed, so nothing here can be
// found by name: `part-of` is what marks a workload as the platform's, and
// `component` is what makes it readable.
const (
	labelPartOf    = "app.kubernetes.io/part-of"
	labelComponent = "app.kubernetes.io/component"
	partOfKitchen  = "kitchen"
	// componentCollector is the node collector's own component label. It is
	// the one workload whose absence is invisible everywhere else, because a
	// DaemonSet refused at admission has no pods to be unhealthy.
	componentCollector = "collector"
)

// The sentences a screen says instead of showing a number it does not have.
// They are constants because more than one screen says each of them, and a
// platform that words the same absence two ways reads as two problems.
const (
	noStoreMessage = "this installation has no telemetry store, " +
		"so nothing measured over time can be shown here"
	noFreshnessMessage = "the telemetry store could not be read, " +
		"so which nodes are still reporting is unknown rather than fine"
	noHostMetricsMessage = "node saturation is collected, but this operator has no reader for host_metrics " +
		"wired up, so these series are absent rather than zero"
	noVolumeUsageMessage = "volume fill is collected, but this operator has no reader for the kubelet's " +
		"volume stats wired up, so usage is unknown rather than zero"
)

// platformNodes answers the Nodes screen: what the cluster is made of, and
// which of its machines the platform is no longer hearing from.
//
// Telemetry freshness is the column that earns this screen. A node whose
// collector died — or was never admitted, which is the PodSecurity failure the
// platform namespace's level exists to prevent — looks perfectly healthy
// everywhere else: its conditions read True, its pods are Running, and it
// simply stops contributing to every number the platform reports. This is where
// that looks broken.
func (s *Server) platformNodes(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	nodes := &corev1.NodeList{}
	if err := s.reader().List(ctx, nodes); err != nil {
		s.writeError(w, err)
		return
	}
	pods := &corev1.PodList{}
	if err := s.reader().List(ctx, pods); err != nil {
		s.writeError(w, err)
		return
	}

	freshness, unread := s.telemetryFreshness(ctx)
	usage, unmeasured := s.nodeUsage(ctx)
	body := platformNodesBody{
		Items:            make([]nodeView, 0, len(nodes.Items)),
		TelemetryMessage: unread,
		// Where no source is wired up, the series are absent and the screen
		// says so. That sentence is the whole difference between "this node is
		// idle" and "nobody measured this node".
		UsageMessage: unmeasured,
	}

	scheduled := map[string]int{}
	for i := range pods.Items {
		scheduled[pods.Items[i].Spec.NodeName]++
	}

	wanted := strings.TrimSpace(req.URL.Query().Get("node"))
	now := time.Now().UTC()
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if wanted != "" && node.Name != wanted {
			continue
		}
		view := newNodeView(node, scheduled[node.Name])
		view.Telemetry = telemetryOf(freshness, node.Name, unread, now)
		view.Usage = usage[node.Name]
		body.Items = append(body.Items, view)

		body.Nodes++
		if view.Ready {
			body.ReadyNodes++
		}
		if view.Telemetry.Silent {
			body.SilentNodes++
		}
	}
	sort.Slice(body.Items, func(i, j int) bool { return body.Items[i].Name < body.Items[j].Name })

	writeJSON(w, http.StatusOK, body)
}

// platformNodesBody is the Nodes screen.
type platformNodesBody struct {
	Items []nodeView `json:"items"`

	Nodes      int `json:"nodes"`
	ReadyNodes int `json:"readyNodes"`
	// SilentNodes is how many exist in the API server and have shipped no
	// telemetry inside the lookback. It is a measured zero only when
	// TelemetryMessage is empty.
	SilentNodes int `json:"silentNodes"`

	// TelemetryMessage says why freshness is missing, and is empty when it is
	// not: a silent node and a node nobody could ask about must not render
	// alike.
	TelemetryMessage string `json:"telemetryMessage,omitempty"`
	// UsageMessage says the same about the saturation series.
	UsageMessage string `json:"usageMessage,omitempty"`
}

// nodeView is one machine.
type nodeView struct {
	Name string `json:"name"`
	// Ready is the NodeReady condition. Schedulable is the cordon, which is a
	// decision somebody took rather than a fault.
	Ready       bool      `json:"ready"`
	Schedulable bool      `json:"schedulable"`
	Roles       []string  `json:"roles,omitempty"`
	Version     string    `json:"kubeletVersion,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	// Conditions are the node's own, pressure conditions included: they are
	// the reading in which a node filling its disk says so itself.
	Conditions []nodeConditionView `json:"conditions,omitempty"`
	// Pods is how many the scheduler has placed here, against what the node
	// says it can hold.
	Pods        int               `json:"pods"`
	Allocatable nodeCapacityView  `json:"allocatable"`
	Telemetry   nodeTelemetryView `json:"telemetry"`
	// Usage is the node's saturation over the recent window, where something
	// reads it out of the store. Absent, with the body's UsageMessage saying
	// why, where nothing does.
	Usage *nodeUsageView `json:"usage,omitempty"`
}

// nodeUsageView is one node's saturation series.
type nodeUsageView struct {
	// BucketSeconds is how wide one point is, which is what turns a run of
	// points into a duration.
	BucketSeconds int `json:"bucketSeconds"`
	// CPU and Memory are utilisation fractions in 0..1, oldest first.
	CPU         []usagePoint         `json:"cpu,omitempty"`
	Memory      []usagePoint         `json:"memory,omitempty"`
	Filesystems []nodeFilesystemView `json:"filesystems,omitempty"`
}

// nodeFilesystemView is one mounted filesystem's fill. The mount points are the
// node's real disks: the collector's exclusion list has already dropped its
// thousand image layers.
type nodeFilesystemView struct {
	MountPoint    string       `json:"mountPoint"`
	Device        string       `json:"device,omitempty"`
	CapacityBytes uint64       `json:"capacityBytes,omitempty"`
	Used          []usagePoint `json:"used,omitempty"`
	// Latest is the newest observed fill, which is the number a table shows
	// beside the sparkline.
	Latest *float64 `json:"latest,omitempty"`
}

// usagePoint is one bucket of a series. Value is null for a bucket nothing was
// observed in, which is deliberately not zero: a scrape that did not happen is
// not a machine that was idle.
type usagePoint struct {
	Start time.Time `json:"start"`
	Value *float64  `json:"value"`
}

// nodeUsage reads node saturation out of the store, or says why it did not.
//
// The window and the bucket width are the signals catalogue's own, so the
// series a screen draws and the series `node.saturated` fires on are the same
// numbers rather than two readings that can disagree.
func (s *Server) nodeUsage(ctx context.Context) (map[string]*nodeUsageView, string) {
	if s.HostMetrics == nil {
		return nil, noHostMetricsMessage
	}
	now := time.Now().UTC()
	nodes, err := s.HostMetrics.NodeUsage(ctx, now.Add(-signals.ResourceWindow), now, signals.ResourceBucket)
	if err != nil {
		s.log().Error(err, "the node saturation query failed")
		return nil, "node saturation could not be read, so these series are absent rather than zero"
	}

	usage := make(map[string]*nodeUsageView, len(nodes))
	for _, node := range nodes {
		view := &nodeUsageView{
			BucketSeconds: int(node.BucketWidth.Seconds()),
			CPU:           usagePoints(node.CPU),
			Memory:        usagePoints(node.Memory),
		}
		for _, filesystem := range node.Filesystems {
			view.Filesystems = append(view.Filesystems, nodeFilesystemView{
				MountPoint:    filesystem.MountPoint,
				Device:        filesystem.Device,
				CapacityBytes: filesystem.CapacityBytes,
				Used:          usagePoints(filesystem.Used),
				Latest:        latestObserved(filesystem.Used),
			})
		}
		usage[node.Node] = view
	}
	return usage, ""
}

// usagePoints projects the gatherer's buckets into the series a screen draws.
func usagePoints(buckets []signals.Bucket) []usagePoint {
	if len(buckets) == 0 {
		return nil
	}
	points := make([]usagePoint, 0, len(buckets))
	for _, bucket := range buckets {
		point := usagePoint{Start: bucket.Start}
		if bucket.Observed {
			value := bucket.Value
			point.Value = &value
		}
		points = append(points, point)
	}
	return points
}

// latestObserved is the newest bucket that was actually measured, walking back
// past the partial one every series ends in.
func latestObserved(buckets []signals.Bucket) *float64 {
	for i := len(buckets) - 1; i >= 0; i-- {
		if buckets[i].Observed {
			value := buckets[i].Value
			return &value
		}
	}
	return nil
}

type nodeConditionView struct {
	Type    string    `json:"type"`
	Status  string    `json:"status"`
	Reason  string    `json:"reason,omitempty"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since"`
}

// nodeCapacityView is what the node says it has to give, in the units it said
// it in: "8" and "8000m" are the same CPU, and only one of them is the node's
// own word for it.
type nodeCapacityView struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Pods   string `json:"pods,omitempty"`
}

// nodeTelemetryView is when the store last heard from this node's collector.
type nodeTelemetryView struct {
	// LastSeen is absent for a node the store has nothing from inside the
	// lookback, which is what Silent means. Both absent and Silent false means
	// the freshness read did not happen at all.
	LastSeen *time.Time `json:"lastSeen,omitempty"`
	Silent   bool       `json:"silent"`
	// AgeSeconds is how long ago that was, so a browser does not have to trust
	// its own clock against the operator's.
	AgeSeconds float64 `json:"ageSeconds,omitempty"`
}

func newNodeView(node *corev1.Node, pods int) nodeView {
	view := nodeView{
		Name:        node.Name,
		Schedulable: !node.Spec.Unschedulable,
		Version:     node.Status.NodeInfo.KubeletVersion,
		CreatedAt:   node.CreationTimestamp.Time,
		Pods:        pods,
		Allocatable: nodeCapacityView{
			CPU:    quantityString(node.Status.Allocatable, corev1.ResourceCPU),
			Memory: quantityString(node.Status.Allocatable, corev1.ResourceMemory),
			Pods:   quantityString(node.Status.Allocatable, corev1.ResourcePods),
		},
	}
	view.Conditions = make([]nodeConditionView, 0, len(node.Status.Conditions))
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			view.Ready = condition.Status == corev1.ConditionTrue
		}
		view.Conditions = append(view.Conditions, nodeConditionView{
			Type:    string(condition.Type),
			Status:  string(condition.Status),
			Reason:  condition.Reason,
			Message: condition.Message,
			Since:   condition.LastTransitionTime.Time,
		})
	}
	for label := range node.Labels {
		if role, found := strings.CutPrefix(label, "node-role.kubernetes.io/"); found && role != "" {
			view.Roles = append(view.Roles, role)
		}
	}
	sort.Strings(view.Roles)
	return view
}

// telemetryOf reads one node out of the freshness answer.
//
// A node absent from that answer shipped nothing inside the lookback, which is
// the contract of the query behind it: silence is an absence rather than an old
// timestamp, so the query stays bounded to a window instead of scanning the
// whole retention to prove a negative. A freshness read that failed leaves
// every node neither fresh nor silent, because nothing was measured.
func telemetryOf(freshness map[string]time.Time, node, unread string, now time.Time) nodeTelemetryView {
	if unread != "" {
		return nodeTelemetryView{}
	}
	seen, known := freshness[node]
	if !known {
		return nodeTelemetryView{Silent: true}
	}
	age := now.Sub(seen)
	return nodeTelemetryView{
		LastSeen:   &seen,
		Silent:     age > signals.NodeSilentAfter,
		AgeSeconds: age.Seconds(),
	}
}

// telemetryFreshness reads when each node last shipped anything, answering with
// the reason instead where it could not.
//
// It never fails the request. Conditions and pod counts come from the API
// server and are worth showing on their own, and a screen that 503s because one
// column is unavailable is a screen that disappears exactly when the collection
// layer is the thing that broke.
func (s *Server) telemetryFreshness(ctx context.Context) (map[string]time.Time, string) {
	store, err := s.logStore(ctx)
	if err != nil {
		if errors.Is(err, errNoLogStore) {
			return nil, noStoreMessage
		}
		s.log().Error(err, "cannot reach the telemetry store for node freshness")
		return nil, noFreshnessMessage
	}
	rows, err := store.TelemetryFreshness(ctx, signals.FreshnessLookback)
	if err != nil {
		s.log().Error(err, "the telemetry freshness query failed")
		return nil, noFreshnessMessage
	}
	freshness := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		freshness[row.Node] = row.LastSeen
	}
	return freshness, ""
}

// platformIngest answers the Ingest screen: whether the platform is still
// hearing from its own collection layer, and what it knows it has lost.
//
// It is the silent-loss detector's data, in one place. The nodes' freshness
// says who stopped reporting; the collector DaemonSet's counts say whether its
// pods exist at all — the failure where a pod listing looks clean because
// admission refused every one of them; and the follower's ledger says how much
// of the flow stream Hubble reported dropping, which is the only reason to
// distrust a request count that looks perfectly plausible.
func (s *Server) platformIngest(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	nodes := &corev1.NodeList{}
	if err := s.reader().List(ctx, nodes); err != nil {
		s.writeError(w, err)
		return
	}
	collector, onNode, err := s.collectorStatus(ctx)
	if err != nil {
		s.writeError(w, err)
		return
	}

	freshness, unread := s.telemetryFreshness(ctx)
	body := platformIngestBody{
		Items:            make([]ingestNodeView, 0, len(nodes.Items)),
		Collector:        collector,
		Flows:            s.flowLoss(),
		TelemetryMessage: unread,
	}

	now := time.Now().UTC()
	for i := range nodes.Items {
		node := &nodes.Items[i]
		view := ingestNodeView{
			Node:      node.Name,
			Collector: onNode[node.Name],
			Telemetry: telemetryOf(freshness, node.Name, unread, now),
		}
		body.Items = append(body.Items, view)
		if view.Telemetry.Silent {
			body.SilentNodes++
		}
		if view.Collector == "" {
			body.NodesWithoutCollector++
		}
	}
	sort.Slice(body.Items, func(i, j int) bool { return body.Items[i].Node < body.Items[j].Node })

	writeJSON(w, http.StatusOK, body)
}

// platformIngestBody is the Ingest screen.
type platformIngestBody struct {
	Items       []ingestNodeView `json:"items"`
	SilentNodes int              `json:"silentNodes"`
	// NodesWithoutCollector is how many nodes have no collector pod at all,
	// which is the shape of the failure that leaves no pod to inspect.
	NodesWithoutCollector int `json:"nodesWithoutCollector"`

	// Collector is the node collector's own DaemonSet.
	Collector collectorView `json:"collector"`
	// Flows is what the follower knows it did not observe. Absent where no
	// follower is wired up at all, which is not the same as no loss.
	Flows *flowLossView `json:"flows,omitempty"`

	TelemetryMessage string `json:"telemetryMessage,omitempty"`
}

// ingestNodeView is one node's side of the pipeline.
type ingestNodeView struct {
	Node string `json:"node"`
	// Collector is what the collector pod on this node is doing — Running,
	// Pending, CrashLoopBackOff — and is empty where there is no pod at all.
	Collector string            `json:"collector,omitempty"`
	Telemetry nodeTelemetryView `json:"telemetry"`
}

// collectorView is the DaemonSet's own arithmetic: what it wants against what
// it has. A DaemonSet's desired count comes from the nodes it selects, so a
// non-zero Desired with nothing available is the admission failure, and Message
// says where the reason is.
type collectorView struct {
	Present   bool   `json:"present"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Desired   int32  `json:"desired"`
	Ready     int32  `json:"ready"`
	Available int32  `json:"available"`
	Message   string `json:"message,omitempty"`
}

// collectorStatus finds the node collector and where its pods are.
//
// It selects on the platform's labels rather than on a name, for the reason the
// component survey does: every name the chart generates carries the release
// name, so none of them is known at compile time.
func (s *Server) collectorStatus(ctx context.Context) (collectorView, map[string]string, error) {
	selector := map[string]string{labelPartOf: partOfKitchen, labelComponent: componentCollector}

	daemonSets := &appsv1.DaemonSetList{}
	if err := s.listPlatform(ctx, daemonSets, selector); err != nil {
		return collectorView{}, nil, err
	}
	pods := &corev1.PodList{}
	if err := s.listPlatform(ctx, pods, selector); err != nil {
		return collectorView{}, nil, err
	}

	onNode := make(map[string]string, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		state := string(pod.Status.Phase)
		for j := range pod.Status.ContainerStatuses {
			if reason := containerMessage(&pod.Status.ContainerStatuses[j]); reason != "" {
				state = reason
				break
			}
		}
		onNode[pod.Spec.NodeName] = state
	}

	if len(daemonSets.Items) == 0 {
		return collectorView{Message: "no DaemonSet in " + controller.PlatformNamespace + " carries " +
			labelComponent + "=" + componentCollector + ": either the node collector is not installed, " +
			"or it was installed by a chart older than these labels"}, onNode, nil
	}

	collector := &daemonSets.Items[0]
	view := collectorView{
		Present:   true,
		Name:      collector.Name,
		Namespace: collector.Namespace,
		Desired:   collector.Status.DesiredNumberScheduled,
		Ready:     collector.Status.NumberReady,
		Available: collector.Status.NumberAvailable,
	}
	if view.Desired > 0 && len(pods.Items) == 0 {
		view.Message = fmt.Sprintf(
			"the collector wants %d pods and has none at all: pods refused at admission leave nothing to "+
				"inspect, and the FailedCreate event on the DaemonSet is where the reason is — "+
				"the platform's Events screen has it", view.Desired)
	}
	return view, onNode, nil
}

// flowLossView is the follower's accounting, as the screen shows it.
type flowLossView struct {
	// Events is how many observations Relay reported dropping, and Notices how
	// many messages carried them — which is what separates one burst from a
	// stream that is losing events continuously.
	Events  uint64 `json:"events"`
	Notices uint64 `json:"notices"`
	// Reconnects is how many times the stream broke and was re-established.
	// Each is a gap of unknown size, which is why it is counted beside the
	// events rather than added to them.
	Reconnects uint64 `json:"reconnects"`
	// WindowSeconds is how far back the counts reach.
	WindowSeconds float64    `json:"windowSeconds"`
	Latest        *time.Time `json:"latest,omitempty"`
	// Lossless is the ordinary case, stated rather than left to be inferred
	// from three zeroes.
	Lossless bool `json:"lossless"`
}

func (s *Server) flowLoss() *flowLossView {
	if s.Flows == nil {
		return nil
	}
	loss := s.Flows.Loss(flows.LossWindow)
	view := &flowLossView{
		Events:        loss.Events,
		Notices:       loss.Notices,
		Reconnects:    loss.Reconnects,
		WindowSeconds: loss.Window.Seconds(),
		Lossless:      loss.Lossless(),
	}
	if !loss.Latest.IsZero() {
		latest := loss.Latest.UTC()
		view.Latest = &latest
	}
	return view
}

// listPlatform lists objects in the platform's own namespace, optionally
// narrowed to a set of labels.
func (s *Server) listPlatform(ctx context.Context, list client.ObjectList, selector map[string]string) error {
	options := []client.ListOption{client.InNamespace(controller.PlatformNamespace)}
	if len(selector) > 0 {
		options = append(options, client.MatchingLabels(selector))
	}
	return s.reader().List(ctx, list, options...)
}

// quantityString reads one quantity off a resource list as the API server
// spelled it, or nothing at all where the node did not report it.
func quantityString(list corev1.ResourceList, name corev1.ResourceName) string {
	if value, ok := list[name]; ok {
		return value.String()
	}
	return ""
}

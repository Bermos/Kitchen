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
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/flows"
)

// The two nodes every test here starts from: one healthy, one under memory
// pressure. They are named rather than numbered because half of these
// assertions are about which node an answer is about.
const (
	quietNode = "node-a"
	sickNode  = "node-b"
)

// node is a Ready node with whatever else the test wants said about it. Not
// being Ready is a condition like any other, so it is passed in rather than
// given a parameter of its own.
func node(name string, extra ...corev1.NodeCondition) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		},
		Status: corev1.NodeStatus{
			Conditions: append([]corev1.NodeCondition{{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
			}}, extra...),
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:  resource.MustParse("4"),
				corev1.ResourcePods: resource.MustParse("110"),
			},
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.1"},
		},
	}
}

// podOn is a pod placed on a node, in whatever shape the test needs.
func podOn(namespace, name, nodeName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

// The freshness column is the whole reason the Nodes screen exists: a node
// whose collector is dead reads healthy everywhere else.
func TestPlatformNodesReportsTheSilentOnes(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(),
		node(quietNode),
		node(sickNode, corev1.NodeCondition{
			Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue,
			Reason: "KubeletHasInsufficientMemory", Message: "node-b has insufficient memory",
		}),
		podOn("kitchen-shop", "shop-production-1", quietNode, corev1.PodRunning),
	)...)
	// node-a reported a minute ago; node-b is absent from the answer, which is
	// how the store says it has heard nothing at all.
	h.logs.freshness = []clickhouse.NodeFreshness{{
		Node: quietNode, LastSeen: time.Now().Add(-time.Minute).UTC(),
	}}

	res := h.do(t, http.MethodGet, "/api/v1/platform/nodes", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /platform/nodes = %d: %s", res.Code, res.Body.String())
	}
	body := decode[platformNodesBody](t, res)
	if body.Nodes != 2 || body.ReadyNodes != 2 || body.SilentNodes != 1 {
		t.Fatalf("expected two ready nodes, one of them silent: %+v", body)
	}
	if body.TelemetryMessage != "" {
		t.Errorf("the freshness read worked, so nothing should explain its absence: %q", body.TelemetryMessage)
	}

	quiet, sick := body.Items[0], body.Items[1]
	if quiet.Name != quietNode || quiet.Telemetry.Silent || quiet.Telemetry.LastSeen == nil {
		t.Errorf("%s reported a minute ago: %+v", quietNode, quiet.Telemetry)
	}
	if quiet.Pods != 1 || quiet.Allocatable.CPU != "4" || len(quiet.Roles) != 1 {
		t.Errorf("the node's own facts should survive: %+v", quiet)
	}
	if !sick.Telemetry.Silent || sick.Telemetry.LastSeen != nil {
		t.Errorf("%s is absent from the freshness answer, which is what silent means: %+v", sickNode, sick.Telemetry)
	}
	// The pressure condition is on the node itself, and it is the reading a
	// disk filling up announces itself in.
	if len(sick.Conditions) != 2 {
		t.Errorf("expected both conditions on %s: %+v", sickNode, sick.Conditions)
	}
	// Nothing reads host_metrics in this operator, so the series are absent and
	// the screen says why rather than drawing zeroes.
	if !strings.Contains(body.UsageMessage, "host_metrics") || quiet.Usage != nil {
		t.Errorf("saturation is not measured here, and should say so: %q / %+v", body.UsageMessage, quiet.Usage)
	}
}

// A store that cannot be reached must not make every node look silent — that is
// the same wrong answer this whole section exists to avoid, arrived at from the
// other side.
func TestPlatformNodesWithoutATelemetryStore(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), node(quietNode))...)
	h.logs.freshnessErr = errors.New("dial tcp 10.0.0.1:8123: connect: connection refused")

	res := h.do(t, http.MethodGet, "/api/v1/platform/nodes", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /platform/nodes = %d: %s", res.Code, res.Body.String())
	}
	body := decode[platformNodesBody](t, res)
	if body.Nodes != 1 || body.SilentNodes != 0 {
		t.Fatalf("an unreadable store measures no silence: %+v", body)
	}
	if body.TelemetryMessage == "" {
		t.Error("the screen must say why the freshness column is empty")
	}
	if body.Items[0].Telemetry.Silent || body.Items[0].Telemetry.LastSeen != nil {
		t.Errorf("neither fresh nor silent, because nothing was measured: %+v", body.Items[0].Telemetry)
	}
	// And the store's own diagnostic stays in the operator's log.
	if strings.Contains(res.Body.String(), "connection refused") {
		t.Errorf("the store's internals reached the browser: %s", res.Body.String())
	}
}

// The ingest screen is the silent-loss detector's data: who stopped reporting,
// whether the collector exists at all, and what the follower knows it lost.
func TestPlatformIngestSurfacesTheCollectorAndTheLoss(t *testing.T) {
	collector := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: controller.PlatformNamespace,
			Name:      "kitchen-collector",
			Labels:    map[string]string{labelPartOf: partOfKitchen, labelComponent: componentCollector},
		},
		// It wants a pod per node and has none: the shape of a DaemonSet whose
		// pods were refused at admission.
		Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2},
	}
	h := newHarness(t, nil, append(fixtures(), node(quietNode), node(sickNode), collector)...)
	h.server.Flows = stubFollower{loss: flows.Loss{
		Events: 4096, Notices: 3, Reconnects: 1, Window: flows.LossWindow, Latest: time.Now(),
	}}
	h.logs.freshness = []clickhouse.NodeFreshness{{Node: quietNode, LastSeen: time.Now().UTC()}}

	res := h.do(t, http.MethodGet, "/api/v1/platform/ingest", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /platform/ingest = %d: %s", res.Code, res.Body.String())
	}
	body := decode[platformIngestBody](t, res)
	if body.SilentNodes != 1 || body.NodesWithoutCollector != 2 {
		t.Fatalf("one silent node and no collector pods anywhere: %+v", body)
	}
	if !body.Collector.Present || body.Collector.Desired != 2 || body.Collector.Available != 0 {
		t.Fatalf("the collector's own counts should be reported: %+v", body.Collector)
	}
	if !strings.Contains(body.Collector.Message, "refused at admission") {
		t.Errorf("a DaemonSet with no pods at all has one explanation worth naming: %q", body.Collector.Message)
	}
	if body.Flows == nil || body.Flows.Events != 4096 || body.Flows.Lossless {
		t.Fatalf("the follower's loss belongs on this screen: %+v", body.Flows)
	}
	if body.Flows.WindowSeconds != flows.LossWindow.Seconds() {
		t.Errorf("the counts are per the follower's window, which the screen must carry: %+v", body.Flows)
	}
}

// stubFollower stands in for the Hubble follower, which the API only ever asks
// one question.
type stubFollower struct{ loss flows.Loss }

func (s stubFollower) Loss(time.Duration) flows.Loss { return s.loss }

// The Workloads screen's headline is the one a pod listing cannot carry: a
// workload that wants pods and has none, with the refusal that says why.
func TestPlatformWorkloadsSurfacesWorkloadsWithNoPods(t *testing.T) {
	refused := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: controller.PlatformNamespace,
			Name:      "kitchen-auth",
			Labels:    map[string]string{labelPartOf: partOfKitchen, labelComponent: "auth"},
		},
		Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
		Status: appsv1.DeploymentStatus{},
	}
	serving := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "kitchen-shop",
			Name:      testEnvironment,
			Labels: map[string]string{
				controller.LabelProject:     feedProject,
				controller.LabelEnvironment: testEnvironment,
			},
		},
		Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1},
	}
	pod := podOn("kitchen-shop", testEnvironment+"-5c9f7d6b4-abcde", quietNode, corev1.PodRunning)
	pod.OwnerReferences = []metav1.OwnerReference{{
		Kind: "ReplicaSet", Name: testEnvironment + "-5c9f7d6b4", Controller: ptr.To(true),
	}}
	pod.Labels = map[string]string{
		controller.LabelProject:     feedProject,
		controller.LabelEnvironment: testEnvironment,
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         controller.AppContainerName,
		RestartCount: 3,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: reasonOOMKilled, ExitCode: 137,
		}},
	}}

	h := newHarness(t, nil, append(fixtures(), refused, serving, pod)...)
	h.logs.k8sEvents = []clickhouse.K8sEvent{{
		Timestamp: time.Now().UTC(),
		Namespace: controller.PlatformNamespace,
		Kind:      kindDeployment,
		Name:      "kitchen-auth",
		Reason:    reasonFailedCreate,
		Message:   `pods "kitchen-auth-" is forbidden: violates PodSecurity "baseline:latest"`,
		Count:     12,
	}}

	res := h.do(t, http.MethodGet, "/api/v1/platform/workloads", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /platform/workloads = %d: %s", res.Code, res.Body.String())
	}
	body := decode[platformWorkloadsBody](t, res)
	if body.Workloads != 2 || body.WithoutPods != 1 {
		t.Fatalf("two workloads, one of them with no pods: %+v", body)
	}
	// The workload with nothing at all sorts first, because it is the worst
	// symptom on the screen.
	worst := body.Items[0]
	if worst.Name != "kitchen-auth" || worst.Pods != 0 || worst.Desired != 1 {
		t.Fatalf("the workload with no pods belongs at the top: %+v", worst)
	}
	if worst.Admission == nil || worst.Admission.Count != 12 {
		t.Fatalf("the FailedCreate that explains it belongs beside it: %+v", worst.Admission)
	}
	if !strings.Contains(worst.Admission.Message, "PodSecurity") ||
		!strings.Contains(worst.Admission.Suspect, "Pod Security") {
		t.Errorf("the refusal should be quoted and named: %+v", worst.Admission)
	}
	if worst.Component != "auth" {
		t.Errorf("a platform workload is named by its component label: %+v", worst)
	}

	// And the pods themselves, with the two facts nothing else carries.
	if len(body.Pods) != 1 || body.Totals.Restarts != 3 || body.Totals.OOMKills != 1 {
		t.Fatalf("the pod's restarts and its OOM kill should be counted: %+v", body)
	}
	served := body.Pods[0]
	if served.Project != feedProject || served.Environment != testEnvironment {
		t.Errorf("an application's pod is attributed to it: %+v", served)
	}
	if served.Workload != "ReplicaSet/"+testEnvironment+"-5c9f7d6b4" || !served.OOMKilled {
		t.Errorf("the pod should name its owner and its kill: %+v", served)
	}
	// The pod is credited to the Deployment its ReplicaSet belongs to, not to
	// the ReplicaSet, which is what makes the count comparable to `desired`.
	if body.Items[1].Pods != 1 {
		t.Errorf("the serving deployment should own its pod: %+v", body.Items[1])
	}
}

// The Edge screen answers with the platform's own objects even where the store
// cannot be reached: a Gateway that is not programmed is exactly what somebody
// opens this screen to find out.
func TestPlatformEdgeReadsTrafficAndTheEdgesOwnObjects(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.platformRequests = clickhouse.PlatformRequests{
		Requests: 1000, RequestsPerSecond: 0.27, Errors: 10, ErrorRate: 0.01, P95Ms: 120, Unrouted: 4,
	}
	h.logs.edgeEntries = map[string][]clickhouse.EdgeEntry{
		clickhouse.EdgeByRoute + "/" + clickhouse.RouteSortRequests:  {{Key: "/api/:id", Requests: 900}},
		clickhouse.EdgeByRoute + "/" + clickhouse.RouteSortErrorRate: {{Key: "/checkout", ErrorRate: 0.5}},
		clickhouse.EdgeByHost + "/" + clickhouse.RouteSortRequests:   {{Key: "shop.apps.example.com"}},
		clickhouse.EdgeByHost + "/" + clickhouse.RouteSortErrorRate:  {{Key: "old.example.com", ErrorRate: 1}},
		clickhouse.EdgeByEnvironment + "/" + clickhouse.RouteSortLatency: {{
			Key: testEnvironment, P95Ms: 900,
		}},
	}
	h.logs.unroutedHosts = []clickhouse.UnroutedHost{{Host: "stale.example.com", Requests: 400}}

	res := h.do(t, http.MethodGet, "/api/v1/platform/edge", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /platform/edge = %d: %s", res.Code, res.Body.String())
	}
	body := decode[edgeBody](t, res)
	if body.Requests.Requests != 1000 || body.Requests.Unrouted != 4 {
		t.Fatalf("the platform's headline should be the platform read's: %+v", body.Requests)
	}
	if len(body.TopRoutes) != 1 || body.WorstRoutes[0].Key != "/checkout" || body.LatencyLeaders[0].P95Ms != 900 {
		t.Fatalf("each ranking is its own read, and they must not be confused: %+v", body)
	}
	if len(body.Unrouted) != 1 || body.Unrouted[0].Host != "stale.example.com" {
		t.Errorf("the unrouted bucket belongs on this screen: %+v", body.Unrouted)
	}
	// A ratio needs a floor, or the worst host on the platform is whichever
	// scanner asked once and got a 404.
	for _, asked := range h.logs.lastEdgeBreakdowns {
		if asked.SortBy == clickhouse.RouteSortErrorRate && asked.MinRequests == 0 {
			t.Errorf("ranking by error rate without a floor ranks noise: %+v", asked)
		}
	}
	// The unrouted read has its own window, because "still asking" is a
	// question about a stretch of time rather than about whatever the operator
	// dragged the chart to.
	if h.logs.lastUnrouted.Since.IsZero() {
		t.Errorf("the unrouted bucket needs a window of its own: %+v", h.logs.lastUnrouted)
	}
}

// The traffic queries are the operator's own, so a store that refuses one is
// reporting a fault in Kitchen: the caller is told which read failed, and
// ClickHouse's diagnostic stays in the operator's log.
func TestAPlatformStoreFailureNamesTheReadAndLeaksNothing(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.platformErr = errors.New("Code: 47. DB::Exception: Unknown identifier: r.requests " +
		"(version 26.3.17.110 (official build))")

	res := h.do(t, http.MethodGet, "/api/v1/platform/edge", "")
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("GET /platform/edge = %d: %s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	if !strings.Contains(body, whatPlatformTraffic+" failed") {
		t.Errorf("the answer should name the read that failed: %s", body)
	}
	if strings.Contains(body, "DB::Exception") {
		t.Errorf("the store's internals reached the browser: %s", body)
	}
}

// The certificate table is the other half of the Edge screen, and the ACME
// error is the one string on it that must never be paraphrased.
func TestPlatformEdgeCarriesTheCertificateTable(t *testing.T) {
	certificate := unstructuredCertificate("kitchen-wildcard", "*.apps.example.com", false,
		"Failed to wait for order resource \"kitchen-wildcard-1-1\" to become ready: "+
			"order is in \"invalid\" state: rejecting request; DNS problem: NXDOMAIN")
	h := newHarness(t, nil, append(fixtures(), certificate)...)

	res := h.do(t, http.MethodGet, "/api/v1/platform/edge", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /platform/edge = %d: %s", res.Code, res.Body.String())
	}
	body := decode[edgeBody](t, res)
	if len(body.Certificates.Items) != 1 {
		t.Fatalf("expected the wildcard certificate: %+v", body.Certificates)
	}
	cert := body.Certificates.Items[0]
	if cert.Ready || !strings.Contains(cert.Message, "NXDOMAIN") {
		t.Errorf("the CA's own words are the useful half: %+v", cert)
	}
	if cert.DaysToExpiry == nil || *cert.DaysToExpiry > 11 || *cert.DaysToExpiry < 9 {
		t.Errorf("expected about ten days to expiry: %+v", cert.DaysToExpiry)
	}
}

// The Storage screen leads with the claim that never bound, because that is the
// first-install hang the prerequisites warn about.
func TestPlatformStorageReportsUnboundClaimsAndTheStore(t *testing.T) {
	pending := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kitchen-shop", Name: "shop-data"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	store := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: controller.PlatformNamespace,
			Name:      "data-kitchen-clickhouse-0",
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:    corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
		},
	}
	mounted := podOn(controller.PlatformNamespace, "kitchen-clickhouse-0", quietNode, corev1.PodRunning)
	mounted.Spec.Volumes = []corev1.Volume{{
		Name: "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: "data-kitchen-clickhouse-0",
		}},
	}}

	h := newHarness(t, nil, append(fixtures(), pending, store, mounted)...)
	h.logs.overview = clickhouse.MetricsOverview{StoreBytes: 5 << 30, StoreRowsPerSecond: 42}

	res := h.do(t, http.MethodGet, "/api/v1/platform/storage", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /platform/storage = %d: %s", res.Code, res.Body.String())
	}
	body := decode[platformStorageBody](t, res)
	if body.Volumes != 2 || body.Unbound != 1 {
		t.Fatalf("two volumes, one unbound: %+v", body)
	}

	var unbound, telemetry volumeView
	for _, item := range body.Items {
		if item.Name == "shop-data" {
			unbound = item
		}
		if item.Name == "data-kitchen-clickhouse-0" {
			telemetry = item
		}
	}
	if unbound.Project != feedProject || !strings.Contains(unbound.Message, "default") {
		t.Errorf("an unbound claim with no class names the suspect: %+v", unbound)
	}
	if len(telemetry.Pods) != 1 || telemetry.Pods[0] != "kitchen-clickhouse-0" {
		t.Errorf("a volume is worth nothing without what mounts it: %+v", telemetry)
	}
	// The store's own disk, judged against the claim underneath it.
	if body.Store.BytesOnDisk != 5<<30 || body.Store.CapacityBytes != 10<<30 {
		t.Fatalf("the store's size and its volume both belong here: %+v", body.Store)
	}
	if body.Store.UsedFraction < 0.49 || body.Store.UsedFraction > 0.51 {
		t.Errorf("used against capacity is the number that matters: %+v", body.Store)
	}
	if body.UsageMessage == "" || telemetry.Usage != nil {
		t.Errorf("nothing measures volume fill here, and the screen must say so: %q", body.UsageMessage)
	}
}

// The Events explorer facets what it returned, and says so: facets over a
// truncated page are a description of the page.
func TestPlatformEventsFacetsTheSelection(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	now := time.Now().UTC()
	h.logs.k8sEvents = []clickhouse.K8sEvent{
		{Timestamp: now, Reason: "FailedScheduling", Kind: "Pod", Namespace: "kitchen-shop", Node: quietNode},
		{Timestamp: now, Reason: "FailedScheduling", Kind: "Pod", Namespace: "kitchen-shop"},
		{Timestamp: now, Reason: "FailedMount", Kind: "Pod", Namespace: controller.PlatformNamespace},
	}

	res := h.do(t, http.MethodGet, "/api/v1/platform/events?search=insufficient&kind=Pod&limit=3", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /platform/events = %d: %s", res.Code, res.Body.String())
	}
	body := decode[platformEventsBody](t, res)
	if len(body.Items) != 3 || !body.Truncated {
		t.Fatalf("three events at a limit of three is a page, not a window: %+v", body)
	}
	facets := map[string][]eventFacetValue{}
	for _, facet := range body.Facets {
		facets[facet.Field] = facet.Values
	}
	if len(facets["reason"]) != 2 || facets["reason"][0].Value != "FailedScheduling" ||
		facets["reason"][0].Count != 2 {
		t.Errorf("the commonest reason leads its facet: %+v", facets["reason"])
	}
	if len(facets["node"]) != 1 {
		t.Errorf("events about an object rather than a machine carry no node: %+v", facets["node"])
	}
	// The filters reach the store rather than being applied to the page.
	if h.logs.lastK8sEvents.Search != "insufficient" || h.logs.lastK8sEvents.Kind != "Pod" {
		t.Errorf("the explorer's filters belong in the query: %+v", h.logs.lastK8sEvents)
	}
}

// unstructuredCertificate is a cert-manager Certificate as the API server would
// hand it over — addressed unstructured, the way the operator addresses every
// cert-manager kind.
func unstructuredCertificate(name, dnsName string, ready bool, message string) runtime.Object {
	status := "False"
	if ready {
		status = "True"
	}
	object := &unstructured.Unstructured{}
	object.SetUnstructuredContent(map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      name,
			"namespace": controller.PlatformNamespace,
		},
		"spec": map[string]any{"dnsNames": []any{dnsName}},
		"status": map[string]any{
			"notAfter": time.Now().Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"conditions": []any{map[string]any{
				"type":    "Ready",
				"status":  status,
				"reason":  "Failed",
				"message": message,
			}},
		},
	})
	return object
}

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
	"net/http"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// appNamespace is where the fixtures' workload runs — the namespace the
// operator derives from the project name.
const appNamespace = "kitchen-shop"

// workloadFixtures are the objects the environment reconciler would have
// materialized for the fixtures' production environment: a Deployment mid-
// rollout, one pod serving and one crash-looping, and the Service that fronts
// them. There is deliberately no HTTPRoute, because "the route is missing" is
// what the objects endpoint exists to make visible.
func workloadFixtures() []runtime.Object {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testEnvironment,
			Namespace: appNamespace,
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "kitchen", Operation: metav1.ManagedFieldsOperationApply},
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(2)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  controller.AppContainerName,
					Image: "registry.example.com/shop@sha256:1111",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
					},
				}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:     1,
			AvailableReplicas: 1,
			UpdatedReplicas:   2,
		},
	}
	serving := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shop-production-aaa",
			Namespace: appNamespace,
			Labels:    map[string]string{controller.LabelEnvironment: testEnvironment},
		},
		Spec: corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			StartTime:  &metav1.Time{Time: time.Now().Add(-2 * time.Hour)},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         controller.AppContainerName,
				RestartCount: 1,
				State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	crashing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shop-production-bbb",
			Namespace: appNamespace,
			Labels:    map[string]string{controller.LabelEnvironment: testEnvironment},
		},
		Spec: corev1.PodSpec{NodeName: "node-2"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			StartTime:  &metav1.Time{Time: time.Now().Add(-5 * time.Minute)},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         controller.AppContainerName,
				RestartCount: 4,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "CrashLoopBackOff",
					Message: "back-off 5m0s restarting failed container",
				}},
			}},
		},
	}
	// A pod of another environment in the same namespace: the endpoint must
	// select on the label, not on the namespace.
	stranger := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shop-pr-7-ccc",
			Namespace: appNamespace,
			Labels:    map[string]string{controller.LabelEnvironment: "shop-pr-7"},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: 9}},
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvironment, Namespace: appNamespace},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{controller.LabelEnvironment: testEnvironment}},
	}
	return []runtime.Object{deployment, serving, crashing, stranger, service}
}

func TestWorkloadReportsWhatIsRunning(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), workloadFixtures()...)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/environments/shop-production/workload", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	workload := decode[workloadView](t, recorder)

	if workload.Namespace != appNamespace || workload.Deployment != testEnvironment {
		t.Fatalf("want the environment's own Deployment, got %+v", workload)
	}
	want := replicaCountsView{Desired: 2, Ready: 1, Available: 1, Updated: 2}
	if workload.Replicas != want {
		t.Fatalf("want replicas %+v, got %+v", want, workload.Replicas)
	}
	// The environment's own two pods add up to five; another environment's pod
	// in the same namespace carries nine, which must not be counted here.
	if workload.Restarts != 5 {
		t.Fatalf("want 5 restarts across the environment's pods, got %d", workload.Restarts)
	}
	if len(workload.Pods) != 2 {
		t.Fatalf("want the environment's two pods, got %d: %+v", len(workload.Pods), workload.Pods)
	}
	if workload.Resources == nil || workload.Resources.CPURequest != "100m" ||
		workload.Resources.MemoryRequest != "128Mi" || workload.Resources.MemoryLimit != "256Mi" {
		t.Fatalf("want the container's resources as written, got %+v", workload.Resources)
	}
	if workload.Image != "registry.example.com/shop@sha256:1111" {
		t.Fatalf("want the running image, got %q", workload.Image)
	}
	// Uptime is measured from the oldest running pod, not from the newest.
	if workload.StartedAt == nil || time.Since(*workload.StartedAt) < time.Hour {
		t.Fatalf("want the oldest running pod's start time, got %v", workload.StartedAt)
	}
	if workload.Pods[1].Message != "CrashLoopBackOff: back-off 5m0s restarting failed container" {
		t.Fatalf("want the crash loop explained, got %q", workload.Pods[1].Message)
	}
	if workload.Pods[0].Node != "node-1" || !workload.Pods[0].Ready {
		t.Fatalf("want the serving pod ready on its node, got %+v", workload.Pods[0])
	}
}

func TestWorkloadSaysWhenNothingIsMaterialized(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/environments/shop-production/workload", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200 — an environment with no workload is a state, not an error — got %d", recorder.Code)
	}
	workload := decode[workloadView](t, recorder)
	if workload.Deployment != "" || workload.Message == "" {
		t.Fatalf("want an empty workload carrying an explanation, got %+v", workload)
	}
}

func TestWorkloadOfAnUnknownEnvironmentIs404(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/environments/nope/workload", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestObjectsCarryTheMaterializedManifests(t *testing.T) {
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvironment, Namespace: appNamespace},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"shop.apps.example.com"},
		},
	}
	objects := append(fixtures(), workloadFixtures()...)
	h := newHarness(t, nil, append(objects, route)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/environments/shop-production/objects", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[objectsView](t, recorder)
	if len(view.Objects) != 3 {
		t.Fatalf("want the three objects an environment is made of, got %+v", view.Objects)
	}

	deployment := view.Objects[0]
	if deployment.Kind != "Deployment" || deployment.APIVersion != "apps/v1" || !deployment.Present {
		t.Fatalf("want the Deployment present and named, got %+v", deployment)
	}
	// A typed read leaves kind and apiVersion empty; the manifest has to
	// carry them or it is not a manifest.
	if deployment.Manifest["kind"] != "Deployment" || deployment.Manifest["apiVersion"] != "apps/v1" {
		t.Fatalf("want kind and apiVersion in the manifest, got %+v", deployment.Manifest)
	}
	meta, ok := deployment.Manifest["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("want metadata in the manifest, got %+v", deployment.Manifest)
	}
	if _, ok := meta["managedFields"]; ok {
		t.Fatal("want managedFields dropped from the manifest")
	}
	if view.Objects[2].Kind != "HTTPRoute" || !view.Objects[2].Present {
		t.Fatalf("want the HTTPRoute present, got %+v", view.Objects[2])
	}
}

func TestObjectsCarryTheScaledObjectOnlyWhereSomethingIdles(t *testing.T) {
	// The scaled object is listed off the Environment's own condition. On a
	// platform without the HTTP add-on that API is not served at all, and an
	// entry for it on every environment would be an error line for an object
	// none of them were ever meant to have.
	h := newHarness(t, nil, append(fixtures(), workloadFixtures()...)...)
	view := decode[objectsView](t, h.do(t, http.MethodGet,
		"/api/v1/environments/shop-production/objects", ""))
	for _, object := range view.Objects {
		if object.Kind == "HTTPScaledObject" {
			t.Fatalf("want no scaled object on a platform that idles nothing, got %+v", view.Objects)
		}
	}

	gvk := controller.HTTPScaledObjectGVK()
	scaled := &unstructured.Unstructured{}
	scaled.SetGroupVersionKind(gvk)
	scaled.SetName(testEnvironment)
	scaled.SetNamespace(appNamespace)

	objects := append(fixtures(), workloadFixtures()...)
	for _, object := range objects {
		env, ok := object.(*kitchenv1alpha1.Environment)
		if !ok || env.Name != testEnvironment {
			continue
		}
		env.Status.Conditions = []metav1.Condition{{
			Type:               controller.ConditionScaleToZero,
			Status:             metav1.ConditionTrue,
			Reason:             "IdlesToZero",
			LastTransitionTime: metav1.Now(),
		}}
	}
	h = newHarness(t, nil, append(objects, scaled)...)

	view = decode[objectsView](t, h.do(t, http.MethodGet,
		"/api/v1/environments/shop-production/objects", ""))
	if len(view.Objects) != 4 {
		t.Fatalf("want the scaled object listed as well, got %+v", view.Objects)
	}
	last := view.Objects[3]
	if last.Kind != gvk.Kind || last.APIVersion != gvk.GroupVersion().String() || !last.Present {
		t.Fatalf("want the scaled object present and named, got %+v", last)
	}
}

func TestObjectsReportWhatWasNeverMaterialized(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), workloadFixtures()...)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/environments/shop-production/objects", "")
	view := decode[objectsView](t, recorder)

	route := view.Objects[2]
	if route.Kind != "HTTPRoute" || route.Present || route.Manifest != nil || route.Message == "" {
		t.Fatalf("want the missing HTTPRoute reported as missing, got %+v", route)
	}
}

// platformFixtures are a cluster with three nodes, one of them not ready, and
// a build queue with one build running and one waiting.
func platformFixtures() []runtime.Object {
	node := func(name string, ready corev1.ConditionStatus) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}},
			},
		}
	}
	running := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-running", Namespace: testNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Git:        kitchenv1alpha1.GitRevision{SHA: "1111111111111", Branch: "main"},
		},
		Status: kitchenv1alpha1.BuildStatus{Phase: kitchenv1alpha1.BuildRunning},
	}
	queued := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-queued", Namespace: testNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Git:        kitchenv1alpha1.GitRevision{SHA: "2222222222222", Branch: "main"},
		},
		Status: kitchenv1alpha1.BuildStatus{Phase: kitchenv1alpha1.BuildQueued},
	}
	return []runtime.Object{
		node("node-1", corev1.ConditionTrue),
		node("node-2", corev1.ConditionTrue),
		node("node-3", corev1.ConditionFalse),
		running, queued,
	}
}

func TestStatusReportsTheClusterTheTunnelAndTheQueue(t *testing.T) {
	// A Kitchen of its own, so it can name a cluster and a tunnel — which
	// means naming the issuer the tokens below are minted by too.
	iss := newIssuer(t)
	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: controller.KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			BaseDomain:  "apps.example.com",
			ClusterName: "chef",
			Auth:        kitchenv1alpha1.AuthSpec{Enabled: true, Host: iss.url()},
			Ingress: kitchenv1alpha1.IngressSpec{
				Cloudflared: kitchenv1alpha1.CloudflaredSpec{Enabled: true},
			},
			Builds: kitchenv1alpha1.BuildsSpec{Concurrency: 2},
		},
		Status: kitchenv1alpha1.KitchenStatus{
			GatewayAddress: "203.0.113.7",
			Conditions: []metav1.Condition{
				{Type: "GatewayProgrammed", Status: metav1.ConditionTrue, Reason: "Programmed",
					LastTransitionTime: metav1.Now()},
				{Type: "TunnelConnected", Status: metav1.ConditionTrue, Reason: "TunnelRunning",
					Message: "cloudflared is available", LastTransitionTime: metav1.Now()},
			},
			Components: []kitchenv1alpha1.ComponentStatus{
				{Name: "logs", Kind: "DaemonSet", Healthy: false, Available: 0, Desired: 3,
					Message: "0 of 3 pods available"},
			},
		},
	}
	h := newHarness(t, kitchen, append(fixtures(), platformFixtures()...)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/status", "", iss.token(t))
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	status := decode[statusView](t, recorder)

	if status.Cluster.Name != "chef" || ptr.Deref(status.Cluster.Nodes, 0) != 3 ||
		ptr.Deref(status.Cluster.ReadyNodes, 0) != 2 {
		t.Fatalf("want chef with 2 of 3 nodes ready, got %+v", status.Cluster)
	}
	if status.Tunnel == nil || !status.Tunnel.Enabled || !status.Tunnel.Connected {
		t.Fatalf("want the tunnel connected, got %+v", status.Tunnel)
	}
	if status.Builds.Running != 1 || status.Builds.Capacity != 2 || status.Builds.Queued != 1 {
		t.Fatalf("want one running and one queued against a limit of two, got %+v", status.Builds)
	}
	// The count says the queue is busy; only the wait says whether it is
	// moving, so the queued build has to arrive with one.
	if len(status.Builds.Waiting) != 1 || status.Builds.Waiting[0].Project != feedProject {
		t.Fatalf("want the queued build named, got %+v", status.Builds.Waiting)
	}
	if status.Builds.OldestWaitSeconds < 1 || status.Builds.OldestWaitSeconds != status.Builds.Waiting[0].WaitSeconds {
		t.Fatalf("want the longest wait reported and matching its build, got %+v", status.Builds)
	}
	if status.Gateway == nil || !status.Gateway.Programmed || status.Gateway.Address != "203.0.113.7" {
		t.Fatalf("want the gateway programmed at its address, got %+v", status.Gateway)
	}
	if len(status.Components) != 1 || status.Components[0].Healthy {
		t.Fatalf("want the survey's unhealthy component, got %+v", status.Components)
	}
}

func TestStatusFallsBackToTheBaseDomainAndTheDefaultConcurrency(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	status := decode[statusView](t, h.do(t, http.MethodGet, "/api/v1/status", ""))

	// The default harness names no cluster and sets no concurrency.
	if status.Cluster.Name != "apps" {
		t.Fatalf("want the base domain's first label, got %q", status.Cluster.Name)
	}
	if status.Builds.Capacity != controller.DefaultBuildConcurrency {
		t.Fatalf("want the build controller's own default, got %d", status.Builds.Capacity)
	}
	if status.Tunnel == nil || status.Tunnel.Enabled || status.Tunnel.Connected {
		t.Fatalf("want no tunnel on an installation that runs none, got %+v", status.Tunnel)
	}
}

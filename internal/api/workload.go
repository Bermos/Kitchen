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
	"fmt"
	"net/http"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// An Environment's phase says whether it is live. These two endpoints say what
// "live" is made of: the Deployment's replica counts and its pods, and the
// objects the reconciler materialized to get there. Everything here is read
// out of the cluster at request time — the platform keeps no second copy of a
// pod's restart count, and one that lagged would be worse than none.

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// environmentWorkload answers what is running for one Environment.
func (s *Server) environmentWorkload(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}

	appNS := controller.AppNamespace(env.Spec.ProjectRef.Name)
	deploy := &appsv1.Deployment{}
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: appNS, Name: env.Name}, deploy)
	if apierrors.IsNotFound(err) {
		// Not an error: an environment whose route is withheld, or one the
		// reconciler has not reached yet, has no workload and says so. Its
		// conditions carry the reason.
		writeJSON(w, http.StatusOK, workloadView{
			Environment: env.Name,
			Namespace:   appNS,
			Message: fmt.Sprintf(
				"no Deployment %q in namespace %q yet: nothing has been materialized for this environment — its conditions say why",
				env.Name, appNS),
		})
		return
	}
	if err != nil {
		s.writeError(w, err)
		return
	}

	pods := &corev1.PodList{}
	if err := s.reader().List(ctx, pods,
		client.InNamespace(appNS),
		client.MatchingLabels{controller.LabelEnvironment: env.Name},
	); err != nil {
		s.writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, newWorkloadView(env.Name, appNS, deploy, pods.Items))
}

func newWorkloadView(name, appNS string, deploy *appsv1.Deployment, pods []corev1.Pod) workloadView {
	view := workloadView{
		Environment: name,
		Namespace:   appNS,
		Deployment:  deploy.Name,
		Replicas: replicaCountsView{
			Desired:   replicasOrOne(deploy.Spec.Replicas),
			Ready:     deploy.Status.ReadyReplicas,
			Available: deploy.Status.AvailableReplicas,
			Updated:   deploy.Status.UpdatedReplicas,
		},
	}
	if container := appContainer(deploy); container != nil {
		view.Image = container.Image
		view.Resources = newResourcesView(container.Resources)
	}

	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	for i := range pods {
		pod := newPodView(&pods[i])
		view.Restarts += pod.Restarts
		view.Pods = append(view.Pods, pod)
		// Uptime is the oldest pod still running: a rollout that replaced one
		// pod ten seconds ago has not reset how long the workload has served.
		if pod.StartedAt != nil && pods[i].Status.Phase == corev1.PodRunning &&
			(view.StartedAt == nil || pod.StartedAt.Before(*view.StartedAt)) {
			view.StartedAt = pod.StartedAt
		}
	}
	return view
}

// appContainer is the container the application runs in, falling back to the
// first one for a Deployment an older operator wrote under another name.
func appContainer(deploy *appsv1.Deployment) *corev1.Container {
	containers := deploy.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name == controller.AppContainerName {
			return &containers[i]
		}
	}
	if len(containers) > 0 {
		return &containers[0]
	}
	return nil
}

// newResourcesView reports the requests and limits as written, and nothing at
// all when the release set neither — an empty resources block is a fact about
// the release, and four empty strings would only look like a failed read.
func newResourcesView(resources corev1.ResourceRequirements) *resourcesView {
	quantity := func(list corev1.ResourceList, name corev1.ResourceName) string {
		if value, ok := list[name]; ok {
			return value.String()
		}
		return ""
	}
	view := resourcesView{
		CPURequest:    quantity(resources.Requests, corev1.ResourceCPU),
		CPULimit:      quantity(resources.Limits, corev1.ResourceCPU),
		MemoryRequest: quantity(resources.Requests, corev1.ResourceMemory),
		MemoryLimit:   quantity(resources.Limits, corev1.ResourceMemory),
	}
	if view == (resourcesView{}) {
		return nil
	}
	return &view
}

func newPodView(pod *corev1.Pod) podView {
	view := podView{
		Name:  pod.Name,
		Phase: string(pod.Status.Phase),
		Node:  pod.Spec.NodeName,
	}
	if at := pod.Status.StartTime; at != nil {
		started := at.Time
		view.StartedAt = &started
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			view.Ready = condition.Status == corev1.ConditionTrue
		}
	}
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		view.Restarts += status.RestartCount
		if view.Message == "" {
			view.Message = containerMessage(status)
		}
	}
	if view.Message == "" && !view.Ready {
		view.Message = pod.Status.Message
	}
	return view
}

// containerMessage is why a container is not serving: the waiting reason a
// pull failure or a crash loop leaves behind, or the exit that ended the last
// run. A container that is running has nothing to explain.
func containerMessage(status *corev1.ContainerStatus) string {
	if waiting := status.State.Waiting; waiting != nil {
		if waiting.Message != "" {
			return fmt.Sprintf("%s: %s", waiting.Reason, waiting.Message)
		}
		return waiting.Reason
	}
	if terminated := status.State.Terminated; terminated != nil {
		return fmt.Sprintf("%s: exit code %d", terminated.Reason, terminated.ExitCode)
	}
	return ""
}

// environmentObjects answers with the Kubernetes objects the reconciler
// materialized for an Environment — operator mode's inspect surface.
//
// It is the platform explaining itself, so the objects are not translated into
// the API's own vocabulary: whoever opens this wants the manifest they would
// have run `kubectl get -o yaml` for, and a summarized one would send them to
// the terminal anyway.
// inspectedObject is one of the objects an Environment is made of, and the
// empty value to read it into.
type inspectedObject struct {
	kind       string
	apiVersion string
	into       client.Object
}

func (s *Server) environmentObjects(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}

	appNS := controller.AppNamespace(env.Spec.ProjectRef.Name)
	view := objectsView{Environment: env.Name, Namespace: appNS, Objects: []materializedObjectView{}}

	// The three objects an Environment is made of, in the order the
	// reconciler creates them — which is also the order they fail in.
	objects := []inspectedObject{
		{"Deployment", "apps/v1", &appsv1.Deployment{}},
		{"Service", "v1", &corev1.Service{}},
		{"HTTPRoute", gatewayv1.GroupVersion.String(), &gatewayv1.HTTPRoute{}},
	}
	// A fourth, wherever the platform idles environments: the record KEDA
	// parks this one by. It is listed off the condition rather than always,
	// because on a platform without the HTTP add-on that API is not served,
	// and every environment would report an error for an object none of them
	// were ever meant to have.
	if meta.FindStatusCondition(env.Status.Conditions, controller.ConditionScaleToZero) != nil {
		gvk := controller.HTTPScaledObjectGVK()
		scaled := &unstructured.Unstructured{}
		scaled.SetGroupVersionKind(gvk)
		objects = append(objects, inspectedObject{gvk.Kind, gvk.GroupVersion().String(), scaled})
	}
	for _, object := range objects {
		view.Objects = append(view.Objects,
			s.materializedObject(ctx, object.kind, object.apiVersion, appNS, env.Name, object.into))
	}

	writeJSON(w, http.StatusOK, view)
}

func (s *Server) materializedObject(
	ctx context.Context,
	kind, apiVersion, namespace, name string,
	into client.Object,
) materializedObjectView {
	view := materializedObjectView{Kind: kind, APIVersion: apiVersion, Name: name, Namespace: namespace}

	err := s.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, into)
	switch {
	case apierrors.IsNotFound(err):
		view.Message = "not materialized"
		return view
	case err != nil:
		// One unreadable object does not spoil the rest: an installation
		// whose Gateway API CRDs are a version behind still has a Deployment
		// worth showing, and the message says which is which.
		view.Message = err.Error()
		return view
	}

	manifest, err := manifestOf(into, kind, apiVersion)
	if err != nil {
		view.Message = err.Error()
		return view
	}
	view.Present = true
	view.Manifest = manifest
	return view
}

// manifestOf turns a typed object into the manifest a reader expects: kind and
// apiVersion restored — a typed client read leaves them empty — and the
// server-side bookkeeping dropped.
func manifestOf(object client.Object, kind, apiVersion string) (map[string]any, error) {
	manifest, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return nil, err
	}
	manifest["kind"] = kind
	manifest["apiVersion"] = apiVersion
	if meta, ok := manifest["metadata"].(map[string]any); ok {
		delete(meta, "managedFields")
		if annotations, ok := meta["annotations"].(map[string]any); ok {
			delete(annotations, corev1.LastAppliedConfigAnnotation)
			if len(annotations) == 0 {
				delete(meta, "annotations")
			}
		}
	}
	return manifest, nil
}

// replicasOrOne applies the API's default for an unset replica count, the same
// way the operator's own component survey does.
func replicasOrOne(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

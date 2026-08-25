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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// Every pod on the platform, applications and platform components alike — and,
// more importantly, every workload that has no pods at all.
//
// That is the whole reason this screen is not just a pod listing. A workload
// whose pods are refused at admission has nothing to show: the pod never
// existed, so nothing is Pending, nothing is CrashLooping, and a listing of
// pods is a listing of the healthy ones. It is what left the log collector dead
// for hours on a cluster whose every condition read True, and it is the trick
// the component survey applies inside `kitchen-system`. Here it is applied to
// the whole cluster: each workload's desired count against what it actually
// has, and the FailedCreate warning that says why where the two differ.

// The workload kinds this screen walks, in the order a reader thinks about
// them.
const (
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	kindDaemonSet   = "DaemonSet"
)

// How many pods one answer carries, and the ceiling. The cut is bounded because
// a platform with three hundred previews has thousands of pods and a screen
// that renders all of them helps nobody — and it is safe because the listing is
// sorted with the unhealthy first, so what the limit drops is always pods that
// are running normally.
const (
	defaultWorkloadPods = 500
	maxWorkloadPods     = 2000
)

// admissionWindow is how far back the FailedCreate warnings are read. They come
// from the events table rather than from the API server because Kubernetes
// expires an event about an hour after it happens, and a workload refused since
// yesterday's deploy would otherwise explain itself with silence.
const admissionWindow = 24 * time.Hour

// reasonFailedCreate is what a controller that could not create a pod reports.
// It is the kubelet's and the controller manager's own string, and there is no
// constant for it in client-go.
const reasonFailedCreate = "FailedCreate"

// platformWorkloads answers the Workloads screen.
func (s *Server) platformWorkloads(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	limit, err := intParam(req, "limit", defaultWorkloadPods)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if limit > maxWorkloadPods {
		limit = maxWorkloadPods
	}
	namespace := strings.TrimSpace(req.URL.Query().Get("namespace"))

	pods := &corev1.PodList{}
	if err := s.reader().List(ctx, pods); err != nil {
		s.writeError(w, err)
		return
	}
	workloads, err := s.listWorkloads(ctx)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := platformWorkloadsBody{
		Items: make([]workloadSummaryView, 0, len(workloads)),
		Pods:  make([]platformPodView, 0, len(pods.Items)),
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if namespace != "" && pod.Namespace != namespace {
			continue
		}
		view := newPlatformPodView(pod)
		body.Pods = append(body.Pods, view)
		body.Totals.add(view)
	}

	// The refusals, read once for every workload rather than per workload: one
	// window of the cluster's warnings, indexed by the object they are about.
	refusals, unread := s.admissionRefusals(ctx)
	body.EventsMessage = unread

	podsPerWorkload := podCounts(pods.Items)
	for _, workload := range workloads {
		if namespace != "" && workload.namespace != namespace {
			continue
		}
		view := workload.view(podsPerWorkload[workload.key()])
		if view.Pods == 0 && view.Desired > 0 {
			view.Admission = refusals[workload.key()]
		}
		body.Items = append(body.Items, view)
		body.Workloads++
		if !view.Healthy {
			body.Unhealthy++
		}
		if view.Pods == 0 && view.Desired > 0 {
			body.WithoutPods++
		}
	}
	sort.Slice(body.Items, func(i, j int) bool { return body.Items[i].less(body.Items[j]) })
	sort.Slice(body.Pods, func(i, j int) bool { return body.Pods[i].less(body.Pods[j]) })

	body.Totals.Pods = len(body.Pods)
	if len(body.Pods) > limit {
		body.Pods = body.Pods[:limit]
		body.Truncated = true
	}
	writeJSON(w, http.StatusOK, body)
}

// platformWorkloadsBody is the Workloads screen: the workloads first, because a
// workload with no pods is the thing a pod listing cannot show.
type platformWorkloadsBody struct {
	Items []workloadSummaryView `json:"items"`
	Pods  []platformPodView     `json:"pods"`

	Workloads int `json:"workloads"`
	Unhealthy int `json:"unhealthy"`
	// WithoutPods is how many workloads want pods and have none. It is called
	// out separately because it is the one number on this screen that a pod
	// listing can never contain.
	WithoutPods int `json:"withoutPods"`

	Totals podTotals `json:"totals"`
	// Truncated says the pod listing was cut at the limit. The cut never hides
	// a problem: the listing is sorted worst first.
	Truncated bool `json:"truncated"`
	// EventsMessage says why the admission column is empty, and is empty when
	// it is not.
	EventsMessage string `json:"eventsMessage,omitempty"`
}

// podTotals is the platform's pods counted by what they are doing.
type podTotals struct {
	Pods     int `json:"pods"`
	Running  int `json:"running"`
	Pending  int `json:"pending"`
	Failed   int `json:"failed"`
	NotReady int `json:"notReady"`
	// Restarts and OOMKills are over the pods that exist now, read off the API
	// server: exact, and available even where nothing was collected.
	Restarts int32 `json:"restarts"`
	OOMKills int   `json:"oomKills"`
}

func (t *podTotals) add(pod platformPodView) {
	switch pod.Phase {
	case string(corev1.PodRunning):
		t.Running++
	case string(corev1.PodPending):
		t.Pending++
	case string(corev1.PodFailed):
		t.Failed++
	}
	if !pod.Ready && pod.Phase != string(corev1.PodSucceeded) {
		t.NotReady++
	}
	t.Restarts += pod.Restarts
	if pod.OOMKilled {
		t.OOMKills++
	}
}

// workloadSummaryView is one Deployment, StatefulSet or DaemonSet.
type workloadSummaryView struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Project and Environment attribute an application's workload; Component
	// names a platform one. A workload is one or the other, never both.
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Component   string `json:"component,omitempty"`

	Desired   int32 `json:"desired"`
	Ready     int32 `json:"ready"`
	Available int32 `json:"available"`
	// Pods is how many exist, which is not derivable from the counts above:
	// zero available can mean pods that are failing or pods that were never
	// created, and only this tells them apart.
	Pods    int  `json:"pods"`
	Healthy bool `json:"healthy"`

	// Admission is the FailedCreate warning that explains a workload with no
	// pods, verbatim, where there is one.
	Admission *admissionRefusalView `json:"admission,omitempty"`
}

// less orders the workloads worst first: the ones with no pods at all, then the
// merely unhealthy, then by name.
func (v workloadSummaryView) less(other workloadSummaryView) bool {
	rank := func(view workloadSummaryView) int {
		switch {
		case view.Desired > 0 && view.Pods == 0:
			return 0
		case !view.Healthy:
			return 1
		default:
			return 2
		}
	}
	if left, right := rank(v), rank(other); left != right {
		return left < right
	}
	if v.Namespace != other.Namespace {
		return v.Namespace < other.Namespace
	}
	return v.Name < other.Name
}

// admissionRefusalView is one FailedCreate, as the cluster worded it.
type admissionRefusalView struct {
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	Count   uint32    `json:"count"`
	At      time.Time `json:"at"`
	// Suspect names what the message betrays where it betrays anything — Pod
	// Security is the one this screen exists for, and it is worth naming rather
	// than leaving in a wall of admission-webhook prose.
	Suspect string `json:"suspect,omitempty"`
}

// platformPodView is one pod, wherever it runs.
type platformPodView struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node,omitempty"`
	// Workload is the object that owns it, as `Kind/name`, where the owner is
	// one this screen also lists.
	Workload    string `json:"workload,omitempty"`
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`

	Phase     string     `json:"phase"`
	Ready     bool       `json:"ready"`
	Restarts  int32      `json:"restarts"`
	OOMKilled bool       `json:"oomKilled"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	// Message is why it is not serving: the waiting reason, the exit that ended
	// the last run, or the scheduler's complaint for a pod it can place
	// nowhere. Empty for a pod that is simply running.
	Message string `json:"message,omitempty"`
}

// less orders the pods worst first, so that the listing's limit can only ever
// cut healthy ones.
func (v platformPodView) less(other platformPodView) bool {
	rank := func(view platformPodView) int {
		switch {
		case view.Phase == string(corev1.PodFailed), view.OOMKilled:
			return 0
		case view.Phase == string(corev1.PodPending):
			return 1
		case !view.Ready && view.Phase != string(corev1.PodSucceeded):
			return 2
		case view.Restarts > 0:
			return 3
		default:
			return 4
		}
	}
	if left, right := rank(v), rank(other); left != right {
		return left < right
	}
	if v.Namespace != other.Namespace {
		return v.Namespace < other.Namespace
	}
	return v.Name < other.Name
}

func newPlatformPodView(pod *corev1.Pod) platformPodView {
	view := platformPodView{
		Namespace:   pod.Namespace,
		Name:        pod.Name,
		Node:        pod.Spec.NodeName,
		Project:     pod.Labels[controller.LabelProject],
		Environment: pod.Labels[controller.LabelEnvironment],
		Phase:       string(pod.Status.Phase),
	}
	if owner := controllerOwner(pod.OwnerReferences); owner != nil {
		view.Workload = owner.Kind + "/" + owner.Name
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
	for _, statuses := range podContainerStatuses(pod) {
		for i := range statuses {
			status := &statuses[i]
			view.Restarts += status.RestartCount
			if oomKilled(status) {
				view.OOMKilled = true
			}
		}
	}
	// Which container to believe is decided once, in podMessage, and for the
	// same reason on both screens: the first container with something to say
	// is very often the one that worked.
	view.Message = podMessage(pod)
	return view
}

// oomKilled reads the one termination reason worth a column of its own: the
// kernel took the decision, and the fix is a limit rather than a bug. Both
// states are read, because by the time anyone looks the container has usually
// restarted and the kill is on the previous run.
func oomKilled(status *corev1.ContainerStatus) bool {
	for _, terminated := range []*corev1.ContainerStateTerminated{
		status.State.Terminated,
		status.LastTerminationState.Terminated,
	} {
		if terminated != nil && terminated.Reason == reasonOOMKilled {
			return true
		}
	}
	return false
}

// controllerOwner is the object that manages a pod: the ReplicaSet a Deployment
// made, or the StatefulSet or DaemonSet itself.
func controllerOwner(owners []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range owners {
		if owners[i].Controller != nil && *owners[i].Controller {
			return &owners[i]
		}
	}
	return nil
}

// workloadKey identifies a workload across the two reads it is assembled from —
// the object itself, and the events about it.
type workloadKey struct {
	kind      string
	namespace string
	name      string
}

// platformWorkload is one workload, flattened out of the three kinds so that
// the screen's arithmetic is written once.
type platformWorkload struct {
	kind      string
	namespace string
	name      string
	labels    map[string]string
	desired   int32
	ready     int32
	available int32
}

func (w platformWorkload) key() workloadKey {
	return workloadKey{kind: w.kind, namespace: w.namespace, name: w.name}
}

func (w platformWorkload) view(pods int) workloadSummaryView {
	return workloadSummaryView{
		Kind:        w.kind,
		Namespace:   w.namespace,
		Name:        w.name,
		Project:     w.labels[controller.LabelProject],
		Environment: w.labels[controller.LabelEnvironment],
		Component:   w.labels[labelComponent],
		Desired:     w.desired,
		Ready:       w.ready,
		Available:   w.available,
		Pods:        pods,
		Healthy:     w.available >= w.desired,
	}
}

// listWorkloads reads the three kinds that own pods.
//
// One kind failing fails the whole read rather than answering with the other
// two: this screen's headline is "these workloads have no pods", and a list
// that came back short because it errored looks exactly like a platform missing
// a workload.
func (s *Server) listWorkloads(ctx context.Context) ([]platformWorkload, error) {
	deployments := &appsv1.DeploymentList{}
	statefulSets := &appsv1.StatefulSetList{}
	daemonSets := &appsv1.DaemonSetList{}
	if err := s.reader().List(ctx, deployments); err != nil {
		return nil, err
	}
	if err := s.reader().List(ctx, statefulSets); err != nil {
		return nil, err
	}
	if err := s.reader().List(ctx, daemonSets); err != nil {
		return nil, err
	}

	workloads := make([]platformWorkload, 0,
		len(deployments.Items)+len(statefulSets.Items)+len(daemonSets.Items))
	for i := range deployments.Items {
		item := &deployments.Items[i]
		workloads = append(workloads, platformWorkload{
			kind: kindDeployment, namespace: item.Namespace, name: item.Name,
			labels:    item.Labels,
			desired:   replicasOrOne(item.Spec.Replicas),
			ready:     item.Status.ReadyReplicas,
			available: item.Status.AvailableReplicas,
		})
	}
	for i := range statefulSets.Items {
		item := &statefulSets.Items[i]
		// ReadyReplicas rather than AvailableReplicas, the same way the
		// component survey reads them: both are populated on a current API
		// server, and only ReadyReplicas has been there as long as the kinds
		// this chart deploys have.
		workloads = append(workloads, platformWorkload{
			kind: kindStatefulSet, namespace: item.Namespace, name: item.Name,
			labels:    item.Labels,
			desired:   replicasOrOne(item.Spec.Replicas),
			ready:     item.Status.ReadyReplicas,
			available: item.Status.ReadyReplicas,
		})
	}
	for i := range daemonSets.Items {
		item := &daemonSets.Items[i]
		// A DaemonSet's desired count comes from the nodes it selects, so it is
		// only ever readable from status.
		workloads = append(workloads, platformWorkload{
			kind: kindDaemonSet, namespace: item.Namespace, name: item.Name,
			labels:    item.Labels,
			desired:   item.Status.DesiredNumberScheduled,
			ready:     item.Status.NumberReady,
			available: item.Status.NumberAvailable,
		})
	}
	return workloads, nil
}

// podCounts is how many pods each workload actually has.
//
// The owner chain is walked rather than the selector matched, because a
// Deployment's pods are owned by its ReplicaSet and a selector match would
// credit a rollout's old and new pods to whichever of the two is asked first.
// A pod whose ReplicaSet is gone is counted for nothing, which is correct: so
// is the pod.
func podCounts(pods []corev1.Pod) map[workloadKey]int {
	counts := map[workloadKey]int{}
	for i := range pods {
		pod := &pods[i]
		owner := controllerOwner(pod.OwnerReferences)
		if owner == nil {
			continue
		}
		switch owner.Kind {
		case "ReplicaSet":
			// The ReplicaSet's own name is the Deployment's plus a hash, and
			// the Deployment is what the screen lists, so the pod is credited
			// to it by name.
			if name, ok := deploymentOf(owner.Name); ok {
				counts[workloadKey{kind: kindDeployment, namespace: pod.Namespace, name: name}]++
			}
		case kindStatefulSet, kindDaemonSet:
			counts[workloadKey{kind: owner.Kind, namespace: pod.Namespace, name: owner.Name}]++
		}
	}
	return counts
}

// deploymentOf strips the pod-template hash a ReplicaSet's name carries.
func deploymentOf(replicaSet string) (string, bool) {
	index := strings.LastIndex(replicaSet, "-")
	if index <= 0 {
		return "", false
	}
	return replicaSet[:index], true
}

// admissionRefusals reads the FailedCreate warnings, indexed by the workload
// each is about.
//
// Like the freshness read, a failure here is a missing column rather than a
// failed request: the counts that make a workload look wrong come from the API
// server, and losing the explanation is not a reason to lose the symptom.
func (s *Server) admissionRefusals(ctx context.Context) (map[workloadKey]*admissionRefusalView, string) {
	store, err := s.logStore(ctx)
	if err != nil {
		if errors.Is(err, errNoLogStore) {
			return nil, noStoreMessage
		}
		s.log().Error(err, "cannot reach the telemetry store for admission refusals")
		return nil, noAdmissionMessage
	}
	events, err := store.QueryK8sEvents(ctx, clickhouse.K8sEventQuery{
		Reason: reasonFailedCreate,
		Since:  time.Now().UTC().Add(-admissionWindow),
		Limit:  clickhouse.MaxK8sEventLimit,
	})
	if err != nil {
		s.log().Error(err, "the admission refusal query failed")
		return nil, noAdmissionMessage
	}

	refusals := make(map[workloadKey]*admissionRefusalView, len(events))
	for _, event := range events {
		key := workloadKey{kind: event.Kind, namespace: event.Namespace, name: event.Name}
		// The events come back newest first, so the first one seen for a
		// workload is the one worth quoting.
		if _, seen := refusals[key]; seen {
			continue
		}
		refusals[key] = &admissionRefusalView{
			Reason:  event.Reason,
			Message: event.Message,
			Count:   event.Count,
			At:      event.Timestamp,
			Suspect: admissionSuspect(event.Message),
		}
	}
	return refusals, ""
}

// admissionSuspect names the cause where the message betrays it. Pod Security
// is the one worth naming: it is the failure the platform namespace's own level
// exists to prevent, and its message is otherwise a paragraph of policy prose
// that reads like a configuration error in the workload.
func admissionSuspect(message string) string {
	if strings.Contains(strings.ToLower(message), "violates podsecurity") {
		return fmt.Sprintf("Pod Security refused the pod: the namespace's enforce level does not allow what "+
			"this workload asks for, which is the failure %s exists to prevent for the platform's own namespace",
			controller.PlatformNamespace)
	}
	return ""
}

// noAdmissionMessage is what the screen says when the warnings could not be
// read. The counts are still true; only the explanation is missing.
const noAdmissionMessage = "the cluster's warnings could not be read, so a workload with no pods is " +
	"reported without the FailedCreate that says why"

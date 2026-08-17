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

package signals

import (
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Bermos/Kitchen/internal/controller"
)

// Reading Kubernetes objects, for the rules that all have to read them the
// same way.
//
// These live apart from any one rule because more than one needs them, and
// because the reading is where the subtlety is: a pod carries its reason in
// two places, and deciding what to *call* a workload decides what its findings
// are fingerprinted on. Each of those has one right answer, and each would be
// got slightly differently wrong in every rule that re-derived it.

// The two labels the component survey reads to name a platform workload. They
// are spelled here because they are unexported in internal/controller; the
// definition that matters is components.go's, and these must follow it.
const (
	labelComponentKind = "app.kubernetes.io/component"
	labelComponentName = "app.kubernetes.io/name"
)

// workloadFacts is one Deployment, StatefulSet or DaemonSet reduced to what
// the rules ask of it. The three kinds report their counts in three different
// places and a rule that cared would have to say so three times.
type workloadFacts struct {
	kind      string
	namespace string
	name      string
	project   string
	// environment is the Environment an application's Deployment materializes,
	// empty for the platform's own workloads.
	environment string
	desired     int32
	available   int32
	hasPods     bool
	// changedAt is when the shortfall last changed, from the workload's own
	// condition, so that the grace period is measured against the workload and
	// not against the evaluation.
	changedAt time.Time
	reason    string
}

func (w workloadFacts) scope() Scope {
	if w.project != "" && w.environment != "" {
		return Scope{Kind: ScopeEnvironment, Project: w.project, Environment: w.environment}
	}
	return Scope{Kind: ScopeWorkload, Namespace: w.namespace, Name: w.name}
}

// snapshotWorkloads flattens the three workload kinds into one list, in a
// fixed order so that a round is reproducible.
func snapshotWorkloads(snapshot *Snapshot) []workloadFacts {
	facts := make([]workloadFacts, 0,
		len(snapshot.Deployments)+len(snapshot.StatefulSets)+len(snapshot.DaemonSets))

	for i := range snapshot.Deployments {
		deployment := &snapshot.Deployments[i]
		fact := newWorkloadFacts(&deployment.ObjectMeta, "Deployment",
			replicasOrOne(deployment.Spec.Replicas), deployment.Status.AvailableReplicas)
		// The Deployment's own controller dates the shortfall better than the
		// object's creation does; where it says nothing, creation stands.
		if at, reason := deploymentShortfall(deployment); !at.IsZero() {
			fact.changedAt, fact.reason = at, reason
		}
		facts = append(facts, fact)
	}
	for i := range snapshot.StatefulSets {
		set := &snapshot.StatefulSets[i]
		// ReadyReplicas rather than AvailableReplicas, matching the component
		// survey: both are populated on a current API server, only one has
		// been for as long as these kinds have.
		fact := newWorkloadFacts(&set.ObjectMeta, "StatefulSet",
			replicasOrOne(set.Spec.Replicas), set.Status.ReadyReplicas)
		facts = append(facts, fact)
	}
	for i := range snapshot.DaemonSets {
		set := &snapshot.DaemonSets[i]
		// A DaemonSet's desired count comes from the nodes it selects, so it
		// is only ever readable from status.
		fact := newWorkloadFacts(&set.ObjectMeta, "DaemonSet",
			set.Status.DesiredNumberScheduled, set.Status.NumberAvailable)
		facts = append(facts, fact)
	}

	for i := range facts {
		facts[i].hasPods = workloadHasPods(snapshot, facts[i])
	}
	sort.Slice(facts, func(i, j int) bool {
		if facts[i].namespace != facts[j].namespace {
			return facts[i].namespace < facts[j].namespace
		}
		if facts[i].kind != facts[j].kind {
			return facts[i].kind < facts[j].kind
		}
		return facts[i].name < facts[j].name
	})
	return facts
}

func newWorkloadFacts(meta *metav1.ObjectMeta, kind string, desired, available int32) workloadFacts {
	return workloadFacts{
		kind:        kind,
		namespace:   meta.Namespace,
		name:        meta.Name,
		project:     meta.Labels[controller.LabelProject],
		environment: meta.Labels[controller.LabelEnvironment],
		desired:     desired,
		available:   available,
		changedAt:   meta.CreationTimestamp.Time,
	}
}

// deploymentShortfall reads when a Deployment stopped having all its pods, and
// what its own controller says about why.
func deploymentShortfall(deployment *appsv1.Deployment) (time.Time, string) {
	for i := range deployment.Status.Conditions {
		condition := &deployment.Status.Conditions[i]
		if condition.Type == appsv1.DeploymentAvailable && condition.Status != corev1.ConditionTrue {
			return condition.LastTransitionTime.Time, withReason(condition.Reason, condition.Message)
		}
	}
	return time.Time{}, ""
}

// workloadHasPods reports whether anything at all exists for a workload, which
// is the question admission refusal turns on.
func workloadHasPods(snapshot *Snapshot, workload workloadFacts) bool {
	for i := range snapshot.Pods {
		pod := &snapshot.Pods[i]
		if pod.Namespace != workload.namespace {
			continue
		}
		if workload.environment != "" &&
			pod.Labels[controller.LabelEnvironment] == workload.environment {
			return true
		}
		// Platform workloads are matched on the name their selector uses,
		// which the chart and platformLabels both set.
		if pod.Labels[labelComponentName] == workload.name ||
			strings.HasPrefix(pod.Name, workload.name+"-") {
			return true
		}
	}
	return false
}

// podScope places a finding about a pod: at the environment when the labels
// say which one, and at the platform's own workload otherwise.
//
// The platform case deliberately does not name the pod. A pod name carries a
// replica-set hash that changes on every rollout, and a fingerprint built on it
// would resolve and reopen every deploy — which is precisely the thing
// fingerprint stability exists to prevent.
func podScope(pod *corev1.Pod, container string) Scope {
	project := pod.Labels[controller.LabelProject]
	environment := pod.Labels[controller.LabelEnvironment]
	if project != "" && environment != "" {
		return Scope{
			Kind:        ScopeEnvironment,
			Project:     project,
			Environment: environment,
			Name:        container,
		}
	}
	name := componentName(pod)
	if container != "" {
		name += "/" + container
	}
	return Scope{Kind: ScopeWorkload, Namespace: pod.Namespace, Name: name}
}

// componentName is the stable name of the platform workload a pod belongs to,
// preferring the labels the component survey reports on.
func componentName(pod *corev1.Pod) string {
	for _, key := range []string{labelComponentKind, labelComponentName} {
		if value := pod.Labels[key]; value != "" {
			return value
		}
	}
	return pod.Name
}

// scopeEvidence links a scoped finding at the screen that shows its numbers.
func scopeEvidence(scope Scope, section string) string {
	if scope.Kind == ScopeEnvironment && scope.Environment != "" {
		return environmentEvidence(scope.Environment, section)
	}
	return workloadEvidence(scope.Namespace, scope.Name)
}

// containerStatuses is a pod's init and application containers together, since
// every rule here cares about both.
func containerStatuses(pod *corev1.Pod) []*corev1.ContainerStatus {
	statuses := make([]*corev1.ContainerStatus, 0,
		len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	for i := range pod.Status.InitContainerStatuses {
		statuses = append(statuses, &pod.Status.InitContainerStatuses[i])
	}
	for i := range pod.Status.ContainerStatuses {
		statuses = append(statuses, &pod.Status.ContainerStatuses[i])
	}
	return statuses
}

func waitingReason(status *corev1.ContainerStatus) string {
	if status.State.Waiting == nil {
		return ""
	}
	return status.State.Waiting.Reason
}

func podCondition(pod *corev1.Pod, conditionType corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == conditionType {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// withReason joins a reason to its detail. The reason is the headline
// ("Unschedulable"); the message is what makes it actionable ("0/3 nodes are
// available: 3 Insufficient memory").
func withReason(reason, message string) string {
	if reason == "" {
		return strings.TrimSpace(message)
	}
	if message = strings.TrimSpace(message); message != "" {
		return reason + ": " + message
	}
	return reason
}

// replicasOrOne applies the API's default for an unset replica count.
func replicasOrOne(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

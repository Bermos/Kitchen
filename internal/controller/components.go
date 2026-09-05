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

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	// labelPartOfKey marks everything that belongs to the platform, chart-
	// created and operator-created alike. The survey selects on it rather
	// than on a list of names, because every name the chart generates is
	// prefixed with the release name and so is not known at compile time.
	labelPartOfKey   = "app.kubernetes.io/part-of"
	labelPartOfValue = "kitchen"

	// labelComponentKind names the role a workload plays: auth, postgres,
	// clickhouse, logs. Workloads without one are reported under their
	// object name.
	labelComponentKind = "app.kubernetes.io/component"

	// maxComponentMessage keeps one explanation from dominating the status.
	maxComponentMessage = 220
)

// surveyComponents records the runtime health of every platform workload and
// reports whether all of them are up.
//
// It exists because the interesting failures are invisible in the places an
// operator looks first. A workload whose pods are rejected at admission has no
// pods at all: `kubectl get pods` shows nothing wrong, because there is nothing
// there to be wrong. That is how the log collector sat dead for hours on a
// cluster whose every condition read True. So the survey compares what each
// workload wants against what it has, and where those differ it goes and finds
// the reason rather than reporting a bare count.
func (r *KitchenReconciler) surveyComponents(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	selector := []client.ListOption{
		client.InNamespace(PlatformNamespace),
		client.MatchingLabels{labelPartOfKey: labelPartOfValue},
	}

	deployments := &appsv1.DeploymentList{}
	statefulSets := &appsv1.StatefulSetList{}
	daemonSets := &appsv1.DaemonSetList{}
	for _, list := range []client.ObjectList{deployments, statefulSets, daemonSets} {
		if err := r.List(ctx, list, selector...); err != nil {
			setCond(condComponentsHealthy, metav1.ConditionFalse, "SurveyFailed", err.Error())
			return false
		}
	}

	surveyed := make([]surveyedWorkload, 0,
		len(deployments.Items)+len(statefulSets.Items)+len(daemonSets.Items))

	for i := range deployments.Items {
		d := &deployments.Items[i]
		surveyed = append(surveyed, r.componentOf(ctx, &d.ObjectMeta, "Deployment", d.Spec.Selector,
			replicasOrOne(d.Spec.Replicas), d.Status.AvailableReplicas))
	}
	for i := range statefulSets.Items {
		s := &statefulSets.Items[i]
		// ReadyReplicas rather than AvailableReplicas: both are populated on
		// a current API server, but only ReadyReplicas has been there for as
		// long as the kinds this chart deploys have.
		surveyed = append(surveyed, r.componentOf(ctx, &s.ObjectMeta, "StatefulSet", s.Spec.Selector,
			replicasOrOne(s.Spec.Replicas), s.Status.ReadyReplicas))
	}
	for i := range daemonSets.Items {
		ds := &daemonSets.Items[i]
		// A DaemonSet's desired count comes from the nodes it selects, not
		// from any replica count, so it is only ever readable from status.
		surveyed = append(surveyed, r.componentOf(ctx, &ds.ObjectMeta, "DaemonSet", ds.Spec.Selector,
			ds.Status.DesiredNumberScheduled, ds.Status.NumberAvailable))
	}

	// The clock check is not a workload, and it goes through the same name
	// resolution as one anyway: status.components is a map list, and a
	// workload that happened to carry the same component label would
	// otherwise make the whole status update fail rather than merely read
	// oddly.
	clock, clockStatus := r.surveyClockSync(ctx, kitchen, time.Now().UTC())
	kitchen.Status.ClockSync = clockStatus
	if clock != nil {
		surveyed = append(surveyed, surveyedWorkload{
			status:     *clock,
			objectName: clockComponentName + "-check",
		})
	}

	// Whether the platform's own stores are reached over TLS, on the same
	// terms: not a workload, and in this list because an unencrypted platform
	// namespace has no pod to be unhealthy and would otherwise be visible
	// nowhere an operator looks.
	if tls := internalTLSComponent(kitchen); tls != nil {
		surveyed = append(surveyed, surveyedWorkload{
			status:     *tls,
			objectName: InternalCACertificateName,
		})
	}

	// The scheduled backup, on the same terms as the clock check: not a
	// workload, and in this list because this list is what an operator reads.
	// It is the row that answers "when did a backup last work", which is the
	// question a platform only asks itself once it is too late.
	if backup := backupComponent(kitchen); backup != nil {
		surveyed = append(surveyed, surveyedWorkload{
			status:     *backup,
			objectName: BackupCronJobName,
		})
	}

	components := resolveNames(surveyed)
	kitchen.Status.Components = components

	var unhealthy []string
	for _, c := range components {
		if !c.Healthy {
			unhealthy = append(unhealthy, c.Name)
		}
	}

	switch {
	case len(components) == 0:
		// Nothing carries the label. Either the chart did not install these
		// workloads or it is an older chart that did not label them; both
		// are indistinguishable from here, and neither is a platform fault.
		setCond(condComponentsHealthy, metav1.ConditionTrue, "NoComponents",
			"no workloads labelled "+labelPartOfKey+"="+labelPartOfValue+" in "+PlatformNamespace)
		return true
	case len(unhealthy) == 0:
		setCond(condComponentsHealthy, metav1.ConditionTrue, "AllHealthy",
			fmt.Sprintf("%d/%d healthy", len(components), len(components)))
		return true
	default:
		setCond(condComponentsHealthy, metav1.ConditionFalse, "ComponentsUnhealthy",
			fmt.Sprintf("%d/%d healthy; waiting on %s",
				len(components)-len(unhealthy), len(components), strings.Join(unhealthy, ", ")))
		return false
	}
}

// surveyedWorkload is one workload's reported status alongside the object name
// it came from, which is the fallback when the preferred name is taken.
type surveyedWorkload struct {
	status     kitchenv1alpha1.ComponentStatus
	objectName string
}

// resolveNames sorts the survey and makes every name unique.
//
// status.components is a map list keyed on name, so a duplicate does not
// produce a confusing report — it makes the whole status update fail, taking
// every condition down with it. Nothing stops two workloads carrying the same
// component label, so the names are resolved rather than trusted: the label
// first, then the object name, then the object name qualified by kind, which
// cannot collide because a name is unique per kind within a namespace.
func resolveNames(surveyed []surveyedWorkload) []kitchenv1alpha1.ComponentStatus {
	sort.Slice(surveyed, func(i, j int) bool {
		if surveyed[i].status.Name != surveyed[j].status.Name {
			return surveyed[i].status.Name < surveyed[j].status.Name
		}
		return surveyed[i].objectName < surveyed[j].objectName
	})

	taken := make(map[string]bool, len(surveyed))
	components := make([]kitchenv1alpha1.ComponentStatus, 0, len(surveyed))
	for _, workload := range surveyed {
		name := workload.status.Name
		if taken[name] {
			name = workload.objectName
		}
		if taken[name] {
			name = workload.objectName + "-" + strings.ToLower(workload.status.Kind)
		}
		taken[name] = true
		workload.status.Name = name
		components = append(components, workload.status)
	}
	return components
}

// componentOf turns one workload into its reported status, looking up why it
// is short of pods when it is.
//
// A shortfall has two shapes and the reason lives in a different place for
// each. A pod refused at admission never exists, so the rejection lands on the
// workload as a FailedCreate event. A pod that is created and then dies carries
// its reason itself — OOMKilled is not an event at all, it is a field of pod
// status — and none of that reaches the workload. The pods are asked first,
// because when there is a pod to ask it knows more than the workload does.
func (r *KitchenReconciler) componentOf(
	ctx context.Context,
	objectMeta *metav1.ObjectMeta,
	kind string,
	selector *metav1.LabelSelector,
	desired, available int32,
) surveyedWorkload {
	name := objectMeta.Labels[labelComponentKind]
	if name == "" {
		name = objectMeta.Name
	}

	component := kitchenv1alpha1.ComponentStatus{
		Name:      name,
		Kind:      kind,
		Desired:   desired,
		Available: available,
		Healthy:   available >= desired,
	}
	if component.Healthy {
		return surveyedWorkload{status: component, objectName: objectMeta.Name}
	}

	component.Message = fmt.Sprintf("%d of %d pods available", available, desired)
	reason := r.podReason(ctx, objectMeta.Namespace, selector)
	if reason == "" {
		reason = latestWarning(ctx, r.APIReader, objectMeta)
	}
	if reason != "" {
		component.Message += ": " + reason
	}
	return surveyedWorkload{status: component, objectName: objectMeta.Name}
}

// podReason asks the pods a workload selects why they are not serving.
//
// It exists because the survey used to report `logs 0/1: 0 of 1 pods available`
// for a collector that was OOM-killing itself every eight seconds. The reason
// was there the whole time — OOMKilled, exit 137, against a 512Mi limit — on
// the pod, which nothing was reading. The diagnosis came from `kubectl describe
// pod` by hand instead.
//
// Best effort, like latestWarning: reads go straight to the API server rather
// than through the cache, because caching every pod in the cluster to answer an
// occasional question would cost far more than it saves, and a survey that
// cannot read pods still reports its counts.
func (r *KitchenReconciler) podReason(
	ctx context.Context,
	namespace string,
	selector *metav1.LabelSelector,
) string {
	reader := r.APIReader
	if reader == nil || selector == nil {
		return ""
	}
	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return ""
	}

	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: labelSelector},
	); err != nil {
		return ""
	}

	for i := range pods.Items {
		if reason := podTrouble(&pods.Items[i]); reason != "" {
			return truncateMessage(reason)
		}
	}
	// Nothing the pods know. Either there are none — the admission case — or
	// they are between states and say nothing yet.
	return ""
}

// waitingIsNormal holds the waiting reasons that mean "starting", not "stuck".
// A pod reports them for a second on every ordinary rollout, and reporting them
// as the explanation for a shortfall would make a healthy deploy look like a
// fault.
var waitingIsNormal = map[string]bool{
	"ContainerCreating": true,
	"PodInitializing":   true,
}

// podTrouble reads one pod for why it is not serving, or "" when it is fine or
// has nothing to say yet.
func podTrouble(pod *corev1.Pod) string {
	// A pod on its way out is a rollout, not a fault.
	if pod.DeletionTimestamp != nil {
		return ""
	}
	for i := range pod.Status.Conditions {
		condition := &pod.Status.Conditions[i]
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return ""
		}
	}

	// A pod that was never scheduled has no container statuses to carry a
	// reason; the scheduler's own is on the condition. Unschedulable is a
	// first-install failure in its own right — no default StorageClass,
	// insufficient memory, a taint nothing tolerates.
	for i := range pod.Status.Conditions {
		condition := &pod.Status.Conditions[i]
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			if reason := withMessage(condition.Reason, condition.Message); reason != "" {
				return reason
			}
		}
	}

	statuses := make([]corev1.ContainerStatus, 0,
		len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)

	// What a container died of, preferring the one that died most recently:
	// with several containers the newest death is the one still happening.
	var died *corev1.ContainerStatus
	for i := range statuses {
		terminated := statuses[i].LastTerminationState.Terminated
		if terminated == nil || terminated.Reason == "" {
			continue
		}
		if died == nil || terminated.FinishedAt.After(died.LastTerminationState.Terminated.FinishedAt.Time) {
			died = &statuses[i]
		}
	}
	if died != nil {
		terminated := died.LastTerminationState.Terminated
		// The exit code is worth carrying: 137 is the kernel's OOM kill and
		// tells the reader the limit was the cause, not the program.
		parts := []string{fmt.Sprintf("%s (exit %d)", terminated.Reason, terminated.ExitCode)}
		if waiting := died.State.Waiting; waiting != nil && waiting.Reason != "" {
			parts = append(parts, waiting.Reason)
		}
		// A pod that has restarted twice and one that has restarted 200 times
		// read very differently, and the count is what tells them apart.
		if died.RestartCount > 0 {
			parts = append(parts, fmt.Sprintf("%d restarts", died.RestartCount))
		}
		return strings.Join(parts, ", ")
	}

	// Never started: ImagePullBackOff, CreateContainerConfigError, a missing
	// secret or ConfigMap.
	for i := range statuses {
		waiting := statuses[i].State.Waiting
		if waiting == nil || waiting.Reason == "" || waitingIsNormal[waiting.Reason] {
			continue
		}
		return withMessage(waiting.Reason, waiting.Message)
	}
	return ""
}

// withMessage joins a reason to its detail, when there is one. The reason
// alone is the headline ("Unschedulable"); the message is what makes it
// actionable ("0/3 nodes are available: 3 Insufficient memory").
func withMessage(reason, message string) string {
	if reason == "" {
		return ""
	}
	if message = strings.TrimSpace(message); message != "" {
		return reason + ": " + message
	}
	return reason
}

// latestWarning returns the most recent warning event on an object, which is
// where the controllers put the things that never become pod state: an
// admission rejection, a missing image pull secret, a quota refusal. Best
// effort — a caller that cannot read events still reports what it already
// knows.
//
// The reader is passed in rather than taken off a receiver because this is not
// only the platform survey's question: a build Job that never creates a pod is
// the same shape of fault on a different kind of object, and reads its reason
// out of the same place.
func latestWarning(ctx context.Context, reader client.Reader, objectMeta *metav1.ObjectMeta) string {
	if reader == nil {
		// Field selectors are not served by the cache, so without a direct
		// reader there is nothing safe to ask for.
		return ""
	}

	events := &corev1.EventList{}
	err := reader.List(ctx, events,
		client.InNamespace(objectMeta.Namespace),
		client.MatchingFields{
			"involvedObject.uid": string(objectMeta.UID),
			"type":               corev1.EventTypeWarning,
		})
	if err != nil || len(events.Items) == 0 {
		return ""
	}

	latest := &events.Items[0]
	for i := range events.Items {
		if eventTime(&events.Items[i]).After(eventTime(latest).Time) {
			latest = &events.Items[i]
		}
	}

	return truncateMessage(latest.Message)
}

// truncateMessage keeps one explanation from dominating the status. The reason
// is at the front of everything here — an admission rejection, a scheduler
// complaint, a container's exit — so cutting the tail loses nothing that
// matters.
func truncateMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > maxComponentMessage {
		return message[:maxComponentMessage] + "…"
	}
	return message
}

// eventTime prefers the series/event time an emitter set over the legacy
// timestamps, which the newer events API leaves zeroed.
func eventTime(event *corev1.Event) metav1.Time {
	if event.Series != nil && !event.Series.LastObservedTime.IsZero() {
		return metav1.Time{Time: event.Series.LastObservedTime.Time}
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp
	}
	if !event.EventTime.IsZero() {
		return metav1.Time{Time: event.EventTime.Time}
	}
	return event.FirstTimestamp
}

// replicasOrOne applies the API's default for an unset replica count.
func replicasOrOne(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

// platformLabels marks an operator-created workload as part of the platform so
// the survey finds it alongside everything the chart installs.
//
// selectorName is what the workload's selector matches on and so can never
// change for an existing object — a Deployment's selector is immutable.
// component is the short name the survey reports, which is free to be readable.
func platformLabels(selectorName, component string) map[string]string {
	return map[string]string{
		labelComponentKey:  selectorName,
		labelComponentKind: component,
		labelManagedByKey:  labelManagedByValue,
		labelPartOfKey:     labelPartOfValue,
	}
}

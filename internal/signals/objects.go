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
	batchv1 "k8s.io/api/batch/v1"
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

	// labelPartOf and labelPartOfKitchen are what the component survey selects
	// the platform's own workloads on, and what tells a Job the operator
	// created from one somebody else's tooling left in the cluster.
	labelPartOf        = "app.kubernetes.io/part-of"
	labelPartOfKitchen = "kitchen"

	// labelBuild is on every Job the build reconciler creates. Its only use
	// here is to leave those Jobs alone; see snapshotJobs.
	labelBuild = "kitchen.bermos.dev/build"
)

// workloadFacts is one Deployment, StatefulSet, DaemonSet or Job reduced to
// what the rules ask of it. The kinds report their counts in four different
// places and a rule that cared would have to say so four times.
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
	// scopeName is the subject a finding about this workload is at, within an
	// environment. It is empty for the Deployment that *is* the environment,
	// which is what makes its fingerprint the environment's own; a Job carries
	// its name here, so that a refused run is its own finding rather than one
	// that collides with the Deployment's.
	scopeName string
}

func (w workloadFacts) scope() Scope {
	if w.project != "" && w.environment != "" {
		return Scope{
			Kind:        ScopeEnvironment,
			Project:     w.project,
			Environment: w.environment,
			Name:        w.scopeName,
		}
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
	sortFacts(facts)
	return facts
}

// sortFacts fixes the order a round walks workloads in, so that two
// evaluations of the same cluster produce the same findings in the same
// sequence.
func sortFacts(facts []workloadFacts) {
	sort.Slice(facts, func(i, j int) bool {
		if facts[i].namespace != facts[j].namespace {
			return facts[i].namespace < facts[j].namespace
		}
		if facts[i].kind != facts[j].kind {
			return facts[i].kind < facts[j].kind
		}
		return facts[i].name < facts[j].name
	})
}

// snapshotJobs is the Jobs the platform created, reduced to the same facts,
// for the one rule that has anything to say about a Job: a Job whose pods are
// refused at admission has no pods at all, and every screen that counts
// failing pods shows nothing wrong.
//
// It is separate from snapshotWorkloads rather than a fourth kind inside it,
// because a Job is a different question from a Deployment and answering it
// takes three decisions the serving kinds never have to make:
//
//   - Which Jobs count. A cluster Kitchen owns has few Deployments that are
//     not the platform's, but a Job is what every other piece of tooling in a
//     cluster creates — a backup, a migration somebody ran by hand, a Helm
//     hook — and none of those are Kitchen's to report on. So a Job is
//     surveyed only when it carries the platform's part-of label or an
//     application's project and environment ones, which between them cover
//     every Job the operator and the API create.
//   - A build's Job is left alone. build.stalled already reports exactly this
//     condition from the Stalled condition the build reconciler writes, and it
//     names the Build rather than a Job with a generated-looking name.
//   - A finished Job is not a fault, and neither is a suspended one. Both are
//     Jobs that want no pods, which is the ordinary state of most Jobs in a
//     cluster and would otherwise be reported as the worst thing this package
//     knows how to say.
//
// Only workload.admission-refused reads these facts, which is why they are
// gathered here rather than in snapshotWorkloads: a Job is not "not ready" for
// having no pods yet, and an environment's Job having none is not that
// environment failing to serve.
func snapshotJobs(snapshot *Snapshot) []workloadFacts {
	facts := make([]workloadFacts, 0, len(snapshot.Jobs))
	for i := range snapshot.Jobs {
		job := &snapshot.Jobs[i]
		if !surveyedJob(job) || jobFinished(job) || jobSuspended(job) {
			continue
		}
		// Creating a pod is immediate on an idle cluster and takes a moment on
		// a busy one, so a Job younger than the grace has not failed to create
		// one yet — the same reading, and the same two minutes, the build
		// reconciler takes of a build Job.
		if snapshot.Now.Sub(jobStartedAt(job)) < JobNoPodGrace {
			continue
		}
		fact := newWorkloadFacts(&job.ObjectMeta, "Job",
			replicasOrOne(job.Spec.Parallelism), job.Status.Active)
		fact.scopeName = job.Name
		fact.hasPods = jobHasPods(job)
		facts = append(facts, fact)
	}
	sortFacts(facts)
	return facts
}

// surveyedJob is a Job Kitchen created: the platform's own — a KEDA or add-on
// install, the gate publisher, a rescan, a self-update — or an application's,
// which the operator and the API label with the project and environment they
// belong to. A build's Job is neither, on purpose.
func surveyedJob(job *batchv1.Job) bool {
	if job.Labels[labelBuild] != "" {
		return false
	}
	if job.Labels[labelPartOf] == labelPartOfKitchen {
		return true
	}
	return job.Labels[controller.LabelProject] != "" &&
		job.Labels[controller.LabelEnvironment] != ""
}

// jobFinished is a Job that has run: it wants no more pods, and having none is
// what finishing looks like. A Job past its TTL needs no test of its own — the
// TTL controller deletes it, so it is not in the snapshot at all — and where
// nothing collects one, its completion time is still here to say it has run.
func jobFinished(job *batchv1.Job) bool {
	if job.Status.Succeeded > 0 || job.Status.CompletionTime != nil {
		return true
	}
	for i := range job.Status.Conditions {
		condition := &job.Status.Conditions[i]
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed {
			return true
		}
	}
	return false
}

// jobSuspended is a Job somebody parked. It has no pods because it was asked
// to have none.
func jobSuspended(job *batchv1.Job) bool {
	return job.Spec.Suspend != nil && *job.Spec.Suspend
}

// jobHasPods reads the Job's own counters rather than the pod list, which is
// the reading the build reconciler already takes of the same question: a pod
// refused at admission is never created, so it is counted nowhere and the
// rejection lands on the Job as an event instead, while a pod that was created
// and has since gone still leaves Succeeded or Failed behind it.
//
// Counting pods would also be wrong here in a way it is not for a Deployment:
// an application's Job shares its namespace and its environment label with the
// Deployment serving that environment, so any test over the pod list would find
// the web pods and call the Job served.
func jobHasPods(job *batchv1.Job) bool {
	return job.Status.Active > 0 || job.Status.Succeeded > 0 || job.Status.Failed > 0
}

// jobStartedAt is when the Job controller took the Job, which is when it began
// having to create a pod. It falls back to creation for the moment between the
// two.
func jobStartedAt(job *batchv1.Job) time.Time {
	if job.Status.StartTime != nil {
		return job.Status.StartTime.Time
	}
	return job.CreationTimestamp.Time
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

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

package v1alpha1

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProcessType is how one of a Project's processes runs.
//
// There is deliberately no `web` here. A Project's web process is
// `spec.runtime` — the port, the replica count and the resources of the thing
// with the URL — and it is singular because the URL is: an Environment
// publishes one hostname, one Service and one route, and a second process
// claiming to be the web one would have to be told which of those it got. So
// this list is what a Project has *besides* its web process, which is exactly
// the gap #78 was filed for: a queue worker and a nightly job that today have
// to be a second Project with a port nothing talks to.
// +kubebuilder:validation:Enum=worker;cron
type ProcessType string

const (
	// ProcessWorker runs continuously and is never addressed: a Deployment
	// with no Service and no route. A queue consumer, a stream reader.
	ProcessWorker ProcessType = "worker"
	// ProcessCron runs on a schedule: a batch/v1 CronJob. The nightly job.
	ProcessCron ProcessType = "cron"
)

// ConcurrencyPolicy is what a scheduled process does when its next run comes
// round while the last one is still going. The values and their meanings are
// batch/v1's, spelled the same way, because that is what they become.
// +kubebuilder:validation:Enum=Allow;Forbid;Replace
type ConcurrencyPolicy string

const (
	// ConcurrencyAllow lets runs overlap.
	ConcurrencyAllow ConcurrencyPolicy = "Allow"
	// ConcurrencyForbid skips the new run while the old one is going. It is
	// the default: a job that takes longer than its interval is far more
	// often a job running behind than a job meant to run twice at once, and
	// the second copy is how a nightly report gets sent twice.
	ConcurrencyForbid ConcurrencyPolicy = "Forbid"
	// ConcurrencyReplace kills the running one and starts the new one.
	ConcurrencyReplace ConcurrencyPolicy = "Replace"
)

// DefaultRunTimeout is how long a scheduled run may take before the platform
// stops it. A job with no ceiling at all is the one that holds a lock until
// somebody notices, so there is a default rather than an absent value.
const DefaultRunTimeout = time.Hour

// ProcessSpec is one process of a Project besides its web process: the same
// image, the same environment, started differently.
//
// The Release is already the right unit — an immutable image plus a config
// snapshot — so a worker and a scheduled job are not another build. They are
// the build's image run with another command, which is why this list is
// snapshotted into the Release along with the environment variables and the
// runtime: rolling back runs the processes the rollback target declared, not
// the ones the Project declares today.
//
// The two rules test `has(self.schedule)` rather than comparing to an empty
// string, and it is not only shorter: the field is `omitempty`, so an absent
// schedule is an absent key and `has` is the exact question.
//
// The health rules are the same shape of refusal rather than a silent
// omission. A process publishes no container port of its own, so a health
// check that named none would be a setting that read back and did nothing;
// and a scheduled run's verdict is its exit status, not a probe.
//
// The singleton rules are `RuntimeSpec`'s, read for a process. A worker is
// exactly the workload that declaration was filed for and the one it did not
// reach (#250), and it refuses a second replica rather than clamping one for
// the same reason: a value silently lowered reads back as a setting that did
// not take. A scheduled process is refused it outright, because whether two of
// its runs may overlap is `concurrencyPolicy` — a second spelling of that
// question would be a setting that reads back and does nothing.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'cron' || has(self.schedule)",message="a cron process needs a schedule"
// +kubebuilder:validation:XValidation:rule="self.type != 'worker' || !has(self.schedule)",message="a worker runs continuously and has no schedule; use type cron"
// +kubebuilder:validation:XValidation:rule="!has(self.health) || has(self.health.port)",message="a process health check must name the port it is made against: a process publishes no port of its own"
// +kubebuilder:validation:XValidation:rule="self.type != 'cron' || !has(self.health)",message="a scheduled process is not kept alive by a health check; how a run went is its exit status"
// +kubebuilder:validation:XValidation:rule="!has(self.singleton) || !self.singleton || self.type == 'worker'",message="only a worker can be a singleton: whether two runs of a scheduled process may overlap is concurrencyPolicy, and Forbid is its default"
// +kubebuilder:validation:XValidation:rule="!has(self.singleton) || !self.singleton || !has(self.replicas) || self.replicas <= 1",message="a singleton worker cannot run more than one replica: leave replicas at 1, or turn singleton off"
type ProcessSpec struct {
	// Name identifies the process within the project. It is a DNS label
	// because it appears in the name of everything the process
	// materializes — `<environment>-<name>` — and in the log query
	// vocabulary as `process:<name>`.
	//
	// `web` is refused: the web process is `spec.runtime`, and a second
	// spelling of it would be a process nothing routes to answering to the
	// name of the one that is routed to.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=48
	// +kubebuilder:validation:XValidation:rule="self != 'web'",message="the web process is spec.runtime, not a named process"
	Name string `json:"name"`

	Type ProcessType `json:"type"`

	// Command replaces the image's entrypoint, and Args its arguments. Both
	// are optional and both are exec form — a list of words, never a shell
	// line — which is the same reason the build jobs take two containers
	// instead of an `sh -c`: a string that reaches a shell is a string
	// somebody can put a `;` in.
	//
	// A worker whose image already starts the right process needs neither. A
	// buildpacks-built image does: the buildpack chose a web server, and
	// `command: ["node", "worker.js"]` is how the same image becomes the
	// worker.
	// +optional
	// +listType=atomic
	Command []string `json:"command,omitempty"`

	// +optional
	// +listType=atomic
	Args []string `json:"args,omitempty"`

	// Replicas is how many copies of a worker run. It is meaningless for a
	// scheduled process — how many run at once is ConcurrencyPolicy's
	// answer — and is ignored there rather than refused, so that changing a
	// process's type does not require deleting a field first.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Singleton says two of this worker must never run at once.
	//
	// It is `RuntimeSpec.Singleton` for a process, and it exists because the
	// web process is the one that least needed it. A poller moved out of the
	// web binary into a worker — which is the arrangement that makes the web
	// process safely scalable — would otherwise lose the guarantee it had by
	// staying in it: a worker takes the API server's default rolling update,
	// which at one replica surges to two copies on every rollout.
	//
	// What it does is what it does for the web process: `strategy: Recreate`
	// on the Deployment, so the old pod stops before the new one starts. A
	// worker is not addressed, so the gap that costs the web process a few
	// seconds of serving costs a worker only a few seconds of not consuming.
	// It refuses Replicas above one rather than clamping it, and it is
	// refused on a scheduled process, whose answer to the same question is
	// ConcurrencyPolicy.
	//
	// One replica does not imply it, and deliberately: a queue consumer at
	// one replica is usually fine overlapping, and inferring the constraint
	// from the count would make the count mean two things.
	// +optional
	Singleton bool `json:"singleton,omitempty"`

	// Resources is what one replica, or one run, asks for.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Health is how the platform checks that a worker is working. It is
	// opt-in here where it is automatic for the web process, and the reason
	// is the same one that makes a worker cheap: nothing addresses it, so
	// there is no port to fall back on and no request whose failure would
	// otherwise show. A worker that serves a health listener says which port
	// on it; one that does not gets no probes, and its liveness is whether
	// its process is still running.
	// +optional
	Health *HealthSpec `json:"health,omitempty"`

	// Schedule is the cron expression a scheduled process runs on, in the
	// five-field form `batch/v1` takes, interpreted in UTC. Required for a
	// cron process and refused on a worker, both at admission.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// ConcurrencyPolicy is what happens when a run is due and the last one
	// has not finished.
	// +kubebuilder:default=Forbid
	// +optional
	ConcurrencyPolicy ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// Timeout is how long one run may take before the platform stops it.
	// Defaults to an hour; see DefaultRunTimeout.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Previews says whether this process runs in preview environments too.
	//
	// **It is off, and that is the decision rather than an omission.** A
	// preview that emails customers nightly is a bad afternoon, and a
	// preview worker draining the production queue is a worse one — the
	// preview shares the project's environment variables, so it is pointed
	// at whatever the production process is pointed at unless somebody
	// overrode it. One rule for both types, opted into per process: a
	// preview that should run its worker says so.
	// +kubebuilder:default=false
	// +optional
	Previews bool `json:"previews,omitempty"`
}

// TimeoutSeconds is how long one run may take, as `activeDeadlineSeconds`
// wants it. A process that names no timeout gets DefaultRunTimeout, and one
// that names a sub-second duration gets a second rather than a zero, which
// batch/v1 reads as "deadline already passed".
func (p ProcessSpec) TimeoutSeconds() int64 {
	if p.Timeout == nil || p.Timeout.Duration <= 0 {
		return int64(DefaultRunTimeout.Seconds())
	}
	if seconds := int64(p.Timeout.Duration.Seconds()); seconds > 0 {
		return seconds
	}
	return 1
}

// ReplicaCount is how many copies of a worker run. Nil means the CRD default
// of one, for a process written before the field had one.
func (p ProcessSpec) ReplicaCount() int32 {
	if p.Replicas == nil {
		return 1
	}
	return *p.Replicas
}

// EffectiveConcurrency is the policy a scheduled run is subject to, defaulted
// the way the CRD defaults it.
func (p ProcessSpec) EffectiveConcurrency() ConcurrencyPolicy {
	if p.ConcurrencyPolicy == "" {
		return ConcurrencyForbid
	}
	return p.ConcurrencyPolicy
}

// RunsIn reports whether this process is materialized in an environment of the
// given type. Production runs everything; a preview runs what opted in.
func (p ProcessSpec) RunsIn(envType EnvironmentType) bool {
	if envType != EnvironmentPreview {
		return true
	}
	return p.Previews
}

// RunPhase is how one run of a scheduled process ended, or that it has not.
// +kubebuilder:validation:Enum=Running;Succeeded;Failed
type RunPhase string

const (
	RunRunning   RunPhase = "Running"
	RunSucceeded RunPhase = "Succeeded"
	RunFailed    RunPhase = "Failed"
)

// ProcessStatus is what an Environment's reconciler last saw of one process.
//
// A `CronJob` whose pods fail silently is the classic way this feature
// disappoints, so the failure is carried here — on the Environment, which is
// what the project page and the API read — rather than left in
// `kubectl get jobs`.
type ProcessStatus struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	Type ProcessType `json:"type"`

	// Workload is the Deployment or CronJob the process materialized as, in
	// the project's application namespace.
	// +optional
	Workload string `json:"workload,omitempty"`

	// Replicas and ReadyReplicas are a worker's, and are left at zero for a
	// scheduled process.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Schedule is a scheduled process's, echoed so a reader does not have to
	// hold the Release open beside this.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Suspended marks a process this environment declares but deliberately
	// does not run: a preview whose process did not opt in. Nothing is
	// materialized for it at all — the flag is what stops a preview's process
	// list reading like a shorter version of the project's.
	// +optional
	Suspended bool `json:"suspended,omitempty"`

	// Active is how many runs are in flight.
	// +optional
	Active int32 `json:"active,omitempty"`

	// LastRun is the most recent run that started, whatever it did next, and
	// LastFailure the most recent one that failed. Both are kept because the
	// question a person asks is "is it working" and the question they ask
	// next is "when did it stop" — and a job that has been failing for a week
	// answers the first with a green tick if only the last run is recorded.
	//
	// There is deliberately no "next run": computing it means parsing the
	// cron expression, and a whole scheduling library carried for one
	// display string is not a trade worth making when the schedule itself is
	// right there beside it.
	// +optional
	LastRun *ProcessRun `json:"lastRun,omitempty"`
	// +optional
	LastFailure *ProcessRun `json:"lastFailure,omitempty"`
}

// ProcessRun is one run of a scheduled process: the Job that ran it, when, and
// what came of it.
type ProcessRun struct {
	// Name is the Job in the application namespace. It is also the value the
	// log pipeline keys on as `run:<name>`, which is what makes a run's
	// output queryable after the pods are gone.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	Phase RunPhase `json:"phase"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Message is the one line a person reads: why it failed, or nothing at
	// all when it did not.
	// +optional
	Message string `json:"message,omitempty"`
}

// FindProcess returns the named process of a list, or nil.
func FindProcess(processes []ProcessSpec, name string) *ProcessSpec {
	for i := range processes {
		if processes[i].Name == name {
			return &processes[i]
		}
	}
	return nil
}

// ProcessNames is every process an environment of the project can
// materialize, the implicit web process first under the name a declared
// process cannot take. It is what a volume claim's process is checked
// against, in the API and in the reconciler alike.
func (p *Project) ProcessNames() []string {
	names := make([]string, 0, len(p.Spec.Processes)+1)
	names = append(names, WebProcessName)
	for _, process := range p.Spec.Processes {
		names = append(names, process.Name)
	}
	return names
}

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
//
// `service` is the third value and the one #271 added. It is the workload a
// monorepo has several of: an image of its own, addressed by the rest of the
// unit over the cluster network, and **not published**. Publishing stays the
// exception the project declares — `spec.runtime`, the one process with the
// URL — so a workload that should not be on the internet is not on it by
// saying nothing, rather than by remembering to turn something off.
// +kubebuilder:validation:Enum=worker;cron;service
type ProcessType string

const (
	// ProcessWorker runs continuously and is never addressed: a Deployment
	// with no Service and no route. A queue consumer, a stream reader.
	ProcessWorker ProcessType = "worker"
	// ProcessCron runs on a schedule: a batch/v1 CronJob. The nightly job.
	ProcessCron ProcessType = "cron"
	// ProcessService runs continuously and *is* addressed, from inside the
	// cluster and from nowhere else: a Deployment and a ClusterIP Service,
	// and no HTTPRoute. It is how one workload of a unit reaches another —
	// the environment's own siblings find it under
	// `KITCHEN_SERVICE_<NAME>`, which resolves to that environment's copy,
	// so a preview's web process talks to the preview's own API and never
	// to production's.
	ProcessService ProcessType = "service"
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

// ProcessSpec is one workload of a Project besides its web process: the
// project's environment, started differently — and, since #271, optionally
// built differently and addressed on its own.
//
// The Release is already the right unit — an immutable image plus a config
// snapshot — so a worker and a scheduled job are not another build. They are
// the build's image run with another command, which is why this list is
// snapshotted into the Release along with the environment variables and the
// runtime: rolling back runs the processes the rollback target declared, not
// the ones the Project declares today. A workload that declares a [Build] of
// its own is the same story with one more frozen field: the Release records
// the digest each workload was built to, so a rollback restores the exact set
// of images that release declared rather than today's.
//
// The two rules test `has(self.schedule)` rather than comparing to an empty
// string, and it is not only shorter: the field is `omitempty`, so an absent
// schedule is an absent key and `has` is the exact question. Only a `cron`
// takes one — a worker and a service both run continuously, and a schedule on
// either would be a setting that read back and did nothing.
//
// The port rules and the health rules are the same shape of refusal rather
// than a silent omission. Only a service is addressed, so only a service has
// a port; a worker publishes no container port of its own, so a health check
// that named none would check nothing; and a scheduled run's verdict is its
// exit status, not a probe.
//
// The singleton rules are `RuntimeSpec`'s, read for a workload. A worker is
// exactly the workload that declaration was filed for and the one it did not
// reach (#250), and it refuses a second replica rather than clamping one for
// the same reason: a value silently lowered reads back as a setting that did
// not take. A service takes it on the same terms. A scheduled process is
// refused it outright, because whether two of its runs may overlap is
// `concurrencyPolicy` — a second spelling of that question would be a setting
// that reads back and does nothing.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'cron' || has(self.schedule)",message="a cron process needs a schedule"
// +kubebuilder:validation:XValidation:rule="self.type == 'cron' || !has(self.schedule)",message="a worker and a service run continuously and have no schedule; use type cron"
// +kubebuilder:validation:XValidation:rule="!has(self.health) || has(self.health.port) || self.type == 'service'",message="a process health check must name the port it is made against: a worker publishes no port of its own"
// +kubebuilder:validation:XValidation:rule="self.type != 'cron' || !has(self.health)",message="a scheduled process is not kept alive by a health check; how a run went is its exit status"
// +kubebuilder:validation:XValidation:rule="self.type != 'service' || has(self.port)",message="a service process has to say which port it listens on: it is what the rest of the unit addresses it by"
// +kubebuilder:validation:XValidation:rule="self.type == 'service' || !has(self.port)",message="only a service process is addressed, so only a service process has a port; a worker that serves a health listener names that port on its health check"
// +kubebuilder:validation:XValidation:rule="!has(self.singleton) || !self.singleton || self.type != 'cron'",message="a scheduled process cannot be a singleton: whether two of its runs may overlap is concurrencyPolicy, and Forbid is its default"
// +kubebuilder:validation:XValidation:rule="!has(self.singleton) || !self.singleton || !has(self.replicas) || self.replicas <= 1",message="a singleton workload cannot run more than one replica: leave replicas at 1, or turn singleton off"
// +kubebuilder:validation:XValidation:rule="self.type != 'cron' || !has(self.build)",message="a scheduled process runs an image, it is not one: give the build to the worker or service that ships it, and run this on that image"
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

	// Port is the port a service workload listens on, and the port the
	// Service in front of it is published on — the two are the same number
	// deliberately, because a workload addressed by the rest of the unit is
	// addressed over whatever protocol it speaks, and a Service that
	// renumbered the port would make `KITCHEN_SERVICE_<NAME>_PORT` and the
	// application's own configuration disagree.
	//
	// It is required on a service and refused on anything else, at admission
	// and at the API. A worker publishes nothing, so a port on one would be
	// a setting that reads back and does nothing — the port a worker's
	// health listener sits on is `health.port`, which is the question that
	// field already asks.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// Build says this workload is built from the repository in its own
	// right, rather than running the project's image with another command.
	//
	// It is what makes a monorepo one project instead of four (#271): four
	// directories, four images, one commit, one Release, one rollback. The
	// Release records the digest each workload was built to, so restoring it
	// restores that exact set — never today's.
	//
	// Absent is the original arrangement and stays the default: the workload
	// runs the project's own image, which is what a worker sharing the web
	// process's codebase wants and is one build rather than two.
	//
	// It is refused on a scheduled process. A cron workload is a command run
	// against an image on a timer; giving it a build of its own would mean
	// an image built on every commit whose only purpose is to be started
	// four times a day, which is the worker or service that ships it —
	// declare the build there and point the schedule at it.
	// +optional
	Build *ProcessBuildSpec `json:"build,omitempty"`

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

	// Previews says whether this workload runs in preview environments too.
	//
	// **A worker and a scheduled job are off unless they say otherwise, and
	// that is the decision rather than an omission.** A preview that emails
	// customers nightly is a bad afternoon, and a preview worker draining
	// the production queue is a worse one — the preview shares the project's
	// environment variables, so it is pointed at whatever the production
	// process is pointed at unless somebody overrode it.
	//
	// **A service is on unless it says otherwise**, and for the reason the
	// unit exists: a service workload is addressed by its own environment's
	// siblings and by nothing else, so leaving it out of a preview protects
	// nothing and breaks the preview — the web process comes up pointed at a
	// Service with no pods behind it. #271's whole argument is that a
	// preview of a multi-workload unit has to be the whole set; a default
	// that quietly shipped half of it would defeat it.
	//
	// It is a pointer so that an absent value can mean "the default for this
	// type" and `false` can mean false, which a bool with `omitempty` cannot
	// say: the field is dropped from the serialized object, and a service
	// deliberately kept out of previews would be silently put back on every
	// write. It is [PreviewsSpec.Enabled]'s reasoning, one level down.
	// +optional
	Previews *bool `json:"previews,omitempty"`
}

// ProcessBuildSpec is one workload's own build: which directory of the
// repository it is, and how the image comes out of it.
//
// It is [ProjectBuildSpec] for a workload, minus one value. `auto` is not
// among the strategies here, and that is deliberate rather than unfinished:
// detection's output is a framework, and what the platform does with a
// framework is fill in the web process's port and tell the buildpacks
// lifecycle what it is building. A workload has neither question open — a
// service names its own port, and a workload asking for buildpacks has said
// which of the two builders to use. What detection would add is a repository
// read per workload per build to answer a question already answered.
type ProcessBuildSpec struct {
	// Strategy is how the image is produced. It defaults to `dockerfile`,
	// which is what a monorepo shipping several images almost always has.
	// +kubebuilder:validation:Enum=dockerfile;buildpacks
	// +kubebuilder:default=dockerfile
	// +optional
	Strategy BuildStrategy `json:"strategy,omitempty"`

	// DockerfilePath is the Dockerfile, relative to RootDirectory, for a
	// dockerfile build.
	// +kubebuilder:default=Dockerfile
	// +optional
	DockerfilePath string `json:"dockerfilePath,omitempty"`

	// DockerfileTarget is which stage of that Dockerfile produces this
	// workload's image — BuildKit's `--target`. Empty is not "the last
	// stage": it is "nothing of its own to say", and the project's answer
	// stands in — the commit's own kitchen.json where it declared one, and
	// `ProjectBuildSpec.DockerfileTarget` where it did not. A unit built
	// from one multi-stage file names the stage once and each workload that
	// differs says so.
	//
	// It is per workload because that is the shape the feature is for: one
	// file that yields an API, a worker and a migration runner is ordinary
	// practice, and without a stage of its own each of them ships whichever
	// stage was written last — a build that succeeds and produces the wrong
	// thing.
	//
	// A workload built with buildpacks inherits nothing — the lifecycle has
	// no stages, so the unit's stage is not a stage of anything that image
	// builds — but one that names a stage itself keeps it and is refused for
	// it. A stage this workload's Dockerfile does not declare fails the
	// build naming this workload.
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9_.-]*$`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	DockerfileTarget string `json:"dockerfileTarget,omitempty"`

	// RootDirectory is the directory of the repository this workload is
	// built from — this workload's **build root**, on exactly the terms
	// `ProjectBuildSpec.RootDirectory` is the project's: it is what is
	// built, DockerfilePath is relative to it, and nothing above it is part
	// of this workload's build. internal/detect is where that meaning is
	// written down, and everything that spells or refuses one of these paths
	// reads it there.
	//
	// It is relative to the repository root, not to the project's own root
	// directory: the whole point is that the unit is a repository holding
	// several workloads, so each names where it lives once rather than
	// relative to whichever of them the project happened to call its own.
	// +kubebuilder:default=.
	// +optional
	RootDirectory string `json:"rootDirectory,omitempty"`
}

// EffectiveStrategy is how this workload is built, defaulted the way the CRD
// defaults it — the same shape as EffectiveConcurrency below, and for the
// same reason: a spec written before the field had a default has to read the
// same as one written after.
//
// There is deliberately no sibling for the two paths. What a root directory
// and a Dockerfile path *mean* — how they are spelled, and that neither may
// reach above what the build sees — is written once in internal/detect, which
// this package cannot import and must not restate: a workload's paths are a
// build's paths, and a second spelling of them here is how a workload's build
// would come to disagree with a project's.
func (b ProcessBuildSpec) EffectiveStrategy() BuildStrategy {
	if b.Strategy == "" {
		return BuildStrategyDockerfile
	}
	return b.Strategy
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

// RunsIn reports whether this workload is materialized in an environment of
// the given type. Production runs everything; a preview runs what opted in,
// and a service is opted in unless it said otherwise — see
// [ProcessSpec.Previews] for why the default turns on the type.
func (p ProcessSpec) RunsIn(envType EnvironmentType) bool {
	if envType != EnvironmentPreview {
		return true
	}
	return p.PreviewsEnabled()
}

// PreviewsEnabled is the declaration read with its per-type default applied.
func (p ProcessSpec) PreviewsEnabled() bool {
	if p.Previews != nil {
		return *p.Previews
	}
	return p.Type == ProcessService
}

// Addressed reports whether anything can reach this workload. Only a service
// is: a worker and a scheduled run have no Service in front of them at all.
func (p ProcessSpec) Addressed() bool {
	return p.Type == ProcessService
}

// LongRunning reports whether this workload is a Deployment rather than a
// CronJob — a worker or a service, both of which run continuously and differ
// only in whether anything addresses them.
func (p ProcessSpec) LongRunning() bool {
	return p.Type == ProcessWorker || p.Type == ProcessService
}

// ServiceEnvPrefix is the variable name a workload's siblings find it under,
// without the suffix: `KITCHEN_SERVICE_API_GATEWAY` for a service named
// `api-gateway`. A process name is a DNS label, so upper-casing it and
// turning its dashes into underscores is the whole conversion — and it is
// injective over DNS labels, since a label cannot contain an underscore.
func ServiceEnvPrefix(processName string) string {
	name := make([]rune, 0, len(processName))
	for _, r := range processName {
		switch {
		case r >= 'a' && r <= 'z':
			name = append(name, r-('a'-'A'))
		case r == '-':
			name = append(name, '_')
		default:
			name = append(name, r)
		}
	}
	return "KITCHEN_SERVICE_" + string(name)
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

	// Address is where a service workload answers, inside the cluster and
	// nowhere else: `http://<host>:<port>`, the same value its siblings read
	// out of `KITCHEN_SERVICE_<NAME>`. It is written by the reconciler that
	// created the Service rather than derived by every reader, so that one
	// place decides what the address is.
	//
	// It is empty for a worker and a scheduled job, which nothing addresses,
	// and for a service this environment does not run.
	// +optional
	Address string `json:"address,omitempty"`

	// Image is what this workload is running, when that is not the
	// Release's own image — a workload with a build of its own. It is the
	// digest reference the Release froze, echoed here so that a reader
	// looking at one environment can see the four images it is running
	// without opening the Release beside it.
	// +optional
	Image string `json:"image,omitempty"`

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

// ProcessNames is every workload an environment of the project can
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

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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// LocalObjectReference references another Kitchen object by name within the
// platform namespace. Cross-namespace references are not supported in v1alpha1.
type LocalObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SecretKeySelector selects a key of a Secret in the platform namespace.
type SecretKeySelector struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// ResourceClaimKeySelector selects a binding key exposed by a ResourceClaim.
type ResourceClaimKeySelector struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// EnvVar is a single environment variable for an application. Exactly one of
// Value, SecretRef, or FromResourceClaim should be set. PreviewValue, when set,
// replaces Value in preview environments.
type EnvVar struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Literal value.
	// +optional
	Value string `json:"value,omitempty"`

	// Literal value used in preview environments instead of Value.
	// +optional
	PreviewValue string `json:"previewValue,omitempty"`

	// Value taken from a Secret (typically synced from Infisical).
	// +optional
	SecretRef *SecretKeySelector `json:"secretRef,omitempty"`

	// Value taken from a ResourceClaim binding (e.g. a provisioned database URL).
	// +optional
	FromResourceClaim *ResourceClaimKeySelector `json:"fromResourceClaim,omitempty"`
}

// HealthSpec is how the platform finds out whether an application is
// *working*, rather than merely started.
//
// Until it existed, Kubernetes marked a pod Ready the moment its container
// process began, so an application applying a migration, warming a cache or
// opening a connection pool took production traffic while it was still doing
// it — on every deploy, and on every rollback, which is the one deploy path
// that must not add a second outage to the one it is fixing.
//
// A health endpoint is where an application says what "working" means for it:
// the queue is being drained, the feed is current, the migration finished.
// Absent one, the platform still checks something it can check itself — see
// [HealthSpec.Path].
type HealthSpec struct {
	// Path is the HTTP path the platform asks for. A 2xx or 3xx answer is
	// the application saying it is working.
	//
	// **Left unset, the check is a TCP connect to the port instead.** That
	// is a weaker claim than an HTTP 200 and much better than asserting a
	// readiness nothing established. It is deliberately not `GET /`: plenty
	// of applications answer that before they are ready, and one that 404s
	// there would never become Ready at all.
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Path string `json:"path,omitempty"`

	// Port the probe is made against, when it is not the port the
	// application is published on. A separate admin or metrics listener is
	// the usual reason.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// PeriodSeconds is how often the check is made. Defaults to
	// DefaultProbePeriodSeconds.
	// +kubebuilder:validation:Minimum=1
	// +optional
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`

	// TimeoutSeconds is how long one check may take before it counts as a
	// failure. Defaults to DefaultProbeTimeoutSeconds.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// FailureThreshold is how many checks in a row have to fail before a
	// running container is taken out of service — and, where the check is an
	// HTTP one, restarted. Defaults to DefaultProbeFailureThreshold.
	// +kubebuilder:validation:Minimum=1
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// StartupFailureThreshold is the same count for a container that has not
	// answered yet, and it is generous — DefaultStartupFailureThreshold
	// checks, so a container has StartupFailureThreshold x PeriodSeconds to
	// come up before the platform gives up on it.
	//
	// It is a separate number, and that is the whole point of a startup
	// probe: slow startup is a legitimate state, and a liveness threshold
	// loose enough to tolerate it is too loose to catch a wedge afterwards.
	// +kubebuilder:validation:Minimum=1
	// +optional
	StartupFailureThreshold int32 `json:"startupFailureThreshold,omitempty"`
}

// The probe timings a health check takes when it names none. They are the
// platform's, not Kubernetes' — the kubelet's own defaults put the failure
// threshold at 3 and the period at 10, which is what these agree with, and
// leave the startup threshold there too, which is what these do not.
const (
	DefaultProbePeriodSeconds      = int32(10)
	DefaultProbeTimeoutSeconds     = int32(2)
	DefaultProbeFailureThreshold   = int32(3)
	DefaultStartupFailureThreshold = int32(30)
)

// Period, Timeout, Failures and StartupFailures read one timing, defaulted.
// A nil receiver answers the defaults, so a workload that declared no health
// check at all is still described by the same four numbers.
func (h *HealthSpec) Period() int32 {
	if h == nil || h.PeriodSeconds <= 0 {
		return DefaultProbePeriodSeconds
	}
	return h.PeriodSeconds
}

func (h *HealthSpec) Timeout() int32 {
	if h == nil || h.TimeoutSeconds <= 0 {
		return DefaultProbeTimeoutSeconds
	}
	return h.TimeoutSeconds
}

func (h *HealthSpec) Failures() int32 {
	if h == nil || h.FailureThreshold <= 0 {
		return DefaultProbeFailureThreshold
	}
	return h.FailureThreshold
}

func (h *HealthSpec) StartupFailures() int32 {
	if h == nil || h.StartupFailureThreshold <= 0 {
		return DefaultStartupFailureThreshold
	}
	return h.StartupFailureThreshold
}

// ProbePort is the port the checks are made against: the health check's own
// where it names one, otherwise the container's. Zero means there is nothing
// to probe — a workload with no port of its own that declared no health check
// — and no probes are written at all.
func (h *HealthSpec) ProbePort(containerPort int32) int32 {
	if h != nil && h.Port > 0 {
		return h.Port
	}
	return containerPort
}

// HTTPPath is the path an HTTP check asks for, or empty for a TCP connect.
func (h *HealthSpec) HTTPPath() string {
	if h == nil {
		return ""
	}
	return h.Path
}

// RuntimeSpec describes how an application runs.
//
// The singleton rule is a refusal rather than a clamp, and deliberately: a
// value silently lowered reads back as a setting that did not take, and the
// project would go on believing it runs three. `has(self.singleton)` is the
// exact question for an `omitempty` bool — false is an absent key — and the
// replicas half has to allow an absent one too, since the CRD's own default
// only fills it in on a write.
//
// +kubebuilder:validation:XValidation:rule="!has(self.singleton) || !self.singleton || !has(self.replicas) || self.replicas <= 1",message="a singleton workload cannot run more than one replica: leave replicas at 1, or turn singleton off"
type RuntimeSpec struct {
	// Container port the application listens on, and the value of PORT in
	// every environment.
	//
	// Left unset it is derived from the framework the build detected — 8080
	// for a static site, 8000 for Python — and falls back to 3000 when
	// nothing was detected. It has no default for exactly that reason: a
	// default here would be indistinguishable from someone having chosen
	// 3000, and detection would never get to answer.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// Replica count for production environments. Previews always run 1.
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Singleton says two of this workload must never run at once.
	//
	// Kitchen models what an application *is* in a fair amount of detail —
	// its criticality, its data class, its residency — and had no way to
	// record the much simpler fact that a second copy of it is a bug. For a
	// stateless web application it is not: a rolling update overlaps the
	// outgoing pod and the incoming one for a few seconds and nobody
	// notices. For an application with a poller, a scheduler or an ingest
	// loop in the same binary as the web server, those few seconds are the
	// loop running twice against a shared store — and duplicate rows in a
	// table something downstream reads as a record of what happened when is
	// not an error at the time and not obviously wrong afterwards.
	//
	// What it does is set `strategy: Recreate` on the Deployment: the old
	// pod stops before the new one starts. The cost is a gap in serving
	// during a deploy, which is the correct trade for a workload that cannot
	// overlap — and the project has said so. It also refuses Replicas above
	// one, at admission and at the API, rather than clamping it.
	//
	// Leader election stays the application's problem. Not overlapping it
	// during a deploy the platform itself initiated is the platform's.
	// +optional
	Singleton bool `json:"singleton,omitempty"`

	// NotRequestDriven says this workload does work nobody asked for.
	//
	// Scale to zero is request-driven by construction: the HTTP add-on parks
	// an environment nobody is asking for and the interceptor brings it back
	// on the next request to its URL. For an application that works only
	// when asked, that is free money. An application with a background loop
	// parked stops — and stops silently. The hole that leaves in whatever it
	// was collecting is indistinguishable from the upstream having been
	// down, which is worse than the pods simply being gone: the environment
	// comes back, serves, and reports nothing wrong, while its data has a
	// gap in it that means something it did not do.
	//
	// Setting it turns idling off for every environment of the Project —
	// previews included, which is where it matters, since previews idle by
	// default and a preview pointed at a real datastore will quietly write a
	// partial record of a period it was asleep for. The Environment says so
	// in its ScaleToZero condition rather than merely not idling.
	//
	// It lives here, on the runtime, because it describes what the workload
	// *is* rather than what it costs — but the idling decision reads the
	// Project's live value, not the Release's frozen copy, for exactly the
	// reason `ScaleToZeroPolicy` is not snapshotted either: rolling back
	// must not quietly un-park an environment, and saying "this one must not
	// be parked" must not have to wait for a build.
	// +optional
	NotRequestDriven bool `json:"notRequestDriven,omitempty"`

	// Command replaces the image's entrypoint, and Args its arguments —
	// the same two fields a ProcessSpec has, in the same exec form: a list
	// of words, never a shell line, for the same reason the build jobs take
	// two containers instead of an `sh -c`.
	//
	// Plenty of programs are configured by flags rather than by environment
	// variables, and the alternative was an entrypoint script in every
	// project translating one into the other — which is precisely the
	// per-project boilerplate Kitchen exists to delete.
	//
	// The `PORT` contract is untouched: a buildpacks-built image still reads
	// it, and an image started with a command of its own is free to ignore
	// it exactly as a Dockerfile build already could.
	// +optional
	// +listType=atomic
	Command []string `json:"command,omitempty"`

	// +optional
	// +listType=atomic
	Args []string `json:"args,omitempty"`

	// PreviewArgs replaces Args in preview environments, and is the sibling
	// of EnvVar.PreviewValue — the same idea for the same reason. A preview
	// runs against a fake or a seeded data source where production runs
	// against the real one, from the same commit and the same artifact,
	// because the artifact is built once and never rebuilt.
	//
	// It replaces the list rather than extending it: an override that
	// appended could not remove a flag, and removing one is half of what a
	// preview wants. An empty list is no override, exactly as an empty
	// PreviewValue is — the sibling is the sibling all the way down, and it
	// is what lets an override be taken away through an API that never
	// distinguishes an absent field from a cleared one.
	// +optional
	// +listType=atomic
	PreviewArgs []string `json:"previewArgs,omitempty"`

	// Compute resources per replica.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Health is how the platform checks that the application is working.
	// Every environment gets probes whether or not this is set — absent, they
	// are a TCP connect to the container port on the platform's default
	// timings — and setting it is how an application says what working means
	// for it.
	//
	// Like the rest of RuntimeSpec it is snapshotted into the Release, so a
	// rollback restores the check the release was running with, and previews
	// inherit it from production.
	// +optional
	Health *HealthSpec `json:"health,omitempty"`

	// Security is the posture every workload of this project runs under —
	// the web process, its workers and its scheduled runs alike, because
	// they are one image and a posture is a property of the image rather
	// than of the command it is started with.
	//
	// A project that declares nothing still gets [SecuritySpec]'s default,
	// which is the platform's and not the image's. Like the rest of
	// RuntimeSpec it is snapshotted into the Release, so a rollback restores
	// the posture that release ran under.
	// +optional
	Security *SecuritySpec `json:"security,omitempty"`

	// Init is what the web process needs done inside the volumes it mounts
	// before its own container starts: directories that have to exist, and
	// configuration files seeded into the volume once (#348).
	//
	// It is here rather than on the claim because it is the *process's*
	// requirement — an image that will not start on an empty filesystem —
	// and it is snapshotted into the Release with the rest of the runtime,
	// so rolling back restores what that release seeded with. Each entry
	// names one of the volumes this workload mounts; see [VolumeInit].
	// +optional
	// +listType=map
	// +listMapKey=volume
	Init []VolumeInit `json:"init,omitempty"`
}

// SecuritySpec is the security posture an application's containers run under.
//
// Until it existed nothing was applied to an application's workloads at all:
// they ran as whatever the image happened to be, in a namespace deliberately
// relaxed to `privileged` for the build tooling's sake — rootless BuildKit
// needs an unconfined seccomp and AppArmor profile, which Pod Security admits
// at that level alone. That is the *build's* requirement, not the
// application's, which is what makes a per-workload security context the
// right lever rather than the namespace level. An application arriving from
// somewhere that pinned a read-only root filesystem, dropped capabilities and
// a non-root user lost all three on the way in, and lost them silently.
//
// **Every field is zero-means-the-default**, the reading HealthSpec's timings
// already have, so an absent block and an empty one are the same posture and
// a field can be taken back off through an API that never distinguishes an
// absent key from a cleared one.
//
// The platform's default is deliberately not the tightest thing available.
// Two hardenings cost a working image nothing and are applied to every
// workload: the runtime's own seccomp profile, and no privilege escalation.
// The three that would — a read-only root filesystem, dropped capabilities,
// a non-root user — are **not** defaulted, because an image that quietly
// writes to its own filesystem is a large and real population and a default
// that broke it would break it on upgrade with nothing said anywhere. Those
// three are what a project asks for here, and what the platform then applies
// and reports.
type SecuritySpec struct {
	// RunAsNonRoot refuses to start a container whose image would run as
	// uid 0. The kubelet checks it before the container starts, so an image
	// that would have run as root fails at the pod rather than three layers
	// into whatever it did with the privilege.
	// +optional
	RunAsNonRoot bool `json:"runAsNonRoot,omitempty"`

	// RunAsUser and RunAsGroup are the uid and gid the containers run as,
	// overriding the image's own. Zero is not "run as root": it is the
	// image's own user, left alone — an image that must run as root already
	// does, and saying so again here would be a setting that changed
	// nothing.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RunAsUser int64 `json:"runAsUser,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +optional
	RunAsGroup int64 `json:"runAsGroup,omitempty"`

	// FSGroup is the supplementary group the volumes mounted into the
	// containers are owned by. A freshly provisioned PersistentVolume comes
	// up owned by `root:root`, so a workload that asked to run as anybody
	// else is handed a volume it cannot write — it starts, reads as
	// healthy, and fails on its first write. The kubelet chowns the volume
	// to this gid before the container starts, which is the only thing that
	// makes the two features — this posture and a volume claim — work
	// together.
	//
	// It is a pod-level field, not a container-level one, so it lands
	// beside RunAsNonRoot rather than in the per-container context. Zero is
	// the same reading RunAsUser's is: the volume's own ownership left
	// alone, not a request to own it as gid 0.
	// +kubebuilder:validation:Minimum=0
	// +optional
	FSGroup int64 `json:"fsGroup,omitempty"`

	// FSGroupChangePolicy is when the kubelet does that chown. Left alone
	// it is Kubernetes' own default, `Always`, which walks the whole volume
	// on every start — correct, and on a large volume slow enough to be the
	// reason a pod takes minutes to come up. `OnRootMismatch` skips the
	// walk when the volume's own root already has the right ownership,
	// which is what a workload with a big persistent volume reaches for.
	//
	// The default is deliberately not moved: `OnRootMismatch` looks only at
	// the top of the volume, so a subtree left behind by a previous uid
	// stays unwritable and the failure arrives long after the change that
	// caused it. That is a trade a project makes knowingly, not one the
	// platform makes for it.
	//
	// It is meaningless without FSGroup and refused without one, rather
	// than stored as a setting that does nothing.
	// +optional
	FSGroupChangePolicy FSGroupChangePolicy `json:"fsGroupChangePolicy,omitempty"`

	// ReadOnlyRootFilesystem mounts the container's own filesystem read
	// only. An application that writes to a path it did not declare fails
	// on the write rather than on the next node it lands on.
	//
	// It is off by default and that is the decision: an image that writes a
	// temporary file, a cache or a socket into its own filesystem is
	// ordinary, and a default that broke it would break it on upgrade with
	// no warning anywhere.
	// +optional
	ReadOnlyRootFilesystem bool `json:"readOnlyRootFilesystem,omitempty"`

	// AllowPrivilegeEscalation puts back the one default the platform
	// tightens on the container. Left alone, no process in the container can
	// gain more privileges than its parent — `no_new_privs`, which is what
	// stops a setuid binary being a way out of the user the container runs
	// as. An image that legitimately needs one (`sudo`, a setuid helper) says
	// so here rather than losing it silently.
	// +optional
	AllowPrivilegeEscalation bool `json:"allowPrivilegeEscalation,omitempty"`

	// DropCapabilities are the Linux capabilities taken away from the
	// containers, in the kernel's own spelling without the `CAP_` prefix —
	// `NET_RAW`, `SYS_ADMIN` — or the single entry `ALL`.
	//
	// There is deliberately no list of capabilities to *add*. The platform
	// drops none by default, so nothing has to be given back, and a project
	// that could add one would be able to grant its own container more than
	// its image asked for.
	//
	// The spelling is checked here as well as at the API, since not
	// everything writes through the API: a capability the kernel has never
	// heard of reaches a container that then does not start.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:items:Pattern=`^[A-Z][A-Z0-9_]*$`
	DropCapabilities []string `json:"dropCapabilities,omitempty"`
}

// FSGroupChangePolicy is when the kubelet changes the ownership of a volume
// it is about to mount. The values and their meanings are corev1's, spelled
// the same way, because that is what they become.
// +kubebuilder:validation:Enum=Always;OnRootMismatch
type FSGroupChangePolicy string

const (
	// FSGroupChangeAlways chowns the whole volume on every start. It is
	// Kubernetes' own default and what an unset policy takes.
	FSGroupChangeAlways FSGroupChangePolicy = "Always"
	// FSGroupChangeOnRootMismatch chowns only when the volume's own root
	// does not already have the ownership asked for, which turns a
	// minutes-long recursive walk of a large volume into a single stat.
	FSGroupChangeOnRootMismatch FSGroupChangePolicy = "OnRootMismatch"
)

// CapabilityDropAll is the entry that means every capability, spelled the way
// corev1 spells it.
const CapabilityDropAll = "ALL"

// SeccompProfileRuntimeDefault is the seccomp profile every workload of every
// project runs under. It is the platform's rather than the project's and
// there is no field for it: it is the container runtime's own profile, which
// Kubernetes does not apply unless it is asked to, so applying it costs a
// working image nothing and leaves nothing to decide.
const SeccompProfileRuntimeDefault = corev1.SeccompProfileTypeRuntimeDefault

// DropsAll reports whether the declaration drops every capability, which is
// the one entry that is not a capability's name.
func (s *SecuritySpec) DropsAll() bool {
	if s == nil {
		return false
	}
	for _, capability := range s.DropCapabilities {
		if strings.EqualFold(capability, CapabilityDropAll) {
			return true
		}
	}
	return false
}

// Declared is the posture in words: what this project asked for beyond the
// platform's default, one phrase per constraint, in the order they are worth
// reading. It is empty for a project that declared nothing.
//
// It exists so that a workload that cannot start under the posture it asked
// for can be told which constraints are in force, rather than leaving a
// CrashLoopBackOff whose cause is three layers down. The API reads it back
// too, so the dashboard and a failure message describe one posture in one
// vocabulary.
func (s *SecuritySpec) Declared() []string {
	if s == nil {
		return nil
	}
	declared := make([]string, 0, 6)
	if s.RunAsNonRoot {
		declared = append(declared, "it must not run as root")
	}
	if s.RunAsUser > 0 {
		declared = append(declared, fmt.Sprintf("it runs as uid %d", s.RunAsUser))
	}
	if s.RunAsGroup > 0 {
		declared = append(declared, fmt.Sprintf("it runs as gid %d", s.RunAsGroup))
	}
	if s.FSGroup > 0 {
		volumes := fmt.Sprintf("its volumes are owned by gid %d", s.FSGroup)
		if s.FSGroupChangePolicy == FSGroupChangeOnRootMismatch {
			volumes += ", changed only when the volume's own root does not already match"
		}
		declared = append(declared, volumes)
	}
	if s.ReadOnlyRootFilesystem {
		declared = append(declared, "its root filesystem is read only")
	}
	if len(s.DropCapabilities) > 0 {
		declared = append(declared, "it drops "+strings.Join(s.DropCapabilities, ", "))
	}
	if s.AllowPrivilegeEscalation {
		declared = append(declared, "privilege escalation is allowed, which the platform otherwise denies")
	}
	return declared
}

// EscalationAllowed is whether a process in the container may gain
// privileges. A project that said nothing gets the platform's answer, which
// is no.
func (s *SecuritySpec) EscalationAllowed() bool {
	return s != nil && s.AllowPrivilegeEscalation
}

// ArgsFor is the argument list an environment of this type starts the
// application with: the preview override where there is one and this is a
// preview, production's otherwise.
//
// An empty PreviewArgs is no override, the same reading an empty PreviewValue
// gets. A preview that should be started with no arguments at all where
// production has some is the one thing this cannot say, and it is the price of
// the two overrides meaning the same thing — which matters more, because the
// alternative is a difference between an absent list and an empty one that no
// JSON body could express and no read of the project could report.
func (r RuntimeSpec) ArgsFor(envType EnvironmentType) []string {
	if envType == EnvironmentPreview && len(r.PreviewArgs) > 0 {
		return r.PreviewArgs
	}
	return r.Args
}

// BuildStrategy selects how an image is produced from a repository.
// +kubebuilder:validation:Enum=auto;dockerfile;buildpacks
type BuildStrategy string

const (
	// BuildStrategyAuto detects the framework and picks a strategy.
	BuildStrategyAuto BuildStrategy = "auto"
	// BuildStrategyDockerfile builds from a Dockerfile in the repository.
	BuildStrategyDockerfile BuildStrategy = "dockerfile"
	// BuildStrategyBuildpacks builds with Cloud Native Buildpacks.
	BuildStrategyBuildpacks BuildStrategy = "buildpacks"
)

// TLSMode selects how edge TLS is provided.
// +kubebuilder:validation:Enum=cloudflared;acme;none
type TLSMode string

const (
	// TLSModeCloudflared terminates TLS at the Cloudflare edge via cloudflared.
	TLSModeCloudflared TLSMode = "cloudflared"
	// TLSModeACME issues certificates with cert-manager.
	TLSModeACME TLSMode = "acme"
	// TLSModeNone serves plain HTTP (development only).
	TLSModeNone TLSMode = "none"
)

// Scheme is how generated URLs are reached from outside in this mode. Every
// mode but "none" is served over HTTPS, whether the certificate is terminated
// by the Gateway or by a tunnel in front of it; "none" gets an HTTP listener
// alone, so every URL the platform publishes — the OIDC issuer included — has
// to name the scheme that is actually served.
func (m TLSMode) Scheme() string {
	if m == TLSModeNone {
		return "http"
	}
	return "https"
}

// Capability is an abstract feature a Connection provider implements. The
// operator matches on capabilities, never on provider names.
// +kubebuilder:validation:Enum=gitSource;statusChecks;imageStore;database;objectStore;backgroundJobs;cache
type Capability string

const (
	CapabilityGitSource    Capability = "gitSource"
	CapabilityStatusChecks Capability = "statusChecks"
	CapabilityImageStore   Capability = "imageStore"
	CapabilityDatabase     Capability = "database"
	// CapabilityObjectStore is an S3-compatible store a bucket can be
	// provisioned in — the bundled MinIO, or a store somebody else runs.
	CapabilityObjectStore Capability = "objectStore"
	// CapabilityBackgroundJobs is durable background work — retries,
	// sleeps, fan-out, cron — run by a service the application's worker
	// connects out to. An inngest claim is provisioned through it.
	CapabilityBackgroundJobs Capability = "backgroundJobs"
	// CapabilityCache is a Redis-speaking server a redis claim is
	// provisioned from: somewhere to put what an application can afford to
	// recompute, or work it cannot afford to lose.
	CapabilityCache Capability = "cache"
)

// ImageSourceSpec is an image this platform did not build (#307).
//
// `ProcessBuildSpec` says how an image is produced from the repository, and
// its absence used to mean one thing — run the project's own image. This is
// the third answer to that question, and the one an application that arrives
// as a published image has: there is no commit, no directory and no builder,
// only a repository somewhere and a version of it to run.
//
// It is one type for both places that ask, because they are one question. A
// named workload declares it as [ProcessSpec.Image]; the web process declares
// it as [ProjectSourceSpec.Image], which is where it has to go because the
// web process is `spec.runtime` and not an entry in the process list.
//
// The version is a tag, a digest, or both. A tag alone is what a vendor
// publishes and what somebody types; a digest is what a Release freezes, so
// that a rollback restores the exact image that release ran rather than
// whatever the tag has moved to since. Naming both is the strictest form —
// this tag, and it must still be this content.
//
// +kubebuilder:validation:XValidation:rule="has(self.tag) || has(self.digest)",message="a vendored image needs a tag or a digest: without one there is no way to say which version of the repository to run"
type ImageSourceSpec struct {
	// Repository is where the image lives, registry host included and
	// without a tag or a digest — `ghcr.io/home-assistant/home-assistant`,
	// `docker.io/library/postgres`. The host is written out rather than
	// defaulted to Docker Hub: every other image reference the platform
	// stores is fully qualified, and a bare `postgres` that resolved
	// somewhere by convention would be the one that did not.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^([a-z0-9]+([.\-_][a-z0-9]+)*(:[0-9]+)?/)?[a-z0-9]+([._\-][a-z0-9]+)*(/[a-z0-9]+([._\-][a-z0-9]+)*)*$`
	Repository string `json:"repository"`

	// Tag is the version as the vendor publishes it — `2026.9.1`, `stable`.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9._-]*$`
	// +optional
	Tag string `json:"tag,omitempty"`

	// Digest pins the exact content, `sha256:` and sixty-four hex digits. It
	// is what a Release records whatever was declared here, because a tag is
	// a moving target and a rollback that followed one would not be a
	// rollback.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	Digest string `json:"digest,omitempty"`

	// ConnectionRef is the credential the image is *pulled* with: a
	// Connection with the imageStore capability, holding a docker config for
	// the registry this repository is on.
	//
	// It is separate from `spec.registry` — the credential the platform
	// *pushes* under — because they are two registries and, for a vendored
	// image, two accounts: the platform never writes where a vendor
	// publishes, and the account it pushes its own builds with has no
	// business being handed to a third party's registry.
	//
	// Absent means an anonymous pull, which is what the great majority of
	// vendored images want: a public image needs no credential at all, and
	// requiring one would be a Connection somebody had to invent.
	// +optional
	ConnectionRef *LocalObjectReference `json:"connectionRef,omitempty"`

	// Signature is whose signature on this image the platform should check,
	// and against what.
	//
	// Absent means the platform still *looks* — whether a vendor signs at all
	// is a fact worth recording either way — and records what it found
	// without being able to say it is the right signer's. See
	// ImageSignatureSpec, and UpstreamSignatureResult for the three answers.
	//
	// It can also be declared once on the pulling Connection, which is where
	// it belongs when a whole registry is one vendor's; what is written here
	// wins for this image.
	// +optional
	Signature *ImageSignatureSpec `json:"signature,omitempty"`
}

// ImageSignatureSpec says whose signature on a vendored image is acceptable.
//
// A signature nobody named an expected signer for proves that *somebody*
// signed the image, which is close to worthless: anyone can sign anything. So
// the platform records "a signature is attached and nothing here says whose
// it should be" as **unverifiable** rather than as verified, and this is how
// an installation stops that being the answer.
type ImageSignatureSpec struct {
	// PublicKeyRef names a Secret in the platform namespace holding the
	// vendor's public key under `public.pem` — the same spelling the
	// platform's own signing key uses, so an operator learns one convention.
	//
	// It is what makes a `verified` result possible. Key-based verification
	// is complete on its own: the bytes either check out under that key or
	// they do not, and the platform needs no trust root, no transparency log
	// and no network to say which.
	// +optional
	PublicKeyRef *LocalObjectReference `json:"publicKeyRef,omitempty"`

	// Identity is the signer the signature must additionally name — a
	// keyless signature's certificate subject, `releases@example.com` or a
	// workflow URI. Matched exactly and case-insensitively, never as a
	// pattern.
	//
	// **On its own it cannot produce a `verified` result**, and that is a
	// deliberate refusal rather than a gap: a certificate embedded in a
	// signature is a claim by whoever wrote the certificate, and Kitchen
	// holds no Fulcio root to chain it to. An identity with no key beside it
	// therefore reads `unverifiable`, saying so. Naming it is still worth
	// doing — the fact travels into the adoption attestation, and the
	// platform will not call a signature verified that names somebody else.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Identity string `json:"identity,omitempty"`

	// Issuer is the OIDC issuer that identity was certified by
	// (`https://token.actions.githubusercontent.com`). It narrows Identity
	// and means nothing without one.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Issuer string `json:"issuer,omitempty"`
}

// SignatureIdentity is the identity this image's signature must name, empty
// where none is required.
func (i ImageSourceSpec) SignatureIdentity() string {
	if i.Signature == nil {
		return ""
	}
	return i.Signature.Identity
}

// Reference is the image reference to run: the digest where one is pinned,
// and the tag otherwise.
//
// A digest wins over a tag when both are named, because both together mean
// "this tag, and it must still be this content" — and the content is what
// runs. What resolves a tag *to* a digest is the acquisition path, which
// records what it found on the Release; this is what it asks the registry
// about.
func (i ImageSourceSpec) Reference() string {
	if i.Digest != "" {
		return i.Repository + "@" + i.Digest
	}
	if i.Tag != "" {
		return i.Repository + ":" + i.Tag
	}
	return i.Repository
}

// PullConnection is the Connection this image is pulled with, empty for an
// anonymous pull.
func (i ImageSourceSpec) PullConnection() string {
	if i.ConnectionRef == nil {
		return ""
	}
	return i.ConnectionRef.Name
}

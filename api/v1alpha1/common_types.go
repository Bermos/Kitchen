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
// +kubebuilder:validation:Enum=gitSource;statusChecks;imageStore;database
type Capability string

const (
	CapabilityGitSource    Capability = "gitSource"
	CapabilityStatusChecks Capability = "statusChecks"
	CapabilityImageStore   Capability = "imageStore"
	CapabilityDatabase     Capability = "database"
)

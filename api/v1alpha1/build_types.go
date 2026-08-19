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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitRevision identifies the commit a Build builds.
type GitRevision struct {
	// +kubebuilder:validation:MinLength=7
	SHA string `json:"sha"`

	// +kubebuilder:validation:MinLength=1
	Branch string `json:"branch"`

	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	Author string `json:"author,omitempty"`

	// Pull request number, when the commit belongs to one.
	// +optional
	PullRequest *int32 `json:"pullRequest,omitempty"`
}

// BuildSpec defines one build execution for one commit. Builds are immutable:
// a rebuild is a new Build object.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Build spec is immutable"
type BuildSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	Git GitRevision `json:"git"`
}

// BuildPhase is the coarse lifecycle summary of a Build.
// +kubebuilder:validation:Enum=Queued;Running;Succeeded;Failed;Cancelled
type BuildPhase string

const (
	BuildQueued    BuildPhase = "Queued"
	BuildRunning   BuildPhase = "Running"
	BuildSucceeded BuildPhase = "Succeeded"
	BuildFailed    BuildPhase = "Failed"
	BuildCancelled BuildPhase = "Cancelled"
)

// ArtifactStatus identifies what a build produced and records whether the
// platform managed to attest it.
//
// The digest is the identity everything downstream keys on: the artifact is
// built once and never rebuilt, so a rebuild is a different artifact and every
// claim about the old one is a claim about something that no longer exists.
//
// What is deliberately *not* here is the evidence itself. Attestations live in
// the registry, attached to this digest through OCI referrers, and a copy of
// them on this object would be a second source of truth that an installation
// which stopped using Kitchen would lose. The fields below say where to look
// and under whose key, and nothing more.
type ArtifactStatus struct {
	// Repository the image was pushed to, without a tag or digest.
	// +optional
	Repository string `json:"repository,omitempty"`

	// Digest of the image manifest, as `sha256:…`. Empty when the builder
	// pushed successfully but did not report a digest, which is the one case
	// where the platform has an image it cannot make claims about.
	// +optional
	Digest string `json:"digest,omitempty"`

	// AttestedAt is when the platform attached its own build record to the
	// digest.
	// +optional
	AttestedAt *metav1.Time `json:"attestedAt,omitempty"`

	// KeyID is the signing key the build record was signed under, which is
	// what a verifier matches against the published public key.
	// +optional
	KeyID string `json:"keyID,omitempty"`

	// Evidence lists what is now attached to the digest, by predicate type.
	//
	// It is an index, not a copy: the attestations themselves stay in the
	// registry, and this says which of them exist so that a screen can show
	// "provenance and an SBOM are attached" without a registry round trip.
	// A reader that needs the content asks the registry, or asks the API for
	// the materialized evidence set, and either way gets the signed bytes
	// rather than this summary.
	// +optional
	Evidence []ArtifactEvidence `json:"evidence,omitempty"`

	// Message explains an artifact the platform could not attest. An
	// unattested artifact is not a failed build — the image is real and the
	// deployment is honest — but it is one that no policy requiring evidence
	// will ever let move, which is where that fact is meant to bite.
	// +optional
	Message string `json:"message,omitempty"`
}

// BuildCacheStatus is what the layer cache did for one build, which is the
// difference between "this build was slow" and "this build was slow because it
// had nothing to reuse".
//
// A cold build is not a fault and a warm one is not a promise: BuildKit and the
// buildpacks lifecycle both decide layer by layer what they can reuse, and
// neither reports how much it did. What is recorded here is what the platform
// can stand behind — whether a cache was configured, where it is, and whether
// it existed when the build started.
type BuildCacheStatus struct {
	// Enabled is whether this build imported from and exported to the cache.
	// False with a Message is a cache the platform turned off for a reason;
	// false without one is an installation that asked for no caching.
	Enabled bool `json:"enabled"`

	// Ref is the registry reference the cache is kept under, empty when
	// there is no cache.
	// +optional
	Ref string `json:"ref,omitempty"`

	// Mode is how much of the build was exported: max or min. Empty for a
	// buildpacks build, whose lifecycle has one cache image and no such
	// choice.
	// +optional
	Mode BuildCacheMode `json:"mode,omitempty"`

	// Warm is whether the cache existed when the build started. A cold build
	// is the first of its scope, or the first after the cache was removed —
	// either way it had nothing to reuse and the next one will.
	Warm bool `json:"warm"`

	// Message explains a cache that is not there: the first build of a
	// project, or a registry that would not keep the cache manifest.
	// +optional
	Message string `json:"message,omitempty"`
}

// ArtifactEvidence names one attestation attached to an artifact.
//
// It carries the predicate type and nothing that interprets it. Which claims
// count as provenance, and which formats of bill of materials are acceptable,
// are policy questions, and phase three is where policy lives — a status field
// that answered them here would be a verdict, and gates do not emit verdicts.
type ArtifactEvidence struct {
	// PredicateType is the URI of what the attestation asserts: SLSA's for
	// provenance, SPDX's or CycloneDX's for a bill of materials, one of
	// Kitchen's own for a claim no standard covers.
	PredicateType string `json:"predicateType"`

	// Manifest is the digest of the manifest that refers to the artifact and
	// holds this evidence, which is where a reader goes to fetch it without
	// listing anything.
	// +optional
	Manifest string `json:"manifest,omitempty"`

	// Source says who made the claim this evidence carries: `builder` for
	// one the build process itself produced and the platform countersigned,
	// `platform` for one the reconciler made on its own account.
	//
	// The distinction matters to anyone reading the evidence. The platform's
	// signature is on both, so the signature cannot tell them apart, and a
	// claim about what a build did is worth more when the thing that did the
	// building made it.
	// +kubebuilder:validation:Enum=builder;platform
	// +optional
	Source string `json:"source,omitempty"`
}

// GatePhase is what a quality gate is doing, or what became of it.
//
// The distinction the whole type exists to draw is between `Failed` and
// `Completed`: a gate that ran and found a hundred critical vulnerabilities
// has **completed**, because it did its job. `Failed` means the gate did not
// run — the image would not pull, the scanner crashed, the timeout expired —
// and there is no evidence either way.
//
// Treating those two the same is the classic way to build a compliance system
// that reports green while its scanners are broken.
// +kubebuilder:validation:Enum=Pending;Running;Completed;Failed;Skipped
type GatePhase string

const (
	GatePending   GatePhase = "Pending"
	GateRunning   GatePhase = "Running"
	GateCompleted GatePhase = "Completed"
	GateFailed    GatePhase = "Failed"
	GateSkipped   GatePhase = "Skipped"
)

// QualityGateStatus is one gate's run over one artifact.
//
// It says whether the gate ran and whether its findings were signed. It does
// not say whether the findings were acceptable, because nothing at this level
// knows: that is a policy question about the environment an artifact is being
// promoted to, and it is answered at promotion.
type QualityGateStatus struct {
	// Name is the gate's, from the platform configuration.
	Name string `json:"name"`

	// Phase is what became of the run. `Completed` means the gate ran and
	// its findings were recorded, whatever they were.
	// +optional
	Phase GatePhase `json:"phase,omitempty"`

	// Source says where the result came from: `platform` for a gate Kitchen
	// ran, `external` for one ingested from somewhere that ran it already —
	// an application's own CI, usually. An external result is evidence about
	// who reported it as much as about what it says, which is why the two are
	// never merged into one word.
	// +kubebuilder:validation:Enum=platform;external
	// +optional
	Source string `json:"source,omitempty"`

	// ReportedBy names the identity that submitted an external result. It is
	// empty for a gate the platform ran, where the platform is the answer.
	// +optional
	ReportedBy string `json:"reportedBy,omitempty"`

	// PredicateType is what the gate's attestation was written under, so a
	// reader can find it among everything else attached to the artifact.
	// +optional
	PredicateType string `json:"predicateType,omitempty"`

	// Attested is when the findings were signed and attached. Absent on a
	// gate that ran but whose findings could not be attested — which is a
	// gate that produced no evidence, and is worth telling apart from one
	// that never ran.
	// +optional
	Attested *metav1.Time `json:"attested,omitempty"`

	// FinishedAt is when the run ended.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Message explains a gate that did not run, or findings that were not
	// attested. It never carries findings: those are in the attestation, in
	// the registry, against the artifact's digest.
	// +optional
	Message string `json:"message,omitempty"`
}

// BuildStatus defines the observed state of a Build.
type BuildStatus struct {
	// +optional
	Phase BuildPhase `json:"phase,omitempty"`

	// Framework detected by the auto strategy (e.g. nextjs, vite, static).
	// +optional
	DetectedFramework string `json:"detectedFramework,omitempty"`

	// Image reference by digest, never by tag.
	// +optional
	Image string `json:"image,omitempty"`

	// Artifact is what the build produced, identified by content rather than
	// by name, and what the platform has asserted about it.
	// +optional
	Artifact *ArtifactStatus `json:"artifact,omitempty"`

	// Cache is what the layer cache did for this build. Absent on a build
	// that never got as far as being run.
	// +optional
	Cache *BuildCacheStatus `json:"cache,omitempty"`
	// Gates is what each quality gate did, which is not the same question as
	// what it found. What it found is in its attestation.
	// +optional
	// +listType=map
	// +listMapKey=name
	Gates []QualityGateStatus `json:"gates,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.spec.git.branch`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Build is the Schema for the builds API.
type Build struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BuildSpec   `json:"spec,omitempty"`
	Status BuildStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BuildList contains a list of Build.
type BuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Build `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Build{}, &BuildList{})
}

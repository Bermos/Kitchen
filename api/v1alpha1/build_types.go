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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProjectLabel names the Project a Build, Environment or Release belongs to,
// which is how everything owned by a project is found without walking owner
// references.
const ProjectLabel = "kitchen.bermos.dev/project"

// BuildNameFor is the name of the Build for a commit the platform was told
// about, rather than one somebody asked to be rebuilt.
//
// It is derived from the commit so that two things learning about the same
// commit produce one Build: a webhook redelivery, and a project's first build
// racing the push that happened while it was being created, both land on an
// AlreadyExists that means "already building" rather than on a second run of
// the same commit. A rebuild somebody asked for is a different question and
// uses this only as a GenerateName prefix.
func BuildNameFor(project, sha string) string {
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return fmt.Sprintf("%s-bld-%s", project, sha)
}

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

// VEXStatus is one ingested OpenVEX document, as the Build indexes it.
//
// It records what the document said about itself and what the platform
// observed about its arrival, and it keeps the two apart on purpose. `Author`
// is the document's own claim about who wrote it; `SubmittedBy` is the
// authenticated identity that sent it, which is the platform's own
// observation and the only half it can vouch for. Conflating them would let
// anyone with a developer's key file an assertion under a security team's
// name and have it read exactly like theirs.
type VEXStatus struct {
	// DocumentID is the document's `@id`. It is how a re-submission of the
	// same document replaces its row rather than adding one; a document
	// carrying no `@id` is indexed under the digest of its envelope instead,
	// because a row nothing can be matched back to is not an index.
	// +optional
	DocumentID string `json:"documentID,omitempty"`

	// Author is the document's declared author, per OpenVEX. Where a
	// statement named its own supplier, that supplier is the author of that
	// statement and this is the author of the document collecting it.
	// +optional
	Author string `json:"author,omitempty"`

	// SubmittedBy is the authenticated identity that handed the document to
	// the platform.
	// +optional
	SubmittedBy string `json:"submittedBy,omitempty"`

	// Statements is how many assertions the document carries, and
	// Vulnerabilities which ones they are about — enough for a reader to see
	// what a document touches without fetching it.
	// +optional
	Statements int32 `json:"statements,omitempty"`
	// +optional
	// +listType=atomic
	Vulnerabilities []string `json:"vulnerabilities,omitempty"`

	// Manifest is the digest of the manifest that now refers to the artifact
	// and holds this evidence.
	// +optional
	Manifest string `json:"manifest,omitempty"`

	// IngestedAt is when the platform signed and attached it. It is not the
	// document's own timestamp, which is the author's claim about when they
	// decided — both matter and neither substitutes for the other.
	// +optional
	IngestedAt *metav1.Time `json:"ingestedAt,omitempty"`
}

// SourceProvenanceStatus is what the git provider said about how the commit
// arrived, recorded at the moment it was asked.
//
// The timing is the point. An approval can be dismissed by a later push and a
// pull request can be edited; what a supervisor needs months later is what was
// true when the change was built, not what the provider's UI says today. So it
// is resolved before the build is scheduled and written down here, and the
// attestation minted afterwards repeats these fields rather than asking again.
//
// Every field is a **third party's claim**. The platform did not witness the
// review; it asked GitHub, and GitHub answered. That is why Provider is
// recorded beside the rest: evidence that hides whose claim it is repeating is
// evidence about nothing.
type SourceProvenanceStatus struct {
	// Provider names who asserted this — `github`, `gitlab`.
	// +optional
	Provider string `json:"provider,omitempty"`

	// PullRequest is the request the commit arrived through. Zero means the
	// provider knows of none, which is what a direct push looks like.
	// +optional
	PullRequest int32 `json:"pullRequest,omitempty"`

	// Title of that request, so a person reading this later recognises it.
	// +optional
	Title string `json:"title,omitempty"`

	// Author opened the request.
	// +optional
	Author string `json:"author,omitempty"`

	// MergedBy merged it, which is not always who approved it and is
	// occasionally the only human involved.
	// +optional
	MergedBy string `json:"mergedBy,omitempty"`

	// Approvers are the reviewers whose approval still stood when this was
	// resolved. Reviews a later push dismissed are already excluded.
	// +optional
	Approvers []string `json:"approvers,omitempty"`

	// SelfApproved is true when the only approvals were the author's own.
	//
	// It is recorded rather than treated as no approval at all: a change its
	// author approved has been approved, and whether that is acceptable is a
	// policy question an installation answers for itself.
	// +optional
	SelfApproved bool `json:"selfApproved,omitempty"`

	// Independent is true when somebody other than the author approved. It is
	// the question four-eyes actually asks.
	// +optional
	Independent bool `json:"independent,omitempty"`

	// MachineIdentity is the allowlisted account this commit was exempted
	// under, when it was. Empty means no exemption was used — which is not
	// the same as no exemption existing.
	// +optional
	MachineIdentity string `json:"machineIdentity,omitempty"`

	// Exception names the break-glass Exception that waived the pull request
	// requirement for this build, when one did — the rule id it waives is
	// `require-pull-request`. Like MachineIdentity, empty means the waiver
	// was not used, not that none exists; every use is a privileged audit
	// record and a field on the signed source attestation.
	// +optional
	Exception string `json:"exception,omitempty"`

	// Required says whether the project demanded pull request provenance for
	// this commit, so that a build carrying none is readable as "not asked
	// for" rather than as "asked for and missing".
	// +optional
	Required bool `json:"required,omitempty"`

	// CheckedAt is when the provider was asked.
	// +optional
	CheckedAt *metav1.Time `json:"checkedAt,omitempty"`

	// Message explains a check that could not be made — a provider that
	// cannot answer, a connection with no such capability. It is not a
	// finding about the commit.
	// +optional
	Message string `json:"message,omitempty"`
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

	// VEX indexes the OpenVEX documents somebody has attached to this
	// artifact: the exploitability assertions that can stop a finding
	// disqualifying it.
	//
	// The documents themselves are in the registry, attached to the digest
	// and readable by anything that speaks OCI referrers — this is an index,
	// for the same reason `Artifact.Evidence` is one. What it adds that the
	// registry cannot is **who submitted it**: the document names its own
	// author, and the authenticated identity that handed it to the platform
	// is a second and different fact. The audit log carries it too; this
	// carries it on an installation whose audit log is off.
	// +optional
	// +listType=atomic
	VEX []VEXStatus `json:"vex,omitempty"`

	// Source is how the commit reached the branch: through review, or not.
	// +optional
	Source *SourceProvenanceStatus `json:"source,omitempty"`

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

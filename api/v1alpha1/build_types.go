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
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProjectLabel names the Project a Build, Environment or Release belongs to,
// which is how everything owned by a project is found without walking owner
// references.
const ProjectLabel = "kitchen.bermos.dev/project"

// PullRequestAnnotation records the pull request a commit turned out to belong
// to, when the platform learned that after the Build already existed.
//
// It is metadata rather than spec because the spec is immutable and this is
// not a correction to it: the Build really was created from a push that
// belonged to no known request. A branch is normally pushed before its pull
// request is opened — often days before — and every provider delivers the push
// first, so the request is new information about a commit already known. The
// annotation is where that information lands; PullRequestNumber is how
// everything reads it, so nothing has to care which of the two events got
// there first.
const PullRequestAnnotation = "kitchen.bermos.dev/pull-request"

// BuildNameFor is the name of the Build for a commit the platform was told
// about, rather than one somebody asked to be rebuilt.
//
// It is derived from the commit so that two things learning about the same
// commit produce one Build: a webhook redelivery, and a project's first build
// racing the push that happened while it was being created, both land on an
// AlreadyExists that means "already building" rather than on a second run of
// the same commit. A rebuild somebody asked for is a different question and
// uses this only as a GenerateName prefix.
//
// The name deliberately says nothing about a pull request, so a push and the
// request opened for it collide here on purpose — one commit is one build.
// What that collision must not do is discard what the second event knew: see
// PullRequestAnnotation.
func BuildNameFor(project, sha string) string {
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return fmt.Sprintf("%s-bld-%s", project, sha)
}

// CommitBodyLimit is how much of a commit body a Build keeps. A message is
// written for whoever reads `git log` and nothing bounds it: a squashed branch
// carries every subject it swallowed, and a generated one can carry a diff. A
// Build's spec is not a git object store, and the object it lives in has a
// size of its own to stay under, so the body is cut here and the repository
// keeps the whole of it.
const CommitBodyLimit = 4096

// SplitCommitMessage separates a commit message into the two things the
// platform shows it as: the subject every table row renders, and the body
// behind it. It is the one implementation of what GitRevision.Message and
// GitRevision.Body mean, and everything that writes a revision — the webhook
// receiver, the API and the provider that resolves a branch to its tip — goes
// through it, so that a Build is split the same way whichever created it.
//
// The body keeps the shape it was written in, paragraphs and trailers and
// all; only the blank space around it goes, and the tail beyond
// CommitBodyLimit.
func SplitCommitMessage(message string) (subject, body string) {
	subject, rest, _ := strings.Cut(message, "\n")
	subject = strings.TrimSpace(subject)
	// The blank line under the subject goes, and so does the trailing
	// newline every provider sends — but not the indent on the body's first
	// line, which TrimSpace would take and which is a code block as often as
	// it is nothing.
	body = strings.ReplaceAll(rest, "\r\n", "\n")
	body = strings.TrimRight(strings.TrimLeft(body, "\n"), " \t\n")
	if len(body) > CommitBodyLimit {
		// Cut on a rune boundary: the field is a string in an object that is
		// serialised as JSON, and half a rune is not one.
		body = strings.ToValidUTF8(body[:CommitBodyLimit], "") + "…"
	}
	return subject, body
}

// CommitSubject is a commit message's first line alone. Writers split with
// SplitCommitMessage; this is for the readers that have only a message —
// a Build recorded before the platform split them, whose spec is immutable.
func CommitSubject(message string) string {
	subject, _ := SplitCommitMessage(message)
	return subject
}

// CommitBody is everything under that first line, on the same terms.
func CommitBody(message string) string {
	_, body := SplitCommitMessage(message)
	return body
}

// GitRevision identifies the commit a Build builds, and is empty for a Build
// that has none — an acquisition, which resolves the digest of an image
// somebody else built (#307).
//
// The SHA and the branch carry no length floor of their own any more. They
// had one, and it could not stay: a Go struct field is never omitted from the
// serialized object however it is tagged, so a Build with no commit is a `git`
// key holding two empty strings, and a floor on either would refuse it. The
// pair is checked together on [BuildSpec] instead, which is where "a commit is
// a SHA *and* a branch, or neither" can actually be said.
type GitRevision struct {
	// +optional
	SHA string `json:"sha,omitempty"`

	// +optional
	Branch string `json:"branch,omitempty"`

	// Message is the commit's *subject* — its first line and nothing else.
	// Every surface the platform shows it on is a row in a table: a build
	// list, a release list, the command palette, an audit pack. Writers split
	// a message with SplitCommitMessage rather than each reader trimming what
	// it was given.
	// +optional
	Message string `json:"message,omitempty"`

	// Body is the rest of the commit message, under the subject: what the
	// commit says about itself at length, kept so that a build can be asked
	// and not only skimmed. Empty for the great majority of commits, which
	// have no body at all, and cut at CommitBodyLimit for the few that have a
	// long one. The schema's limit is that cut plus a little: it counts
	// characters where the cut counts bytes, and the ellipsis marking a cut
	// body is one more.
	// +kubebuilder:validation:MaxLength=4100
	// +optional
	Body string `json:"body,omitempty"`

	// +optional
	Author string `json:"author,omitempty"`

	// Pull request number, when the commit belongs to one.
	// +optional
	PullRequest *int32 `json:"pullRequest,omitempty"`
}

// BuildSpec defines one build execution for one commit. Builds are immutable:
// a rebuild is a new Build object.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Build spec is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.git) || (!has(self.git.sha) && !has(self.git.branch)) || (has(self.git.sha) && size(self.git.sha) >= 7 && has(self.git.branch) && size(self.git.branch) > 0)",message="a build of a repository names the commit it builds: a SHA of at least seven characters and the branch it is on. A build that names neither is the acquisition of an image somebody else built, and it names no commit at all."
type BuildSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	// Git is the commit this Build builds, empty for a project whose source
	// is an image rather than a repository (#307).
	//
	// Nothing fakes a commit. An acquisition — a Build that resolves a
	// vendored image's digest and produces a Release without running a
	// builder — leaves both halves empty rather than inventing a SHA and a
	// branch for the fields to hold, because every rule that asks a commit a
	// question would then get an answer it could not check. What reads it
	// asks [Build.FromRepository] first.
	// +optional
	Git GitRevision `json:"git,omitempty"`
}

// FromRepository reports whether this Build has a commit behind it: a build
// of a repository rather than the acquisition of an image somebody else
// built. It is the Build's half of ProjectSourceSpec.HasRepository, and it is
// read off what the Build was created with rather than off the project, whose
// source could have been changed since.
func (b *Build) FromRepository() bool { return b.Spec.Git.SHA != "" }

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

// ArtifactSourceType is where an artifact's evidence came from — see
// ArtifactStatus.SourceType for why it is an enum with one value.
type ArtifactSourceType string

const (
	// ArtifactSourceBuilt is an artifact this platform built: its evidence
	// is the build's own, harvested from the builder and countersigned, plus
	// what the reconciler asserts about the build it orchestrated.
	ArtifactSourceBuilt ArtifactSourceType = "built"
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

	// SourceType says where this artifact's evidence comes from, which is a
	// different question from who signed it and a different one again from
	// who made each claim (`Evidence[].Source`).
	//
	// It has one value today and that is the point: every artifact Kitchen
	// holds evidence about is an artifact Kitchen built, so the enum reads
	// `built` everywhere and says so explicitly rather than by silence. An
	// artifact the platform did not build carries evidence of a different
	// kind — a vendor's own assertions, or the platform's observations of
	// something it only pulled — and the reader has to be able to tell them
	// apart *without* knowing which release of Kitchen wrote the field. So
	// the enum exists now, with one value, and gains values rather than
	// gaining a shape.
	// +kubebuilder:validation:Enum=built
	// +optional
	SourceType ArtifactSourceType `json:"sourceType,omitempty"`

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

	// Workload is which of the unit's artifacts this document is about,
	// empty for the web process's — which is every document a
	// single-workload project has ever filed.
	//
	// A VEX statement suppresses a finding on *an image*, and a unit ships
	// several: "this CVE does not apply" said about the API says nothing
	// about the worker, which may not even carry the package. The row is
	// keyed by document and workload together for that reason — the same
	// document may be filed about two artifacts and each filing is its own
	// assertion.
	// +optional
	Workload string `json:"workload,omitempty"`

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

// BuildFailureStatus is why a build failed, in the words of whatever failed.
//
// The Job's own condition says "Job has reached the specified backoff limit",
// which is true of every failed build and explains none of them. What explains
// one is a container: which of the pod's containers stopped the build, how it
// exited, and the last thing it printed before it did.
//
// All of it is read off the pod at the moment the failure is observed and
// written down here, because the pod is deleted with the job's TTL and the
// Build is what outlives it. It is on the Build rather than only on the pod
// for a second reason: a pod is the operator's to read, and a build that
// failed is the developer's to fix.
type BuildFailureStatus struct {
	// Container that ended the build. Init containers count — a clone that
	// cannot authenticate fails the build as surely as a compiler does — and
	// naming it is what separates "the source never arrived" from "the source
	// arrived and would not build".
	// +optional
	Container string `json:"container,omitempty"`

	// ExitCode the container exited with. Absent when nothing exited: a pod
	// evicted before it ran, or a container that never started because its
	// image could not be pulled.
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// Reason is Kubernetes' own word for the ending — Error, OOMKilled,
	// Evicted, DeadlineExceeded, ImagePullBackOff. It is kept unchanged
	// rather than translated: it is what everything else written about the
	// cluster calls this, and a search for it should find this build.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is the failure in one line, assembled from the container, its
	// exit and whatever the kubelet said about it.
	// +optional
	Message string `json:"message,omitempty"`

	// Log is the tail of the failing container's output, oldest line first.
	//
	// It is a copy, not the log: the whole build log is in the telemetry
	// store and outlives this. What this is for is the case the log store
	// cannot serve — a collector that never started, a build that failed
	// before its first line was shipped — where the last few lines are the
	// difference between a diagnosis and a shrug.
	// +optional
	// +listType=atomic
	Log []string `json:"log,omitempty"`
}

// BuildStatus defines the observed state of a Build.
type BuildStatus struct {
	// +optional
	Phase BuildPhase `json:"phase,omitempty"`

	// Framework detected by the auto strategy (e.g. nextjs, vite, static).
	// +optional
	DetectedFramework string `json:"detectedFramework,omitempty"`

	// DockerfileTarget is the stage of the Dockerfile this build was told to
	// produce, empty for the file's last stage. It is written when the build
	// job is created, from the commit's own kitchen.json where it declared
	// one and from the project where it did not.
	//
	// It is recorded rather than derived because the project's setting moves
	// and a build does not: what this build shipped is the target it was
	// given, and a screen that recomputed it from today's settings would
	// describe an image nobody built.
	// +optional
	DockerfileTarget string `json:"dockerfileTarget,omitempty"`

	// Config is the kitchen.json this commit carried, when it carried one.
	//
	// It is on the Build rather than on the Project because it belongs to a
	// commit: the file is read at the commit under build, and a build of an
	// older commit is built with the settings that commit declared. That is
	// also what makes a rollback exact — the Release this build writes
	// freezes the merged result, so redeploying it redeploys the
	// configuration it was built with rather than today's.
	// +optional
	Config *RepoConfig `json:"config,omitempty"`

	// Image reference by digest, never by tag.
	// +optional
	Image string `json:"image,omitempty"`

	// Artifact is what the build produced, identified by content rather than
	// by name, and what the platform has asserted about it.
	// +optional
	Artifact *ArtifactStatus `json:"artifact,omitempty"`

	// Workloads is one row per workload of the unit that was built in its
	// own right — the monorepo case #271 exists for. The web process is not
	// among them: its image is `status.image` and always has been.
	//
	// A Build is one commit, so it is over when every one of its workloads
	// is: they are created together, they run at once against the platform's
	// build concurrency, and a Build succeeds only when all of them pushed.
	// The first one to fail fails the Build, naming itself — a unit that
	// half-deployed would be worse than one that did not deploy, because
	// three of its four workloads would be a commit ahead of the fourth.
	// +optional
	// +listType=map
	// +listMapKey=name
	Workloads []WorkloadBuildStatus `json:"workloads,omitempty"`

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

	// Preview names the preview environment this build's release was routed
	// to, written at the moment it was routed there.
	//
	// It is a record of an action taken rather than a description of the
	// world, and that is the point: a pull request opened after its branch was
	// pushed is learned about late, and the release of a build that already
	// finished has to be routed to a preview after the fact. This is what
	// makes that repair happen exactly once — a preview torn down when its
	// request closed is not resurrected the next time the build that made it
	// is reconciled.
	// +optional
	Preview string `json:"preview,omitempty"`

	// Failure is why this build failed, when it did. It is set on the
	// transition into Failed and never cleared, because a Build is never
	// rebuilt — a rebuild is another Build.
	// +optional
	Failure *BuildFailureStatus `json:"failure,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// WorkloadBuildStatus is what one workload's own build did.
//
// It carries the same three facts the Build carries about its own image —
// where it was pushed, what came out, and what went wrong — because a unit
// whose fourth workload failed has to be able to say which one and why
// without anybody going and reading four Jobs.
type WorkloadBuildStatus struct {
	// Name is the workload's, as the project declares it.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Job is the build Job in the application namespace. It is named after
	// the Build and the workload, so a person reading the namespace can see
	// which commit each pod belongs to.
	// +optional
	Job string `json:"job,omitempty"`

	// Phase is what became of this workload's build. It is the Build's own
	// vocabulary, read for one workload.
	// +optional
	Phase BuildPhase `json:"phase,omitempty"`

	// Image is the digest reference the workload was built to, empty until
	// it has been. This is what the Release freezes.
	// +optional
	Image string `json:"image,omitempty"`

	// Repository is where the image was pushed, without a tag or digest.
	// +optional
	Repository string `json:"repository,omitempty"`

	// DockerfileTarget is the stage of this workload's Dockerfile its build
	// was told to produce, empty for the file's last stage. It is recorded
	// beside the rest of this workload's outcome, and for the reason the
	// Build records its own: the setting moves and the image does not, so an
	// old build reads as the artifacts it actually shipped rather than as
	// the ones today's settings would produce.
	// +optional
	DockerfileTarget string `json:"dockerfileTarget,omitempty"`

	// DetectedFramework is what `strategy: auto` made of this workload's own
	// root directory — `dockerfile` where it found one, the framework it
	// recognised otherwise. It is empty for a workload that named its
	// strategy, which asked detection nothing.
	//
	// It is per workload rather than folded into the Build's own
	// `detectedFramework` because a unit is several directories: the field
	// above is the project's own image, and a monorepo whose API is a
	// Dockerfile and whose worker is Python has two answers, not one.
	// +optional
	DetectedFramework string `json:"detectedFramework,omitempty"`

	// Artifact is what this workload's build produced, identified by content,
	// and what the platform has asserted about it.
	//
	// It is the same shape as the Build's own `status.artifact` because it is
	// the same thing about a different image: a unit of five workloads ships
	// five artifacts, and evidence that described one of them while the
	// Release deployed all five would be a compliance surface reporting
	// success over four images it never looked at (#300).
	// +optional
	Artifact *ArtifactStatus `json:"artifact,omitempty"`

	// Gates is what each quality gate did over *this workload's* artifact.
	// A gate is a claim about an image, so a unit of several images runs
	// each gate once per image and records each run against the image it
	// ran over — the Build's own `status.gates` being the web process's.
	// +optional
	// +listType=map
	// +listMapKey=name
	Gates []QualityGateStatus `json:"gates,omitempty"`

	// Message explains a workload that did not build. It is empty for one
	// that did.
	// +optional
	Message string `json:"message,omitempty"`
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

// PullRequestNumber is the pull request this build's commit belongs to, or nil
// when it belongs to none the platform has heard of.
//
// It is the spec's answer when the event that created the Build knew one, and
// the annotation's when the platform was told afterwards. Everything that acts
// on what the platform knows about a build's request — the preview
// environment, the source attestation, what the API reports — asks this rather
// than the spec, because reading the spec alone answers "was this a pull
// request build?" with "did the pull request event happen to arrive first?".
//
// What does *not* ask this is anything a late answer could relax or freeze:
// the review requirement in requiresPullRequest, and the spec a rebuild
// inherits. Both are documented where they are.
func (b *Build) PullRequestNumber() *int32 {
	if b.Spec.Git.PullRequest != nil {
		return b.Spec.Git.PullRequest
	}
	value, ok := b.Annotations[PullRequestAnnotation]
	if !ok {
		return nil
	}
	number, err := strconv.ParseInt(value, 10, 32)
	if err != nil || number <= 0 {
		return nil
	}
	pullRequest := int32(number)
	return &pullRequest
}

// BuildArtifact is one image of a unit, with the workload it belongs to.
//
// It is the shape everything that has to act on *every* artifact of a build
// iterates: attaching evidence, running a gate, reading a set back out of the
// registry, and deciding whether the Release those images make up is attested
// at all. Before #300 each of those read `status.artifact` alone, which for a
// unit of five workloads described one image and said nothing about four.
// +kubebuilder:object:generate=false
type BuildArtifact struct {
	// Workload is which workload's image this is, empty for the web
	// process's — the project's own image, whose evidence is exactly what it
	// was before a unit could be more than one thing.
	Workload string

	// Artifact is the evidence record itself. It is never nil in what
	// Artifacts answers.
	Artifact *ArtifactStatus
}

// Name is what a message calls this artifact: the workload's own name, and
// `web` for the project's own image — which is the name the process list, the
// Release's `ImageFor` and the dashboard all already use for it.
func (a BuildArtifact) Name() string {
	if a.Workload == "" {
		return WebProcessName
	}
	return a.Workload
}

// Attested reports whether the platform got its own build record onto this
// artifact's digest.
func (a BuildArtifact) Attested() bool {
	return a.Artifact != nil && a.Artifact.AttestedAt != nil
}

// Artifacts is every image this build produced that there is an evidence
// record for, the web process's first and the workloads' in the order the
// Build recorded them.
//
// A workload that has not pushed yet, or whose build failed, has no artifact
// and so no entry — it is not an artifact missing evidence, it is not an
// artifact. What it is not allowed to be is *invisible*: a Build is over only
// once every workload pushed, so a succeeded Build with a workload absent
// from this list is a workload whose digest could not be read, and
// ArtifactsWithoutEvidence is what says so.
func (b *Build) Artifacts() []BuildArtifact {
	artifacts := make([]BuildArtifact, 0, len(b.Status.Workloads)+1)
	if b.Status.Artifact != nil {
		artifacts = append(artifacts, BuildArtifact{Artifact: b.Status.Artifact})
	}
	for i := range b.Status.Workloads {
		if b.Status.Workloads[i].Artifact == nil {
			continue
		}
		artifacts = append(artifacts, BuildArtifact{
			Workload: b.Status.Workloads[i].Name,
			Artifact: b.Status.Workloads[i].Artifact,
		})
	}
	return artifacts
}

// ArtifactFor is one workload's evidence record — the web process's for the
// empty name — or nil where that workload produced none.
func (b *Build) ArtifactFor(workload string) *ArtifactStatus {
	if workload == "" || workload == WebProcessName {
		return b.Status.Artifact
	}
	for i := range b.Status.Workloads {
		if b.Status.Workloads[i].Name == workload {
			return b.Status.Workloads[i].Artifact
		}
	}
	return nil
}

// ArtifactsWithoutEvidence names every image of this unit that carries no
// signed evidence, in the order Artifacts lists them.
//
// This is the Release-level answer, and it is deliberately per artifact
// rather than a boolean: a unit is attested when *every* image it deploys is,
// and a unit that is not has to be able to say which image is missing. The
// two ways to be missing are one answer here — a workload that pushed and
// could not be attested, and a workload the Build has no artifact record for
// at all — because from the outside they are the same fact: nothing signed
// describes an image this release deploys.
func (b *Build) ArtifactsWithoutEvidence() []string {
	missing := []string{}
	if b.Status.Artifact == nil || b.Status.Artifact.AttestedAt == nil {
		missing = append(missing, WebProcessName)
	}
	for i := range b.Status.Workloads {
		workload := &b.Status.Workloads[i]
		if workload.Phase == BuildFailed {
			// A workload that did not build ships no image, so there is no
			// artifact for evidence to be missing from. The Build's own
			// failure is what says so.
			continue
		}
		if workload.Artifact == nil || workload.Artifact.AttestedAt == nil {
			missing = append(missing, workload.Name)
		}
	}
	return missing
}

// FullyAttested is whether every image this unit deploys carries the
// platform's signed build record — the one question a Release-level
// compliance answer asks.
func (b *Build) FullyAttested() bool {
	return len(b.ArtifactsWithoutEvidence()) == 0
}

// ImageOf is the digest reference one workload was built to — the web
// process's for the empty name, which is `status.image` and always has been.
func (b *Build) ImageOf(workload string) string {
	if workload == "" || workload == WebProcessName {
		return b.Status.Image
	}
	for i := range b.Status.Workloads {
		if b.Status.Workloads[i].Name == workload {
			return b.Status.Workloads[i].Image
		}
	}
	return ""
}

// GatesFor is where one image's quality gate rows live: the Build's own list
// for the web process — which is where every gate result this platform has
// ever recorded is — and the workload's row for anything else.
//
// It answers a pointer because both the reconciler and the API write into it,
// and two implementations of "which list does this gate result belong in"
// would eventually put a result about the API into the worker's row.
//
// A workload the Build has never heard of gets a scratch slice: a lost gate
// row is a smaller failure than a nil dereference, and every caller reaches
// this through an artifact read off the Build's own list.
func (b *Build) GatesFor(workload string) *[]QualityGateStatus {
	if workload == "" || workload == WebProcessName {
		return &b.Status.Gates
	}
	for i := range b.Status.Workloads {
		if b.Status.Workloads[i].Name == workload {
			return &b.Status.Workloads[i].Gates
		}
	}
	return &[]QualityGateStatus{}
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

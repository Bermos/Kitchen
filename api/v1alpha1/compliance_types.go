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
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The compliance surface: the two things the platform produces so that what it
// did can be substantiated later. See docs/COMPLIANCE.md for the model they
// belong to.
//
// Both are on the platform singleton rather than on a Project, because both
// are the operator's word rather than the application team's. A team that
// could turn its own audit log off, or sign its own evidence with a key it
// chose, would be attesting to nothing.

// AuditSpec configures the tamper-evident record of state transitions.
//
// Retention is its own knob rather than the telemetry one. Everything else in
// the store is an account of how the platform behaved and ages out in weeks;
// the audit log is the evidence an incident is reconstructed from months
// later, and a shared retention would either throw that away with the logs or
// keep a year of request rows to save it.
type AuditSpec struct {
	// Enabled records every state transition the platform makes.
	//
	// Off is a deliberate choice with a consequence worth stating: the
	// decisions the rest of the compliance suite produces still happen, they
	// just leave nothing behind that can be shown to anyone.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// RetentionDays is how long audit records are kept.
	//
	// The floor is 90 days and it is not a rounded-up guess: the incident
	// reporting duty the log exists to serve runs from when an institution
	// became aware, which can be well after the transition that caused it,
	// and a log that has already aged out cannot substantiate the report.
	// Installations under a records-retention obligation will want years
	// rather than months — the ceiling is disk.
	// +kubebuilder:validation:Minimum=90
	// +kubebuilder:default=365
	// +optional
	RetentionDays int32 `json:"retentionDays,omitempty"`
}

// AttestationSpec configures the evidence attached to built artifacts.
//
// What it configures is custody of the signing key, which is the only part of
// the scheme an institution cannot delegate: the envelopes, the statements and
// the way they are attached to an image are all standards, verifiable by
// anything that speaks them, and deliberately so.
type AttestationSpec struct {
	// Enabled signs an attestation for every artifact the platform builds
	// and attaches it to the artifact's digest.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// SigningKeyRef names the Secret in the platform namespace holding the
	// key attestations are signed with: the keys `tls.key` and `tls.crt`
	// spelling are deliberately not used — it is `private.pem` and
	// `public.pem`, an ECDSA P-256 keypair in PKCS#8 and PKIX PEM.
	//
	// Left unset the operator generates one once, into
	// `kitchen-attestation-key`, and keeps it across upgrades. Supplying one
	// is how an installation whose key custody rules forbid a key the
	// platform generated brings its own.
	// +optional
	SigningKeyRef *LocalObjectReference `json:"signingKeyRef,omitempty"`

	// Build asks the builder itself for provenance and a bill of materials,
	// which are claims the reconciler cannot make on its own.
	// +optional
	Build BuildAttestationSpec `json:"build,omitempty"`

	// Vendored is the evidence produced about artifacts the platform did not
	// build (#309), which is a different question from Build in every
	// respect: there is no builder to ask.
	// +optional
	Vendored VendoredAttestationSpec `json:"vendored,omitempty"`
}

// VendoredAttestationSpec configures what the platform does about an artifact
// somebody else published.
//
// It has one knob because there is one decision. Harvesting what the vendor
// attached, checking it describes the digest being deployed, countersigning
// it, and recording who admitted the digest are all free — they read a
// registry the platform is already pulling from and cost no compute — so none
// of them is optional. Generating a bill of materials over an image the
// platform did not build is not free: it pulls the whole artifact into a
// scanner pod in the application's namespace.
type VendoredAttestationSpec struct {
	// SBOM generates a bill of materials over a vendored digest **where the
	// vendor published none**, and attests it as the platform's own
	// observation rather than as the vendor's claim.
	//
	// It never replaces a vendor's own: a bill of materials the publisher
	// stands behind is the better evidence, and a generated one beside it
	// would be a second answer to one question. It is on by default because
	// an artifact with no bill of materials cannot be rescanned at all — the
	// continuous re-evaluation pass matches an SBOM against today's
	// vulnerability database, so an artifact without one is one nobody ever
	// looks inside again.
	// +kubebuilder:default=true
	// +optional
	SBOM bool `json:"sbom"`

	// SBOMGenerator is the image that produces it: a scanner pointed at the
	// artifact, writing SPDX or CycloneDX to a file. It is **not** the
	// builder-side generator above — that one speaks BuildKit's scanner
	// protocol and runs inside a build, and this one is an ordinary
	// container run over a digest — which is why it is its own setting
	// rather than a reuse of one word for two tools.
	//
	// Empty uses a pinned default, pinned for the reason the other one is:
	// evidence about an artifact should not change because somebody else's
	// tag moved overnight.
	// +optional
	SBOMGenerator string `json:"sbomGenerator,omitempty"`

	// TimeoutSeconds bounds one generation. A vendored image can be large
	// and the pod pulls all of it, so this is generous by default; a run
	// that overruns is recorded as a generation that did not happen, which
	// is a different fact from an artifact with no bill of materials to
	// generate one from.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:default=1800
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// BuildAttestationSpec configures the evidence the *builder* produces, as
// distinct from the evidence the reconciler produces about the build.
//
// Both knobs cost build time, which is why they are knobs. Provenance is
// nearly free — BuildKit already has everything it records. An SBOM is not: it
// runs a scanner image over the finished filesystem, and that image is pulled
// on every build, because the build pod is ephemeral and nothing survives it.
// An installation that cannot reach the generator, or will not spend the
// seconds, turns it off and says why in its own records rather than having the
// platform decide for it.
type BuildAttestationSpec struct {
	// Provenance asks the builder how the artifact was produced: the source
	// commit it resolved, the base images it pulled and their digests, and
	// the parameters it was invoked with.
	//
	// This is SLSA provenance, and it is a different and stronger claim than
	// Kitchen's own build record: it is made by the process that did the
	// work rather than by the one that asked for it.
	// +kubebuilder:default=true
	// +optional
	Provenance bool `json:"provenance"`

	// SBOM asks the builder for a bill of materials for the finished image.
	// +kubebuilder:default=true
	// +optional
	SBOM bool `json:"sbom"`

	// SBOMGenerator is the scanner image the builder runs to produce it.
	//
	// The **format follows the generator**, and the platform records what
	// came out rather than converting it: the default emits SPDX 2.3, which
	// Grype, Trivy and OSV-Scanner all read unmodified, and a generator that
	// emits CycloneDX produces a CycloneDX attestation whose predicate type
	// says so. Kitchen does not transcode between them — a bill of materials
	// rewritten by something that did not scan the image is a claim by the
	// transcoder.
	//
	// Left unset the operator uses a pinned default. Pinning matters here
	// more than it looks: the tag the ecosystem points at is a floating one,
	// and a build's evidence should not change because an image someone else
	// owns moved overnight.
	// +optional
	SBOMGenerator string `json:"sbomGenerator,omitempty"`
}

// QualityGateSpec is one gate the platform runs over every artifact it builds.
//
// A gate is a pod: an image somebody else wrote, pointed at the artifact, that
// writes what it found to a file. Kitchen contributes the artifact reference,
// the credential to pull it with, and a signature over the result — and
// nothing else. That is what makes adding a gate a change to this list rather
// than a change to every application repository.
//
// **A gate records findings and never a verdict.** Whether a finding is
// disqualifying is a policy question about the environment being deployed to,
// and putting the answer here would fix it platform-wide at the moment of
// scanning — which is precisely what makes the same scan unable to be
// acceptable in staging and blocking in production.
type QualityGateSpec struct {
	// Name identifies the gate in its attestation and on the Build. It has to
	// be stable: a policy that requires "trivy" to have run is matching on
	// this, and renaming a gate silently invalidates every artifact that
	// carries the old name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Image is the runner. It is run as an unprivileged user with no access
	// to the cluster, and the only thing it is given is the artifact and the
	// credential to pull it.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Args are passed to the image. Kubernetes' own `$(VAR)` expansion
	// applies, so a gate names the artifact as `$(KITCHEN_ARTIFACT)` rather
	// than through any templating of Kitchen's — along with
	// `$(KITCHEN_FINDINGS)` for where to write, and `$(KITCHEN_PROJECT)`,
	// `$(KITCHEN_BUILD)` and `$(KITCHEN_COMMIT)` for what is being scanned.
	// +optional
	Args []string `json:"args,omitempty"`

	// Version is what the gate's own version is recorded as in the
	// attestation. A finding is only reproducible against the version that
	// produced it, and the image tag is not always the answer — a scanner
	// whose vulnerability database updates hourly is a different gate every
	// hour under the same tag.
	// +optional
	Version string `json:"version,omitempty"`

	// Format names the shape of what the gate writes, recorded alongside the
	// findings so a reader knows how to parse them. It is informational:
	// nothing here validates it, because a gate that lied about its format
	// would still have produced whatever it produced.
	// +optional
	Format string `json:"format,omitempty"`

	// Disabled stops the gate running without removing it, so that turning
	// one off is a visible line in the configuration rather than a deletion
	// nobody can date.
	// +optional
	Disabled bool `json:"disabled,omitempty"`

	// TimeoutSeconds bounds one run. A gate that hangs must not hold up the
	// evidence the other gates produced.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:default=900
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// VEXSpec configures what the platform will admit as an exploitability
// assertion about an artifact.
//
// It is here, on the operator's own object, for the reason Gates is: a VEX
// statement is the one piece of evidence whose effect is to make a finding
// stop counting, and a team that could decide unaided whose word suppresses
// its own vulnerabilities would be marking its own homework at the one point
// where the marking matters.
//
// The division of labour is deliberate and is two levels rather than one.
// **This** list says whose documents may be attached to an artifact at all —
// platform-wide admission, the operator's word. The environment's bundle
// parameter `vexTrustedAuthors` says whose statements *that environment*
// then takes the word of, out of what was admitted. Neither replaces the
// other: production can be stricter than the platform, and nothing can be
// looser than it.
type VEXSpec struct {
	// Enabled admits OpenVEX documents through POST /builds/{name}/vex.
	//
	// Off is a defensible position and not a broken one: an installation that
	// wants every finding to count, or that has not decided who may assert
	// otherwise, turns it off and gets a refusal at ingest naming this field.
	// Documents already attached stay attached and stay readable — evidence
	// is never retracted by a setting — and whether they suppress anything is
	// still the environment's bundle's question.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// TrustedAuthors, when non-empty, is the closed list of document authors
	// the platform will sign and attach anything for. A document whose author
	// is not on it is refused at ingest, naming the list.
	//
	// Empty means the platform admits any authenticated caller's document and
	// the attribution is the control — which is the right default for an
	// installation that has not yet decided, and the wrong one for an
	// installation that has. Matching is exact and case-insensitive, with no
	// patterns, for the same reason MachineIdentities has none: a glob here
	// would eventually admit more than whoever wrote it meant.
	// +optional
	// +listType=atomic
	TrustedAuthors []string `json:"trustedAuthors,omitempty"`
}

// AdmitsAuthor reports whether a document by this author may be attached.
// Nil-safe, like every accessor on an optional spec block, and empty means
// every author.
func (s *VEXSpec) AdmitsAuthor(author string) bool {
	if s == nil || len(s.TrustedAuthors) == 0 {
		return true
	}
	for _, trusted := range s.TrustedAuthors {
		if strings.EqualFold(strings.TrimSpace(trusted), strings.TrimSpace(author)) {
			return true
		}
	}
	return false
}

// ExceptionApproverRole is who may approve an exception of a given duration:
// a project role, or the platform's operators for the longest grants.
// +kubebuilder:validation:Enum=developer;admin;operator
type ExceptionApproverRole string

const (
	// ExceptionApproverDeveloper: anyone holding developer on the project —
	// in practice, whoever is on call for it.
	ExceptionApproverDeveloper ExceptionApproverRole = "developer"
	// ExceptionApproverAdmin: the project's admins — the environment's
	// owning team.
	ExceptionApproverAdmin ExceptionApproverRole = "admin"
	// ExceptionApproverOperator: the platform's operators — above the team.
	ExceptionApproverOperator ExceptionApproverRole = "operator"
)

// ExceptionTier is one rung of the escalation ladder: grants up to
// MaxDuration need an approver holding at least Role.
type ExceptionTier struct {
	// MaxDuration is the longest grant this rung covers, e.g. "24h".
	MaxDuration metav1.Duration `json:"maxDuration"`

	// Role is the least role the approver must hold: developer or admin on
	// the project, or operator on the platform.
	Role ExceptionApproverRole `json:"role"`
}

// ExceptionPolicySpec configures break-glass exceptions — today, the
// escalation ladder: who may approve a waiver, escalating with how long it
// is asked for. A short grant is an on-call decision; a long one is a
// standing risk somebody senior has to own.
type ExceptionPolicySpec struct {
	// Ladder maps requested duration to the approver role it takes,
	// evaluated shortest rung first. A duration beyond every rung always
	// needs a platform operator. Empty uses the compiled-in default:
	// up to 24h needs developer, up to 720h (30 days) needs admin, and
	// anything longer needs an operator.
	// +optional
	// +listType=atomic
	Ladder []ExceptionTier `json:"ladder,omitempty"`
}

// defaultExceptionLadder is the compiled-in ladder an empty spec means. It
// mirrors the documented default rather than being a CRD default, so an
// upgraded singleton and a fresh one read the same.
var defaultExceptionLadder = []ExceptionTier{
	{MaxDuration: metav1.Duration{Duration: 24 * time.Hour}, Role: ExceptionApproverDeveloper},
	{MaxDuration: metav1.Duration{Duration: 720 * time.Hour}, Role: ExceptionApproverAdmin},
}

// EffectiveLadder is the ladder in force: the configured one, or the
// compiled-in default when none is. Nil-safe, like every accessor on an
// optional spec block.
func (s *ExceptionPolicySpec) EffectiveLadder() []ExceptionTier {
	if s == nil || len(s.Ladder) == 0 {
		return defaultExceptionLadder
	}
	return s.Ladder
}

// RequiredApproverRole answers who may approve a grant of this duration: the
// lowest rung whose MaxDuration covers it, and the platform's operators for
// anything beyond every rung — a duration the ladder never thought of is the
// biggest ask, not the smallest.
func (s *ExceptionPolicySpec) RequiredApproverRole(duration time.Duration) ExceptionApproverRole {
	required := ExceptionApproverOperator
	best := time.Duration(-1)
	for _, tier := range s.EffectiveLadder() {
		if duration > tier.MaxDuration.Duration {
			continue
		}
		if best < 0 || tier.MaxDuration.Duration < best {
			best = tier.MaxDuration.Duration
			required = tier.Role
		}
	}
	return required
}

// ComplianceSpec configures what evidence the platform produces about its own
// operation.
type ComplianceSpec struct {
	// Audit is the tamper-evident log of state transitions.
	// +optional
	Audit AuditSpec `json:"audit,omitempty"`

	// Attestation is the signed evidence attached to built artifacts.
	// +optional
	Attestation AttestationSpec `json:"attestation,omitempty"`

	// Gates are the quality gates run over every artifact the platform
	// builds. They live here, on the operator's own object, and not on a
	// Project: a team that chose which scanners ran over its own code would
	// be marking its own homework.
	// +optional
	// +listType=map
	// +listMapKey=name
	Gates []QualityGateSpec `json:"gates,omitempty"`

	// MachineIdentities are accounts whose commits are exempt from a
	// project's pull request requirement.
	//
	// The list exists because the requirement is otherwise unsatisfiable by
	// the automation every repository has. Renovate opens and merges its own
	// dependency bumps; release-please merges its own release commits; this
	// repository's release automation would fail the check on day one. None
	// of them will ever have an independent reviewer, and the realistic
	// alternative to naming them here is somebody turning the requirement off
	// altogether.
	//
	// Naming them is what makes the exemption **auditable**: every use of it
	// is an audit record saying which identity was exempted for which commit,
	// so "who is allowed to bypass review" is a question with a written
	// answer and a history, rather than a property of whoever last edited a
	// pipeline. They are the operator's list, not a project's, for the same
	// reason: a team that could add its own service account to its own
	// exemption list has no requirement at all.
	//
	// Entries are provider usernames, matched case-insensitively and exactly
	// — no patterns. A glob here would eventually exempt more than whoever
	// wrote it meant, and an exemption that surprises its author is the one
	// kind this must not have.
	// +optional
	MachineIdentities []string `json:"machineIdentities,omitempty"`

	// Exceptions configures break-glass exceptions: the escalation ladder
	// deciding who may approve a waiver of a given duration. The default
	// `{}` keeps an upgraded singleton reading the same as a fresh one; the
	// compiled-in ladder applies while nothing is configured.
	// +kubebuilder:default={}
	// +optional
	Exceptions *ExceptionPolicySpec `json:"exceptions,omitempty"`

	// VEX is what the platform will admit as an exploitability assertion:
	// the OpenVEX documents that can stop a finding disqualifying an
	// artifact. It lives here for the same reason Gates and Rescan do.
	// +kubebuilder:default={}
	// +optional
	VEX *VEXSpec `json:"vex,omitempty"`

	// Rescan is the continuous re-evaluation pass: what turns the promotion
	// gate into an ongoing control. It lives here for the same reason Gates
	// does — a team that chose how often its own running release was checked
	// against today's vulnerability database would be marking its own
	// homework, on a slower schedule.
	// +optional
	Rescan RescanSpec `json:"rescan,omitempty"`

	// Access is the recertification cadence, the orphaned-identity window
	// and the out-of-band write detection. It lives here rather than
	// anywhere else for the reason everything here does: a team that decided
	// how often its own access was reviewed would not be reviewed.
	// +kubebuilder:default={}
	// +optional
	Access AccessComplianceSpec `json:"access,omitempty"`
}

// AccessComplianceSpec configures the access half of the compliance suite:
// how often the platform asks somebody to look at who holds what, when an
// identity counts as dormant, and whether it watches its own objects for
// writes no reconcile made.
//
// None of it can block a deployment, and that is deliberate rather than
// incidental. An access control that took a workload down when a review ran
// late would be a control switched off within the month — so an overdue cycle
// is reported, an orphaned identity is listed, and an out-of-band write is
// recorded and surfaced. The consequence of all three is that somebody has to
// look, which is what a recertification control is.
type AccessComplianceSpec struct {
	// Enabled runs the cadence, the orphan survey and the detection. Off
	// leaves the routes answering and the register readable — a cycle
	// somebody opens by hand still works — and stops the platform opening
	// cycles or watching its own objects on its own.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// IntervalDays is how often a cycle opens: counted from the last cycle's
	// close, so an installation that recertifies late does not immediately
	// owe two.
	//
	// Ninety days is the default because it is what a quarterly control cycle
	// means in practice, and the floor is 7 rather than 1 because a
	// recertification nobody has time to finish before the next one opens is
	// a queue rather than a control. Zero opens no cycle at all: the cadence
	// is off, the surface stays, and an operator opens one when they need
	// one.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3650
	// +kubebuilder:default=90
	// +optional
	IntervalDays int32 `json:"intervalDays,omitempty"`

	// DueDays is how long a cycle has from opening before it is Overdue.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=14
	// +optional
	DueDays int32 `json:"dueDays,omitempty"`

	// InactivityDays is how long without a recorded action makes an identity
	// dormant. Ninety days matches the review cadence on purpose: an account
	// that did nothing between two reviews is exactly the one a reviewer
	// should be asked about.
	//
	// Read it knowing what the log holds. The audit log records *writes*, so
	// an account that only ever reads — opens the dashboard, follows logs,
	// looks at a build — is dormant by this measure and is not dormant in
	// fact. That is the honest limit of the evidence, and it is why dormancy
	// alone is not an orphan.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=90
	// +optional
	InactivityDays int32 `json:"inactivityDays,omitempty"`

	// DetectOutOfBandWrites watches Kitchen's own objects for changes no
	// reconcile and no API call made — somebody with cluster access editing
	// a Project, an Environment's requirements or the operator list
	// directly.
	//
	// It is detection and never prevention: anybody holding cluster-admin can
	// do all of it, and Kitchen's answer is to notice and say so loudly
	// rather than to pretend it can refuse. See docs/COMPLIANCE.md §11.4 for
	// what the mechanism sees and what it cannot.
	// +kubebuilder:default=true
	// +optional
	DetectOutOfBandWrites bool `json:"detectOutOfBandWrites"`

	// ExpectedManagers are Kubernetes field-manager names whose writes to
	// Kitchen's objects are not out of band, in addition to the platform's
	// own and Helm's.
	//
	// It exists because a real cluster has legitimate writers Kitchen has
	// never heard of — a GitOps controller applying the singleton, a policy
	// mutating webhook, a backup tool restoring. Naming them is the operator
	// saying "this one is expected", which is a decision on the record rather
	// than an alert everybody learns to ignore. Matched exactly.
	// +optional
	// +listType=atomic
	ExpectedManagers []string `json:"expectedManagers,omitempty"`
}

// AuditStatus reports whether the audit log is actually recording, which is
// not the same question as whether it is enabled: it needs the telemetry
// store, and an installation configured without one records nothing however
// the spec reads.
type AuditStatus struct {
	// Recording is true when the operator has a store to append to.
	Recording bool `json:"recording"`

	// Sequence is the number of the last record appended, so that a jump
	// backwards is visible without reading the log itself. Zero means
	// nothing has been recorded yet.
	// +optional
	Sequence int64 `json:"sequence,omitempty"`

	// Immutable is true when the store has taken the audit table's mutation
	// privileges away from the platform's own credential — so a compromised
	// operator or API can append to the log and cannot rewrite it.
	//
	// False is not a fault and does not stop anything: it means the claim
	// this installation can make about its log is the hash chain's alone,
	// which is a smaller claim and an honest one. The message says why the
	// revoke did not take. docs/COMPLIANCE.md §12.3 states exactly what the
	// true case does and does not cover.
	// +optional
	Immutable bool `json:"immutable,omitempty"`

	// ImmutabilityMessage explains an audit table whose mutation privileges
	// are still held by the platform's own user.
	// +optional
	ImmutabilityMessage string `json:"immutabilityMessage,omitempty"`

	// Message explains a log that is not recording.
	// +optional
	Message string `json:"message,omitempty"`
}

// AttestationStatus reports the identity evidence is signed under. The key id
// is the SHA-256 of the public key's DER encoding, which is what a verifier
// matches an envelope's `keyid` against.
type AttestationStatus struct {
	// Signing is true when the operator holds a usable key.
	Signing bool `json:"signing"`

	// KeyID identifies the public key attestations are signed under.
	// +optional
	KeyID string `json:"keyID,omitempty"`

	// SecretName is where that key lives, so an operator rotating it knows
	// what to replace without guessing at defaults.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// Message explains a platform that is not signing.
	// +optional
	Message string `json:"message,omitempty"`
}

// PolicyStatus reports whether policy decisions are being stored, which —
// like AuditStatus — is a different question from whether they are being
// made. The engine is a pure function and always answers; what needs the
// telemetry store is keeping the answer, with the inputs that make it
// reproducible. An installation without one still gets decisions, and this
// is where it is told they leave no replayable record behind.
type PolicyStatus struct {
	// Storing is true when the operator has a store to keep decisions in.
	Storing bool `json:"storing"`

	// Message explains decisions that are not being stored.
	// +optional
	Message string `json:"message,omitempty"`
}

// ComplianceStatus reports what the compliance machinery is doing.
type ComplianceStatus struct {
	// +optional
	Audit *AuditStatus `json:"audit,omitempty"`

	// +optional
	Attestation *AttestationStatus `json:"attestation,omitempty"`

	// +optional
	Policy *PolicyStatus `json:"policy,omitempty"`

	// +optional
	Rescan *RescanStatus `json:"rescan,omitempty"`

	// +optional
	Access *AccessComplianceStatus `json:"access,omitempty"`
}

// AccessComplianceStatus is where the access controls stand: whether a
// recertification is open and by when, how many identities look orphaned, and
// how many writes the platform has seen that it did not make.
//
// It is on the singleton rather than anywhere else because all three are
// facts about the installation, and because this is where an operator looks
// when the dashboard says something is wrong. The counts are the survey's
// last pass, not a live number — see the sweep in internal/controller.
type AccessComplianceStatus struct {
	// Reviewing is true when the cadence is on and has somewhere to run.
	Reviewing bool `json:"reviewing"`

	// OpenReview names the cycle currently open, and DueBy when it is
	// expected to be closed. Both empty means there is none, which is the
	// ordinary state between cycles.
	// +optional
	OpenReview string `json:"openReview,omitempty"`
	// +optional
	DueBy *metav1.Time `json:"dueBy,omitempty"`

	// LastClosed is when a cycle last closed — the answer to "when was
	// access last recertified here", which is the question that gets asked.
	// +optional
	LastClosed *metav1.Time `json:"lastClosed,omitempty"`

	// Identities is how many grants the last survey found and Orphaned how
	// many of them were dormant *and* unknown to the identity provider.
	// +optional
	Identities int32 `json:"identities,omitempty"`
	// +optional
	Orphaned int32 `json:"orphaned,omitempty"`

	// OutOfBandWrites is how many Kitchen-managed objects currently carry a
	// field manager the platform does not recognise, and LastOutOfBand when
	// the newest such write was made.
	//
	// It is a standing count rather than a running total: a foreign manager
	// stays on an object's managedFields until the platform writes those
	// fields again, so this answers "what is currently marked" and the audit
	// log answers "what happened".
	// +optional
	OutOfBandWrites int32 `json:"outOfBandWrites,omitempty"`
	// +optional
	LastOutOfBand *metav1.Time `json:"lastOutOfBand,omitempty"`

	// Message explains a survey that is not running, or names what it could
	// not read. The failure mode of evidence is silence, so a survey that
	// could not reach the identity provider says so rather than reporting
	// zero orphans.
	// +optional
	Message string `json:"message,omitempty"`
}

// VulnerabilityScannerSpec is the matcher the continuous re-evaluation pass
// runs over a deployed artifact's bill of materials.
//
// It is shaped like QualityGateSpec on purpose — an image, its arguments, the
// version to record and the format to record it as — because it is the same
// kind of thing: an image somebody else wrote, pointed at a file, whose output
// the platform signs and never edits. What it is not is a gate: a gate runs
// once, at build time, against the vulnerability database of that day. This
// runs again and again against the database of *today*, which is the whole
// difference between a gate and a control.
//
// There is deliberately no compiled-in default image. A scanner is pulled on
// every scan of every environment, its database is refreshed on somebody
// else's schedule, and an installation that has not chosen one has not decided
// anything the platform should decide for it — so rescan with no scanner
// configured reports itself as configured-and-inert rather than quietly
// picking a scanner and a vendor.
type VulnerabilityScannerSpec struct {
	// Name identifies the scanner in its attestation, the way a gate's name
	// does. It has to be stable: a decision that cites a scan cites this.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:default=scanner
	// +optional
	Name string `json:"name,omitempty"`

	// Image is the matcher. It is run as an unprivileged user with no access
	// to the cluster and no service account token; what it is given is the
	// bill of materials on a volume and a file to write its findings to.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Args are passed to the image. Kubernetes' own `$(VAR)` expansion
	// applies, exactly as it does for a gate, so the scanner names its inputs
	// as `$(KITCHEN_SBOM)` and `$(KITCHEN_FINDINGS)` — and, where it can say
	// so, `$(KITCHEN_DATA_SNAPSHOT)` for the file it writes its vulnerability
	// database's identifier to.
	//
	// Nothing from an API request ever reaches this list: it is the platform
	// operator's configuration, read off the singleton, and the pod is
	// assembled from it and from the artifact reference alone.
	// +optional
	Args []string `json:"args,omitempty"`

	// Version is what the scanner's own version is recorded as. It is not the
	// same claim as the data snapshot and does not replace it: the binary and
	// the database it matches against move on different schedules, and a
	// finding is only reproducible against both.
	// +optional
	Version string `json:"version,omitempty"`

	// Format names the shape of what the scanner writes, so the operator
	// knows how to read a finding out of it: `grype-json`, `trivy-json` or
	// `osv-json` are understood, and anything else is carried verbatim and
	// normalized on a best-effort basis. The raw report is signed either way
	// — the normalized list is the platform's reading of it, never a
	// replacement for it.
	// +optional
	Format string `json:"format,omitempty"`

	// TimeoutSeconds bounds one scan. A scanner that hangs must not hold a
	// place in the sweep's concurrency budget forever.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:default=900
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// RescanSpec configures the continuous re-evaluation pass: the thing that
// makes this a control rather than a gate.
//
// An artifact compliant in March is not necessarily compliant in June, and
// nothing about the artifact changed — the world did. So the platform walks
// every currently-deployed release on an interval, matches its bill of
// materials against a *current* vulnerability database, and re-runs the
// environment's own bar through the same code path a promotion uses. No
// rebuild, no redeploy: the artifact is untouched and what changes is the
// evidence attached to it and the decision recorded about it.
//
// It also is the only thing that judges exception expiry. There is no expiry
// engine: an expired grant simply stops appearing in the listing every
// evaluation materializes its input from, the rules it waived fire unwaived,
// and this pass is where that becomes a verdict somebody can read.
type RescanSpec struct {
	// Enabled walks every deployed release on Interval.
	//
	// Off by default, because it costs a scanner pod per environment per
	// interval in the application's own namespace, and an installation should
	// turn that on knowing it. Off does not mean the question goes unasked —
	// it means nobody is asking it, which is a state
	// `GET /api/v1/compliance/drift` reports rather than hides.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled"`

	// Interval is how often each deployed release is re-evaluated.
	//
	// Daily is the default because vulnerability databases move daily; the
	// floor is an hour, which is already far below the rate at which the
	// answer can change. The interval is per (environment, release) pair and
	// counted from the last scan that finished, so a sweep spreads itself out
	// instead of firing every environment at the same minute forever.
	// +kubebuilder:default="24h"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`

	// Scanner is the matcher run over each artifact's bill of materials.
	// Absent, the pass is enabled and inert, and says so.
	// +optional
	Scanner *VulnerabilityScannerSpec `json:"scanner,omitempty"`

	// Concurrency bounds how many scans are in flight at once, across the
	// whole platform.
	//
	// It is here because the first sweep after an upgrade has every
	// environment due at the same instant: two hundred environments would be
	// two hundred image pulls into two hundred namespaces, which is a denial
	// of service the platform performed on itself. Four at a time finishes a
	// two-hundred-environment install inside an hour and is invisible while
	// it does.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	// +kubebuilder:default=4
	// +optional
	Concurrency int32 `json:"concurrency,omitempty"`
}

// EffectiveInterval is the interval in force, with the compiled-in default
// standing in for a singleton written before the field existed. Nil-safe.
func (s *RescanSpec) EffectiveInterval() time.Duration {
	if s == nil || s.Interval.Duration < time.Hour {
		return 24 * time.Hour
	}
	return s.Interval.Duration
}

// EffectiveConcurrency is how many scans may be in flight, with the same
// treatment of a zero written before the field existed.
func (s *RescanSpec) EffectiveConcurrency() int {
	if s == nil || s.Concurrency < 1 {
		return 4
	}
	return int(s.Concurrency)
}

// RescanStatus reports whether the re-evaluation pass is actually running,
// which — like AuditStatus and PolicyStatus — is a different question from
// whether it is enabled: it needs a scanner configured, and a pass that is on
// with nothing to run reports nothing rather than nothing being wrong.
type RescanStatus struct {
	// Running is true when the sweep is on and has a scanner to run.
	Running bool `json:"running"`

	// LastSweep is when the sweep last looked at the whole estate — not when
	// it last scanned anything, which is per environment.
	// +optional
	LastSweep *metav1.Time `json:"lastSweep,omitempty"`

	// Environments is how many deployed (environment, release) pairs the last
	// pass considered, and Scanning how many had a scan in flight when it
	// finished. Both are the sweep's own count, so a zero next to a running
	// pass is a platform with nothing deployed rather than a broken sweep.
	// +optional
	Environments int32 `json:"environments,omitempty"`
	// +optional
	Scanning int32 `json:"scanning,omitempty"`

	// Message explains a pass that is not running.
	// +optional
	Message string `json:"message,omitempty"`
}

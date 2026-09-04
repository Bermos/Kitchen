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

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The audit pack (issue #142): one project's whole compliance answer for one
// window, as a document that leaves the platform.
//
// Everything in it already exists behind another endpoint. What this adds is
// that it is *one request* and *one file* — which is the entire feature,
// because the thing an institution is cited for is never that the evidence was
// missing, it is that the evidence lived in four systems and the four did not
// agree. Kitchen has one reconciled graph, so the export is a read rather than
// a project.
//
// # Three properties this file exists to hold
//
//   - **The pack is byte-reproducible for a range.** Nothing in the signed
//     bytes is read off a clock: there is no generation timestamp anywhere in
//     the payload, every list carries a total order that does not depend on
//     map iteration or on the order a store happened to answer in, and every
//     phase that would otherwise be judged "now" is judged at the range's end
//     instead. Two exports of the same range against an unchanged estate are
//     the same bytes, and `TestAPackIsByteReproducible` is the check.
//
//   - **It is verifiable with Kitchen switched off.** The signature is a DSSE
//     envelope over an in-toto Statement whose subject is the sha256 of the
//     pack's own canonical bytes, under the platform's published key. The
//     procedure is four commands with `jq`, `openssl` and `sha256sum`, it is
//     carried inside the pack so it travels with the document, and
//     `TestThePublishedProcedureActuallyVerifies` runs it.
//
//   - **What it cannot say, it says.** A range older than the audit log's
//     retention is reported as truncated rather than answered with less; a
//     section that hit its cap says so; an unsigned platform produces a pack
//     that states it is unsigned rather than one that looks signed. The
//     absences are the findings.
//
// # What is deliberately not in it
//
// No credential, ever — the API never reads one back and an export is not an
// exception. Connections appear by name, provider and capability; the secrets
// behind them do not, and `connections[].credential` says so in a word rather
// than leaving a reader to assume.
//
// No attestation *bodies* from the registry. The pack carries the platform's
// index of what is attached to each artifact — predicate type, manifest
// digest, who made the claim — and the coordinates to fetch it with `cosign`.
// That is a performance decision and an honesty one at once: fanning out one
// registry round trip per artifact per predicate is what would put this over
// the minute, and the registry is where the evidence lives anyway. §5.1's
// whole argument is that the evidence does not need Kitchen; a pack that
// copied it would be quietly claiming the opposite.

// AuditPackSchema identifies the document's shape. It is inside the signed
// bytes so that a reader who meets a pack years later knows which version of
// this file wrote it.
const AuditPackSchema = "https://kitchen.bermos.dev/audit-pack/v1"

// auditPackFormats are the three renderings of one document.
const (
	// auditPackJSON is the pack itself: the canonical bytes, and the only
	// ones the digest and the signature are about.
	auditPackJSON = "json"
	// auditPackDSSE is the signature over those bytes — a fresh one on every
	// request, because ECDSA signatures carry a nonce and two signings of
	// identical bytes are two different envelopes. It is *not* reproducible
	// and does not have to be: what has to reproduce is the thing signed.
	auditPackDSSE = "dsse"
	// auditPackHTML is the same pack rendered for somebody who is not an
	// engineer. Derived from the pack and from nothing else, so it is
	// deterministic too — but it is a rendering, it is not signed, and it
	// carries the digest so a printout can be tied back to the bytes.
	auditPackHTML = "html"
)

// maxAuditPackDecisions and maxAuditPackAuditRecords cap the two store reads.
// They are the stores' own maxima rather than numbers of this file's own: a
// pack that quietly asked for less than the store would answer would be a
// second, invisible retention policy.
const (
	maxAuditPackDecisions    = clickhouse.MaxDecisionLimit
	maxAuditPackAuditRecords = clickhouse.MaxAuditLimit
	maxAuditPackRecords      = clickhouse.MaxSignedRecordLimit
)

// auditPack is the document. Field order here *is* the canonical order — the
// JSON encoder writes struct fields in declaration order — so the layout of
// this struct is part of the format rather than a matter of taste.
type auditPack struct {
	Schema  string         `json:"schema"`
	Project string         `json:"project"`
	Range   auditPackRange `json:"range"`

	Platform        auditPackPlatform        `json:"platform"`
	Reproducibility auditPackReproducibility `json:"reproducibility"`
	Retention       auditPackRetention       `json:"retention"`
	Verification    auditPackVerification    `json:"verification"`

	Inventory     auditPackInventory   `json:"inventory"`
	Access        auditPackAccess      `json:"access"`
	ChangeLog     []auditPackChange    `json:"changeLog"`
	Promotions    []auditPackPromotion `json:"promotions"`
	Decisions     auditPackDecisions   `json:"decisions"`
	Attestations  []auditPackArtifact  `json:"attestations"`
	Exceptions    []auditPackException `json:"exceptions"`
	Drift         auditPackDrift       `json:"drift"`
	AuditLog      auditPackAuditLog    `json:"auditLog"`
	SignedRecords auditPackRecords     `json:"signedRecords"`
}

// auditPackRange is the window, half-open: `from` is in it and `to` is not.
// Both are required — see packRange.
type auditPackRange struct {
	From string `json:"from"`
	To   string `json:"to"`
	// HalfOpen is the interval spelled out, because "from and to" is the one
	// thing every reader of an export assumes and half of them assume wrong.
	HalfOpen string `json:"halfOpen"`
}

// auditPackPlatform is who produced the pack and under what conditions. The
// clock belongs here rather than in a footnote: every timestamp in the
// document is worth exactly what the clock that stamped it is worth (§12.6).
type auditPackPlatform struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	ClusterName string `json:"clusterName,omitempty"`
	BaseDomain  string `json:"baseDomain,omitempty"`

	AuditRecording      bool   `json:"auditRecording"`
	AuditImmutable      bool   `json:"auditImmutable"`
	ImmutabilityMessage string `json:"immutabilityMessage,omitempty"`
	AuditMessage        string `json:"auditMessage,omitempty"`
	DecisionsStored     bool   `json:"decisionsStored"`
	DecisionsMessage    string `json:"decisionsMessage,omitempty"`
	Rescanning          bool   `json:"rescanning"`
	RescanMessage       string `json:"rescanMessage,omitempty"`

	ClockSync *auditPackClock `json:"clockSync,omitempty"`
}

// auditPackClock is the drift measurement and, always, the method — a number
// here without its caveat would be worse than no number.
type auditPackClock struct {
	Checked          *time.Time `json:"checked,omitempty"`
	Method           string     `json:"method,omitempty"`
	Nodes            int32      `json:"nodes,omitempty"`
	Drifted          int32      `json:"drifted,omitempty"`
	MaxDriftSeconds  int32      `json:"maxDriftSeconds,omitempty"`
	WorstNode        string     `json:"worstNode,omitempty"`
	WorstDriftMillis int64      `json:"worstDriftMillis,omitempty"`
	Message          string     `json:"message,omitempty"`
}

// auditPackReproducibility states, inside the document, exactly what the
// reproducibility claim covers. It is a field rather than a line in the docs
// because the pack is read six months later by somebody who has the file and
// not the repository.
type auditPackReproducibility struct {
	// Claim is the promise in one sentence.
	Claim string `json:"claim"`
	// RangeBound are the sections wholly determined by the range: two exports
	// of the same window agree on these however much later one is taken,
	// short of the evidence itself being deleted.
	RangeBound []string `json:"rangeBound"`
	// CurrentState are the sections that describe the estate as it is. They
	// are not a snapshot of `to` and do not pretend to be: the platform
	// reconciles the graph rather than versioning it, so "which environments
	// exist" is answerable now and was never recorded for March.
	CurrentState []string `json:"currentState"`
	// Excluded is what is signed and what is not.
	Excluded []string `json:"excluded"`
}

// auditPackRetention is the honest half of the coverage question: whether the
// store can still answer for the whole of the range that was asked for.
type auditPackRetention struct {
	// AuditDays is the retention in force for the audit log and the decision
	// register — one class, because the decisions follow the audit knob.
	AuditDays int32 `json:"auditDays"`
	// FloorDays is the documented minimum, and Overridden whether somebody
	// signed off on going under it.
	FloorDays  int32 `json:"floorDays"`
	Overridden bool  `json:"overridden"`
	// OverrideReason and OverrideApprovedBy are the written decision behind
	// an audit retention under the floor. Not a credential, and read back in
	// full on purpose.
	OverrideReason     string `json:"overrideReason,omitempty"`
	OverrideApprovedBy string `json:"overrideApprovedBy,omitempty"`

	// Oldest is what the last sweep measured: nothing of this class is older
	// than this. Absent when no sweep has run, which is itself reported.
	Oldest    *time.Time `json:"oldest,omitempty"`
	LastSweep *time.Time `json:"lastSweep,omitempty"`

	// Truncated is the finding this section exists for: the range starts
	// before the oldest evidence the store still holds, so the pack is
	// answering for less than it was asked for.
	Truncated bool `json:"truncated"`
	// CoveredFrom is the earliest instant this pack can actually speak to,
	// which is the later of `range.from` and `oldest`.
	CoveredFrom string `json:"coveredFrom,omitempty"`
	Message     string `json:"message"`
	// Note says what is *not* retention-bounded, so an absence in a section
	// this does not cover is not read as a deletion.
	Note string `json:"note"`
}

// auditPackVerification is the procedure, carried with the document.
type auditPackVerification struct {
	// Signed says whether an envelope exists at all. False is a smaller
	// claim, not a fault, and Message says why.
	Signed        bool   `json:"signed"`
	Message       string `json:"message,omitempty"`
	PredicateType string `json:"predicateType"`
	PayloadType   string `json:"payloadType"`
	KeyID         string `json:"keyID,omitempty"`
	// PublicKey is the PEM of the verification key. Publishing it is not the
	// API reading a credential back — a public key is public by
	// construction, and evidence signed under a key nobody can obtain is
	// evidence nobody can check.
	PublicKey string `json:"publicKey,omitempty"`
	// Procedure is the four commands, in order.
	Procedure []string `json:"procedure"`
	// Warning is the one thing a reader must not get wrong about the key
	// above.
	Warning string `json:"warning"`
}

// auditPackInventory is the estate behind this project.
type auditPackInventory struct {
	Project      auditPackProject        `json:"project"`
	Environments []auditPackEnvironment  `json:"environments"`
	Releases     []auditPackRelease      `json:"releases"`
	Claims       []auditPackClaim        `json:"claims"`
	Connections  []auditPackConnection   `json:"connections"`
	Domains      []auditPackDomain       `json:"domains"`
	ThirdParties []string                `json:"thirdParties"`
	Scope        auditPackInventoryScope `json:"scope"`
}

// auditPackInventoryScope says which releases are in the document and why,
// because "the releases" is the one word in the issue's scope list that could
// mean every release the project ever cut.
type auditPackInventoryScope struct {
	Releases string `json:"releases"`
	Depth    string `json:"depth"`
}

type auditPackProject struct {
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"createdAt"`
	DataClass   string    `json:"dataClass"`
	Criticality string    `json:"criticality"`
	RTO         string    `json:"rto,omitempty"`
	RPO         string    `json:"rpo,omitempty"`
	Repository  string    `json:"repository,omitempty"`
	Branch      string    `json:"branch,omitempty"`
	// RequirePullRequest is the project's own four-eyes setting, which is
	// what makes a build's missing review readable as "not asked for" rather
	// than "asked for and missing".
	RequirePullRequest bool   `json:"requirePullRequest"`
	SourceConnection   string `json:"sourceConnection,omitempty"`
	RegistryConnection string `json:"registryConnection,omitempty"`
}

type auditPackEnvironment struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	DataClass   string `json:"dataClass"`
	Residency   string `json:"residency"`
	Criticality string `json:"criticality"`
	RTO         string `json:"rto,omitempty"`
	RPO         string `json:"rpo,omitempty"`
	// Inherited names the designation fields derived from the project rather
	// than declared here, so nothing reads as a declaration nobody made.
	Inherited []string `json:"inherited,omitempty"`

	URL     string `json:"url,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Release string `json:"release,omitempty"`
	Image   string `json:"image,omitempty"`

	// Owners are who may change what this environment demands — segregation
	// of duties as a schema (§8), and an empty list means the platform's
	// operators alone.
	Owners []string `json:"owners"`
	// BundleDigest and Parameters are the bar itself. Absent means this
	// environment declares none, which is a finding rather than a blank.
	BundleDigest string            `json:"bundleDigest,omitempty"`
	Parameters   map[string]string `json:"parameters,omitempty"`

	Domains   []string  `json:"domains,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type auditPackRelease struct {
	Name      string    `json:"name"`
	Build     string    `json:"build,omitempty"`
	Image     string    `json:"image,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// InRange says the release was cut inside the window; a release that is
	// here only because it was promoted or is still deployed says false.
	InRange bool `json:"inRange"`
}

type auditPackClaim struct {
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Connection string    `json:"connection,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Phase      string    `json:"phase,omitempty"`
	DataClass  string    `json:"dataClass"`
	Provenance string    `json:"provenance"`
	Residency  string    `json:"residency"`
	CreatedAt  time.Time `json:"createdAt"`
}

type auditPackConnection struct {
	Name         string   `json:"name"`
	Provider     string   `json:"provider,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// UsedFor is source, registry, or the claim it provisions.
	UsedFor []string `json:"usedFor"`
	// Credential is the word this field exists to carry: the platform holds
	// one and this document does not. A blank here would invite a reader to
	// conclude there is none.
	Credential string `json:"credential"`
}

type auditPackDomain struct {
	Hostname    string    `json:"hostname"`
	Environment string    `json:"environment"`
	Verified    bool      `json:"verified"`
	TLSMode     string    `json:"tlsMode,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// auditPackAccess is who holds what on this project, and the recertification
// cycles that have looked at it.
type auditPackAccess struct {
	Grants []auditPackGrant  `json:"grants"`
	Cycles []auditPackReview `json:"cycles"`
	Note   string            `json:"note"`
}

type auditPackGrant struct {
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	Role    string `json:"role"`
}

// auditPackReview is one recertification cycle as it bears on this project.
type auditPackReview struct {
	Name     string `json:"name"`
	Scope    string `json:"scope"`
	Project  string `json:"project,omitempty"`
	OpenedBy string `json:"openedBy"`
	ClosedBy string `json:"closedBy,omitempty"`
	Reason   string `json:"reason,omitempty"`

	Reviewers []string `json:"reviewers"`

	DueBy      time.Time  `json:"dueBy"`
	OpenedAt   *time.Time `json:"openedAt,omitempty"`
	SnapshotAt *time.Time `json:"snapshotAt,omitempty"`
	ClosedAt   *time.Time `json:"closedAt,omitempty"`
	// Phase is judged at the range's end rather than now, which is what
	// keeps a pack of last quarter from changing its mind in April.
	Phase string `json:"phase"`

	Pending      int32 `json:"pending"`
	Confirmed    int32 `json:"confirmed"`
	Revoked      int32 `json:"revoked"`
	SelfReviewed int32 `json:"selfReviewed"`
	Orphaned     int32 `json:"orphaned"`

	// Entries are the decisions about grants that name this project, or the
	// platform role — which is a grant on every project including this one.
	// EntriesTotal is the cycle's whole size, so an omission is visible.
	Entries      []auditPackReviewEntry `json:"entries"`
	EntriesTotal int32                  `json:"entriesTotal"`
	EntriesNote  string                 `json:"entriesNote,omitempty"`

	// RecordID points into signedRecords, where the cycle's own signed
	// artefact is carried whole. Empty with a message is a cycle that closed
	// unattested, which is a finding.
	RecordID      string     `json:"recordID,omitempty"`
	Subject       string     `json:"subject,omitempty"`
	PredicateType string     `json:"predicateType,omitempty"`
	SignedAt      *time.Time `json:"signedAt,omitempty"`
	ArtifactNote  string     `json:"artifactNote,omitempty"`
}

type auditPackReviewEntry struct {
	Subject    string     `json:"subject"`
	Email      string     `json:"email,omitempty"`
	Grant      string     `json:"grant"`
	Role       string     `json:"role"`
	Decision   string     `json:"decision"`
	DecidedBy  string     `json:"decidedBy,omitempty"`
	DecidedAt  *time.Time `json:"decidedAt,omitempty"`
	Note       string     `json:"note,omitempty"`
	SelfReview bool       `json:"selfReview,omitempty"`
	Inactive   bool       `json:"inactive,omitempty"`
	Orphaned   bool       `json:"orphaned,omitempty"`
	Applied    bool       `json:"applied,omitempty"`
}

// auditPackChange is one change to the project: the commit, who wrote it, who
// approved it, and what became of it.
type auditPackChange struct {
	Release   string    `json:"release"`
	Build     string    `json:"build,omitempty"`
	CreatedAt time.Time `json:"createdAt"`

	Commit  string `json:"commit,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Message string `json:"message,omitempty"`
	Author  string `json:"author,omitempty"`

	Image  string `json:"image,omitempty"`
	Digest string `json:"digest,omitempty"`

	Review *auditPackReviewProvenance `json:"review,omitempty"`
	// ReviewNote is why there is no review block, where there is none. A
	// build the provider could not be asked about and a build that went
	// through no pull request are different findings.
	ReviewNote string `json:"reviewNote,omitempty"`

	// Deployments are where this release landed, from the environments' own
	// history — the answer to "what was running, and when".
	Deployments []auditPackDeployment `json:"deployments,omitempty"`
}

// auditPackReviewProvenance is §8's answer for one commit.
type auditPackReviewProvenance struct {
	Provider    string   `json:"provider,omitempty"`
	PullRequest int32    `json:"pullRequest,omitempty"`
	Title       string   `json:"title,omitempty"`
	Author      string   `json:"author,omitempty"`
	MergedBy    string   `json:"mergedBy,omitempty"`
	Approvers   []string `json:"approvers"`
	// SelfApproved and Independent are the two halves of the four-eyes
	// question, and they are separate because "approved by its author" is
	// approval and is not independence.
	SelfApproved bool `json:"selfApproved"`
	Independent  bool `json:"independent"`
	// Required says whether the project demanded a reviewed pull request for
	// this commit at all.
	Required bool `json:"required"`
	// MachineIdentity and Exception are the two ways the requirement was not
	// met and the build happened anyway. Empty means neither was used.
	MachineIdentity string     `json:"machineIdentity,omitempty"`
	Exception       string     `json:"exception,omitempty"`
	CheckedAt       *time.Time `json:"checkedAt,omitempty"`
	Message         string     `json:"message,omitempty"`
}

// auditPackDeployment is one interval a release was current on an environment.
type auditPackDeployment struct {
	Environment string     `json:"environment"`
	From        time.Time  `json:"from"`
	To          *time.Time `json:"to,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	By          string     `json:"by,omitempty"`
	// Current says the release is still the environment's, so `to` is open.
	Current bool `json:"current"`
}

// auditPackPromotion is one request for a release to land, and the decision
// that answered it.
type auditPackPromotion struct {
	Name        string    `json:"name"`
	Environment string    `json:"environment"`
	Release     string    `json:"release"`
	RequestedBy string    `json:"requestedBy"`
	Trigger     string    `json:"trigger"`
	Reason      string    `json:"reason,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`

	Phase      string     `json:"phase,omitempty"`
	Verdict    string     `json:"verdict,omitempty"`
	UnmetRules []string   `json:"unmetRules,omitempty"`
	DecisionID string     `json:"decisionID,omitempty"`
	AppliedAt  *time.Time `json:"appliedAt,omitempty"`
	Message    string     `json:"message,omitempty"`
}

// auditPackDecisions is the decision register's slice of the range, with the
// reproduction inputs — which is what makes a verdict in this document
// something an examiner can re-run rather than something they have to accept.
type auditPackDecisions struct {
	Items     []auditPackDecision `json:"items"`
	Truncated bool                `json:"truncated"`
	Limit     int                 `json:"limit"`
	Message   string              `json:"message,omitempty"`
	Note      string              `json:"note"`
}

type auditPackDecision struct {
	ID          string    `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Kind        string    `json:"kind"`
	Environment string    `json:"environment,omitempty"`
	Release     string    `json:"release,omitempty"`
	Artifact    string    `json:"artifact,omitempty"`

	BundleDigest string `json:"bundleDigest"`
	InputDigest  string `json:"inputDigest"`
	DataSnapshot string `json:"dataSnapshot,omitempty"`
	Verdict      string `json:"verdict"`
	DecidedBy    string `json:"decidedBy,omitempty"`

	// RulesFired and Input pass through verbatim: they are the engine's own
	// encoding, and re-encoding them would break the input digest above.
	RulesFired json.RawMessage `json:"rulesFired,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
}

// auditPackArtifact is the evidence set for one artifact, as an index.
type auditPackArtifact struct {
	Release string `json:"release"`
	Build   string `json:"build,omitempty"`
	// Workload is which image of the unit this row indexes, absent for the
	// project's own. A release deploys one image per workload that declared
	// a build of its own, and each is attested in its own right — so a unit
	// of five workloads is five rows against one release, not one.
	Workload   string `json:"workload,omitempty"`
	Repository string `json:"repository,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Image      string `json:"image,omitempty"`

	AttestedAt *time.Time `json:"attestedAt,omitempty"`
	KeyID      string     `json:"keyID,omitempty"`

	Evidence []auditPackEvidence `json:"evidence"`
	Gates    []auditPackGate     `json:"gates,omitempty"`
	VEX      []auditPackVEX      `json:"vex,omitempty"`

	// NewestScan is what the platform's own policy last concluded about this
	// artifact, which is the same thing policy.NewestVulnerabilityScan makes
	// the engine conclude: an artifact is judged on its newest scan, so the
	// pack reports the newest one and not a list a reader would have to
	// order for themselves.
	NewestScan *auditPackScan `json:"newestScan,omitempty"`

	// Environments are where this artifact is running now.
	Environments []string `json:"environments,omitempty"`
	// Fetch is the command that pulls the evidence itself, since this
	// section is an index and says so.
	Fetch   string `json:"fetch,omitempty"`
	Message string `json:"message,omitempty"`
}

type auditPackEvidence struct {
	PredicateType string `json:"predicateType"`
	Manifest      string `json:"manifest,omitempty"`
	// Source is `builder` for a claim the build process made and the
	// platform countersigned, `platform` for one the reconciler made alone.
	// The signature cannot tell them apart, and the difference matters.
	Source string `json:"source,omitempty"`
}

type auditPackGate struct {
	Name          string     `json:"name"`
	Phase         string     `json:"phase,omitempty"`
	Source        string     `json:"source,omitempty"`
	ReportedBy    string     `json:"reportedBy,omitempty"`
	PredicateType string     `json:"predicateType,omitempty"`
	Attested      *time.Time `json:"attested,omitempty"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	Message       string     `json:"message,omitempty"`
}

type auditPackVEX struct {
	Author      string     `json:"author,omitempty"`
	SubmittedBy string     `json:"submittedBy,omitempty"`
	SubmittedAt *time.Time `json:"submittedAt,omitempty"`
	Statements  int32      `json:"statements,omitempty"`
	Digest      string     `json:"digest,omitempty"`
}

type auditPackScan struct {
	DecisionID   string    `json:"decisionID,omitempty"`
	ScannedAt    time.Time `json:"scannedAt"`
	DataSnapshot string    `json:"dataSnapshot,omitempty"`
	Verdict      string    `json:"verdict,omitempty"`
	Environment  string    `json:"environment,omitempty"`
}

// auditPackException is one break-glass grant that overlapped the range.
type auditPackException struct {
	Name        string   `json:"name"`
	Environment string   `json:"environment"`
	Release     string   `json:"release,omitempty"`
	RuleIDs     []string `json:"ruleIDs"`
	Reason      string   `json:"reason"`
	RequestedBy string   `json:"requestedBy"`
	ApprovedBy  string   `json:"approvedBy"`
	IncidentRef string   `json:"incidentRef,omitempty"`

	CreatedAt    time.Time  `json:"createdAt"`
	ExpiresAt    time.Time  `json:"expiresAt"`
	AutoRollback bool       `json:"autoRollback"`
	ResolvedBy   string     `json:"resolvedBy,omitempty"`
	ResolvedAt   *time.Time `json:"resolvedAt,omitempty"`
	// Phase is judged at the range's end, for the reason a cycle's is.
	Phase string `json:"phase"`
	// UsedBy names every promotion that actually relied on the waiver, which
	// is the difference between a grant somebody asked for and a rule
	// somebody waived.
	UsedBy []string `json:"usedBy,omitempty"`
	// ActiveAtRangeEnd is the one thing an examiner scans this list for.
	ActiveAtRangeEnd bool `json:"activeAtRangeEnd"`
}

// auditPackDrift is what is running that no longer meets its bar, and how the
// question was answered through the window.
type auditPackDrift struct {
	// Current is the same derivation GET /compliance/drift makes, restricted
	// to this project — one row per deployed (environment, release) pair,
	// compliant ones included, because a pack that showed only the failures
	// would not be an answer about the estate.
	Current []driftItemView `json:"current"`
	Counts  map[string]int  `json:"counts"`
	// History is every re-evaluation stored inside the range, oldest first:
	// the same pairs asked the same question on a schedule. Full inputs are
	// in `decisions`, keyed by id.
	History []auditPackDriftEvent `json:"history"`
	Note    string                `json:"note"`
}

type auditPackDriftEvent struct {
	DecisionID   string    `json:"decisionID"`
	Timestamp    time.Time `json:"timestamp"`
	Environment  string    `json:"environment,omitempty"`
	Release      string    `json:"release,omitempty"`
	Verdict      string    `json:"verdict"`
	DataSnapshot string    `json:"dataSnapshot,omitempty"`
	// Unwaived is how many rules fired without a grant covering them — the
	// number that decides whether the verdict is a finding or a formality.
	Unwaived int `json:"unwaived"`
	// Waived names the grants that made the difference, where any did.
	Waived []string `json:"waived,omitempty"`
}

// auditPackAuditLog is the tamper-evident log's slice of the range.
type auditPackAuditLog struct {
	Items     []auditRecordBody `json:"items"`
	Truncated bool              `json:"truncated"`
	Limit     int               `json:"limit"`
	// Privileged is how many of the records moved a control rather than a
	// workload — the count an examiner reads first.
	Privileged int `json:"privileged"`
	// Anchor is the sequence the platform published outside the table. A
	// pack whose newest record sits below it is looking at a log that has
	// been cut short from the end, which is the one edit the chain cannot
	// see on its own.
	Anchor  int64  `json:"anchor"`
	Message string `json:"message,omitempty"`
	Note    string `json:"note"`
}

// auditPackRecords carries the signed statements that have no registry to
// live in, whole. This is the one section that is evidence rather than an
// index: the envelopes are here byte for byte, so they verify out of the pack
// with no store to ask.
type auditPackRecords struct {
	Items     []auditPackRecord `json:"items"`
	Truncated bool              `json:"truncated"`
	Message   string            `json:"message,omitempty"`
	Note      string            `json:"note"`
}

type auditPackRecord struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Subject   string    `json:"subject"`
	Project   string    `json:"project,omitempty"`
	// Envelope is the DSSE envelope verbatim, as JSON. Verbatim is the whole
	// point: the payload bytes inside it are what the signature covers, so
	// nothing here is ever re-encoded.
	Envelope json.RawMessage `json:"envelope"`
}

// projectAuditPack answers GET /api/v1/projects/{name}/audit-pack.
func (s *Server) projectAuditPack(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	from, to, err := packRange(req)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	format, err := packFormat(req)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		s.writeError(w, err)
		return
	}
	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	// The key is resolved before the pack is assembled rather than after,
	// because whether the platform can sign is a *field* of the document: a
	// pack that could not be signed has to say so inside its own bytes.
	key, keyErr := controller.SigningKeyFor(ctx, s.Client, kitchen)
	if keyErr != nil {
		s.log().Error(keyErr, "the audit pack's signing key could not be read", "project", project.Name)
	}

	pack, err := s.assembleAuditPack(ctx, req, assembly{
		project: project,
		kitchen: kitchen,
		store:   store,
		from:    from,
		to:      to,
		key:     key,
		keyErr:  keyErr,
	})
	if err != nil {
		s.writeError(w, err)
		return
	}

	payload, err := canonicalPackJSON(pack)
	if err != nil {
		s.writeError(w, err)
		return
	}
	digest := digestOf(payload)

	// Recorded before it is served, and refused if it cannot be: an export
	// nobody can prove happened is the one shape of this feature that would
	// be worse than not having it. It is not a privileged transition — it
	// moves no control — but it is an export, exactly as a platform backup
	// is.
	if !s.recorded(w, req, audit.Transition{
		Object:    project,
		Kind:      audit.KindEvidenceExport,
		Operation: clickhouse.AuditExport,
		Project:   project.Name,
		Reason: fmt.Sprintf("an audit pack for %s covering %s to %s was exported",
			project.Name, pack.Range.From, pack.Range.To),
		Details: map[string]any{
			"from":     pack.Range.From,
			"to":       pack.Range.To,
			"format":   format,
			"digest":   digest,
			"bytes":    len(payload),
			"schema":   AuditPackSchema,
			"signed":   pack.Verification.Signed,
			"sections": packCounts(pack),
		},
	}) {
		return
	}

	switch format {
	case auditPackDSSE:
		s.writeAuditPackEnvelope(ctx, w, project, pack, payload, digest, key)
	case auditPackHTML:
		s.writeAuditPackHTML(w, project, pack, digest, len(payload))
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Kitchen-Pack-Digest", digest)
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=%q", packFilename(pack, "json")))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}
}

// writeAuditPackEnvelope signs the canonical bytes and serves the envelope.
func (s *Server) writeAuditPackEnvelope(
	ctx context.Context,
	w http.ResponseWriter,
	project *kitchenv1alpha1.Project,
	pack auditPack,
	payload []byte,
	digest string,
	key *attestation.ECDSAKey,
) {
	if key == nil {
		// 409 rather than 500: the request is well formed and the platform is
		// answering honestly about a capability it does not have. The pack
		// itself still exists and says the same thing in its verification
		// block.
		writeJSON(w, http.StatusConflict, errorBody{
			Error: "this platform holds no signing key, so there is nothing to sign the pack with — " +
				"the pack itself is served at ?format=json and its verification block says the same",
		})
		return
	}

	statement, err := attestation.NewStatement(
		packSubjectName(s.Namespace, project.Name), digest,
		attestation.PredicateAuditPack, map[string]any{
			"schema":  AuditPackSchema,
			"project": pack.Project,
			"from":    pack.Range.From,
			"to":      pack.Range.To,
			"bytes":   len(payload),
			// The one timestamp anywhere near this feature, and it is
			// deliberately *outside* the reproducible bytes: the envelope is
			// the record of this export, the pack is the document.
			"exportedAt": time.Now().UTC().Format(time.RFC3339),
			"platform":   "kitchen.bermos.dev",
			"sections":   packCounts(pack),
		})
	if err != nil {
		s.writeError(w, err)
		return
	}
	envelope, err := attestation.Sign(ctx, statement, key)
	if err != nil {
		s.log().Error(err, "the audit pack could not be signed", "project", project.Name)
		writeJSON(w, http.StatusInternalServerError, errorBody{
			Error: "the pack was assembled but could not be signed; the operator's log has the reason",
		})
		return
	}

	encoded, err := json.Marshal(envelope)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Kitchen-Pack-Digest", digest)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", packFilename(pack, "dsse.json")))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// packSubjectName is the statement subject's readable half: a path to the
// document, beside the digest that is its actual identity.
func packSubjectName(namespace, project string) string {
	return "kitchen.bermos.dev/audit-packs/" + namespace + "/" + project
}

// packFilename names the download after the project and the window, so two
// packs of two quarters do not land on top of each other.
func packFilename(pack auditPack, extension string) string {
	return fmt.Sprintf("kitchen-audit-pack-%s-%s-%s.%s",
		pack.Project, day(pack.Range.From), day(pack.Range.To), extension)
}

func day(stamp string) string {
	if len(stamp) < 10 {
		return stamp
	}
	return stamp[:10]
}

// packCounts is the document's shape in numbers: what the audit record and
// the signed statement both carry, so that "how big was the pack somebody
// took in March" is answerable without the pack.
func packCounts(pack auditPack) map[string]int {
	return map[string]int{
		"environments":  len(pack.Inventory.Environments),
		"releases":      len(pack.Inventory.Releases),
		"claims":        len(pack.Inventory.Claims),
		"connections":   len(pack.Inventory.Connections),
		"domains":       len(pack.Inventory.Domains),
		"changes":       len(pack.ChangeLog),
		"promotions":    len(pack.Promotions),
		"decisions":     len(pack.Decisions.Items),
		"attestations":  len(pack.Attestations),
		"exceptions":    len(pack.Exceptions),
		"accessCycles":  len(pack.Access.Cycles),
		"driftRows":     len(pack.Drift.Current),
		"auditRecords":  len(pack.AuditLog.Items),
		"signedRecords": len(pack.SignedRecords.Items),
	}
}

// canonicalPackJSON encodes the pack the one way this API ever encodes it.
//
// Three choices make it canonical, and all three are load-bearing:
//
//   - **Struct field order.** Go writes fields in declaration order, so the
//     layout of auditPack is the document's field order and does not vary.
//   - **Sorted map keys.** encoding/json sorts them, which is why the two
//     maps in the document (a bundle's parameters, the drift counts) are safe
//     to carry at all.
//   - **No HTML escaping and no indentation.** Escaping would turn a commit
//     message's `&` into `&` for no reason, and indentation is
//     whitespace nothing verifies.
func canonicalPackJSON(pack auditPack) ([]byte, error) {
	buffer := &bytes.Buffer{}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(pack); err != nil {
		return nil, err
	}
	// Encode appends a newline. The bytes that are hashed and signed are the
	// document, not the document and a line ending.
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// digestOf is the pack's identity: sha256 over the canonical bytes, in the
// `sha256:<hex>` form every other digest on this platform uses — which is
// also what `sha256sum` prints, so a reader can compute it with one command.
func digestOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// packRange reads the window, and insists on both ends.
//
// A pack whose range ends "now" is not reproducible, and reproducibility is
// the feature — so the default is refused rather than filled in. The clients
// pick a window and say so; the API does not guess one.
func packRange(req *http.Request) (time.Time, time.Time, error) {
	from, err := timeParam(req, "from")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := timeParam(req, "to")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if from.IsZero() || to.IsZero() {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"an audit pack needs both ends of its window: ?from= and ?to=, RFC 3339. " +
				"A pack that ended \"now\" could not be reproduced, and reproducibility is the point")
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("to must be after from")
	}
	return from.UTC(), to.UTC(), nil
}

// packFormat reads which of the three renderings was asked for.
func packFormat(req *http.Request) (string, error) {
	format := strings.ToLower(strings.TrimSpace(req.URL.Query().Get("format")))
	switch format {
	case "", auditPackJSON:
		return auditPackJSON, nil
	case auditPackDSSE, auditPackHTML:
		return format, nil
	default:
		return "", fmt.Errorf(
			"format %q is not one this endpoint serves: json (the pack), dsse (the signature over it) "+
				"or html (the same pack rendered for a reader)", format)
	}
}

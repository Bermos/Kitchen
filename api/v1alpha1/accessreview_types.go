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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// An AccessReview is one recertification cycle: the platform's answer to "who
// holds what here, who looked at that, and what did they decide about each of
// them". It opens on a cadence, freezes a snapshot of every grant at the
// instant it opened, collects one decision per grant from a named reviewer,
// and closes — leaving a timestamped artefact that outlives the object.
//
// # Why an object and not a table
//
// The obvious alternative is a row per cycle in the telemetry store, and it
// would be smaller. It is the wrong shape for three reasons, all of which are
// this repository's existing shapes rather than new arguments:
//
//   - **A cycle has a reconciler.** It opens, it comes due, it closes, and
//     something has to notice each of those on a clock. That is a
//     RequeueAfter, which is what the Exception reconciler already is.
//   - **A decision is a write, and a write surface waits for its reconciler.**
//     A revocation somebody records has to actually take the grant off the
//     Project, which is a reconcile over cluster objects and not a row.
//   - **It is backed up with everything else.** The platform backup carries
//     custom resources; a table would need its own export, and the one
//     artefact an auditor asks for would be the one thing not in the archive.
//
// The *evidence* still leaves the object: on close, the platform signs a
// statement of the whole cycle and keeps the envelope in the store's
// signed_records table, exactly as a resource claim's data-class declaration
// is kept. So the object is the workflow and the envelope is the artefact,
// and deleting the object does not delete the evidence.
//
// # The reviewer may be the reviewed
//
// Segregation of duties is the subject of this issue, and the honest answer
// here is the same one §8.4 gives for self-approval: it is **recorded, not
// refused**. An installation with one operator has exactly one person who can
// review that operator's own grant, and a platform that refused would either
// make the control unsatisfiable or push somebody into creating a second
// account to satisfy it — which is worse evidence, not better. So a decision a
// reviewer makes about their own grant is stamped `selfReview: true`, counted
// on the cycle's status, carried into the closing artefact, and named in the
// audit record. What the platform will not do is let it pass unremarked.

// AccessReviewScope is what one cycle covers.
// +kubebuilder:validation:Enum=platform;project;all
type AccessReviewScope string

const (
	// AccessReviewPlatform covers the operator list on the Kitchen
	// singleton: who owns the platform.
	AccessReviewPlatform AccessReviewScope = "platform"
	// AccessReviewProject covers one Project's grants.
	AccessReviewProject AccessReviewScope = "project"
	// AccessReviewAll covers both — every grant on the installation. It is
	// what the cadence opens, because "who has what" is one question and an
	// answer assembled from twelve cycles is not an answer.
	AccessReviewAll AccessReviewScope = "all"
)

// AccessReviewSpec is what the cycle is: its scope, who is expected to review
// it, and by when. It is immutable, for an Exception's reason — a
// recertification is a record of what somebody looked at and decided, and a
// record whose terms can be edited afterwards is not a record. What changes
// during a cycle is its status, because a decision is an act rather than an
// edit.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="AccessReview spec is immutable: a recertification cycle is a record of what was reviewed; open a new cycle instead of editing this one"
// +kubebuilder:validation:XValidation:rule="self.scope != 'project' || has(self.projectRef)",message="a project-scoped review must name the project it is about in projectRef"
// +kubebuilder:validation:XValidation:rule="self.scope == 'project' || !has(self.projectRef)",message="projectRef belongs to a project-scoped review only: a platform or all-scoped cycle covers every project"
type AccessReviewSpec struct {
	// Scope is what this cycle covers.
	// +kubebuilder:default=all
	Scope AccessReviewScope `json:"scope"`

	// ProjectRef names the project, for a project-scoped cycle. Refused at
	// admission on any other scope, and required on that one.
	// +optional
	ProjectRef *LocalObjectReference `json:"projectRef,omitempty"`

	// Reviewers are who is expected to decide. It is a list rather than one
	// name because a review of the whole installation is rarely one person's
	// afternoon, and because a reviewer who leaves mid-cycle must not take
	// the cycle with them.
	//
	// It is an expectation and not an enforcement: the API admits a decision
	// from any platform operator and records who actually made it, since a
	// cycle that could only be closed by a named person is a cycle that
	// stalls the week they are on holiday. Whether the decider was on this
	// list is visible on every entry.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=subject
	Reviewers []AccessSubject `json:"reviewers"`

	// OpenedBy names who or what opened the cycle: an identity for one a
	// person asked for, `system:controller/accessreview` for one the cadence
	// opened.
	// +kubebuilder:validation:MinLength=1
	OpenedBy string `json:"openedBy"`

	// DueBy is when the cycle is expected to be closed. Passing it makes the
	// cycle Overdue and is recorded; it does not revoke anything and does not
	// stop a deployment. An access review that took a workload down would be
	// a control nobody would leave switched on.
	DueBy metav1.Time `json:"dueBy"`

	// Reason is why this cycle exists, in whoever opened it's words — the
	// cadence, an audit, somebody leaving.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// AccessReviewPhase is where a cycle is in its life.
// +kubebuilder:validation:Enum=Open;Overdue;Closed
type AccessReviewPhase string

const (
	// AccessReviewOpen: the snapshot is taken and decisions are being made.
	AccessReviewOpen AccessReviewPhase = "Open"
	// AccessReviewOverdue: the due date passed with decisions outstanding.
	// The cycle is still open and can still be closed; the phase is the
	// platform saying so out loud rather than a consequence.
	AccessReviewOverdue AccessReviewPhase = "Overdue"
	// AccessReviewClosed: somebody closed it. Terminal, and the object is
	// retained — the register is the history, and the signed artefact is
	// kept whether or not the object survives.
	AccessReviewClosed AccessReviewPhase = "Closed"
)

// AccessDecision is what a reviewer said about one grant.
// +kubebuilder:validation:Enum=confirm;revoke
type AccessDecision string

const (
	// AccessConfirm: this account should still hold this role.
	AccessConfirm AccessDecision = "confirm"
	// AccessRevoke: it should not. Closing the cycle takes the grant off,
	// which is what makes this a control rather than a survey.
	AccessRevoke AccessDecision = "revoke"
)

// AccessReviewEntry is one grant as the cycle found it, plus what was decided
// about it and what became of that decision.
//
// The snapshot half is frozen when the cycle opens and never rewritten: a
// review is of what was true at an instant, and a row that drifted while
// somebody was reading it would be a review of nothing. A grant added after
// the cycle opened is simply not in this cycle — it is in the next one.
type AccessReviewEntry struct {
	AccessSubject `json:",inline"`

	// Grant says where the role is held: `platform` for the operator list,
	// or the project's name.
	// +kubebuilder:validation:MinLength=1
	Grant string `json:"grant"`

	// Role is what is held there — `operator` on the platform, or one of the
	// three project roles.
	// +kubebuilder:validation:MinLength=1
	Role string `json:"role"`

	// LastActive is when this identity was last recorded doing something, out
	// of the audit log. Absent means nothing has ever been recorded of them,
	// which is not the same as "they never signed in": the log records
	// writes, so somebody who only ever reads looks identical here. That
	// blind spot is stated rather than papered over.
	// +optional
	LastActive *metav1.Time `json:"lastActive,omitempty"`

	// Inactive is LastActive older than the configured window, or absent.
	// +optional
	Inactive bool `json:"inactive,omitempty"`

	// Unknown is the identity provider's directory holding no account for
	// this subject — a grant pointing at nobody. It is only ever set when the
	// directory actually answered: an installation federated to an issuer of
	// its own serves no directory, and claiming every grant there was unknown
	// would be a lie with a checkbox.
	// +optional
	Unknown bool `json:"unknown,omitempty"`

	// Orphaned is Inactive and Unknown together: no recent activity and no
	// owner. It is the pair rather than either alone because either alone has
	// an innocent reading — a quiet quarter, or an issuer that answers no
	// directory — and the pair does not.
	// +optional
	Orphaned bool `json:"orphaned,omitempty"`

	// Decision is what the reviewer said. Empty is undecided, which is what
	// `pending` counts.
	// +optional
	Decision AccessDecision `json:"decision,omitempty"`

	// DecidedBy names who decided, DecidedAt when, and Note their words.
	// +optional
	DecidedBy string `json:"decidedBy,omitempty"`
	// +optional
	DecidedAt *metav1.Time `json:"decidedAt,omitempty"`
	// +optional
	Note string `json:"note,omitempty"`

	// SelfReview records that the decider is the subject of this entry. It is
	// allowed and marked rather than refused — see the type comment — so that
	// "who reviewed their own access" is a question with a written answer.
	// +optional
	SelfReview bool `json:"selfReview,omitempty"`

	// Applied records that a revocation was actually carried out when the
	// cycle closed, and ApplyMessage says why one was not. The pair exists
	// because a revocation the platform declines — the last operator, a
	// grant somebody already removed — must not read as one it performed.
	// +optional
	Applied bool `json:"applied,omitempty"`
	// +optional
	ApplyMessage string `json:"applyMessage,omitempty"`
}

// AccessReviewArtifact points at the retained evidence: the signed statement
// the platform minted when the cycle closed.
//
// It is a pointer and deliberately not a copy. The envelope lives in the
// store's signed_records table, keyed by the digest below, exactly as a
// claim's data-class declaration does — and an artefact copied onto the
// object it describes would be a second source of truth for the one document
// that must have only one.
type AccessReviewArtifact struct {
	// RecordID is the signed record's id in the store.
	// +optional
	RecordID string `json:"recordID,omitempty"`

	// Subject is the cycle's identity digest, which is the statement's
	// subject: sha256 over namespace, name and UID.
	// +optional
	Subject string `json:"subject,omitempty"`

	// PredicateType is what kind of statement it is.
	// +optional
	PredicateType string `json:"predicateType,omitempty"`

	// SignedAt is when the envelope was minted.
	// +optional
	SignedAt *metav1.Time `json:"signedAt,omitempty"`

	// Message explains a cycle that closed with no artefact behind it — no
	// signing key, or no store to keep it in. The cycle still closed and the
	// decisions are still on the object and in the audit log; what is missing
	// is the portable evidence, and saying so is better than a blank field.
	// +optional
	Message string `json:"message,omitempty"`
}

// AccessReviewStatus is the cycle itself: the snapshot, the decisions, the
// tallies and the artefact.
type AccessReviewStatus struct {
	// +optional
	Phase AccessReviewPhase `json:"phase,omitempty"`

	// OpenedAt is when the cycle was stamped Open, and SnapshotAt the
	// instant the grants below were frozen. They are usually the same and
	// are two fields because they answer two questions: when the review
	// started, and what moment it is a review *of*.
	// +optional
	OpenedAt *metav1.Time `json:"openedAt,omitempty"`
	// +optional
	SnapshotAt *metav1.Time `json:"snapshotAt,omitempty"`

	// Entries is every grant in scope at SnapshotAt, with its decision.
	// +optional
	// +listType=atomic
	Entries []AccessReviewEntry `json:"entries,omitempty"`

	// The tallies, so a list screen and a `kubectl get` need not add up a
	// hundred entries to say where a cycle stands.
	// +optional
	Pending int32 `json:"pending,omitempty"`
	// +optional
	Confirmed int32 `json:"confirmed,omitempty"`
	// +optional
	Revoked int32 `json:"revoked,omitempty"`
	// +optional
	SelfReviewed int32 `json:"selfReviewed,omitempty"`
	// +optional
	Orphaned int32 `json:"orphaned,omitempty"`

	// ClosedBy names who closed the cycle and ClosedAt when.
	// +optional
	ClosedBy string `json:"closedBy,omitempty"`
	// +optional
	ClosedAt *metav1.Time `json:"closedAt,omitempty"`

	// Artifact is the retained evidence this cycle produced.
	// +optional
	Artifact *AccessReviewArtifact `json:"artifact,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EffectivePhase is the phase as of `at`, computed rather than read — an
// Exception's rule and for its reason. A cycle past its due date is overdue
// whether or not a reconcile has stamped it yet, and a closed cycle stays
// closed however long ago it was due. Every read judges through this, so a
// stamped-late status row cannot make an overdue cycle look current.
func (r *AccessReview) EffectivePhase(at time.Time) AccessReviewPhase {
	if r.Status.Phase == AccessReviewClosed {
		return AccessReviewClosed
	}
	if !r.Spec.DueBy.IsZero() && !at.Before(r.Spec.DueBy.Time) {
		return AccessReviewOverdue
	}
	return AccessReviewOpen
}

// Covers reports whether a grant held at `grant` — `platform`, or a project's
// name — falls inside this cycle's scope.
func (r *AccessReview) Covers(grant string) bool {
	switch r.Spec.Scope {
	case AccessReviewAll:
		return true
	case AccessReviewPlatform:
		return grant == string(AccessReviewPlatform)
	case AccessReviewProject:
		return r.Spec.ProjectRef != nil && grant == r.Spec.ProjectRef.Name
	default:
		return false
	}
}

// EntryKey identifies one entry within a cycle: the subject and where the
// grant is held. It is the pair rather than the subject alone because one
// account can hold a role on four projects, and those are four decisions.
func EntryKey(subject, grant string) string {
	return grant + "\x00" + subject
}

// Key is this entry's own identity, as a decision names it.
func (e *AccessReviewEntry) Key() string {
	return EntryKey(e.Subject, e.Grant)
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pending",type=integer,JSONPath=`.status.pending`
// +kubebuilder:printcolumn:name="Revoked",type=integer,JSONPath=`.status.revoked`
// +kubebuilder:printcolumn:name="Due",type=string,JSONPath=`.spec.dueBy`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AccessReview is the Schema for the accessreviews API: one recertification
// cycle, retained after it closes so the register stays queryable
// historically.
type AccessReview struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AccessReviewSpec   `json:"spec,omitempty"`
	Status AccessReviewStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AccessReviewList contains a list of AccessReview.
type AccessReviewList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AccessReview `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AccessReview{}, &AccessReviewList{})
}

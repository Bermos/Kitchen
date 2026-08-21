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

// An Exception is the break-glass object: a named, bounded, two-person grant
// that waives specific policy rules for one project's environment. One object
// type covers both an emergency deployment and an accepted security finding,
// because both are the same act — somebody deciding, on the record, that a
// rule does not apply here for a while.
//
// The design principle it exists for: never hard-block an emergency
// deployment. A blocked hotfix means someone deploys around Kitchen entirely
// and there is no record at all. So the platform allows it, requires a
// reason, and records it permanently and loudly — the rules still evaluate
// and still report, the waiving is visible on every decision, and the fact
// travels with the artifact as a BreakGlass attestation.

// ExceptionSpec is the grant itself. It is immutable, like a Promotion's
// spec and for the same reason: an exception is a record of what two people
// agreed to, and a record that can be edited afterwards is not a record.
// Extending or narrowing a grant is a new Exception; ending one early is a
// resolution, which lives on status because it is an act, not an edit.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Exception spec is immutable: a break-glass grant is a record of what was agreed; grant a new one instead of editing this"
// +kubebuilder:validation:XValidation:rule="self.approvedBy != self.requestedBy",message="an exception takes two people: the approver must be somebody other than the requester"
type ExceptionSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	// EnvironmentRef names the environment the waiver applies to. An
	// exception is scoped to the Promotion pair — a rule waived for staging
	// says nothing about production.
	EnvironmentRef LocalObjectReference `json:"environmentRef"`

	// ReleaseRef optionally narrows the grant to one release. Empty covers
	// the whole environment: any release promoted there while the exception
	// is active has the named rules waived.
	// +optional
	ReleaseRef *LocalObjectReference `json:"releaseRef,omitempty"`

	// RuleIDs names the rules this exception waives, by the stable ids the
	// bundle publishes. It is a list of specific rules and never a blanket
	// bypass: a rule not named here still blocks, however urgent the hour.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +listType=atomic
	RuleIDs []string `json:"ruleIDs"`

	// Reason is why, in the requester's own words. Required: an exception
	// without a reason is a bypass, and a bypass is what this object exists
	// to replace.
	// +kubebuilder:validation:MinLength=1
	Reason string `json:"reason"`

	// RequestedBy names who asked for the exception.
	// +kubebuilder:validation:MinLength=1
	RequestedBy string `json:"requestedBy"`

	// ApprovedBy names the second human — always a second: the CEL rule on
	// this spec refuses an approver who is the requester. The API verifies
	// the approver holds the role the escalation ladder demands for the
	// requested duration; the assertion that they agreed is the requester's,
	// on the record.
	// +kubebuilder:validation:MinLength=1
	ApprovedBy string `json:"approvedBy"`

	// IncidentRef links the incident this exception was granted for — a
	// ticket id, a postmortem URL. Optional but expected for an emergency.
	// +optional
	IncidentRef string `json:"incidentRef,omitempty"`

	// ExpiresAt bounds the grant. There is no unbounded exception: past this
	// moment the named rules block again, and the deployed release goes
	// non-compliant on the next evaluation pass.
	ExpiresAt metav1.Time `json:"expiresAt"`

	// AutoRollback asks the platform to roll the environment back when the
	// exception expires unresolved. Off by default: the default consequence
	// of expiry is that further promotions block, not that a running
	// workload is yanked.
	// +kubebuilder:default=false
	// +optional
	AutoRollback bool `json:"autoRollback,omitempty"`
}

// ExceptionPhase is where an exception is in its life.
// +kubebuilder:validation:Enum=Active;Expired;Resolved
type ExceptionPhase string

const (
	// ExceptionActive: the grant stands; matching rules are waived.
	ExceptionActive ExceptionPhase = "Active"
	// ExceptionExpired: time ran out before anybody resolved it. The rules
	// block again, and the register keeps the object as history.
	ExceptionExpired ExceptionPhase = "Expired"
	// ExceptionResolved: a person ended it — the finding was fixed, the
	// incident closed. Terminal, and the object is retained: the register is
	// queryable historically, and project deletion is what garbage-collects
	// these records.
	ExceptionResolved ExceptionPhase = "Resolved"
)

// ExceptionStatus is what became of the grant: its phase, who relied on it,
// and — when resolved — who ended it.
type ExceptionStatus struct {
	// +optional
	Phase ExceptionPhase `json:"phase,omitempty"`

	// UsedBy names every Promotion that relied on this exception — was
	// allowed-with-exception because it waived a fired rule. The register's
	// answer to "what actually went out under this grant".
	// +optional
	// +listType=atomic
	UsedBy []string `json:"usedBy,omitempty"`

	// ResolvedBy names who resolved it, when somebody did. The resolution's
	// reason is in the audit log, where an auditable act belongs.
	// +optional
	ResolvedBy string `json:"resolvedBy,omitempty"`

	// ResolvedAt is when.
	// +optional
	ResolvedAt *metav1.Time `json:"resolvedAt,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EffectivePhase is the phase as of `at`, computed rather than read: the
// reconciler flips Active to Expired on its own clock, but a reader must not
// depend on having been reconciled recently — an exception past its expiry
// is expired whether or not the status row moved yet. Resolved is terminal
// and wins over the clock. This is the one expiry rule; the API's reads, the
// promotion reconciler's listing and the rescan sweep (#134) all judge
// through it.
func (e *Exception) EffectivePhase(at time.Time) ExceptionPhase {
	if e.Status.Phase == ExceptionResolved {
		return ExceptionResolved
	}
	if !e.Spec.ExpiresAt.IsZero() && !at.Before(e.Spec.ExpiresAt.Time) {
		return ExceptionExpired
	}
	return ExceptionActive
}

// Covers reports whether this exception is in scope for a promotion of
// `release` into `environment` under `project`: same project, same
// environment, and either no release named or that one.
func (e *Exception) Covers(project, environment, release string) bool {
	if e.Spec.ProjectRef.Name != project || e.Spec.EnvironmentRef.Name != environment {
		return false
	}
	if e.Spec.ReleaseRef == nil || e.Spec.ReleaseRef.Name == "" {
		return true
	}
	return e.Spec.ReleaseRef.Name == release
}

// WaivesRule reports whether the exception names a rule id.
func (e *Exception) WaivesRule(rule string) bool {
	for _, id := range e.Spec.RuleIDs {
		if id == rule {
			return true
		}
	}
	return false
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="Environment",type=string,JSONPath=`.spec.environmentRef.name`
// +kubebuilder:printcolumn:name="Rules",type=string,JSONPath=`.spec.ruleIDs`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=`.spec.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Exception is the Schema for the exceptions API: one bounded, two-person,
// per-rule break-glass grant, retained after it ends so the register stays
// queryable historically.
type Exception struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ExceptionSpec   `json:"spec,omitempty"`
	Status ExceptionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ExceptionList contains a list of Exception.
type ExceptionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Exception `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Exception{}, &ExceptionList{})
}

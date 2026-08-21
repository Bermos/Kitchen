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

// PromotionTrigger is who asked for a promotion: a person, or the platform
// acting on its own pipeline.
// +kubebuilder:validation:Enum=manual;automatic
type PromotionTrigger string

const (
	// PromotionManual: somebody asked, through the API or the CLI.
	PromotionManual PromotionTrigger = "manual"
	// PromotionAutomatic: the platform asked — a production-branch build
	// finished, or the previous stage applied and the next one auto-promotes.
	PromotionAutomatic PromotionTrigger = "automatic"
)

// PromotionSpec is one request to move one Release into one Environment. It
// is immutable, like a Release's spec and for the same reason: a promotion is
// a record of what was asked, and a record that can be edited afterwards is
// not a record. Retrying a blocked promotion is a new Promotion, never an
// edit of the old one.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Promotion spec is immutable"
type PromotionSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	// EnvironmentRef names where the release should land. The environment
	// must belong to the project — the reconciler fails a promotion whose
	// references disagree, since a cross-object rule cannot live in CEL.
	EnvironmentRef LocalObjectReference `json:"environmentRef"`

	// ReleaseRef names what should land there: an immutable snapshot, so the
	// digest that was built is the digest that is deployed — a promotion never
	// rebuilds anything, which is also what makes a rollback a promotion of an
	// older release.
	ReleaseRef LocalObjectReference `json:"releaseRef"`

	// RequestedBy names who asked: an account for a manual promotion, a
	// controller identity (`system:controller/...`) for an automatic one.
	// +kubebuilder:validation:MinLength=1
	RequestedBy string `json:"requestedBy"`

	// Trigger says whether a person or the pipeline asked.
	Trigger PromotionTrigger `json:"trigger"`

	// Reason is the requester's own words for why, carried into the audit
	// record. Optional for routine moves; an emergency promotion under an
	// exception is expected to have one.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// PromotionPhase is where a promotion is in its short life.
// +kubebuilder:validation:Enum=Pending;Evaluating;Allowed;AllowedWithException;Blocked;Applied;Failed
type PromotionPhase string

const (
	// PromotionPending: created, not yet picked up.
	PromotionPending PromotionPhase = "Pending"
	// PromotionEvaluating: the references resolved and the environment's
	// requirements are being evaluated.
	PromotionEvaluating PromotionPhase = "Evaluating"
	// PromotionAllowed: the policy allowed it; the flip is about to happen.
	PromotionAllowed PromotionPhase = "Allowed"
	// PromotionAllowedWithException: every rule that fired was waived by an
	// active exception; the flip is about to happen, loudly recorded.
	PromotionAllowedWithException PromotionPhase = "AllowedWithException"
	// PromotionBlocked: at least one rule stands unwaived. Terminal — the
	// spec is immutable, so nothing about this object can change the answer;
	// a retry (new evidence attached, an exception granted) is a new
	// Promotion. status.unmetRules names what stood in the way.
	PromotionBlocked PromotionPhase = "Blocked"
	// PromotionApplied: the environment now runs the release.
	PromotionApplied PromotionPhase = "Applied"
	// PromotionFailed: the promotion could not be carried out at all — the
	// references do not line up, or an object it names is gone. Terminal,
	// with status.message saying why. Distinct from Blocked: nothing judged
	// the artifact, the request itself was unusable.
	PromotionFailed PromotionPhase = "Failed"
)

// PromotionStatus is what became of the request: the phase, the decision that
// judged it, and — when blocked — the rules that stood in the way, by id.
type PromotionStatus struct {
	// +optional
	Phase PromotionPhase `json:"phase,omitempty"`

	// Verdict is the policy engine's word: allowed, allowed-with-exception or
	// blocked. Exactly three; there is no fourth state.
	// +optional
	Verdict string `json:"verdict,omitempty"`

	// DecisionID is the stored decision this promotion acted on — the way to
	// its fired rules, its input and the bundle it can be replayed from.
	// +optional
	DecisionID string `json:"decisionID,omitempty"`

	// UnmetRules names the rules that fired unwaived, as the stable ids the
	// bundle publishes — a blocked promotion says exactly what is missing,
	// never a generic refusal.
	// +optional
	UnmetRules []string `json:"unmetRules,omitempty"`

	// Message is the phase in a sentence: why it failed, what blocked it, or
	// what was applied.
	// +optional
	Message string `json:"message,omitempty"`

	// EvaluatedAt is when the policy was evaluated.
	// +optional
	EvaluatedAt *metav1.Time `json:"evaluatedAt,omitempty"`

	// AppliedAt is when the environment was actually moved.
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="Environment",type=string,JSONPath=`.spec.environmentRef.name`
// +kubebuilder:printcolumn:name="Release",type=string,JSONPath=`.spec.releaseRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Promotion is the Schema for the promotions API: one immutable request to
// move one Release into one Environment, with the evaluated decision on its
// status. It is how an artifact moves through a project's stages — the same
// digest at every stage, judged against each environment's own bar.
type Promotion struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PromotionSpec   `json:"spec,omitempty"`
	Status PromotionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PromotionList contains a list of Promotion.
type PromotionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Promotion `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Promotion{}, &PromotionList{})
}

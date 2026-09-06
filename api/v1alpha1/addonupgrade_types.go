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

// AddonUpgradeSpec is the transition one upgrade attempt carried out: which
// entry, from what, to what, and by which job.
//
// It is immutable, like a PlatformUpdate's and a Build's, because it is a
// record of something that was attempted rather than a request for something
// to happen. An attempt that failed and was retried is a second object, never
// an edit of the first: the pair of them is what "it failed at 04:03 and
// succeeded at 05:07" looks like.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="AddonUpgrade spec is immutable"
type AddonUpgradeSpec struct {
	// Addon is the catalogue entry this upgrade is of, which is the Addon
	// object's name.
	// +kubebuilder:validation:MaxLength=63
	Addon string `json:"addon"`

	// From is what was installed before, chart by chart. A version left
	// empty is one the platform never recorded — an install from before the
	// operator labelled its jobs — and it is empty rather than guessed.
	// +optional
	From []AddonChartStatus `json:"from,omitempty"`

	// To is what the upgrade installs, chart by chart. An entry installs one
	// or several, and both sides are listed in the order the entry installs
	// them, so a KEDA pair bump reads as the pair it is.
	// +optional
	To []AddonChartStatus `json:"to,omitempty"`

	// Namespace the release lives in.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`

	// JobName is the install job that carried it, in the platform namespace,
	// for as long as it exists — it is reaped an hour after it finishes,
	// while this record stays. That is the whole reason this object exists:
	// by the time anybody correlates six degraded projects with something the
	// platform did, the job is long gone and `status.charts` has always said
	// what it says now.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	JobName string `json:"jobName,omitempty"`
}

// AddonUpgradePhase is the outcome of one attempt.
// +kubebuilder:validation:Enum=Running;Succeeded;Failed
type AddonUpgradePhase string

const (
	// AddonUpgradeRunning has an install job in flight.
	AddonUpgradeRunning AddonUpgradePhase = "Running"
	// AddonUpgradeSucceeded installed the versions in spec.to.
	AddonUpgradeSucceeded AddonUpgradePhase = "Succeeded"
	// AddonUpgradeFailed did not. What helm managed to apply before it gave
	// up is whatever it was; the message is what the job said.
	AddonUpgradeFailed AddonUpgradePhase = "Failed"
)

// AddonUpgradeStatus is how the attempt went.
//
// Like a PlatformUpdate's, it is derived from the install job rather than
// remembered: the reconcile that reports an upgrade finished is usually not
// the one that started it, and may not even be the same process.
type AddonUpgradeStatus struct {
	// +optional
	Phase AddonUpgradePhase `json:"phase,omitempty"`

	// StartedAt is when the install job was created, not when this record
	// was, so the timestamp is the platform change's and not the observer's.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Message explains a failure in helm's own words.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Addon",type=string,JSONPath=`.spec.addon`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Started",type=date,JSONPath=`.status.startedAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AddonUpgrade is one attempt to move a platform dependency from the versions
// it was installed at to the ones the operator now pins.
//
// It is `PlatformUpdate`'s shape applied one layer down. An addon's
// `status.charts` is singular and current: when the operator carries a
// dependency forward, the previous versions are overwritten and nothing
// remembers that they changed, or when. Every other class of platform change
// is recoverable after the fact — releases through PlatformUpdate, reboots and
// reprogrammings through events, operator actions through the audit log — and
// this is the fourth.
//
// It owner-references nothing and is never deleted: the list is the addon's
// upgrade history, and a history that is garbage-collected with the thing it
// is about is not one.
type AddonUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AddonUpgradeSpec   `json:"spec,omitempty"`
	Status AddonUpgradeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AddonUpgradeList contains a list of AddonUpgrade.
type AddonUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AddonUpgrade `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AddonUpgrade{}, &AddonUpgradeList{})
}

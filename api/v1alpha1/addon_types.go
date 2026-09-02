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

// An Addon is one platform dependency the operator can install into the
// cluster it owns, asked for by name.
//
// The name *is* the catalogue entry — `keda`, `cloudnative-pg` — which is
// what makes two Addons for one dependency impossible to write rather than
// merely discouraged, and what keeps the object free of anything a general
// installer would need. There is no repository URL here, no chart name, no
// version and no values file: the catalogue is compiled into the operator,
// and the only thing this object contributes to the install job's argv is a
// namespace, checked against a DNS label first.
//
// That is the line the platform does not cross. An Addon's install job is
// bound to an account that can apply CRDs and ClusterRoles, so an object that
// could name what to install would make that grant unbounded and its audit
// record — "the platform installed entry X at pin Y" — worth nothing.
//
// **The object is the request; the chart value is the grant.** Anyone with
// RBAC in this namespace can create one of these, so the gate cannot be
// whether the object exists. The chart passes the operator the entries it
// permitted, as the account each may install with, and an Addon naming an
// entry outside that set is Refused with the chart value that would permit
// it.

// AddonSpec is what an installation has asked the platform to do about one
// catalogue entry.
type AddonSpec struct {
	// Install is whether the platform should install this entry. False
	// leaves the cluster alone: an entry already serving is still reported,
	// still used, and never written to.
	//
	// It is the request, and it is separate from the grant — the chart value
	// that creates the install job's account. Both are required, and they
	// are two decisions: one is "I want this", the other is "the operator
	// may hold an account that can install it".
	// +optional
	Install bool `json:"install,omitempty"`

	// Namespace the entry is installed into. Empty is the entry's own
	// default, which is upstream's — what an installation taking the release
	// over by hand later will expect to find.
	//
	// It is the one value from this object that reaches helm, as its own
	// argv element and never a shell's, and only after it has been checked
	// against a DNS label.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
}

// AddonChartStatus is one chart of an entry and the version of it that is
// installed. An entry installs one or several — KEDA installs two, in order,
// which is the whole reason the ordering lives in an operator rather than in
// a Helm release.
type AddonChartStatus struct {
	// Name of the chart, as helm knows it.
	Name string `json:"name"`

	// Version installed. Empty where an install job from before the operator
	// labelled its jobs left it unknown.
	// +optional
	Version string `json:"version,omitempty"`
}

// AddonStatus is what the platform did about the entry, and what it found.
type AddonStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Managed is true when the platform installed this entry, and false when
	// it found it already serving. It is what the operator reads as
	// permission to upgrade the release — and nothing it does ever writes to
	// a release it did not create.
	//
	// It is a copy rather than the fact itself: the install job says what it
	// installed and where, outlives the reconcile that read it, and this is
	// re-derived from it on every pass, so a status write that never lands
	// costs a pass and not the release.
	// +optional
	Managed bool `json:"managed,omitempty"`

	// Serving is whether the cluster answers the entry's own API — the only
	// question that decides whether the dependency is usable, whoever
	// installed it.
	// +optional
	Serving bool `json:"serving,omitempty"`

	// Namespace the entry runs in, as the platform installed it or expected
	// to find it.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Charts installed, in the order the entry installs them.
	// +optional
	Charts []AddonChartStatus `json:"charts,omitempty"`

	// ObservedGeneration is the spec generation this status was written for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// The condition an Addon carries, and the reasons the operator writes on it.
const (
	// AddonReady says whether the entry is serving in this cluster. It is a
	// fact about the cluster and not about the install: an entry somebody
	// else installed is Ready, with a reason saying the platform manages
	// nothing about it.
	AddonReady = "Ready"

	// AddonRefused is the reason for an Addon naming an entry this
	// installation did not permit, or one the operator has no catalogue
	// entry for. Nothing is installed and nothing is retried; the message
	// names the chart value that would permit it.
	AddonRefused = "Refused"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Install",type=boolean,JSONPath=`.spec.install`
// +kubebuilder:printcolumn:name="Serving",type=boolean,JSONPath=`.status.serving`
// +kubebuilder:printcolumn:name="Managed",type=boolean,JSONPath=`.status.managed`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Addon is the Schema for the addons API.
type Addon struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AddonSpec   `json:"spec,omitempty"`
	Status AddonStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AddonList contains a list of Addon.
type AddonList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Addon `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Addon{}, &AddonList{})
}

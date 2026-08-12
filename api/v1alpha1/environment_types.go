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

// EnvironmentType distinguishes the single production Environment from
// ephemeral previews.
// +kubebuilder:validation:Enum=production;preview
type EnvironmentType string

const (
	EnvironmentProduction EnvironmentType = "production"
	EnvironmentPreview    EnvironmentType = "preview"
)

// PreviewInfo links a preview Environment to its pull request.
type PreviewInfo struct {
	// +kubebuilder:validation:Minimum=1
	PullRequest int32 `json:"pullRequest"`

	// +kubebuilder:validation:MinLength=1
	Branch string `json:"branch"`
}

// EnvironmentSpec defines a running instance of a Release with a URL.
// Rollback is changing ReleaseRef to an older Release.
type EnvironmentSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	// +kubebuilder:default=production
	Type EnvironmentType `json:"type,omitempty"`

	ReleaseRef LocalObjectReference `json:"releaseRef"`

	// Required when Type is preview.
	// +optional
	Preview *PreviewInfo `json:"preview,omitempty"`
}

// EnvironmentPhase is the coarse lifecycle summary of an Environment.
// +kubebuilder:validation:Enum=Pending;Deploying;Live;Degraded;Terminating
type EnvironmentPhase string

const (
	EnvironmentPending     EnvironmentPhase = "Pending"
	EnvironmentDeploying   EnvironmentPhase = "Deploying"
	EnvironmentLive        EnvironmentPhase = "Live"
	EnvironmentDegraded    EnvironmentPhase = "Degraded"
	EnvironmentTerminating EnvironmentPhase = "Terminating"
)

// EnvironmentStatus defines the observed state of an Environment.
type EnvironmentStatus struct {
	// +optional
	Phase EnvironmentPhase `json:"phase,omitempty"`

	// Primary URL the Environment is reachable at.
	// +optional
	URL string `json:"url,omitempty"`

	// Release the running workload was last reconciled to.
	// +optional
	ObservedRelease string `json:"observedRelease,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Environment is the Schema for the environments API.
type Environment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EnvironmentSpec   `json:"spec,omitempty"`
	Status EnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EnvironmentList contains a list of Environment.
type EnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Environment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Environment{}, &EnvironmentList{})
}

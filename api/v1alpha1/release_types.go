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

// ConfigSnapshot is a frozen copy of a Project's deployable configuration at
// build time. Old Releases do not drift when the Project spec changes later,
// which is what makes rollback exact.
type ConfigSnapshot struct {
	// +optional
	// +listType=map
	// +listMapKey=name
	Env []EnvVar `json:"env,omitempty"`

	// +optional
	Runtime RuntimeSpec `json:"runtime,omitempty"`

	// Processes are the project's workers and scheduled jobs as they stood
	// when this Release was built. They are here for the same reason the
	// environment variables are: a rollback that ran today's worker command
	// against yesterday's image would not be a rollback.
	// +optional
	// +listType=map
	// +listMapKey=name
	Processes []ProcessSpec `json:"processes,omitempty"`
}

// ReleaseSpec is an immutable snapshot: an image digest plus the configuration
// it should run with. Named Release (not Deployment) to avoid colliding with
// apps/v1 Deployment.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Release spec is immutable"
type ReleaseSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	BuildRef LocalObjectReference `json:"buildRef"`

	// Image reference by digest, never by tag.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// +optional
	ConfigSnapshot ConfigSnapshot `json:"configSnapshot,omitempty"`
}

// ReleaseStatus defines the observed state of a Release.
type ReleaseStatus struct {
	// Environments currently running this Release (informational).
	// +optional
	Environments []string `json:"environments,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="Build",type=string,JSONPath=`.spec.buildRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Release is the Schema for the releases API.
type Release struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReleaseSpec   `json:"spec,omitempty"`
	Status ReleaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReleaseList contains a list of Release.
type ReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Release `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Release{}, &ReleaseList{})
}

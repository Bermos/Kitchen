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

// GitRevision identifies the commit a Build builds.
type GitRevision struct {
	// +kubebuilder:validation:MinLength=7
	SHA string `json:"sha"`

	// +kubebuilder:validation:MinLength=1
	Branch string `json:"branch"`

	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	Author string `json:"author,omitempty"`

	// Pull request number, when the commit belongs to one.
	// +optional
	PullRequest *int32 `json:"pullRequest,omitempty"`
}

// BuildSpec defines one build execution for one commit. Builds are immutable:
// a rebuild is a new Build object.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Build spec is immutable"
type BuildSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	Git GitRevision `json:"git"`
}

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

// BuildStatus defines the observed state of a Build.
type BuildStatus struct {
	// +optional
	Phase BuildPhase `json:"phase,omitempty"`

	// Framework detected by the auto strategy (e.g. nextjs, vite, static).
	// +optional
	DetectedFramework string `json:"detectedFramework,omitempty"`

	// Image reference by digest, never by tag.
	// +optional
	Image string `json:"image,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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

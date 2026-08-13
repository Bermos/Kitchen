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

// GitSourceSpec links a Project to a repository via a gitSource Connection.
type GitSourceSpec struct {
	// Connection with the gitSource capability.
	ConnectionRef LocalObjectReference `json:"connectionRef"`

	// Repository in the provider's owner/name form.
	// +kubebuilder:validation:MinLength=1
	Repo string `json:"repo"`

	// Branch whose builds auto-promote to the production Environment.
	// +kubebuilder:default=main
	ProductionBranch string `json:"productionBranch,omitempty"`
}

// ProjectBuildSpec overrides platform build defaults for one project.
type ProjectBuildSpec struct {
	// +kubebuilder:default=auto
	Strategy BuildStrategy `json:"strategy,omitempty"`

	// Path to the Dockerfile, relative to RootDirectory. Used when the
	// strategy is (or resolves to) dockerfile.
	// +kubebuilder:default=Dockerfile
	DockerfilePath string `json:"dockerfilePath,omitempty"`

	// Directory within the repository to build from (monorepo support).
	// +kubebuilder:default=.
	RootDirectory string `json:"rootDirectory,omitempty"`
}

// RegistrySpec selects where built images are stored.
type RegistrySpec struct {
	// Connection with the imageStore capability.
	ConnectionRef LocalObjectReference `json:"connectionRef"`
}

// PreviewsSpec configures preview environments for pull requests.
type PreviewsSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// Grace period before a preview Environment is torn down after its
	// pull request closes.
	// +optional
	TTLAfterClosed *metav1.Duration `json:"ttlAfterClosed,omitempty"`
}

// ProjectSpec defines the desired state of a Project: a repository that
// becomes a running application.
type ProjectSpec struct {
	Source GitSourceSpec `json:"source"`

	// +optional
	Build ProjectBuildSpec `json:"build,omitempty"`

	Registry RegistrySpec `json:"registry"`

	// +optional
	Previews PreviewsSpec `json:"previews,omitempty"`

	// Environment variables, overlaid per environment type.
	// +optional
	// +listType=map
	// +listMapKey=name
	Env []EnvVar `json:"env,omitempty"`

	// +optional
	Runtime RuntimeSpec `json:"runtime,omitempty"`
}

// ProjectStatus defines the observed state of a Project.
type ProjectStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// The Project's production Environment, once created.
	// +optional
	ProductionEnvironmentRef *LocalObjectReference `json:"productionEnvironmentRef,omitempty"`

	// Most recently created Build.
	// +optional
	LatestBuildRef *LocalObjectReference `json:"latestBuildRef,omitempty"`

	// Provider-side identifier of the registered git webhook.
	// +optional
	WebhookID string `json:"webhookId,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.source.repo`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Project is the Schema for the projects API.
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProjectSpec   `json:"spec,omitempty"`
	Status ProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProjectList contains a list of Project.
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Project{}, &ProjectList{})
}

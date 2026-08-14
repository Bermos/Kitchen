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

// ProjectSecretsSpec syncs secrets from a secret store into the project's
// application namespace as native k8s Secrets, one per environment type:
// kitchen-secrets-production and kitchen-secrets-preview. Environment
// variables reference them by the alias "kitchen-secrets" in
// spec.env[].secretRef, which resolves to the right one per environment —
// production and previews read the same key from different store
// environments.
type ProjectSecretsSpec struct {
	// Connection with the secretStore capability.
	ConnectionRef LocalObjectReference `json:"connectionRef"`

	// ProjectSlug names the project in the secret store to sync from (for
	// Infisical, the project slug).
	// +kubebuilder:validation:MinLength=1
	ProjectSlug string `json:"projectSlug"`

	// SecretsPath is the folder within the store's project to sync,
	// recursively.
	// +kubebuilder:default="/"
	// +optional
	SecretsPath string `json:"secretsPath,omitempty"`

	// ProductionEnv is the store environment production syncs from. The
	// default matches Infisical's built-in environments.
	// +kubebuilder:default=prod
	// +optional
	ProductionEnv string `json:"productionEnv,omitempty"`

	// PreviewEnv is the store environment previews sync from. All previews
	// share it: a preview is unreleased code, not unreleased credentials.
	// +kubebuilder:default=staging
	// +optional
	PreviewEnv string `json:"previewEnv,omitempty"`
}

// PreviewsSpec configures preview environments for pull requests.
type PreviewsSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// Protected gates preview URLs behind platform login: the Environment's
	// route goes through the forward-auth gate, which sends anonymous
	// requests to the identity provider and only proxies signed-in ones to
	// the application. The application itself needs no changes. Production
	// environments are never gated.
	//
	// It defaults to on, so unreleased work is not published to anyone who
	// guesses the URL. With no gate on the platform the Environment gets no
	// route at all rather than a public one — set this to false to serve
	// previews openly on purpose.
	// +kubebuilder:default=true
	// +optional
	Protected *bool `json:"protected,omitempty"`

	// Grace period before a preview Environment is torn down after its
	// pull request closes.
	// +optional
	TTLAfterClosed *metav1.Duration `json:"ttlAfterClosed,omitempty"`
}

// IsProtected reports whether previews of this Project are gated behind
// platform login. Previews written before the field existed are protected: it
// is the safe reading of an absent value, and the API server defaults it to
// true on the next write anyway.
func (p PreviewsSpec) IsProtected() bool {
	return p.Protected == nil || *p.Protected
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

	// Secrets synced from a secret store into the application namespace,
	// scoped per environment type.
	// +optional
	Secrets *ProjectSecretsSpec `json:"secrets,omitempty"`

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

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
	corev1 "k8s.io/api/core/v1"
)

// LocalObjectReference references another Kitchen object by name within the
// platform namespace. Cross-namespace references are not supported in v1alpha1.
type LocalObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SecretKeySelector selects a key of a Secret in the platform namespace.
type SecretKeySelector struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// ResourceClaimKeySelector selects a binding key exposed by a ResourceClaim.
type ResourceClaimKeySelector struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// EnvVar is a single environment variable for an application. Exactly one of
// Value, SecretRef, or FromResourceClaim should be set. PreviewValue, when set,
// replaces Value in preview environments.
type EnvVar struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Literal value.
	// +optional
	Value string `json:"value,omitempty"`

	// Literal value used in preview environments instead of Value.
	// +optional
	PreviewValue string `json:"previewValue,omitempty"`

	// Value taken from a Secret in the application namespace. The name
	// "kitchen-secrets" resolves per environment type to the Secret the
	// project's secret store syncs into (see Project spec.secrets):
	// kitchen-secrets-production or kitchen-secrets-preview — which is how the
	// same key gets production values in production and preview values in
	// previews. Any other name is used as written.
	// +optional
	SecretRef *SecretKeySelector `json:"secretRef,omitempty"`

	// Value taken from a ResourceClaim binding (e.g. a provisioned database URL).
	// +optional
	FromResourceClaim *ResourceClaimKeySelector `json:"fromResourceClaim,omitempty"`
}

// RuntimeSpec describes how an application runs.
type RuntimeSpec struct {
	// Container port the application listens on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=3000
	Port int32 `json:"port,omitempty"`

	// Replica count for production environments. Previews always run 1.
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Compute resources per replica.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// BuildStrategy selects how an image is produced from a repository.
// +kubebuilder:validation:Enum=auto;dockerfile;buildpacks
type BuildStrategy string

const (
	// BuildStrategyAuto detects the framework and picks a strategy.
	BuildStrategyAuto BuildStrategy = "auto"
	// BuildStrategyDockerfile builds from a Dockerfile in the repository.
	BuildStrategyDockerfile BuildStrategy = "dockerfile"
	// BuildStrategyBuildpacks builds with Cloud Native Buildpacks.
	BuildStrategyBuildpacks BuildStrategy = "buildpacks"
)

// TLSMode selects how edge TLS is provided.
// +kubebuilder:validation:Enum=cloudflared;acme;none
type TLSMode string

const (
	// TLSModeCloudflared terminates TLS at the Cloudflare edge via cloudflared.
	TLSModeCloudflared TLSMode = "cloudflared"
	// TLSModeACME issues certificates with cert-manager.
	TLSModeACME TLSMode = "acme"
	// TLSModeNone serves plain HTTP (development only).
	TLSModeNone TLSMode = "none"
)

// Capability is an abstract feature a Connection provider implements. The
// operator matches on capabilities, never on provider names.
// +kubebuilder:validation:Enum=gitSource;statusChecks;imageStore;database;secretStore
type Capability string

const (
	CapabilityGitSource    Capability = "gitSource"
	CapabilityStatusChecks Capability = "statusChecks"
	CapabilityImageStore   Capability = "imageStore"
	CapabilityDatabase     Capability = "database"
	// CapabilitySecretStore syncs secrets from an external store into app
	// namespaces as native k8s Secrets (first provider: infisical).
	CapabilitySecretStore Capability = "secretStore"
)

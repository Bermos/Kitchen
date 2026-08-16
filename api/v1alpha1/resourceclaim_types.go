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
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClaimDeletionPolicy says what happens to the provisioned resource when its
// claim is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type ClaimDeletionPolicy string

const (
	// ClaimRetain keeps the provisioned resource at the provider. Only the
	// platform's own bookkeeping — binding Secrets and preview branches — is
	// removed with the claim.
	ClaimRetain ClaimDeletionPolicy = "Retain"
	// ClaimDelete deprovisions the resource, destroying its data.
	ClaimDelete ClaimDeletionPolicy = "Delete"
)

// ResourceClaimSpec is a Project's request for a provisioned resource (e.g. a
// Postgres database) from a capable Connection.
type ResourceClaimSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	// Connection with a capability matching Type (e.g. database).
	ConnectionRef LocalObjectReference `json:"connectionRef"`

	// Kind of resource requested.
	// +kubebuilder:validation:Enum=postgres
	Type string `json:"type"`

	// DeletionPolicy is what deleting the claim does to the provisioned
	// resource. Retain is the default because a claim can own a production
	// database: destroying data has to be opted into, never implied by
	// removing the platform object in front of it. Preview branches and the
	// binding Secrets are cleaned up under either policy.
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy ClaimDeletionPolicy `json:"deletionPolicy,omitempty"`

	// Provider-specific configuration, validated by the plugin.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Config *runtime.RawExtension `json:"config,omitempty"`
}

// claimConfig is the provider-agnostic slice of spec.config the platform
// itself reads; everything else in there belongs to the plugin.
type claimConfig struct {
	PreviewBranching bool `json:"previewBranching,omitempty"`
}

// PreviewBranching reports whether spec.config asks for a database branch per
// preview Environment. Config the platform cannot read counts as off — the
// plugin's own validation is where a malformed config becomes an error.
func (c *ResourceClaim) PreviewBranching() bool {
	if c.Spec.Config == nil || len(c.Spec.Config.Raw) == 0 {
		return false
	}
	var cfg claimConfig
	if err := json.Unmarshal(c.Spec.Config.Raw, &cfg); err != nil {
		return false
	}
	return cfg.PreviewBranching
}

// ClaimPhase is the coarse lifecycle summary of a ResourceClaim.
// +kubebuilder:validation:Enum=Pending;Bound;Failed
type ClaimPhase string

const (
	ClaimPending ClaimPhase = "Pending"
	ClaimBound   ClaimPhase = "Bound"
	ClaimFailed  ClaimPhase = "Failed"
)

// ClaimBranch records one provider-side database branch the claim created for
// a preview Environment, and the Secret its binding was written into.
type ClaimBranch struct {
	// Environment is the preview Environment the branch belongs to.
	Environment string `json:"environment"`

	// ID is the provider-side branch identifier.
	ID string `json:"id"`

	// SecretName is the per-environment binding Secret in the project
	// namespace.
	SecretName string `json:"secretName"`
}

// ResourceClaimStatus defines the observed state of a ResourceClaim.
type ResourceClaimStatus struct {
	// +optional
	Phase ClaimPhase `json:"phase,omitempty"`

	// Secret in the project namespace holding the binding keys
	// (url, host, password, ...), per environment where applicable.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// InstanceID is the provider-side identifier of the provisioned resource,
	// opaque to the platform. It is what deprovisioning and branch operations
	// address.
	// +optional
	InstanceID string `json:"instanceID,omitempty"`

	// Branches are the provider-side branches created for preview
	// Environments, torn down with them.
	// +optional
	Branches []ClaimBranch `json:"branches,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ResourceClaim is the Schema for the resourceclaims API.
type ResourceClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourceClaimSpec   `json:"spec,omitempty"`
	Status ResourceClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ResourceClaimList contains a list of ResourceClaim.
type ResourceClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceClaim{}, &ResourceClaimList{})
}

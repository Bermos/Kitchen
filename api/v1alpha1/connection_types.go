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
	"k8s.io/apimachinery/pkg/runtime"
)

// CredentialsReference names the Secret a Connection's credential lives in.
//
// It is a type of its own rather than a LocalObjectReference because it is
// the one reference in the API that may legitimately name nothing: the cnpg
// provider provisions into the cluster Kitchen is installed in, with the
// operator's own account, and so has no credential for anybody to store. The
// CEL rule on ConnectionSpec is what keeps that the exception rather than a
// hole — every other provider is still refused without one.
type CredentialsReference struct {
	// +optional
	Name string `json:"name,omitempty"`
}

// ConnectionSpec defines a plugin instance: a link to an external system such
// as a git provider, an image registry, or a database provisioner.
// +kubebuilder:validation:XValidation:rule="self.provider == 'cnpg' || (has(self.credentialsSecretRef) && has(self.credentialsSecretRef.name) && size(self.credentialsSecretRef.name) > 0)",message="credentialsSecretRef is required: it names the Secret holding this provider's credential. Only a cnpg connection goes without one, because it provisions Postgres into this cluster with the operator's own account and there is no credential to hold."
// +kubebuilder:validation:XValidation:rule="self.provider != 'cnpg' || !has(self.credentialsSecretRef) || !has(self.credentialsSecretRef.name) || size(self.credentialsSecretRef.name) == 0",message="a cnpg connection takes no credentialsSecretRef: it provisions into this cluster with the operator's own account, and a Secret here would name a credential nothing reads."
type ConnectionSpec struct {
	// Provider selects the plugin implementation.
	// +kubebuilder:validation:Enum=github;gitlab;gitea;dockerRegistry;neon;cnpg
	Provider string `json:"provider"`

	// Secret holding the provider credentials (typically synced from
	// Infisical). Every provider but cnpg requires one; see
	// CredentialsReference for why that one does not.
	// +optional
	CredentialsSecretRef CredentialsReference `json:"credentialsSecretRef,omitempty"`

	// Provider-specific configuration, validated by the plugin.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Config *runtime.RawExtension `json:"config,omitempty"`
}

// ConnectionStatus defines the observed state of a Connection.
type ConnectionStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Capabilities the provider implements. The operator matches on these,
	// never on provider names.
	// +optional
	Capabilities []Capability `json:"capabilities,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Connected",type=string,JSONPath=`.status.conditions[?(@.type=="Connected")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Connection is the Schema for the connections API.
type Connection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConnectionSpec   `json:"spec,omitempty"`
	Status ConnectionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConnectionList contains a list of Connection.
type ConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Connection `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Connection{}, &ConnectionList{})
}

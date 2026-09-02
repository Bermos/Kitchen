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

// ConnectionProvidersWithoutCredential is the set of providers that hold no
// credential, because they provision into the cluster the platform is
// installed in with the operator's own account. It is what the two rules on
// ConnectionSpec are written against; the set in the markers is held to this
// one by a test, since a marker cannot read a Go value.
var ConnectionProvidersWithoutCredential = []string{"cnpg", "valkey"}

// ProviderNeedsCredential reports whether a provider has a credential to
// store at all. It is what lets the reconciler and the API stop looking for
// a Secret that is not meant to exist, rather than reporting its absence as
// a fault.
func ProviderNeedsCredential(provider string) bool {
	for _, name := range ConnectionProvidersWithoutCredential {
		if name == provider {
			return false
		}
	}
	return true
}

// ConnectionSpec defines a plugin instance: a link to an external system such
// as a git provider, an image registry, or a database provisioner.
// +kubebuilder:validation:XValidation:rule="self.provider in ['cnpg', 'valkey'] || (has(self.credentialsSecretRef) && has(self.credentialsSecretRef.name) && size(self.credentialsSecretRef.name) > 0)",message="credentialsSecretRef is required: it names the Secret holding this provider's credential. Only a provider that provisions into this cluster with the operator's own account goes without one.",messageExpression="'credentialsSecretRef is required: it names the Secret holding the credential of a ' + self.provider + ' connection. Only a provider that provisions into this cluster with the account the operator itself holds (cnpg) goes without one.'"
// +kubebuilder:validation:XValidation:rule="!(self.provider in ['cnpg', 'valkey']) || !has(self.credentialsSecretRef) || !has(self.credentialsSecretRef.name) || size(self.credentialsSecretRef.name) == 0",message="this provider takes no credentialsSecretRef: it provisions into this cluster with the operator's own account, and a Secret here would name a credential nothing reads.",messageExpression="'a ' + self.provider + ' connection takes no credentialsSecretRef: it provisions into this cluster with the account the operator itself holds, and a Secret here would name a credential nothing reads.'"
type ConnectionSpec struct {
	// Provider selects the plugin implementation.
	// +kubebuilder:validation:Enum=github;gitlab;gitea;dockerRegistry;neon;cnpg;s3;inngest;valkey;redis
	Provider string `json:"provider"`

	// Secret holding the provider credentials (typically synced from
	// Infisical). Every provider outside ConnectionProvidersWithoutCredential
	// requires one; see CredentialsReference for why those do not.
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

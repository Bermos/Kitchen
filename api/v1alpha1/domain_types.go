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

// DomainSpec attaches a custom domain to an Environment.
type DomainSpec struct {
	// Fully qualified hostname, e.g. shop.example.com.
	// +kubebuilder:validation:MinLength=1
	Hostname string `json:"hostname"`

	EnvironmentRef LocalObjectReference `json:"environmentRef"`

	// TLS mode for this domain. Empty inherits the platform default from
	// the Kitchen object.
	// +optional
	TLS TLSMode `json:"tls,omitempty"`
}

// DomainVerification is the exact DNS change that proves ownership of the
// hostname, spelled out so the API and the dashboard can show it verbatim.
// Either record satisfies the check.
type DomainVerification struct {
	// TXTRecord is the name of the TXT record to create:
	// _kitchen-challenge.<hostname>.
	TXTRecord string `json:"txtRecord"`

	// TXTValue is the exact value that record must carry. It is derived
	// deterministically from the Domain's UID, so a wiped status recomputes
	// the same token.
	TXTValue string `json:"txtValue"`

	// CNAMETarget is the alternative: a CNAME from the hostname itself to
	// this name both proves ownership and points traffic at the platform.
	// +optional
	CNAMETarget string `json:"cnameTarget,omitempty"`
}

// DomainStatus defines the observed state of a Domain.
type DomainStatus struct {
	// DNS ownership verified (TXT or CNAME check).
	// +optional
	Verified bool `json:"verified,omitempty"`

	// Verification is the DNS record the owner has to create for Verified to
	// become true.
	// +optional
	Verification *DomainVerification `json:"verification,omitempty"`

	// TLSMode in effect for this domain: the spec's own, or the platform
	// default it inherits when the spec leaves it empty.
	// +optional
	TLSMode TLSMode `json:"tlsMode,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.spec.hostname`
// +kubebuilder:printcolumn:name="Environment",type=string,JSONPath=`.spec.environmentRef.name`
// +kubebuilder:printcolumn:name="Verified",type=boolean,JSONPath=`.status.verified`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Domain is the Schema for the domains API.
type Domain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DomainSpec   `json:"spec,omitempty"`
	Status DomainStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DomainList contains a list of Domain.
type DomainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Domain `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Domain{}, &DomainList{})
}

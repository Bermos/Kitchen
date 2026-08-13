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

// CloudflaredSpec configures the optional cloudflared tunnel fronting the Gateway.
type CloudflaredSpec struct {
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Secret holding the tunnel credentials.
	// +optional
	TunnelSecretRef *LocalObjectReference `json:"tunnelSecretRef,omitempty"`
}

// IngressSpec configures how traffic enters the cluster.
type IngressSpec struct {
	// GatewayClass used for the shared Gateway. Cilium's Gateway API
	// implementation is the expected default.
	// +kubebuilder:default=cilium
	GatewayClassName string `json:"gatewayClassName,omitempty"`

	// +optional
	Cloudflared CloudflaredSpec `json:"cloudflared,omitempty"`
}

// TLSSpec configures platform-wide TLS defaults.
type TLSSpec struct {
	// +kubebuilder:default=acme
	Mode TLSMode `json:"mode,omitempty"`
}

// BuildsSpec configures platform-wide build defaults.
type BuildsSpec struct {
	// +kubebuilder:default=auto
	DefaultStrategy BuildStrategy `json:"defaultStrategy,omitempty"`

	// Maximum number of builds running at once.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	Concurrency int32 `json:"concurrency,omitempty"`
}

// ClickHouseSpec configures the telemetry store.
type ClickHouseSpec struct {
	// Days to retain logs, metrics, traces and flow data.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=30
	RetentionDays int32 `json:"retentionDays,omitempty"`
}

// ObservabilitySpec configures the telemetry pipeline.
type ObservabilitySpec struct {
	// +optional
	ClickHouse ClickHouseSpec `json:"clickhouse,omitempty"`
}

// APISpec configures how the operator's API is exposed.
type APISpec struct {
	// ExternalURL is the base URL the operator API (including the git
	// webhook receiver) is reachable at from outside the cluster.
	// Defaults to https://kitchen.<baseDomain>.
	// +optional
	ExternalURL string `json:"externalURL,omitempty"`
}

// AuthSpec configures the platform's identity provider: the OIDC issuer the
// UI, the operator API and opted-in apps all authenticate against.
type AuthSpec struct {
	// Whether the platform has an identity provider at all. Without one there
	// is no login for the UI and no issuer for apps to claim clients from.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// Hostname the identity provider is served on, and therefore the OIDC
	// issuer (https://<host>). Defaults to auth.<baseDomain>.
	// +optional
	Host string `json:"host,omitempty"`
}

// KitchenSpec defines platform-wide configuration. There is exactly one
// Kitchen object per cluster; it is the operator's runtime configuration and
// is editable from the management UI.
type KitchenSpec struct {
	// Base domain for generated URLs: <slug>.<baseDomain>. Requires wildcard
	// DNS (and wildcard TLS unless cloudflared is enabled).
	// +kubebuilder:validation:MinLength=1
	BaseDomain string `json:"baseDomain"`

	// +optional
	API APISpec `json:"api,omitempty"`

	// +optional
	Ingress IngressSpec `json:"ingress,omitempty"`

	// +optional
	TLS TLSSpec `json:"tls,omitempty"`

	// +optional
	Auth AuthSpec `json:"auth,omitempty"`

	// +optional
	Builds BuildsSpec `json:"builds,omitempty"`

	// +optional
	Observability ObservabilitySpec `json:"observability,omitempty"`
}

// KitchenStatus defines the observed state of the platform.
type KitchenStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Externally reachable address of the shared Gateway, once programmed.
	// +optional
	GatewayAddress string `json:"gatewayAddress,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="BaseDomain",type=string,JSONPath=`.spec.baseDomain`
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.status.gatewayAddress`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Kitchen is the cluster-wide platform configuration singleton.
type Kitchen struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KitchenSpec   `json:"spec,omitempty"`
	Status KitchenStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KitchenList contains a list of Kitchen.
type KitchenList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Kitchen `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Kitchen{}, &KitchenList{})
}

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

// CloudflareSolverSpec configures the Cloudflare DNS-01 solver.
type CloudflareSolverSpec struct {
	// APITokenSecretRef selects the Cloudflare API token to write challenge
	// records with. The token needs Zone:DNS:Edit on the zone the base domain
	// belongs to, and Zone:Zone:Read to find it.
	APITokenSecretRef SecretKeySelector `json:"apiTokenSecretRef"`
}

// ACMEDNS01Spec selects the DNS-01 solver the issuer challenges with. There is
// deliberately no HTTP-01 alternative: every generated URL is a subdomain of
// the base domain, so the platform needs a wildcard certificate, and ACME
// issues those over DNS-01 only.
type ACMEDNS01Spec struct {
	// +optional
	Cloudflare *CloudflareSolverSpec `json:"cloudflare,omitempty"`
}

// ACMESpec configures the ClusterIssuer the operator creates in acme TLS mode,
// and therefore how the wildcard certificate for the base domain is obtained.
type ACMESpec struct {
	// Email the CA contacts about expiring certificates and account problems.
	// +kubebuilder:validation:MinLength=1
	Email string `json:"email"`

	// Server is the ACME directory URL. Point it at Let's Encrypt's staging
	// directory while setting the platform up: staging has far higher rate
	// limits, at the cost of a certificate browsers do not trust.
	// +kubebuilder:default="https://acme-v02.api.letsencrypt.org/directory"
	// +optional
	Server string `json:"server,omitempty"`

	// DNS01 configures how challenges are solved.
	DNS01 ACMEDNS01Spec `json:"dns01"`
}

// TLSSpec configures platform-wide TLS defaults.
type TLSSpec struct {
	// +kubebuilder:default=acme
	Mode TLSMode `json:"mode,omitempty"`

	// ACME configures the issuer the operator creates, and the wildcard
	// certificate it requests from it, when Mode is acme. Without it the
	// operator manages no certificate and the Gateway's HTTPS listener waits
	// for one to appear by other means.
	// +optional
	ACME *ACMESpec `json:"acme,omitempty"`
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

	// SecretRef names the Secret in the platform namespace holding the
	// connection details for the store: the keys host, httpPort, database,
	// username and password. The chart writes it as
	// <release>-clickhouse, whether it runs ClickHouse itself or points at
	// an external one. Without it the operator manages no telemetry schema
	// and the collectors have nowhere to ship to.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
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

// PreviewGateSpec configures the forward-auth gate that protected preview
// environments are served through: an in-path component that turns an
// anonymous request into a platform login and only then proxies to the app.
type PreviewGateSpec struct {
	// Whether the platform runs a gate at all. A Project asking for protected
	// previews without one gets no route rather than a public one.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// Hostname the gate serves its OAuth callback on. It is the only redirect
	// URI registered with the identity provider, which is what lets previews
	// come and go without touching the OAuth client.
	// Defaults to previews.<baseDomain>.
	// +optional
	Host string `json:"host,omitempty"`

	// Replicas of the gate. It is in the request path of every protected
	// preview, so it is worth more than one.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// How long a visitor's session on a preview lasts before the gate sends
	// them back to the identity provider — which, if they are still signed in
	// there, costs them a redirect and no interaction.
	// +kubebuilder:default="8h"
	// +optional
	SessionTTL *metav1.Duration `json:"sessionTTL,omitempty"`
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

	// SecretRef names the Secret in the platform namespace holding the
	// operator's view of the identity provider: the keys issuer, serviceKey
	// and, optionally, internalURL — a cluster-internal address for the same
	// issuer, for clusters that cannot resolve their own base domain from the
	// inside. The chart writes it as <release>-auth. Without it the operator
	// cannot register OAuth clients, so nothing that depends on one — the
	// preview gate, later the oidcClient claims — can be reconciled.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// +kubebuilder:default={}
	// +optional
	PreviewGate PreviewGateSpec `json:"previewGate,omitempty"`
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

	// An installation with no auth block at all still has an identity
	// provider: the defaults inside it only apply once the object is there.
	// +kubebuilder:default={}
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

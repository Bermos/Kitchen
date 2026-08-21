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

// Claim types, which are what decides who provisions the resource.
const (
	// ClaimTypePostgres is a database from a database-capable Connection.
	ClaimTypePostgres = "postgres"

	// ClaimTypeOIDCClient is an OAuth client at the platform's own identity
	// provider, so that a deployed application signs its users in with the
	// same accounts the dashboard uses. It is the one claim type with no
	// Connection: the provider is the issuer the platform is already
	// configured with, and the operator registers the client with the
	// service credential it holds for it.
	ClaimTypeOIDCClient = "oidcClient"
)

// ResourceClaimSpec is a Project's request for a provisioned resource — a
// Postgres database from a capable Connection, or an OAuth client from the
// platform's own identity provider.
// +kubebuilder:validation:XValidation:rule="self.type == 'oidcClient' || has(self.connectionRef)",message="connectionRef is required: it names the Connection that provisions the resource. Only an oidcClient claim goes without one, because the platform's own identity provider provisions it."
// +kubebuilder:validation:XValidation:rule="self.type != 'oidcClient' || !has(self.connectionRef)",message="an oidcClient claim takes no connectionRef: the client is registered at the identity provider named by the Kitchen object's spec.auth, and a Connection here would name a provider nothing would ask."
type ResourceClaimSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	// Connection with a capability matching Type (e.g. database). Required
	// for every type but oidcClient, which has no Connection to name.
	// +optional
	ConnectionRef *LocalObjectReference `json:"connectionRef,omitempty"`

	// Kind of resource requested. It is immutable: the type decides who
	// provisions the resource and what the binding Secret holds, so changing
	// it on a bound claim would leave a database behind while the
	// application's environment quietly started reading OAuth credentials
	// out of the same keys. Ask for the other one and delete this.
	// +kubebuilder:validation:Enum=postgres;oidcClient
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable: delete the claim and ask for the other kind"
	Type string `json:"type"`

	// DeletionPolicy is what deleting the claim does to the provisioned
	// resource. Retain is the default because a claim can own a production
	// database: destroying data has to be opted into, never implied by
	// removing the platform object in front of it. Preview branches and the
	// binding Secrets are cleaned up under either policy.
	//
	// It says nothing about an oidcClient claim, whose client is always
	// deregistered: the policy exists to protect data from a deletion nobody
	// meant, and an OAuth client holds none — what it holds is permission to
	// sign people in, which is the thing that must not outlive the claim.
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy ClaimDeletionPolicy `json:"deletionPolicy,omitempty"`

	// Provider-specific configuration, validated by the plugin.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Config *runtime.RawExtension `json:"config,omitempty"`

	// DataClass is the sensitivity classification of the data this claim's
	// resource holds. It may not exceed the project's own class — children
	// narrow a classification, never widen it — which the API enforces when
	// the claim is created. Absent means unclassified, surfaced as such and
	// never defaulted.
	// +optional
	DataClass DataClass `json:"dataClass,omitempty"`
}

// claimConfig is the provider-agnostic slice of spec.config the platform
// itself reads; everything else in there belongs to the plugin.
type claimConfig struct {
	PreviewBranching bool `json:"previewBranching,omitempty"`
}

// OIDCClientConfig is the oidcClient slice of spec.config: what the operator
// registers the client with, and what it keeps its redirect list made of.
type OIDCClientConfig struct {
	// CallbackPaths are appended to every URL the project's Environments are
	// reachable at to build the client's redirect list. They are paths and
	// not URLs because that is the whole point of the type: the operator owns
	// the URLs, and a preview that does not exist yet cannot be written down.
	// +optional
	CallbackPaths []string `json:"callbackPaths,omitempty"`

	// RedirectURIs are registered verbatim alongside the generated ones, for
	// the addresses the platform does not own — a developer's
	// http://localhost:3000/auth/callback being the whole of why this exists.
	// +optional
	RedirectURIs []string `json:"redirectURIs,omitempty"`

	// Scopes the client may ask for.
	// +optional
	Scopes []string `json:"scopes,omitempty"`
}

// DefaultOIDCCallbackPaths is where an application is assumed to receive the
// authorization code when the claim does not say.
//
// Two of them, because there is no one answer and the cost of a redirect URI
// nothing serves is nothing: every generated URI is on the application's own
// origin, so the worst an unused one can do is land a code the application
// never asked for on a page it does not have. The first is the plain
// convention; the second is what Auth.js builds for a provider called
// kitchen, which is the framework most likely to be on the other end.
// Naming callbackPaths replaces both.
var DefaultOIDCCallbackPaths = []string{"/auth/callback", "/api/auth/callback/kitchen"}

// DefaultOIDCScopes is what a client is registered for when the claim does
// not say. offline_access is included because an application that cannot
// refresh a token signs its users out every hour.
var DefaultOIDCScopes = []string{"openid", "profile", "email", "offline_access"}

// OIDCClient is the claim's OAuth client configuration with the defaults
// filled in. Config the platform cannot read gets the defaults whole — a
// malformed config is refused by the API before it is written, and a claim
// that reached the cluster another way is better registered conventionally
// than not at all.
func (c *ResourceClaim) OIDCClient() OIDCClientConfig {
	cfg := OIDCClientConfig{}
	if c.Spec.Config != nil && len(c.Spec.Config.Raw) > 0 {
		if err := json.Unmarshal(c.Spec.Config.Raw, &cfg); err != nil {
			cfg = OIDCClientConfig{}
		}
	}
	if len(cfg.CallbackPaths) == 0 {
		cfg.CallbackPaths = DefaultOIDCCallbackPaths
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = DefaultOIDCScopes
	}
	return cfg
}

// Connection is the Connection the claim provisions through, empty for a
// claim type that has none.
func (c *ResourceClaim) Connection() string {
	if c.Spec.ConnectionRef == nil {
		return ""
	}
	return c.Spec.ConnectionRef.Name
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

	// Provenance is the provider's declaration of what this branch's data
	// derives from — production for a branch of a production database,
	// however cheap the copy. Empty means the provider declared nothing.
	// It is per-branch because it is the branch, not the primary, that a
	// preview's workload reads, and the policy engine judges the preview on
	// exactly this value.
	// +kubebuilder:validation:Enum=production;masked;synthetic
	// +optional
	Provenance string `json:"provenance,omitempty"`
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

	// DataProvenance is the provider's declaration of what the provisioned
	// data derives from: production, masked or synthetic. Empty means the
	// provider declared nothing (undeclared) — surfaced as such, and treated
	// by policy as the worst case rather than as clean. It is a declaration
	// the platform records and attests (a signed
	// kitchen.bermos.dev DataClass/v1 statement), never something it
	// inspects the data to establish.
	// +kubebuilder:validation:Enum=production;masked;synthetic
	// +optional
	DataProvenance string `json:"dataProvenance,omitempty"`

	// Residency is where the provisioned resource actually is, as the
	// provider reported it — a Neon region id, for the provider that ships.
	// It is recorded from the provider's answer rather than from anything
	// declared, which is what makes it the placement of record: an
	// environment's residency says where data is meant to be, this says
	// where this resource's data is. Empty when the provider reports no
	// placement, and read as "unknown" by the inventory rather than
	// defaulted.
	// +optional
	Residency string `json:"residency,omitempty"`

	// RedirectURIs is the redirect list an oidcClient claim's client is
	// registered with, as the operator last wrote it. It is what a reconcile
	// compares the project's environment URLs against, so that a preview
	// appearing costs one call to the issuer and a reconcile that changes
	// nothing costs none.
	// +optional
	RedirectURIs []string `json:"redirectURIs,omitempty"`

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

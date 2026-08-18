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
//
// +kubebuilder:validation:XValidation:rule="has(self.cloudflare)",message="spec.tls.acme.dns01 needs a solver: set dns01.cloudflare. Every generated URL is a subdomain, so the platform needs a wildcard certificate, and ACME issues wildcards over DNS-01 only."
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

// TLSSpec configures platform-wide TLS defaults. acme mode cannot be
// half-configured: a Kitchen asking for it with no acme block is refused at
// admission, rather than accepted and then reported as a condition on an object
// whose HTTPS listener has no certificate to terminate with.
//
// +kubebuilder:validation:XValidation:rule="self.mode != 'acme' || has(self.acme)",message="spec.tls.acme is required when tls.mode is acme: the shared Gateway's HTTPS listener terminates with the wildcard certificate the operator requests from it. Configure it, or set tls.mode to cloudflared or none."
type TLSSpec struct {
	// +kubebuilder:default=acme
	Mode TLSMode `json:"mode,omitempty"`

	// ACME configures the issuer the operator creates, and the wildcard
	// certificate it requests from it. It is required in acme mode and
	// pointless outside it: the wildcard is read by the Gateway's HTTPS
	// listener, which exists in acme mode alone.
	// +optional
	ACME *ACMESpec `json:"acme,omitempty"`
}

// InterceptorSpec locates the KEDA HTTP add-on's interceptor proxy: the
// component that sits between the shared Gateway and an idling application,
// holds the first request while KEDA scales the workload back up, and forwards
// it once there is a pod to forward it to.
//
// The add-on is a Helm release of its own rather than part of Kitchen's — it
// cannot be a sub-chart of anything, see the chart's Chart.yaml — so this is
// how the operator is told where it went.
type InterceptorSpec struct {
	// Service name of the interceptor's proxy. The add-on names it after its
	// own chart rather than after the release, so this is stable.
	// +kubebuilder:default=keda-add-ons-http-interceptor-proxy
	// +optional
	Service string `json:"service,omitempty"`

	// Namespace the interceptor runs in.
	// +kubebuilder:default=keda
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Port the proxy accepts live traffic on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	// +optional
	Port int32 `json:"port,omitempty"`
}

// ScaleToZeroSpec is the platform switch for idling environments down to no
// pods at all. It is off by default: it needs KEDA and its HTTP add-on running
// in the cluster, and those are the one platform dependency Kitchen's chart
// cannot install for you.
//
// With it on, each Project decides for itself which of its environments idle,
// through its own `spec.scaleToZero`.
type ScaleToZeroSpec struct {
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Interceptor InterceptorSpec `json:"interceptor,omitempty"`
}

// ImageRegistrySpec configures the registry the platform runs for itself: the
// one a fresh install pushes to, so that building a project needs no registry
// account and no credential of anyone's.
//
// It is published on the shared Gateway at registry.<baseDomain>, on the
// platform's own wildcard certificate, and that is the whole reason it works.
// The node's container runtime is what pulls an image, and it trusts neither
// the cluster's CA nor a plain-HTTP address without being configured to —
// which is node configuration, and Kitchen is a chart installed into someone
// else's cluster. A publicly trusted certificate is the one route that asks
// the node for nothing.
//
// The corollary is that it needs TLS to exist: in tls.mode none there is no
// certificate to be trusted and no HTTPS listener to serve it, so the
// platform reports the registry as unavailable rather than publishing a
// registry no node can pull from.
type ImageRegistrySpec struct {
	// Enabled publishes the bundled registry and seeds the Connection that
	// points at it. Off means an installation brings its own registry, which
	// is a Connection someone creates by hand.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// Host the registry is published on, and therefore the prefix images are
	// pushed under: <host>/<project>:<sha>. Defaults to
	// registry.<baseDomain>.
	// +optional
	Host string `json:"host,omitempty"`

	// Service in the platform namespace the route sends traffic to. The
	// chart writes it as <release>-registry.
	// +optional
	Service string `json:"service,omitempty"`

	// Port the registry's Service publishes.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=5000
	// +optional
	Port int32 `json:"port,omitempty"`

	// SecretRef names the Secret in the platform namespace holding the
	// registry's own credential: the keys username and password. The chart
	// writes it as <release>-registry, generating the password once and
	// keeping it across upgrades. It is what the operator copies into the
	// seeded Connection's credential.
	//
	// Left unset it is the conventional release name's, kitchen-registry —
	// which is what makes the registry appear on an upgrade, where this
	// object is not re-applied and so names none of these fields.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
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

// HubbleSpec configures the network flow pipeline. Cilium is the platform's
// CNI (a prerequisite, not something Kitchen installs), and Hubble Relay is
// its cluster-wide flow API; when an address is given, the operator follows
// it and ships flow observations into the telemetry store, which is what the
// dashboard's traffic view draws.
type HubbleSpec struct {
	// RelayAddress is the host:port of Hubble Relay's gRPC endpoint,
	// typically "hubble-relay.kube-system.svc.cluster.local:80" once Hubble
	// is enabled in Cilium. Empty means no flow collection — the traffic
	// view stays empty and says why.
	// +optional
	RelayAddress string `json:"relayAddress,omitempty"`
}

// MetricsSpec configures the half of the resource telemetry the collector
// cannot produce, so that an environment's replica count and restarts are a
// history rather than the instant the page happened to be opened.
//
// CPU and memory come off the node, from the collector's kubeletstats
// receiver. Restarts, OOM kills, resource limits and replica counts do not:
// they are facts about API objects, not about a running container, and no
// receiver exposes them. Worse, a restart is a lifetime counter, and bucketed
// in the store a counter loses every transition landing on a bucket boundary
// — so the difference has to be taken where the previous sample is
// remembered. The operator takes it, and exports the result over OTLP to the
// collector like any other client.
type MetricsSpec struct {
	// Enabled turns the sampler on. Off means restarts, limits and replica
	// counts go uncollected; the collector's own CPU and memory are
	// unaffected.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// IntervalSeconds between samples of every application container.
	// Below the default the row count climbs faster than the answers
	// improve; above about a minute a short-lived spike falls between two
	// samples and is never seen at all.
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=300
	// +kubebuilder:default=30
	// +optional
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`
}

// TracesSpec configures where applications send their spans.
//
// Traces come from the applications themselves: an application that is not
// instrumented has no spans to send, and nothing the platform can see from
// outside it — not Hubble's L7 data, not the Gateway's access logs — is a
// substitute. What the platform does is remove every other obstacle: the
// collector's OTLP endpoint is always there, and every environment is told
// where it is.
//
// The endpoint is also where the operator exports the metrics it samples, so
// switching this off leaves the collector receiving nothing at all.
type TracesSpec struct {
	// Enabled advertises the endpoint to applications, through the OTLP
	// environment variables every SDK reads. The collector receives either
	// way; off means applications are not told where to send.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// Endpoint is the OTLP/HTTP base URL applications export to, as it is
	// advertised to them. The default is the collector's Service under the
	// conventional release name; the chart always writes its own, because
	// every name it generates carries the release's. Set it by hand only to
	// put something else in front.
	//
	// The Service routes to the collector pod on the caller's own node, so
	// the name is stable but the path is node-local.
	// +kubebuilder:default="http://kitchen-otlp.kitchen-system.svc.cluster.local:4318"
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// ObservabilitySpec configures the telemetry pipeline.
type ObservabilitySpec struct {
	// +optional
	ClickHouse ClickHouseSpec `json:"clickhouse,omitempty"`

	// +optional
	Hubble HubbleSpec `json:"hubble,omitempty"`

	// The empty-object defaults are what make these on by default for an
	// installation that predates them. Structural defaulting only descends
	// into objects that are present, so without one an upgraded Kitchen —
	// whose singleton the chart does not re-apply — would read back with
	// every field at its zero value, and the platform would quietly stop
	// collecting what it had just learned to collect.
	// +kubebuilder:default={}
	// +optional
	Metrics MetricsSpec `json:"metrics,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Traces TracesSpec `json:"traces,omitempty"`
}

// APISpec configures how the operator's API is exposed.
type APISpec struct {
	// ExternalURL is the base URL the operator API (including the git
	// webhook receiver) is reachable at from outside the cluster.
	// Defaults to kitchen.<baseDomain>, under the scheme tls.mode serves:
	// https, or http when tls.mode is none.
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
	// issuer — under the scheme tls.mode serves, so https://<host> unless
	// tls.mode is none. Defaults to auth.<baseDomain>.
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

	// ClusterName is what this cluster is called in the dashboard's status
	// bar. Kitchen owns the cluster it is installed into, so this names the
	// installation as much as the machines: it is what someone with a staging
	// platform and a production one reads to tell which is on screen.
	// Defaults to the first label of the base domain.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`

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

	// The empty-object default is what gives an installation that predates
	// the field a registry at all: structural defaulting only descends into
	// objects that are present.
	// +kubebuilder:default={}
	// +optional
	Registry ImageRegistrySpec `json:"registry,omitempty"`

	// +kubebuilder:default={}
	// +optional
	ScaleToZero ScaleToZeroSpec `json:"scaleToZero,omitempty"`

	// +optional
	Observability ObservabilitySpec `json:"observability,omitempty"`
}

// ComponentStatus reports the runtime health of one platform workload.
//
// This is deliberately about pods, not about reconciliation: the conditions
// say whether the operator could do its job, this says whether what it (or the
// chart) asked for is actually running.
type ComponentStatus struct {
	// Name of the component, taken from app.kubernetes.io/component and
	// falling back to the object name for workloads that set no such label.
	Name string `json:"name"`

	// Kind of workload backing it: Deployment, StatefulSet or DaemonSet.
	Kind string `json:"kind"`

	// Healthy is true when every pod the workload wants is available.
	Healthy bool `json:"healthy"`

	// Available pods right now.
	Available int32 `json:"available"`

	// Desired pods. For a DaemonSet this is however many nodes it selects,
	// so it moves with the cluster rather than with any configured replica
	// count.
	Desired int32 `json:"desired"`

	// Message explains an unhealthy component, and carries the reason from
	// the workload's most recent warning event where there is one. A
	// workload whose pods are rejected at admission reports no pods at all
	// rather than failing pods, so without the event there is nothing to
	// read: see the PodSecurity note in the chart README.
	// +optional
	Message string `json:"message,omitempty"`
}

// ImageRegistryStatus records what the operator did about the bundled
// registry. Connection is the interesting half: it is written once, when the
// operator seeds the Connection, and read forever after as "this has been
// seeded". A Connection someone deletes stays deleted — the seed is a good
// default, not a fixture the platform keeps reinstating.
type ImageRegistryStatus struct {
	// Host the registry is published on.
	// +optional
	Host string `json:"host,omitempty"`

	// Connection the operator seeded, by name. Set once and never cleared
	// while the registry stays enabled.
	// +optional
	Connection string `json:"connection,omitempty"`
}

// KitchenStatus defines the observed state of the platform.
type KitchenStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Externally reachable address of the shared Gateway, once programmed.
	// +optional
	GatewayAddress string `json:"gatewayAddress,omitempty"`

	// Registry reports the bundled image registry: where it is published,
	// and the Connection the operator seeded to point at it.
	// +optional
	Registry *ImageRegistryStatus `json:"registry,omitempty"`

	// Components reports every platform workload the operator can see, in
	// name order, whether or not it is healthy. Something missing from this
	// list was never created; something in it with Healthy false was created
	// and is not running.
	// +optional
	// +listType=map
	// +listMapKey=name
	Components []ComponentStatus `json:"components,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="BaseDomain",type=string,JSONPath=`.spec.baseDomain`
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.status.gatewayAddress`
// +kubebuilder:printcolumn:name="Components",type=string,JSONPath=`.status.conditions[?(@.type=="ComponentsHealthy")].message`
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

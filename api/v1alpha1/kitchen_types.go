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

	// PublicAddresses are the addresses the internet reaches this platform on,
	// where they are not the Gateway's own.
	//
	// The Gateway is programmed with whatever its LoadBalancer implementation
	// assigned it, and on bare metal that is routinely a private address: a
	// router forwards :80 and :443 from 85.195.238.240 to the Gateway at
	// 10.0.10.240, and both of those are correct. Nothing inside the cluster
	// can see the outside of that translation, so this is where it is written
	// down.
	//
	// It is what a published name is compared against — the `dns.mismatch`
	// signal — and it exists for no other reason. Left empty with a private
	// Gateway address, the platform does not know what its own records ought
	// to say and declines to guess: a name that resolves nowhere is still a
	// finding, a name that resolves somewhere is not compared.
	//
	// Addresses, not hostnames: it is the answer a lookup should give.
	// +optional
	PublicAddresses []string `json:"publicAddresses,omitempty"`

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
// in the cluster, and those are the one platform dependency Kitchen's *chart*
// cannot install — which is not the same as the platform not installing them.
// Whether the platform installs them is the `keda` Addon's spec.install; this
// is whether environments idle once something has.
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

// DatabasesSpec is the platform's own Postgres: whether it runs one at all,
// and where the databases it provisions live.
//
// A `postgres` ResourceClaim binds through a Connection, and one of the two
// providers behind that is CloudNativePG — a database in the cluster Kitchen
// was installed into, so that an installation with no SaaS account, or one
// that will not put application data at a third party, has a database at all.
// The provisioner needs nothing from here: it works against any CloudNativePG
// in the cluster, whoever installed it. What this configures is the one thing
// the platform itself decides: which namespace the databases go in. Whether
// the platform installs CloudNativePG, and where that operator runs, is the
// `cloudnative-pg` Addon.
type DatabasesSpec struct {
	// Namespace is where the provisioned database Clusters are created. It is
	// deliberately not a project's application namespace: deleting a project
	// deletes that namespace, and a claim under `deletionPolicy: Retain` has
	// to survive exactly that.
	// +kubebuilder:default=kitchen-databases
	// +optional
	Namespace string `json:"namespace,omitempty"`
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

// ObjectStoreSpec configures the object store the platform runs for itself:
// a single MinIO an application's objectStore claim becomes a bucket in, so
// that a file an application did not build into its image has somewhere to
// go without anyone opening an account at a cloud.
//
// Unlike the registry it is not published on the Gateway. An application
// runs in the cluster, and a Service address is all it needs; the node's
// container runtime is not in the path, so nothing here has to be trusted by
// anything outside. It follows that a bucket in it cannot be publicly
// readable — there is no public to read it — and a claim asking for that is
// refused rather than granted a policy that publishes nothing.
type ObjectStoreSpec struct {
	// Enabled seeds the Connection that points at the bundled store. Off —
	// the default — means an installation brings its own S3-compatible
	// store, which is an s3 Connection someone creates. The chart renders
	// the store itself only when its own objectStore.enabled is set.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Service in the platform namespace the store answers on. The chart
	// writes it as <release>-objectstore.
	// +optional
	Service string `json:"service,omitempty"`

	// Port the store's Service publishes.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=9000
	// +optional
	Port int32 `json:"port,omitempty"`

	// Region the store reports for its buckets. It is a formality an S3
	// client insists on; the bundled store is told the same value.
	// +kubebuilder:default=us-east-1
	// +optional
	Region string `json:"region,omitempty"`

	// SecretRef names the Secret in the platform namespace holding the
	// store's root credential: the keys accessKeyId and secretAccessKey. The
	// chart writes it as <release>-objectstore, generating the secret once
	// and keeping it across upgrades. It is what the operator copies into the
	// seeded Connection's credential, and what mints every claim's own.
	//
	// Left unset it is the conventional release name's, kitchen-objectstore.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
}

// PodSecurityLevel is one of the three Pod Security Standards levels a
// namespace can be labelled with.
// +kubebuilder:validation:Enum=privileged;baseline;restricted
type PodSecurityLevel string

const (
	// PodSecurityPrivileged is unrestricted: every pod the platform builds
	// with is admitted, including the BuildKit builder.
	PodSecurityPrivileged PodSecurityLevel = "privileged"

	// PodSecurityBaseline blocks known privilege escalations. It refuses the
	// Dockerfile builder — rootless BuildKit needs seccomp and AppArmor
	// unconfined, and baseline forbids both — so a platform running here
	// builds with buildpacks alone.
	PodSecurityBaseline PodSecurityLevel = "baseline"

	// PodSecurityRestricted is the hardened profile. Application images
	// choose their own user and capabilities, so most of them are refused by
	// it; it is offered for an installation that vets its own images.
	PodSecurityRestricted PodSecurityLevel = "restricted"
)

// AppNamespacesSpec configures the per-project namespaces the operator
// creates: one per Project, holding that project's builds, its environments'
// workloads and the secrets behind its resource claims.
type AppNamespacesSpec struct {
	// PodSecurity is the Pod Security Standards level enforced on every
	// application namespace, written as the enforce, audit and warn labels
	// when the namespace is created and kept in step afterwards.
	//
	// It is set rather than inherited for the same reason the platform
	// namespace's is: clusters disagree about the default (kind is
	// privileged, Talos is baseline), and the level decides whether the
	// Dockerfile build strategy works at all. Rootless BuildKit needs an
	// unconfined seccomp profile and an unconfined AppArmor profile, both of
	// which baseline forbids, and a Job whose pods Pod Security refuses
	// creates no pod at all — the build simply never starts.
	//
	// Lower it to baseline on an installation that builds with buildpacks
	// alone: the lifecycle asks for neither relaxation.
	//
	// It is deliberately not on the settings screen. Everything else the
	// dashboard can change here is a choice about the platform; this is a
	// fact about the cluster underneath it, decided once at install time
	// beside namespace.podSecurity — and lowering it from a browser would
	// stop every Dockerfile build in the installation with no failure the
	// person who did it would recognise.
	// +kubebuilder:default=privileged
	// +optional
	PodSecurity PodSecurityLevel `json:"podSecurity,omitempty"`
}

// BuildsSpec configures platform-wide build defaults.
type BuildsSpec struct {
	// +kubebuilder:default=auto
	DefaultStrategy BuildStrategy `json:"defaultStrategy,omitempty"`

	// Maximum number of builds running at once.
	//
	// It is half of what the platform's builds can take from the cluster;
	// Resources is the other half, and the two are read together.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	Concurrency int32 `json:"concurrency,omitempty"`

	// Resources is the ceiling one build may take.
	//
	// Concurrency times this is the whole of the platform's build footprint,
	// which is why the two are one decision rather than two settings that
	// happen to be near each other.
	//
	// The empty-object default is what gives an installation that predates
	// the field a ceiling at all: structural defaulting only descends into
	// objects that are present.
	// +kubebuilder:default={}
	// +optional
	Resources BuildResourcesSpec `json:"resources,omitempty"`

	// TimeoutMinutes is the longest one build may run before the platform
	// ends it. It is the ceiling in time that Resources is in capacity, and
	// it exists for the same reason the job needs an end at all: a build
	// that hangs where the reconciler cannot see it — a builder waiting on a
	// registry that never answers — otherwise occupies a build slot forever.
	//
	// An hour is far past anything this platform is meant to build, which is
	// the point: it is a backstop, not a time budget. It is a setting because
	// "meant to" was doing the work in that sentence — a cold-cache monorepo,
	// a Rust workspace or a buildpacks build on a small node can legitimately
	// take longer, and an installation that knows that has to be able to say
	// so.
	//
	// 0 is no deadline at all, which is the installation that would rather
	// end a runaway build by hand than lose an hour's work to a number.
	//
	// It is a pointer because that zero is a setting: an int32 with
	// `omitempty` serializes 0 as an absent field, so the default would be
	// applied back over it and clearing the deadline would be unsayable —
	// through a merge patch it would arrive as a deletion. Unset is the
	// default, which is also what an installation that predates the field
	// gets.
	//
	// It reaches a build as the Job's activeDeadlineSeconds, which is
	// immutable once a Job exists: changing this affects builds started
	// after the change, never the one in flight.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=60
	// +optional
	TimeoutMinutes *int32 `json:"timeoutMinutes,omitempty"`

	// ReleaseRetention is how many Releases each Project keeps. Every
	// successful build leaves one behind, so without a bound a busy project
	// accumulates them — and the images they point at — forever.
	//
	// It is a build setting because a Release is what a build leaves behind.
	// The count is per Project, and a Release an Environment references is
	// never deleted however old it is: the newest ReleaseRetention are kept,
	// and anything still serving is kept on top of them, so rollback targets
	// do not vanish out from under a parked environment.
	//
	// 0 keeps every Release forever.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=10
	ReleaseRetention int32 `json:"releaseRetention,omitempty"`

	// Cache is the layer cache builds reuse between runs.
	//
	// The empty-object default is what gives an installation that predates
	// the field a cache at all: structural defaulting only descends into
	// objects that are present.
	// +kubebuilder:default={}
	// +optional
	Cache BuildCacheSpec `json:"cache,omitempty"`
}

// BuildResourcesSpec is what one build is allowed to take: two Kubernetes
// quantities, written onto every container of the build pod as its request
// and as its limit at once.
//
// It is the operator's and not a project's. A repository declares what its
// application runs with (RepoResources) because that is a fact about the
// application; how much of the cluster a *build* may take is a fact about the
// platform, and a project that could raise its own ceiling would be a project
// that could evict its neighbours.
//
// Request and limit are the same number deliberately. The limit alone would
// bound one build and tell the scheduler nothing, so two builds would still be
// placed where only one fits and the node would meet them both at once — which
// is the situation this exists to end. Equal to the request, the ceiling is
// reserved before the build starts: a node with no room queues the build
// instead of starting it on top of the applications the platform is there to
// serve, and the arithmetic in Concurrency's comment is true rather than
// hopeful.
//
// An empty string is no ceiling for that resource, which is what every
// installation had before this field existed. It is a deliberate setting, not
// the default: the default is the ceiling below, and clearing it is how an
// installation with a build machine's worth of headroom says so.
type BuildResourcesSpec struct {
	// CPU one build may take, as a Kubernetes quantity ("2", "500m").
	//
	// CPU is compressible — a build that wants more is throttled, never
	// killed — so this bounds how much of the node a build can crowd out
	// rather than whether it finishes.
	// +kubebuilder:validation:Pattern=`^$|^[0-9]+(\.[0-9]+)?(m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)?$`
	// +kubebuilder:default="2"
	// +optional
	CPU string `json:"cpu,omitempty"`

	// Memory one build may take, as a Kubernetes quantity ("4Gi", "512Mi").
	//
	// Memory is not compressible: a build that asks for more than this is
	// killed by the kernel, and the platform reports that as a build failure
	// naming the ceiling rather than as an unexplained non-zero exit. Four
	// gibibytes is past where a front-end build of any size peaks and well
	// short of what a single-node cluster can lose without noticing.
	// +kubebuilder:validation:Pattern=`^$|^[0-9]+(\.[0-9]+)?(m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)?$`
	// +kubebuilder:default="4Gi"
	// +optional
	Memory string `json:"memory,omitempty"`
}

// BuildCacheMode is how much of a build BuildKit writes to the cache.
// +kubebuilder:validation:Enum=max;min
type BuildCacheMode string

const (
	// BuildCacheModeMax caches every intermediate layer, so a change part
	// way down a Dockerfile still reuses everything above it. It is what
	// makes a dependency install survive a source change, and it costs
	// registry storage.
	BuildCacheModeMax BuildCacheMode = "max"

	// BuildCacheModeMin caches only the layers of the image that came out.
	// Cheaper to store and much weaker: a multi-stage build's build stage is
	// not in the final image, so nothing about it is reused.
	BuildCacheModeMin BuildCacheMode = "min"
)

// BuildCacheScope is what a cache is shared across — what two builds have to
// agree on to reuse each other's layers.
// +kubebuilder:validation:Enum=project;branch
type BuildCacheScope string

const (
	// BuildCacheScopeProject gives a project one cache, which every branch
	// reads and every build overwrites. One tag per project, so it is
	// bounded without anything having to prune it, and a branch cut from
	// production starts warm.
	BuildCacheScopeProject BuildCacheScope = "project"

	// BuildCacheScopeBranch gives each branch its own, which is a better hit
	// rate on a long-lived branch whose dependencies have moved away from
	// production's — at one cache tag per branch that ever built, which
	// nothing removes.
	BuildCacheScopeBranch BuildCacheScope = "branch"
)

// BuildCacheSpec configures the layer cache, which is the difference between a
// rebuild that reinstalls every dependency and one that does not.
//
// The cache lives in the registry the project already pushes to: BuildKit
// exports a cache manifest beside the image, and the buildpacks lifecycle
// exports a cache image. Neither needs infrastructure the platform does not
// already have, which is why this is on by default.
type BuildCacheSpec struct {
	// Enabled builds against the layer cache. Off means every build starts
	// from nothing, which is slower and the only way to be certain a build
	// reused nothing at all.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Mode is how much of a BuildKit build is cached. It does not reach the
	// buildpacks lifecycle, which has one cache image and no such choice.
	// +kubebuilder:default=max
	// +optional
	Mode BuildCacheMode `json:"mode,omitempty"`

	// Scope is what two builds have to share to reuse each other's layers.
	// +kubebuilder:default=project
	// +optional
	Scope BuildCacheScope `json:"scope,omitempty"`
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

	// ClockSync measures how far the cluster's clocks are from the
	// operator's own, because every timestamp in the store is only worth
	// what the clock that stamped it is worth. The empty-object default is
	// what turns it on for an installation that predates the field, for the
	// reason spelled out above Metrics.
	// +kubebuilder:default={}
	// +optional
	ClockSync ClockSyncSpec `json:"clockSync,omitempty"`
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
	// inside — and directoryURL, where the operator alone reaches the private
	// /kitchen prefix, which is served on a listener no HTTPRoute publishes.
	// Both addresses fall back to the issuer when absent.
	// The chart writes it as <release>-auth. Without it the operator
	// cannot register OAuth clients, so nothing that depends on one — the
	// preview gate, later the oidcClient claims — can be reconciled.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// DatabaseSecretRef names the Secret in the platform namespace holding the
	// connection to the identity provider's own Postgres: the key dsn, or the
	// pieces host, port, database, username and password. The chart writes it
	// as <release>-postgres.
	//
	// The identity provider is handed this connection by the chart and does not
	// need the operator to find it. What does is the backup: the accounts,
	// sessions and OAuth clients in that database are the one part of the
	// platform's state that deliberately does not live in a CRD
	// (docs/SCOPE.md item 9), so they are also the one part an export has to
	// reach into a database for, and a restore has to put back.
	//
	// Left unset it is the conventional release name's, kitchen-postgres —
	// which is what gives an installation whose Kitchen object predates this
	// field a backup with its accounts in it rather than one that quietly
	// leaves them out.
	// +optional
	DatabaseSecretRef *LocalObjectReference `json:"databaseSecretRef,omitempty"`

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

	// Access is who may do what with the platform itself. It belongs on this
	// object because this object is already the platform's configuration and
	// is already edited through PATCH /settings, so granting somebody the
	// operator hat needs no new store and no kubectl.
	//
	// It deliberately carries no `+kubebuilder:default={}`, unlike the fields
	// above it. Those defaults exist so that an installation predating a
	// field still gets the feature; here the absence is the information. An
	// absent operator list means nobody has ever said who the operators are —
	// an installation upgrading into enforcement, where every account today
	// can call every route and so every account read honestly *is* an
	// operator, and the list is seeded from the accounts that exist. An empty
	// list means somebody narrowed it to nobody on purpose, and is left
	// exactly as it is. Defaulting the object into existence would collapse
	// those two into one on the first write, and the upgraded installation
	// would be locked out of its own platform by a minor version bump.
	// +optional
	Access AccessSpec `json:"access,omitempty"`

	// +optional
	Builds BuildsSpec `json:"builds,omitempty"`

	// AppNamespaces configures the namespaces the operator creates for
	// projects. The empty-object default is what gives an installation
	// predating the field the Pod Security level below: structural defaulting
	// only descends into objects that are present.
	// +kubebuilder:default={}
	// +optional
	AppNamespaces AppNamespacesSpec `json:"appNamespaces,omitempty"`

	// The empty-object default is what gives an installation that predates
	// the field a registry at all: structural defaulting only descends into
	// objects that are present.
	// +kubebuilder:default={}
	// +optional
	Registry ImageRegistrySpec `json:"registry,omitempty"`

	// ObjectStore configures the platform's own S3-compatible store, and
	// the Connection the operator seeds to point at it. Off by default.
	// +kubebuilder:default={}
	// +optional
	ObjectStore ObjectStoreSpec `json:"objectStore,omitempty"`

	// +kubebuilder:default={}
	// +optional
	ScaleToZero ScaleToZeroSpec `json:"scaleToZero,omitempty"`

	// Databases configures the platform's own Postgres — where provisioned
	// databases live, and whether the operator installs CloudNativePG itself.
	// The empty-object default is what gives an installation predating the
	// field the namespace defaults: structural defaulting only descends into
	// objects that are present.
	// +kubebuilder:default={}
	// +optional
	Databases DatabasesSpec `json:"databases,omitempty"`

	// +optional
	Observability ObservabilitySpec `json:"observability,omitempty"`

	// Compliance configures the evidence the platform produces about its own
	// operation: the audit log and the attestations attached to artifacts.
	// +optional
	Compliance ComplianceSpec `json:"compliance,omitempty"`

	// Retention is how long each class of what the platform keeps is kept.
	//
	// It is here, at the top of the object, rather than inside
	// `observability` or inside `compliance`, because it spans both and
	// belongs to neither: container logs are telemetry, audit records are
	// evidence, and "how long do you keep it" is one question asked of the
	// whole platform. It deliberately carries no `+kubebuilder:default={}` —
	// an absent block means every class inherits the knob it used to have,
	// which is exactly what an upgraded installation should keep doing.
	// +optional
	Retention RetentionSpec `json:"retention,omitempty"`

	// Residency declares where this installation's data is located — the
	// region or jurisdiction of the cluster itself, in the operator's own
	// vocabulary ("CH", "eu-central-1"). It is the platform-wide default an
	// Environment without a residency of its own inherits in the compliance
	// inventory.
	//
	// It is declared, not observed: Kitchen runs inside the cluster and has
	// no vantage point to verify where the metal is, so this field records
	// the answer the institution is accountable for rather than pretending
	// to measure one. Provisioned resources are the exception — a provider
	// that reports its actual placement gets that recorded on the claim's
	// status, which takes precedence over any declaration.
	// +optional
	Residency string `json:"residency,omitempty"`
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
	//
	// One entry is not a workload at all. The clock-sync check reports
	// itself here under kind `Node`, because a cluster whose clocks disagree
	// is broken in exactly the way this list exists to surface — invisibly,
	// until somebody tries to correlate two timestamps in an incident
	// report — and because this is the list an operator already reads.
	Kind string `json:"kind"`

	// Healthy is true when every pod the workload wants is available.
	Healthy bool `json:"healthy"`

	// Available pods right now. For the clock-sync entry: nodes inside the
	// drift threshold.
	Available int32 `json:"available"`

	// Desired pods. For a DaemonSet this is however many nodes it selects,
	// so it moves with the cluster rather than with any configured replica
	// count. For the clock-sync entry: nodes measured.
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

// ObjectStoreStatus records what the operator did about the bundled object
// store, on the same seed-once terms as ImageRegistryStatus: Connection is
// written when the operator seeds it and read forever after as "this has
// been seeded", so a Connection someone deletes stays deleted.
type ObjectStoreStatus struct {
	// Endpoint the store is reached at inside the cluster.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Connection the operator seeded, by name. Set once and never cleared
	// while the store stays enabled.
	// +optional
	Connection string `json:"connection,omitempty"`
}

// AddonsStatus records which catalogue entries the operator has seeded an
// Addon for, on the same seed-once terms as the registry Connection: written
// when the operator creates one, read forever after as "this has been
// seeded". An Addon somebody deletes stays deleted — an installation that
// would rather run its own KEDA, or no CloudNativePG at all, has to be able
// to end up with the object gone rather than reinstated on the next
// reconcile.
//
// What each entry then *is* — installed by the platform or found already
// serving, at which versions, in which namespace — is the Addon's own status.
// It used to be two blocks here, and they were read by nothing outside the
// two files that wrote them.
type AddonsStatus struct {
	// Seeded names every catalogue entry an Addon has been created for.
	// +optional
	Seeded []string `json:"seeded,omitempty"`
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

	// ObjectStore reports the bundled object store: where it is reached, and
	// the Connection the operator seeded to point at it.
	// +optional
	ObjectStore *ObjectStoreStatus `json:"objectStore,omitempty"`

	// Components reports every platform workload the operator can see, in
	// name order, whether or not it is healthy. Something missing from this
	// list was never created; something in it with Healthy false was created
	// and is not running.
	// +optional
	// +listType=map
	// +listMapKey=name
	Components []ComponentStatus `json:"components,omitempty"`

	// Compliance reports the audit log and the signing identity, so that an
	// installation which believes it is producing evidence can find out that
	// it is not.
	// +optional
	Compliance *ComplianceStatus `json:"compliance,omitempty"`

	// Addons reports which catalogue entries the operator has seeded an Addon
	// for. Each Addon carries its own state; this is only the record that
	// stops a deleted one coming back.
	// +optional
	Addons *AddonsStatus `json:"addons,omitempty"`

	// Retention reports the retention model as it is actually in force, per
	// class, with what each class currently holds and how far back it goes.
	// +optional
	Retention *RetentionStatus `json:"retention,omitempty"`

	// ClockSync reports the last measurement of node clock drift. The
	// *consequence* of drift is a component in the survey above; this is the
	// measurement behind it, including the method, because no number here
	// should be read without the caveat that belongs to it.
	// +optional
	ClockSync *ClockSyncStatus `json:"clockSync,omitempty"`
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

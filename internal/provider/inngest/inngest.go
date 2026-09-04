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

// Package inngest is the contract of the inngest claim type: durable
// background work — retries, sleeps, fan-out, concurrency limits and cron —
// run by Inngest, with the application's worker holding an outbound
// connection to it.
//
// It is shaped after internal/provider/database, and the one place it
// differs in kind is what "provision" means. Inngest has no app to create:
// an app comes into existence the first time a worker connects with its ID,
// and syncs its functions on every connection. Nor does its management API
// mint keys — https://api-docs.inngest.com/api-specs/v2.json lists the
// account's event keys and signing keys (GET /keys/events, GET /keys/signing,
// each filtered by the X-Inngest-Env header) and has no endpoint that
// creates or revokes one. So a Provisioner here *reads* the binding for an
// environment rather than creating it, and the platform's own contribution
// is the environment per preview: Inngest's branch environments
// (https://www.inngest.com/docs/platform/environments) are an isolated event
// stream and function set each, share the account's one branch key pair, and
// are selected by the INNGEST_ENV variable the binding carries.
//
// Two providers ship, and they differ in who runs Inngest:
//
//   - Inngest Cloud, above: the platform reads the account's keys and gives
//     each preview a branch environment.
//   - A server of the claim's own, run in this cluster (selfhosted.go). There
//     the platform mints the keys, because it is the server that checks them,
//     and a preview gets a *server* of its own rather than an environment
//     inside one — which is the answer to the tenancy question #268 left
//     open: a self-hosted Inngest has no environments to separate two
//     previews' event streams with, so nothing separates them but running two
//     servers.
//
// Against Cloud only connect mode is provisioned
// (https://www.inngest.com/docs/setup/connect). In serve mode Inngest calls
// the application over HTTP, which — from the internet — meets the preview
// gate and gets a login page; in connect mode the worker dials out, which
// works behind the gate and, the cost, never crosses the interceptor, so
// nothing reading such a claim scales to zero. A self-hosted server is in
// this cluster and calls the environment's own URL, so serve is allowed
// there and a serve binding holds no pods up. The Declarations say so, and
// the claim's status says which of the two it got.
package inngest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// ErrUnsupportedProvider is returned by Default for providers without an
// implementation.
var ErrUnsupportedProvider = errors.New("unsupported inngest provider")

// ErrUnsatisfiable marks a claim the provider cannot serve as asked: a mode
// it does not provision, or an environment with no event key — which the
// API cannot mint, so retrying without somebody creating one in the Inngest
// dashboard refuses again. The reconciler lands it on the claim as a
// failure with the message, rather than requeueing forever.
var ErrUnsatisfiable = errors.New("claim cannot be satisfied")

// ErrNotReady marks a resource that exists and is not serving yet. Inngest
// Cloud answers within one request and never returns it; it is part of the
// contract for a provisioner that has to wait — a self-hosted server coming
// up — so that the reconciler holds such a claim Pending rather than Failed.
var ErrNotReady = errors.New("not ready yet")

// The keys of the binding Secret, spelled as the environment variables the
// SDKs read (https://www.inngest.com/docs/sdk/environment-variables), so
// that fromResourceClaim names the key and the application reads a
// variable of the same name.
const (
	// KeyEventKey authenticates events the application sends.
	KeyEventKey = "INNGEST_EVENT_KEY"
	// KeySigningKey authenticates the worker's connection and the sync of
	// its functions.
	KeySigningKey = "INNGEST_SIGNING_KEY"
	// KeyEnv selects the Inngest environment. It is what routes a preview to
	// its branch environment: every branch environment shares one key pair,
	// and the variable is how a worker says which one it is. Empty for the
	// production binding, whose keys select their environment themselves.
	KeyEnv = "INNGEST_ENV"
	// KeyBaseURL is where the worker reaches Inngest. Empty on Inngest Cloud
	// on purpose: the SDKs use it for the event API and the REST API alike,
	// and Cloud serves those from two different hosts, so setting it to
	// either would misroute the other. A self-hosted server sets it — it
	// serves both from one address, which is what makes the variable usable
	// at all (https://www.inngest.com/docs/self-hosting).
	KeyBaseURL = "INNGEST_BASE_URL"
	// KeyDev is INNGEST_DEV, which the SDKs read as "which Inngest is this":
	// 0 is cloud mode, where signatures are verified. A self-hosted server
	// is reached in cloud mode with INNGEST_BASE_URL pointed at it, which is
	// exactly what the self-hosting guide says to set, so the self-hosted
	// binding carries "0" and Cloud's carries nothing — unset is cloud mode
	// already.
	KeyDev = "INNGEST_DEV"
	// KeyConnectGatewayURL is the address of the connect gateway, and it is
	// the one key here that is not an SDK variable: the SDKs take the
	// gateway as `gatewayUrl` in code (`connect({ gatewayUrl })`), and a
	// self-hosted gateway is on a port of its own — 8289 — that nothing
	// could otherwise guess. It is spelled in the family's style so that it
	// sits beside the others in `fromResourceClaim`, and it is empty on
	// Inngest Cloud, whose SDKs discover the gateway themselves.
	KeyConnectGatewayURL = "INNGEST_CONNECT_GATEWAY_URL"
)

// Binding is everything a worker needs to connect to Inngest for one
// environment. The fields become the keys of the binding Secret.
type Binding struct {
	EventKey   string
	SigningKey string
	// Env is the INNGEST_ENV value: a branch environment's name, empty for
	// production and empty throughout on a self-hosted server, which has no
	// environments — a preview gets a server instead.
	Env string
	// BaseURL is INNGEST_BASE_URL: empty for Inngest Cloud.
	BaseURL string
	// Dev is INNGEST_DEV: "0" against a self-hosted server, empty for
	// Inngest Cloud.
	Dev string
	// ConnectGatewayURL is where a connect worker's WebSocket goes, for a
	// server whose gateway is not where the SDK would look. Empty for
	// Inngest Cloud.
	ConnectGatewayURL string
}

// SecretData is the binding as the claim's Secret carries it. Every key is
// present whatever its value, so that a variable reading it never fails to
// resolve: an empty INNGEST_ENV or INNGEST_BASE_URL is what the SDKs read as
// unset.
func (b Binding) SecretData() map[string][]byte {
	return map[string][]byte{
		KeyEventKey:          []byte(b.EventKey),
		KeySigningKey:        []byte(b.SigningKey),
		KeyEnv:               []byte(b.Env),
		KeyBaseURL:           []byte(b.BaseURL),
		KeyDev:               []byte(b.Dev),
		KeyConnectGatewayURL: []byte(b.ConnectGatewayURL),
	}
}

// ModeConnect is the mode every provisioner serves: the worker dials out.
const ModeConnect = kitchenv1alpha1.InngestModeConnect

// ModeServe is the mode only a self-hosted server serves: Inngest calls the
// application over HTTP.
const ModeServe = kitchenv1alpha1.InngestModeServe

// DefaultEnvironment is the Inngest environment production binds to when the
// claim names none.
const DefaultEnvironment = kitchenv1alpha1.InngestDefaultEnvironment

// Requirements are what a claim asks for: which app, which environment for
// production, and how the worker reaches Inngest. They come off the claim's
// spec.config, and a provisioner that cannot answer them says so — with
// ErrUnsatisfiable — before it binds anything.
type Requirements struct {
	// App is the Inngest app ID the application's client is created with.
	// The provisioner cannot create it; it is what the app's status is read
	// under.
	App string
	// Environment is the Inngest environment production binds to. Empty
	// means DefaultEnvironment.
	Environment string
	// Mode is ModeConnect or ModeServe, or empty for connect. A provisioner
	// that does not serve the mode refuses it — with ErrUnsatisfiable and a
	// sentence saying which one it does serve — before it binds anything.
	Mode string
	// ServeURL is where Inngest calls the application in serve mode: the URL
	// of the environment this binding is for, plus the claim's serve path.
	//
	// It is the platform's to work out rather than the claim's to state —
	// a preview's hostname carries a pull request number nothing in the
	// repository has heard of — and it is empty in connect mode, and while
	// an environment has no URL yet. A provisioner that has no server to
	// tell ignores it.
	ServeURL string
}

// Instance is the production binding of one app.
type Instance struct {
	// ID is what the claim records as its instance: the app ID at Inngest
	// Cloud, the namespaced name of the server for a self-hosted one. It is
	// what the other operations address it by.
	ID string
	// Name is what the provider calls the thing it made, recorded on the
	// claim so that a resource provisioned under one naming rule keeps its
	// name when the rule changes (internal/provider/naming). Empty at
	// Inngest Cloud, which the platform names nothing at.
	Name string
	// Environment is the Inngest environment the binding selects. Empty for
	// a self-hosted server, which has none.
	Environment string
	Binding     Binding
	// Reason and Message are the provider's own words for the claim's
	// Provisioned condition: the two providers do very different things —
	// one reads keys, the other runs a server — and a condition that said
	// the same about both would be true of neither.
	Reason  string
	Message string
}

// Branch is what one preview environment gets of its own: a branch
// environment at Inngest Cloud, a server of its own when the platform runs
// Inngest itself, and the binding that reaches it.
type Branch struct {
	// ID is the branch's identifier at the provider, opaque; what archiving
	// or teardown addresses.
	ID      string
	Binding Binding
}

// App is what the provider knows about the claim's app: whether a worker
// has connected with its ID yet, and what the last sync said. A claim binds
// before any of it exists — the app is the application's to bring — so this
// is reported on the claim rather than waited for.
type App struct {
	// Found is whether the app exists at the provider at all.
	Found bool
	// Method is how the app last synced, in the provider's vocabulary:
	// CONNECT, SERVE, API or UNSPECIFIED.
	Method string
	// Functions is how many functions the last sync registered.
	Functions int
	// SyncStatus is the last sync's outcome: success, duplicate, pending or
	// error. SyncError carries the provider's words when it is error.
	SyncStatus string
	SyncError  string
	// SDK names the language and version that synced the app.
	SDK string
}

// Provisioner is an Inngest provider bound to one Connection.
//
// Provision and CreateBranch are idempotent by name — a branch environment
// that exists is found (and unarchived) rather than created twice, a server
// that is there is bound rather than made again — and DeleteBranch treats
// already-absent as success.
type Provisioner interface {
	// Provision binds the claim's production Inngest, refusing —
	// ErrUnsatisfiable — what it cannot serve as asked. The resource is what
	// a provisioner that has something to name names it from; one that
	// creates nothing ignores it.
	Provision(ctx context.Context, res naming.Resource, req Requirements) (Instance, error)
	// CreateBranch finds or creates what the named preview environment gets
	// of its own, beside the claim's own instance, and reads its binding.
	// The requirements are that preview's — the same mode and app as the
	// claim, and the preview's own ServeURL.
	CreateBranch(ctx context.Context, instanceID, name string, req Requirements) (Branch, error)
	// DeleteBranch takes it back; already gone is fine.
	DeleteBranch(ctx context.Context, instanceID, branchID string) error
}

// AppReporter is a Provisioner that can be asked whether a worker has
// connected as the claim's app, and what its last sync did.
//
// It is an optional interface because it is a real difference between the
// two implementations rather than a method one of them has to fake: Inngest
// Cloud's v2 API answers GET /apps/{id}, and a self-hosted server publishes
// no app inventory the platform could read — its own dashboard, at the
// binding's INNGEST_BASE_URL, is where the apps and their functions are.
// A claim through a provider that does not report says so on its
// AppConnected condition rather than reporting a guess.
type AppReporter interface {
	Provisioner
	// App reports on the claim's app in the given environment.
	App(ctx context.Context, environment, appID string) (App, error)
}

// Deprovisioner is a Provisioner with something of its own to destroy when
// the claim goes.
//
// Inngest Cloud has nothing: the keys are the account's, the app record is
// the application's, and archiving a branch environment deletes nothing
// there. A self-hosted server is a workload this platform created, and it —
// with the Postgres and the queue behind it — is destroyed with the claim.
// The claim type holds no deletionPolicy for exactly that reason: there is
// no third party holding anything for the policy to choose about.
type Deprovisioner interface {
	Provisioner
	// Deprovision destroys the claim's own instance and everything under it.
	Deprovision(ctx context.Context, instanceID string) error
}

// IdlingProvisioner is a Provisioner that can park a preview's own resource
// while the preview environment reading it is parked, and bring it back when
// the preview wakes (#294) — the same optional interface, for the same
// reason, that internal/provider/cache and internal/provider/database have.
//
// Both operations are idempotent and tolerant of absence: parking something
// already parked, or waking something already awake, is success, and so is
// either against a branch that is no longer there.
type IdlingProvisioner interface {
	Provisioner
	// IdleBranch takes a preview's own Inngest down to no compute. What it
	// has run is on the volume behind it and is untouched: this is a park,
	// not a teardown.
	IdleBranch(ctx context.Context, branchID string) error
	// WakeBranch brings it back. It returns when the server has been asked
	// for, not when it is serving — the claim's own readiness path is what
	// reports that.
	WakeBranch(ctx context.Context, branchID string) error
}

// Options is what a Provisioner is built from. It is a struct rather than an
// argument list because the two implementations need different halves of
// it: Inngest Cloud needs an API key and never touches the cluster, and the
// self-hosted one is the other way round.
type Options struct {
	// Connection the claim provisions through.
	Connection *kitchenv1alpha1.Connection
	// Token from the Connection's credentials secret: an Inngest API key.
	// Empty for the self-hosted provider, which has no credential because it
	// runs the server itself.
	Token string
	// Cluster is the platform's own cluster. Nil for a provider that never
	// touches it; the self-hosted one is refused without it.
	Cluster client.Client
	// Namespace the self-hosted provisioner runs its servers in.
	Namespace string
	// Postgres and Cache are where a self-hosted server keeps its state.
	// They are the platform's own in-cluster provisioners — a CloudNativePG
	// Cluster and a Valkey — resolved by NewSelfHosted when they are nil,
	// and injected by tests.
	Postgres PostgresProvisioner
	Cache    CacheProvisioner
}

// Factory builds a Provisioner for a Connection.
type Factory func(opts Options) (Provisioner, error)

// ProviderCloud is the Connection provider name for Inngest Cloud.
const ProviderCloud = "inngest"

// ProviderSelfHosted is the Connection provider name for an Inngest this
// cluster runs: one server per claim, and one per preview.
//
// It is a provider of its own rather than a mode on the one above, because
// everything a Connection *is* differs between the two. Inngest Cloud is an
// account reached with an API key; this holds no credential at all — it
// provisions with the operator's own account, the way cnpg and valkey do,
// and the CRD's credential rule is written against that set. A declaration
// is per provider too, and these two declare opposite things about previews
// and about idling. A mode inside one provider's config could express
// neither.
const ProviderSelfHosted = "inngestSelfHosted"

// Declarations is what each Inngest provider says about itself before it
// has bound anything, written next to Default so that a provider and its
// declaration are added together.
var Declarations = map[string]contract.Declaration{
	ProviderCloud: {
		Preview: contract.PreviewBranch,
		PreviewNote: "an Inngest branch environment of the preview's own — its own event stream, function " +
			"set and run history, empty rather than a copy of production's, selected by INNGEST_ENV on " +
			"the account's shared branch keys; archived, not deleted, when the preview goes",
		IdleNote: "the branch environment is Inngest's to run and this platform has no lever on it; the " +
			"worker that reads it never idles either, for the reason beside this one",
		KeepsPodsRunning: true,
		WorkloadNote: "a connect worker holds an outbound WebSocket to Inngest's gateway that never crosses " +
			"the interceptor, so nothing can tell when it is idle — and scale to zero is a project-level " +
			"policy, so every environment of the project keeps its pods, previews included",
	},
	ProviderSelfHosted: {
		Preview: contract.PreviewFresh,
		PreviewNote: "an Inngest server of the preview's own, run in this cluster — its own event stream, " +
			"function set and run history, empty rather than a copy of production's, on its own storage; " +
			"created with the preview and destroyed with it, which is what keeps one pull request's events " +
			"from triggering another's functions",
		CanIdle: true,
		IdleNote: "the preview's server is scaled to no pods with it and back up on wake; the volume its runs " +
			"and its queue are on survives the park, so a preview that wakes finds the work it left",
		KeepsPodsRunning: true,
		WorkloadNote: "in connect mode, where the worker holds an outbound WebSocket to the server's gateway " +
			"that never crosses the interceptor — and scale to zero is a project-level policy, so every " +
			"environment of the project keeps its pods. A claim in serve mode declares otherwise: the server " +
			"is in this cluster and calls the environment's own URL, so the call crosses the interceptor and " +
			"wakes it, and the project keeps its scale to zero",
	},
}

// Default resolves the built-in providers.
func Default(opts Options) (Provisioner, error) {
	conn := opts.Connection
	switch conn.Spec.Provider {
	case ProviderCloud:
		apiURL := DefaultCloudAPIURL
		if conn.Spec.Config != nil {
			var cfg struct {
				APIURL string `json:"apiUrl"`
			}
			if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
				return nil, fmt.Errorf("invalid inngest config: %w", err)
			}
			if cfg.APIURL != "" {
				apiURL = cfg.APIURL
			}
		}
		return &Cloud{APIURL: apiURL, Token: opts.Token}, nil
	case ProviderSelfHosted:
		return NewSelfHosted(opts)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProvider, conn.Spec.Provider)
	}
}

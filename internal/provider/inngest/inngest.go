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
// Only connect mode is provisioned (https://www.inngest.com/docs/setup/connect).
// In serve mode Inngest calls the application over HTTP, which meets the
// preview gate and gets a login page; in connect mode the worker dials out,
// which works behind the gate and — the cost — never crosses the interceptor,
// so nothing reading this claim scales to zero. The Declaration says so.
package inngest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
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
	// either would misroute the other. A self-hosted server sets it.
	KeyBaseURL = "INNGEST_BASE_URL"
)

// Binding is everything a worker needs to connect to Inngest for one
// environment. The fields become the keys of the binding Secret.
type Binding struct {
	EventKey   string
	SigningKey string
	// Env is the INNGEST_ENV value: a branch environment's name, empty for
	// production.
	Env string
	// BaseURL is INNGEST_BASE_URL: empty for Inngest Cloud.
	BaseURL string
}

// SecretData is the binding as the claim's Secret carries it. Every key is
// present whatever its value, so that a variable reading it never fails to
// resolve: an empty INNGEST_ENV or INNGEST_BASE_URL is what the SDKs read as
// unset.
func (b Binding) SecretData() map[string][]byte {
	return map[string][]byte{
		KeyEventKey:   []byte(b.EventKey),
		KeySigningKey: []byte(b.SigningKey),
		KeyEnv:        []byte(b.Env),
		KeyBaseURL:    []byte(b.BaseURL),
	}
}

// ModeConnect is the one mode a Provisioner serves: the worker dials out.
const ModeConnect = kitchenv1alpha1.InngestModeConnect

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
	// Mode is ModeConnect or empty. Anything else is refused.
	Mode string
}

// Instance is the production binding of one app.
type Instance struct {
	// ID is what the claim records as its instance: the app ID, which is
	// what the other operations look the app up under.
	ID string
	// Environment is the Inngest environment the binding selects.
	Environment string
	Binding     Binding
}

// Branch is one preview's own Inngest environment and the binding that
// selects it.
type Branch struct {
	// ID is the environment's ID at the provider, opaque; what archiving
	// addresses.
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
// that exists is found (and unarchived) rather than created twice — and
// DeleteBranch treats already-absent as success. There is no Deprovision:
// nothing an app claim binds is the platform's to destroy. The keys are the
// account's, the app record is the application's, and archiving a branch
// environment deletes nothing at Inngest.
type Provisioner interface {
	// Provision reads the binding for the claim's production environment,
	// refusing — ErrUnsatisfiable — what it cannot serve as asked.
	Provision(ctx context.Context, req Requirements) (Instance, error)
	// CreateBranch finds or creates the branch environment of the given
	// name, unarchives it if the provider archived it, and reads its
	// binding.
	CreateBranch(ctx context.Context, name string) (Branch, error)
	// DeleteBranch archives a branch environment; already gone is fine.
	DeleteBranch(ctx context.Context, branchID string) error
	// App reports on the claim's app in the given environment.
	App(ctx context.Context, environment, appID string) (App, error)
}

// Options is what a Provisioner is built from.
type Options struct {
	// Connection the claim provisions through.
	Connection *kitchenv1alpha1.Connection
	// Token from the Connection's credentials secret: an Inngest API key.
	Token string
}

// Factory builds a Provisioner for a Connection.
type Factory func(opts Options) (Provisioner, error)

// ProviderCloud is the Connection provider name for Inngest Cloud — the one
// implementation that ships. A self-hosted Inngest is a different provider
// with a different tenancy story, and is not this one.
const ProviderCloud = "inngest"

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
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProvider, conn.Spec.Provider)
	}
}

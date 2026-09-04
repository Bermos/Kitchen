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

package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/inngest"
)

// The inngest half of the claim API: durable background work from Inngest,
// through a backgroundJobs-capable Connection. The block names the app, the
// Inngest environment production binds to, the mode and — in serve mode —
// where the application's handler is mounted.
//
// Two of those depend on which Inngest the connection is, and both are
// refused here rather than left to fail on the claim: serve mode against
// Inngest Cloud, whose call would come from the internet and meet a
// protected preview's gate, and an Inngest environment by name against a
// self-hosted server, which has no environments — a preview there gets a
// server of its own instead.

// inngestClaimShaper is the claimShaper for type inngest.
type inngestClaimShaper struct{}

func (inngestClaimShaper) fields() []claimField {
	return []claimField{
		{
			name:  "inngest",
			set:   func(body *createClaimRequest) bool { return body.Inngest != nil },
			lacks: "no app, no Inngest environment and no worker mode",
		},
	}
}

// config checks the shape of what an inngest claim asks for and answers it
// as the reconciler reads it, nil when the claim takes every default.
func (inngestClaimShaper) config(
	w http.ResponseWriter,
	body *createClaimRequest,
	_ *kitchenv1alpha1.Project,
	provider string,
) (*runtime.RawExtension, bool) {
	if body.Inngest == nil {
		return nil, true
	}
	selfHosted := provider == inngest.ProviderSelfHosted
	cfg := kitchenv1alpha1.InngestConfig{
		App:         strings.TrimSpace(body.Inngest.App),
		Environment: strings.TrimSpace(body.Inngest.Environment),
		Mode:        strings.TrimSpace(body.Inngest.Mode),
		ServePath:   strings.TrimSpace(body.Inngest.ServePath),
	}
	if cfg.App != "" {
		if errs := validation.IsDNS1123Subdomain(cfg.App); len(errs) > 0 {
			badRequest(w, "inngest.app is the app ID the application's Inngest client is created with — "+
				"lowercase letters, digits, '-' and '.' (got %q): %s", body.Inngest.App, strings.Join(errs, "; "))
			return nil, false
		}
	}
	if cfg.Environment != "" && (len(cfg.Environment) > 64 || strings.ContainsAny(cfg.Environment, " \t\n")) {
		badRequest(w, "inngest.environment names an Inngest environment — production, or a custom one created "+
			"in the Inngest dashboard — in at most 64 characters and no whitespace (got %q)", body.Inngest.Environment)
		return nil, false
	}
	if cfg.Environment != "" && cfg.Environment != kitchenv1alpha1.InngestDefaultEnvironment && selfHosted {
		badRequest(w, "inngest.environment is refused through a %s connection (got %q): a self-hosted server has "+
			"no environments to select — that is what Inngest Cloud's branch environments are, and it is why a "+
			"preview here gets a server of its own instead", inngest.ProviderSelfHosted, body.Inngest.Environment)
		return nil, false
	}
	switch {
	case cfg.Mode == "" || cfg.Mode == inngest.ModeConnect:
	case cfg.Mode == inngest.ModeServe && selfHosted:
	case cfg.Mode == inngest.ModeServe:
		badRequest(w, "inngest.mode serve is refused through a %s connection: Inngest Cloud would call the "+
			"application over HTTP from the internet, which a protected preview answers with a login page. "+
			"Use connect, where the worker dials out — or claim through a %s connection, whose server is in "+
			"this cluster and calls the environment's own URL", inngest.ProviderCloud, inngest.ProviderSelfHosted)
		return nil, false
	default:
		badRequest(w, "inngest.mode must be %s (got %q): how the worker and Inngest reach each other",
			strings.Join(kitchenv1alpha1.InngestModes, " or "), body.Inngest.Mode)
		return nil, false
	}
	if cfg.ServePath != "" && !strings.HasPrefix(cfg.ServePath, "/") {
		badRequest(w, "inngest.servePath is where the application's Inngest handler is mounted and starts with "+
			"a '/' (got %q), e.g. %s", body.Inngest.ServePath, kitchenv1alpha1.InngestDefaultServePath)
		return nil, false
	}
	if cfg.App == "" && cfg.Environment == "" && cfg.Mode == "" && cfg.ServePath == "" {
		return nil, true
	}
	raw, err := json.Marshal(struct {
		Inngest kitchenv1alpha1.InngestConfig `json:"inngest"`
	}{cfg})
	if err != nil {
		badRequest(w, "%s", err.Error())
		return nil, false
	}
	return &runtime.RawExtension{Raw: raw}, true
}

// view answers what the claim binds, with the defaults filled in: the app
// the worker is expected to connect as, the environment production reads,
// and the mode — so that a claim never answers "unset" to a question it
// does have an answer to.
func (inngestClaimShaper) view(claim *kitchenv1alpha1.ResourceClaim, view *claimView) {
	cfg := claim.Inngest()
	view.Inngest = &claimInngestView{
		App:         cfg.App,
		Environment: cfg.Environment,
		Mode:        cfg.Mode,
		ServePath:   cfg.ServePath,
	}
}

// deletionOutcome says what goes, and the two providers differ in the whole
// of it: at Inngest Cloud nothing the platform could destroy is involved,
// and a self-hosted server is a workload this platform runs, so it goes with
// the claim. The claim's own instance id is what tells them apart — a
// self-hosted one is a namespaced object name, Cloud's is the app ID — which
// is the only handle a deleted claim's view has.
func (inngestClaimShaper) deletionOutcome(claim *kitchenv1alpha1.ResourceClaim) string {
	if claim != nil && strings.Contains(claim.Status.InstanceID, "/") {
		return "the binding is removed and the Inngest server this claim's environments use — with the " +
			"Postgres and the queue behind it, and every event and function run they hold — is destroyed " +
			"with the claim"
	}
	return "the binding is removed and the preview branch environments are archived; the app record and the " +
		"environment's keys stay at Inngest"
}

// claimInngestView is an inngest claim's configuration as the API answers
// it, defaults filled in.
type claimInngestView struct {
	// App is the Inngest app ID the worker connects as.
	App string `json:"app"`
	// Environment is the Inngest environment production binds to.
	Environment string `json:"environment"`
	// Mode is how the worker and Inngest reach each other: connect or serve.
	Mode string `json:"mode"`
	// ServePath is where the application's Inngest handler is mounted, for
	// a claim in serve mode.
	ServePath string `json:"servePath"`
}

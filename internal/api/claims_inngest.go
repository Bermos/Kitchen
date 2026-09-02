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
// Inngest environment production binds to, and the mode — and only connect
// is a mode the platform provisions, so anything else is refused here with
// the reason, before there is a claim to fail.

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
) (*runtime.RawExtension, bool) {
	if body.Inngest == nil {
		return nil, true
	}
	cfg := kitchenv1alpha1.InngestConfig{
		App:         strings.TrimSpace(body.Inngest.App),
		Environment: strings.TrimSpace(body.Inngest.Environment),
		Mode:        strings.TrimSpace(body.Inngest.Mode),
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
	if cfg.Mode != "" && cfg.Mode != inngest.ModeConnect {
		badRequest(w, "inngest.mode must be connect (got %q): the worker holds an outbound connection to Inngest, "+
			"which is what works behind a protected preview's gate. In serve mode Inngest would call the "+
			"application over HTTP and meet a login page, so the platform does not provision it", body.Inngest.Mode)
		return nil, false
	}
	if cfg.App == "" && cfg.Environment == "" && cfg.Mode == "" {
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
	view.Inngest = &claimInngestView{App: cfg.App, Environment: cfg.Environment, Mode: cfg.Mode}
}

func (inngestClaimShaper) deletionOutcome(*kitchenv1alpha1.ResourceClaim) string {
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
	// Mode is how the worker reaches Inngest: connect.
	Mode string `json:"mode"`
}

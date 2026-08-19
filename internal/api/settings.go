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
	"context"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/ui"
)

// The settings endpoints are the Kitchen singleton as the UI sees it: the
// platform's runtime configuration, which is a custom resource precisely so
// it can be edited from here rather than through another `helm upgrade`.

type settingsView struct {
	BaseDomain       string `json:"baseDomain"`
	APIExternalURL   string `json:"apiExternalURL,omitempty"`
	GatewayClassName string `json:"gatewayClassName,omitempty"`
	AuthEnabled      bool   `json:"authEnabled"`
	AuthHost         string `json:"authHost,omitempty"`
	BuildStrategy    string `json:"buildStrategy,omitempty"`
	BuildConcurrency int32  `json:"buildConcurrency,omitempty"`
	// No omitempty: 0 is a setting here — keep every release — not an absent
	// one, and the dashboard has to be able to tell the two apart.
	ReleaseRetention int32           `json:"releaseRetention"`
	LogRetentionDays int32           `json:"logRetentionDays,omitempty"`
	GatewayAddress   string          `json:"gatewayAddress,omitempty"`
	Conditions       []conditionView `json:"conditions,omitempty"`
}

func newSettingsView(kitchen *kitchenv1alpha1.Kitchen) settingsView {
	view := settingsView{
		BaseDomain:       kitchen.Spec.BaseDomain,
		APIExternalURL:   externalURL(kitchen),
		GatewayClassName: kitchen.Spec.Ingress.GatewayClassName,
		AuthEnabled:      kitchen.Spec.Auth.Enabled,
		BuildStrategy:    string(kitchen.Spec.Builds.DefaultStrategy),
		BuildConcurrency: kitchen.Spec.Builds.Concurrency,
		ReleaseRetention: kitchen.Spec.Builds.ReleaseRetention,
		LogRetentionDays: kitchen.Spec.Observability.ClickHouse.RetentionDays,
		GatewayAddress:   kitchen.Status.GatewayAddress,
		Conditions:       conditionViews(kitchen.Status.Conditions),
	}
	if cfg, err := issuerFor(kitchen); err == nil {
		view.AuthHost = cfg.issuer
	}
	return view
}

func (s *Server) getKitchen(req *http.Request) (*kitchenv1alpha1.Kitchen, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	err := s.Client.Get(req.Context(), types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen)
	return kitchen, err
}

func (s *Server) getSettings(w http.ResponseWriter, req *http.Request) {
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newSettingsView(kitchen))
}

// patchSettingsRequest carries the fields the UI can change. Everything else
// on the singleton — the base domain, the issuer, the ingress — shapes URLs
// and credentials the platform has already handed out, so changing those
// stays a deliberate kubectl operation for now.
type patchSettingsRequest struct {
	BuildStrategy    *string `json:"buildStrategy"`
	BuildConcurrency *int32  `json:"buildConcurrency"`
	ReleaseRetention *int32  `json:"releaseRetention"`
	LogRetentionDays *int32  `json:"logRetentionDays"`
}

func (s *Server) patchSettings(w http.ResponseWriter, req *http.Request) {
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := patchSettingsRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	patch := client.MergeFrom(kitchen.DeepCopy())
	if body.BuildStrategy != nil {
		strategy := kitchenv1alpha1.BuildStrategy(strings.TrimSpace(*body.BuildStrategy))
		switch strategy {
		case kitchenv1alpha1.BuildStrategyAuto, kitchenv1alpha1.BuildStrategyDockerfile, kitchenv1alpha1.BuildStrategyBuildpacks:
			kitchen.Spec.Builds.DefaultStrategy = strategy
		default:
			badRequest(w, "buildStrategy must be auto, dockerfile or buildpacks (got %q)", *body.BuildStrategy)
			return
		}
	}
	if body.BuildConcurrency != nil {
		if *body.BuildConcurrency < 1 {
			badRequest(w, "buildConcurrency must be at least 1 (got %d)", *body.BuildConcurrency)
			return
		}
		kitchen.Spec.Builds.Concurrency = *body.BuildConcurrency
	}
	if body.ReleaseRetention != nil {
		// Zero is the one setting here that means "no bound": every release a
		// project ever built is kept, which is what the platform did before
		// there was a count at all.
		if *body.ReleaseRetention < 0 {
			badRequest(w, "releaseRetention cannot be negative (got %d); 0 keeps every release", *body.ReleaseRetention)
			return
		}
		kitchen.Spec.Builds.ReleaseRetention = *body.ReleaseRetention
	}
	if body.LogRetentionDays != nil {
		if *body.LogRetentionDays < 1 {
			badRequest(w, "logRetentionDays must be at least 1 (got %d)", *body.LogRetentionDays)
			return
		}
		kitchen.Spec.Observability.ClickHouse.RetentionDays = *body.LogRetentionDays
	}

	// Platform settings are the operator's own configuration, so a change to
	// them is recorded like any other: what moved, and who moved it.
	if !s.recorded(w, req, audit.Transition{
		Object:    kitchen,
		Kind:      audit.KindKitchen,
		Operation: clickhouse.AuditUpdate,
		Reason:    "platform settings were changed",
		Details:   map[string]any{"fields": changedSettingsFields(body)},
	}) {
		return
	}
	if err := s.Client.Patch(req.Context(), kitchen, patch); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(req.Context())
	s.log().Info("platform settings changed through the api", "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newSettingsView(kitchen))
}

// UIConfig resolves the dashboard's bootstrap configuration off the Kitchen
// object, with the same derivations the API's own token check uses — so the
// login the UI starts is always aimed at the issuer the API will accept a
// token from.
func UIConfig(c client.Client, clientID string) func(ctx context.Context) (ui.Config, error) {
	return func(ctx context.Context) (ui.Config, error) {
		kitchen := &kitchenv1alpha1.Kitchen{}
		if err := c.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
			return ui.Config{}, err
		}
		cfg := ui.Config{ClientID: clientID, APIURL: externalURL(kitchen)}
		if issuer, err := issuerFor(kitchen); err == nil {
			cfg.Issuer = issuer.issuer
		}
		return cfg, nil
	}
}

// changedSettingsFields names the settings a PATCH carried, for the audit
// record's details.
func changedSettingsFields(body patchSettingsRequest) []string {
	fields := []string{}
	for _, field := range []struct {
		name    string
		changed bool
	}{
		{"buildStrategy", body.BuildStrategy != nil},
		{"buildConcurrency", body.BuildConcurrency != nil},
		{"releaseRetention", body.ReleaseRetention != nil},
		{"logRetentionDays", body.LogRetentionDays != nil},
	} {
		if field.changed {
			fields = append(fields, field.name)
		}
	}
	return fields
}

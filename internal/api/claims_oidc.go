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
	"net/url"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The oidcClient half of the claim API: an OAuth client at the platform's
// own identity provider, which has no Connection to name because the client
// is registered at the issuer the platform is already configured with, by
// the operator's own credential.

// oidcClaimShaper is the claimShaper for type oidcClient.
type oidcClaimShaper struct{}

func (oidcClaimShaper) fields() []claimField {
	return []claimField{
		{
			name:  "callbackPaths",
			set:   func(body *createClaimRequest) bool { return len(body.CallbackPaths) > 0 },
			lacks: "no redirect list",
		},
		{
			name:  "redirectURIs",
			set:   func(body *createClaimRequest) bool { return len(body.RedirectURIs) > 0 },
			lacks: "no redirect list",
		},
		{
			name:  "scopes",
			set:   func(body *createClaimRequest) bool { return len(body.Scopes) > 0 },
			lacks: "no scopes to ask an issuer for",
		},
	}
}

// config validates the client's registration details and answers them as
// the reconciler reads them, nil when the claim takes every default.
func (oidcClaimShaper) config(
	w http.ResponseWriter,
	body *createClaimRequest,
	_ *kitchenv1alpha1.Project,
	_ string,
) (*runtime.RawExtension, bool) {
	cfg := kitchenv1alpha1.OIDCClientConfig{}
	for _, path := range body.CallbackPaths {
		path = strings.TrimSpace(path)
		if !strings.HasPrefix(path, "/") {
			badRequest(w, "callbackPaths are paths, not URLs, and start with '/' (got %q): they are appended "+
				"to every URL the project's environments are reachable at, which is what keeps previews "+
				"working without anyone writing their URLs down", path)
			return nil, false
		}
		cfg.CallbackPaths = append(cfg.CallbackPaths, path)
	}
	for _, uri := range body.RedirectURIs {
		uri = strings.TrimSpace(uri)
		parsed, err := url.Parse(uri)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") {
			badRequest(w, "redirectURIs are absolute http(s) URLs (got %q): they are registered verbatim, "+
				"for the addresses the platform does not own", uri)
			return nil, false
		}
		cfg.RedirectURIs = append(cfg.RedirectURIs, uri)
	}
	for _, scope := range body.Scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || strings.ContainsAny(scope, " \t") {
			badRequest(w, "scopes are single words (got %q)", scope)
			return nil, false
		}
		cfg.Scopes = append(cfg.Scopes, scope)
	}
	if len(cfg.Scopes) > 0 && !slices.Contains(cfg.Scopes, "openid") {
		badRequest(w, "scopes must include openid: without it the issuer answers with an OAuth token and no "+
			"identity, which is not what a sign-in needs")
		return nil, false
	}
	if len(cfg.CallbackPaths) == 0 && len(cfg.RedirectURIs) == 0 && len(cfg.Scopes) == 0 {
		return nil, true
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return nil, false
	}
	return &runtime.RawExtension{Raw: raw}, true
}

// view answers what the claim asked to be registered with, with the
// platform's defaults filled in, so that a claim never answers "unset" to a
// question it does have an answer to.
func (oidcClaimShaper) view(claim *kitchenv1alpha1.ResourceClaim, view *claimView) {
	cfg := claim.OIDCClient()
	view.CallbackPaths = cfg.CallbackPaths
	view.Scopes = cfg.Scopes
}

func (oidcClaimShaper) deletionOutcome(*kitchenv1alpha1.ResourceClaim) string {
	return "the OAuth client is deregistered"
}

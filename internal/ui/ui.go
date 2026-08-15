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

// Package ui serves the Kitchen dashboard: the Vue SPA in ui/, embedded into
// the operator binary at image build time so the chart has no second image to
// ship. It is served unauthenticated — the files are the same for everyone and
// hold no state; everything with state is behind the API's token check.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/Bermos/Kitchen/internal/version"
)

// The image build copies ui/dist in here before compiling. A source checkout
// has only the placeholder below, so `go build` and `make run` work without a
// Node toolchain — the dashboard then says how to get itself built.
//
//go:embed all:dist
var distFS embed.FS

// Config is what the SPA needs to know before anyone is signed in: where to
// sign in, as whom, and for what audience. It is public by nature — the same
// values are visible in every login redirect.
type Config struct {
	// Issuer is the identity provider's URL, empty when auth is disabled.
	Issuer string `json:"issuer"`
	// ClientID the UI authenticates as.
	ClientID string `json:"clientId"`
	// APIURL is the operator API's external base URL — also the token
	// audience the UI must ask for (`resource=`).
	APIURL string `json:"apiURL"`
	// Version is the release this operator was built from, which is the
	// release the whole platform is on: one tag publishes the chart and both
	// images. Handler fills it in — it is a fact about the binary, not about
	// the cluster, so no caller has to supply it.
	Version string `json:"version"`
}

// placeholder is served when the binary was built without the SPA — a source
// build outside the image. Everything still routes; it just says what to do.
const placeholder = `<!doctype html>
<meta charset="utf-8">
<title>Kitchen</title>
<style>body{font-family:system-ui;background:#0b0c0e;color:#b4b9c1;display:grid;place-items:center;min-height:100vh;margin:0}code{color:#7da6ff}</style>
<div><h1>Kitchen</h1><p>This operator binary was built without the dashboard.
Build it with <code>make ui-build</code> (or use the released image), or run the UI
separately with <code>cd ui &amp;&amp; npm run dev</code>.</p></div>`

// Handler serves the dashboard: static assets, /config.json, and the SPA
// fallback that makes deep links like /projects/shop land in index.html.
// config is resolved per request so pointing the platform at another issuer
// needs no operator restart.
func Handler(config func(ctx context.Context) (Config, error)) http.Handler {
	dist, err := fs.Sub(distFS, "dist")
	if err != nil {
		// The embed pattern guarantees the directory exists; nothing at
		// runtime can make this fail.
		panic(err)
	}
	fileServer := http.FileServerFS(dist)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "the dashboard only serves GET", http.StatusMethodNotAllowed)
			return
		}

		if req.URL.Path == "/config.json" {
			cfg, err := config(req.Context())
			if err != nil {
				http.Error(w, "the platform is not configured yet", http.StatusServiceUnavailable)
				return
			}
			cfg.Version = version.Version
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(cfg)
			return
		}

		name := strings.TrimPrefix(path.Clean(req.URL.Path), "/")
		if name != "" && name != "index.html" {
			if file, err := dist.Open(name); err == nil {
				_ = file.Close()
				// Vite fingerprints everything under assets/, so those may
				// be cached forever; the rest changes with the build.
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, req)
				return
			}
		}

		// Everything else is a client-side route: serve the app shell and
		// let the router take it from there. Never cached, so a deploy of
		// the operator is a deploy of the UI.
		w.Header().Set("Cache-Control", "no-store")
		index, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(placeholder))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}

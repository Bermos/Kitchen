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
	"net/url"
	"path"
	"slices"
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

// permissionsPolicy switches off the powerful features the dashboard has no
// use for. It is a list of denials rather than an allowlist: a feature nobody
// asks for costs nothing to refuse, and a screen that one day wants the
// clipboard or fullscreen is not on it.
const permissionsPolicy = "accelerometer=(), camera=(), display-capture=(), geolocation=(), " +
	"gyroscope=(), magnetometer=(), microphone=(), midi=(), payment=(), usb=(), " +
	"xr-spatial-tracking=()"

// hstsMaxAge is a year, the value the header is only worth setting at. It goes
// out with includeSubDomains and without preload: preload is a submission to a
// list baked into browsers and an undertaking about a domain Kitchen does not
// own, which is the installation's decision to make rather than ours.
const hstsMaxAge = "max-age=31536000; includeSubDomains"

// contentSecurityPolicy is the policy the dashboard is served under, built
// around what it is about to be told to talk to.
//
// The dashboard is a Vite build: one module script and one stylesheet, both
// fingerprinted under assets/, no inline script, no CDN, and its icons and
// fonts are bundled precisely so nothing is fetched from the internet at
// runtime. So `default-src 'self'` costs it nothing and `connect-src` can name
// its three destinations exactly — itself, the API and the identity provider,
// the last two read off the same Config that is served at /config.json rather
// than spelled out a second time here.
//
// `style-src` is the one relaxation, and it is a real need rather than a
// hedge: the colour-mode switch suppresses its own transition by inserting a
// <style> element for the duration of the swap, which `'self'` alone refuses.
func contentSecurityPolicy(cfg Config) string {
	connect := []string{"'self'"}
	for _, configured := range []string{cfg.APIURL, cfg.Issuer} {
		if origin := originOf(configured); origin != "" && !slices.Contains(connect, origin) {
			connect = append(connect, origin)
		}
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src " + strings.Join(connect, " "),
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"form-action 'self'",
	}, "; ")
}

// originOf reduces a configured URL to the scheme://host a CSP source list
// names. Anything that is not an absolute http(s) URL — an empty field on an
// installation that has not been configured yet, or a value nobody validated —
// yields nothing rather than a source expression, since a malformed one would
// be dropped by the browser along with the rest of the directive.
func originOf(configured string) string {
	parsed, err := url.Parse(configured)
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// setSecurityHeaders writes the browser-side hardening onto every dashboard
// response — the app shell, its assets and /config.json alike, since all three
// are the same origin and an omission on any one of them is an omission.
//
// Strict-Transport-Security is the only conditional one, and the condition is
// the platform's own scheme rather than the request's: the operator is reached
// through the shared Gateway, which terminates TLS, so a request arriving here
// is plain HTTP whatever the outside world sees. `tls.mode: none` publishes
// http:// URLs (TLSMode.Scheme()), and pinning HSTS on a host that has no
// certificate would lock the installation out of its own dashboard.
func setSecurityHeaders(header http.Header, cfg Config) {
	header.Set("Content-Security-Policy", contentSecurityPolicy(cfg))
	header.Set("X-Content-Type-Options", "nosniff")
	// frame-ancestors above says the same thing to anything from the last
	// decade; this is for what does not read it.
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Permissions-Policy", permissionsPolicy)
	if strings.HasPrefix(cfg.APIURL, "https://") {
		header.Set("Strict-Transport-Security", hstsMaxAge)
	}
}

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
		// The config is resolved for every response, not only for
		// /config.json, because the policy names the origins the dashboard
		// is about to be told to talk to. A resolver that cannot answer yet
		// leaves the zero Config, whose policy is 'self' and nothing else —
		// the stricter of the two, and an installation with no singleton is
		// serving no sign-in for it to get in the way of.
		cfg, cfgErr := config(req.Context())
		if cfgErr != nil {
			cfg = Config{}
		}
		setSecurityHeaders(w.Header(), cfg)

		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "the dashboard only serves GET", http.StatusMethodNotAllowed)
			return
		}

		if req.URL.Path == "/config.json" {
			if cfgErr != nil {
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

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

// Package previewgate is the forward-auth gate protected preview
// environments are served through: a small reverse proxy that turns an
// anonymous request into a platform login and only then forwards it to the
// application. The application never learns any of this happened.
//
// The gate sits in the request path rather than answering authorization
// subrequests, because Gateway API has no external-authorization filter and
// Cilium's implementation exposes none of Envoy's. Routing a protected
// Environment through a backend is something every Gateway API
// implementation can do, so this stays portable — see docs/AUTH.md.
package previewgate

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Paths the gate reserves on every host it serves. Everything else belongs to
// the application behind it.
const (
	// PathPrefix is reserved on protected hosts; an application cannot use it.
	PathPrefix = "/_kitchen/gate/"

	// StartPath begins a login. It runs on the gate's own host, so the flow
	// cookie it sets is there when the identity provider redirects back.
	StartPath = PathPrefix + "start"

	// CallbackPath is the OAuth redirect URI: the one URL registered with the
	// identity provider, whatever preview the visitor started from.
	CallbackPath = PathPrefix + "callback"

	// SessionPath lands the finished login back on the preview's own host,
	// which is the only place a cookie for that host can be set.
	SessionPath = PathPrefix + "session"

	// SignOutPath drops the session cookie for the host it is called on.
	SignOutPath = PathPrefix + "signout"
)

// UpstreamHeader carries the application the request belongs to. The
// Environment reconciler sets it on the HTTPRoute with a RequestHeaderModifier
// filter, which overwrites whatever the client sent, and the gate refuses
// anything that is not an in-cluster Service address.
const UpstreamHeader = "X-Kitchen-Upstream"

// Headers the gate hands the application about the visitor. Inbound copies
// are dropped, so an application can trust them exactly as far as it trusts
// the platform.
const (
	UserHeader  = "X-Kitchen-User"
	EmailHeader = "X-Kitchen-User-Email"
)

// CookieName is the gate's session cookie. It is host-scoped — no Domain
// attribute — so a session for one preview is not sent to any other host on
// the base domain, least of all to the applications themselves.
const CookieName = "kitchen_preview_session"

// FlowCookieName holds the in-flight login (the state nonce, the PKCE
// verifier and where to return to). It only ever exists on the gate's own
// host, and only for the seconds a login takes.
const FlowCookieName = "kitchen_preview_flow"

// Schemes a URL in the gate's configuration may use.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// Defaults chosen so a Deployment only has to be told the things that are
// genuinely per-installation.
const (
	DefaultAddr       = ":8080"
	DefaultHealthAddr = ":8081"
	DefaultSessionTTL = 8 * time.Hour
	// DefaultScopes is the minimum that identifies a person: who they are and
	// how to address them in a log line.
	DefaultScopes = "openid profile email"

	// loginTTL bounds how long a login may take from the first redirect to
	// the callback.
	loginTTL = 10 * time.Minute

	// handoffTTL bounds the last hop, from the callback to the preview host
	// the session cookie is set on.
	handoffTTL = time.Minute

	// minSecretLength keeps a signing key from being guessable. Everything
	// the gate trusts — sessions, handoffs, OAuth state — is signed with it.
	minSecretLength = 32
)

// Config is everything the gate needs to run. It comes from the environment
// only: the chart renders it from values and the secrets the operator writes.
type Config struct {
	// Addr the proxy listens on.
	Addr string

	// HealthAddr serves the probes. It is a second listener on purpose: the
	// proxy's own port belongs to the applications behind it, and an
	// application is entitled to its own /healthz.
	HealthAddr string

	// Issuer is the public OIDC issuer, where browsers are sent to sign in.
	Issuer string

	// IssuerInternalURL is where the gate itself reaches the issuer, for
	// clusters that cannot resolve their own base domain from the inside.
	// Empty means the public issuer.
	IssuerInternalURL string

	// ClientID and ClientSecret are the gate's OAuth client, registered by
	// the operator with dynamic client registration.
	ClientID     string
	ClientSecret string

	// CallbackURL is the redirect URI that client is registered with. Its
	// host is the gate's own host, and the only hostname the gate serves that
	// is not a preview.
	CallbackURL string

	// Scopes requested at the authorization endpoint.
	Scopes string

	// CookieSecret signs sessions, handoffs and OAuth state.
	CookieSecret string

	// CookieSecure marks cookies Secure. It is off only for installations
	// serving plain HTTP (Kitchen's TLS mode "none"), where a Secure cookie
	// would never come back.
	CookieSecure bool

	// SessionTTL is how long a login lasts before the visitor is sent back to
	// the identity provider — which, if they still have a session there,
	// costs them a redirect and no interaction.
	SessionTTL time.Duration
}

// GateHost is the hostname the gate serves its own endpoints on, taken from
// the callback URL so the two can never disagree.
func (c Config) GateHost() string {
	parsed, err := url.Parse(c.CallbackURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// LoadConfig reads the configuration from the environment, collecting every
// problem before failing so a misconfigured Deployment reports all of them in
// one crash loop rather than one per restart.
func LoadConfig(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	get := func(name string) string {
		value, _ := lookup(name)
		return strings.TrimSpace(value)
	}

	var problems []string
	required := func(name string) string {
		value := get(name)
		if value == "" {
			problems = append(problems, name+" is required")
		}
		return value
	}
	fallback := func(name, or string) string {
		if value := get(name); value != "" {
			return value
		}
		return or
	}

	cfg := Config{
		Addr:              fallback("KITCHEN_GATE_ADDR", DefaultAddr),
		HealthAddr:        fallback("KITCHEN_GATE_HEALTH_ADDR", DefaultHealthAddr),
		Issuer:            strings.TrimSuffix(required("KITCHEN_GATE_ISSUER"), "/"),
		IssuerInternalURL: strings.TrimSuffix(get("KITCHEN_GATE_ISSUER_INTERNAL_URL"), "/"),
		ClientID:          required("KITCHEN_GATE_CLIENT_ID"),
		ClientSecret:      required("KITCHEN_GATE_CLIENT_SECRET"),
		CallbackURL:       required("KITCHEN_GATE_CALLBACK_URL"),
		Scopes:            fallback("KITCHEN_GATE_SCOPES", DefaultScopes),
		CookieSecret:      required("KITCHEN_GATE_COOKIE_SECRET"),
		CookieSecure:      true,
		SessionTTL:        DefaultSessionTTL,
	}

	if raw := get("KITCHEN_GATE_COOKIE_SECURE"); raw != "" {
		secure, err := strconv.ParseBool(raw)
		if err != nil {
			problems = append(problems, "KITCHEN_GATE_COOKIE_SECURE must be a boolean (got "+raw+")")
		}
		cfg.CookieSecure = secure
	}
	if raw := get("KITCHEN_GATE_SESSION_TTL"); raw != "" {
		ttl, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			problems = append(problems, "KITCHEN_GATE_SESSION_TTL must be a duration (got "+raw+")")
		case ttl < time.Minute:
			problems = append(problems, "KITCHEN_GATE_SESSION_TTL must be at least a minute (got "+raw+")")
		default:
			cfg.SessionTTL = ttl
		}
	}

	if cfg.CookieSecret != "" && len(cfg.CookieSecret) < minSecretLength {
		problems = append(problems,
			fmt.Sprintf("KITCHEN_GATE_COOKIE_SECRET must be at least %d characters: it signs every session", minSecretLength))
	}
	problems = append(problems, checkAbsolute("KITCHEN_GATE_ISSUER", cfg.Issuer)...)
	if cfg.IssuerInternalURL != "" {
		problems = append(problems, checkAbsolute("KITCHEN_GATE_ISSUER_INTERNAL_URL", cfg.IssuerInternalURL)...)
	}
	problems = append(problems, checkAbsolute("KITCHEN_GATE_CALLBACK_URL", cfg.CallbackURL)...)
	if callback, err := url.Parse(cfg.CallbackURL); err == nil && callback.Host != "" && callback.Path != CallbackPath {
		problems = append(problems,
			"KITCHEN_GATE_CALLBACK_URL must end in "+CallbackPath+" (got "+callback.Path+")")
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// issuerBase is where the gate reaches the issuer over the back channel.
func (c Config) issuerBase() string {
	if c.IssuerInternalURL != "" {
		return c.IssuerInternalURL
	}
	return c.Issuer
}

func checkAbsolute(name, value string) []string {
	if value == "" {
		// Already reported as missing.
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != schemeHTTP && parsed.Scheme != schemeHTTPS) {
		return []string{name + " must be an absolute http(s) URL (got " + value + ")"}
	}
	return nil
}

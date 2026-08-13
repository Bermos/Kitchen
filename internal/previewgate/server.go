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

package previewgate

import (
	"context"
	"crypto/subtle"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Server is the gate. One instance serves every protected preview on the
// platform: which application a request belongs to comes from the route it
// arrived on, not from configuration.
type Server struct {
	cfg    Config
	sign   *signer
	oidc   *oidcClient
	proxy  *httputil.ReverseProxy
	log    logr.Logger
	scheme string // scheme of the gate's own host, from the callback URL
}

// contextKey carries the resolved upstream and visitor into the proxy's
// rewrite hook, which only gets the request.
type contextKey struct{}

type routedRequest struct {
	upstream *url.URL
	visitor  claims
}

// NewServer builds a gate from a validated configuration. The HTTP client is
// the one used to talk to the identity provider; nil gets a sensible default.
func NewServer(cfg Config, httpClient *http.Client, log logr.Logger) *Server {
	callback, _ := url.Parse(cfg.CallbackURL)
	s := &Server{
		cfg:    cfg,
		sign:   newSigner(cfg.CookieSecret),
		oidc:   newOIDCClient(cfg, httpClient),
		log:    log,
		scheme: callback.Scheme,
	}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:      s.rewrite,
		ErrorHandler: s.upstreamUnreachable,
	}
	return s
}

// ServeHTTP is the whole gate: its own endpoints, then the application.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, PathPrefix) {
		s.serveGateEndpoint(w, r)
		return
	}

	// The gate's own hostname carries no application. Falling through would
	// send a visitor to log in only to arrive back here with nothing to
	// proxy to.
	if r.Host == s.cfg.GateHost() {
		s.fail(w, r, http.StatusNotFound, "Nothing here",
			"This hostname only serves the platform's preview sign-in.")
		return
	}

	visitor, err := s.session(r)
	if err != nil {
		s.startLogin(w, r)
		return
	}
	s.forward(w, r, visitor)
}

func (s *Server) serveGateEndpoint(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case StartPath, CallbackPath:
		// Both belong to the flow that runs on the gate's own host: the flow
		// cookie is set there, and the redirect URI registered with the
		// identity provider points there.
		if r.Host != s.cfg.GateHost() {
			s.fail(w, r, http.StatusNotFound, "Nothing here", "This endpoint is served by the platform's gate host.")
			return
		}
		if r.URL.Path == StartPath {
			s.handleStart(w, r)
		} else {
			s.handleCallback(w, r)
		}
	case SessionPath:
		s.handleSession(w, r)
	case SignOutPath:
		s.handleSignOut(w, r)
	default:
		s.fail(w, r, http.StatusNotFound, "Nothing here", "No such gate endpoint.")
	}
}

// startLogin sends an anonymous visitor to the gate's host, carrying a signed
// note of where they were going. Signing it is what keeps the gate from being
// turned into an open redirector by anyone who can write a URL.
func (s *Server) startLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// A redirect would lose the body, and replaying it after a login is
		// worse than saying so.
		s.fail(w, r, http.StatusUnauthorized, "Sign in required",
			"This preview is protected. Open it in a browser to sign in, then retry this request.")
		return
	}

	token, err := s.sign.mint(claims{
		Purpose:   purposeReturn,
		Host:      r.Host,
		ReturnURL: s.requestURL(r),
	}, loginTTL)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Sign in unavailable", "The gate could not start a login.")
		return
	}

	start := url.URL{
		Scheme:   s.scheme,
		Host:     s.cfg.GateHost(),
		Path:     StartPath,
		RawQuery: url.Values{"rd": {token}}.Encode(),
	}
	redirect(w, r, start.String())
}

// handleStart begins the OAuth flow on the gate's own host.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	// The host of a return token is the preview it was minted on, so it is
	// not checked against this one — the signature is what makes the URL
	// trustworthy.
	returnTo, err := s.sign.verify(r.URL.Query().Get("rd"), purposeReturn, "")
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "Sign in expired",
			"This sign-in link is no longer valid. Open the preview URL again.")
		return
	}

	nonce, err := randomString(16)
	if err == nil {
		var verifier string
		if verifier, err = newVerifier(); err == nil {
			err = s.beginAuthorization(w, r, returnTo, nonce, verifier)
		}
	}
	if err != nil {
		s.log.Error(err, "could not start a login", "host", returnTo.Host)
		s.fail(w, r, http.StatusBadGateway, "Sign in unavailable",
			"The platform's identity provider could not be reached.")
	}
}

func (s *Server) beginAuthorization(w http.ResponseWriter, r *http.Request, returnTo claims, nonce, verifier string) error {
	authorize, err := s.oidc.authorizeURL(r.Context(), nonce, verifier)
	if err != nil {
		return err
	}
	flow, err := s.sign.mint(claims{
		Purpose:   purposeFlow,
		Host:      s.cfg.GateHost(),
		Nonce:     nonce,
		Verifier:  verifier,
		ReturnURL: returnTo.ReturnURL,
	}, loginTTL)
	if err != nil {
		return err
	}
	s.setCookie(w, FlowCookieName, flow, PathPrefix, int(loginTTL.Seconds()))
	redirect(w, r, authorize)
	return nil
}

// handleCallback finishes the OAuth flow and hands the result to the preview
// the visitor came from.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if failure := query.Get("error"); failure != "" {
		s.clearCookie(w, FlowCookieName, PathPrefix)
		s.log.Info("the identity provider refused the login", "error", failure)
		s.fail(w, r, http.StatusForbidden, "Sign in failed",
			"The platform's identity provider refused the login: "+failure)
		return
	}

	cookie, err := r.Cookie(FlowCookieName)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "Sign in expired",
			"This login took too long or started in another browser. Open the preview URL again.")
		return
	}
	flow, err := s.sign.verify(cookie.Value, purposeFlow, s.cfg.GateHost())
	s.clearCookie(w, FlowCookieName, PathPrefix)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "Sign in expired",
			"This login is no longer valid. Open the preview URL again.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(flow.Nonce)) != 1 {
		s.fail(w, r, http.StatusBadRequest, "Sign in failed",
			"This login did not come back the way it went out.")
		return
	}

	visitor, err := s.oidc.exchange(r.Context(), query.Get("code"), flow.Verifier)
	if err != nil {
		s.log.Error(err, "could not finish a login")
		s.fail(w, r, http.StatusBadGateway, "Sign in failed",
			"The platform's identity provider did not complete the login.")
		return
	}

	returnTo, err := url.Parse(flow.ReturnURL)
	if err != nil || returnTo.Host == "" {
		s.fail(w, r, http.StatusBadRequest, "Sign in failed", "There is nowhere to return to.")
		return
	}

	// The session cookie has to be set on the preview's own host, which is
	// the one place this request is not. One more hop, carrying a token that
	// is good for a minute and for that host only.
	handoff, err := s.sign.mint(claims{
		Purpose:   purposeHandoff,
		Host:      returnTo.Host,
		Subject:   visitor.Subject,
		Email:     visitor.Email,
		ReturnURL: flow.ReturnURL,
	}, handoffTTL)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Sign in failed", "The gate could not hand the login back.")
		return
	}

	landing := url.URL{
		Scheme:   returnTo.Scheme,
		Host:     returnTo.Host,
		Path:     SessionPath,
		RawQuery: url.Values{"token": {handoff}}.Encode(),
	}
	s.log.Info("signed a visitor in", "host", returnTo.Host, "subject", visitor.Subject)
	redirect(w, r, landing.String())
}

// handleSession turns the hand-off into a session cookie for this host.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	handoff, err := s.sign.verify(r.URL.Query().Get("token"), purposeHandoff, r.Host)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "Sign in expired",
			"This sign-in has already been used or has expired. Open the preview URL again.")
		return
	}

	session, err := s.sign.mint(claims{
		Purpose: purposeSession,
		Host:    r.Host,
		Subject: handoff.Subject,
		Email:   handoff.Email,
	}, s.cfg.SessionTTL)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Sign in failed", "The gate could not start a session.")
		return
	}
	s.setCookie(w, CookieName, session, "/", int(s.cfg.SessionTTL.Seconds()))

	// The return URL was signed alongside the identity, but it is still a URL
	// from a query string: only its path is used, and only on this host.
	target := "/"
	if parsed, err := url.Parse(handoff.ReturnURL); err == nil && parsed.Host == r.Host {
		target = parsed.RequestURI()
	}
	redirect(w, r, target)
}

func (s *Server) handleSignOut(w http.ResponseWriter, _ *http.Request) {
	s.clearCookie(w, CookieName, "/")
	s.page(w, http.StatusOK, "Signed out", "You are signed out of this preview on this device.")
}

// session returns the visitor a request carries, if any.
func (s *Server) session(r *http.Request) (claims, error) {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return claims{}, errInvalidToken
	}
	return s.sign.verify(cookie.Value, purposeSession, r.Host)
}

// forward proxies a signed-in request to the application.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, visitor claims) {
	upstream, err := parseUpstream(r.Header.Get(UpstreamHeader))
	if err != nil {
		// Either the route was built without the header, or someone reached
		// the gate directly. Both are the platform's problem, not a visitor's.
		s.log.Error(err, "no application to forward to", "host", r.Host)
		s.fail(w, r, http.StatusBadGateway, "Not available",
			"This hostname is not routed to an application.")
		return
	}
	ctx := context.WithValue(r.Context(), contextKey{}, routedRequest{upstream: upstream, visitor: visitor})
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// rewrite builds the request the application sees. Nothing about the gate
// survives it except the two headers naming the visitor.
func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	routed, _ := pr.In.Context().Value(contextKey{}).(routedRequest)

	// Captured before SetXForwarded, which drops the inbound X-Forwarded-*
	// headers — and the edge is the only thing that knows the real scheme.
	proto := forwardedProto(pr.In)

	pr.SetURL(routed.upstream)
	pr.Out.Host = pr.In.Host
	pr.SetXForwarded()
	pr.Out.Header.Set("X-Forwarded-Proto", proto)

	// The application is not part of the platform's trust boundary: it must
	// never see the cookies that let this request through.
	stripCookie(pr.Out.Header, CookieName)
	stripCookie(pr.Out.Header, FlowCookieName)
	pr.Out.Header.Del(UpstreamHeader)
	pr.Out.Header.Set(UserHeader, routed.visitor.Subject)
	pr.Out.Header.Set(EmailHeader, routed.visitor.Email)
}

func (s *Server) upstreamUnreachable(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error(err, "the application did not answer", "host", r.Host)
	s.fail(w, r, http.StatusBadGateway, "Not available",
		"The application behind this preview did not answer.")
}

// HealthHandler serves the probes. It is deliberately a separate listener:
// the proxy's port belongs to the applications behind the gate, and an
// application is entitled to its own /healthz.
//
// Readiness is the issuer being reachable, because a gate that cannot reach
// it can only turn visitors away.
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.oidc.endpoints(r.Context()); err != nil {
			s.log.V(1).Info("not ready", "error", err.Error())
			writeText(w, http.StatusServiceUnavailable, "identity provider unavailable\n")
			return
		}
		writeText(w, http.StatusOK, "ok\n")
	})
	return mux
}

// requestURL reconstructs the absolute URL a request was made to, which is
// where the visitor is sent back after signing in.
func (s *Server) requestURL(r *http.Request) string {
	return (&url.URL{
		Scheme:   forwardedProto(r),
		Host:     r.Host,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}).String()
}

func (s *Server) setCookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:  name,
		Value: value,
		Path:  path,
		// No Domain: the cookie belongs to this hostname alone, so no
		// application on the base domain ever receives it.
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		// Lax, not Strict: the visitor arrives here by a redirect from the
		// identity provider, and Strict would drop the cookie on the way.
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// fail logs nothing by itself — callers log what is worth logging — and
// renders a page a person can act on.
func (s *Server) fail(w http.ResponseWriter, _ *http.Request, status int, title, message string) {
	s.page(w, status, title, message)
}

func (s *Server) page(w http.ResponseWriter, status int, title, message string) {
	body := fmt.Sprintf(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — Kitchen</title></head>
<body style="font-family: system-ui, sans-serif; margin: 4rem auto; max-width: 34rem; line-height: 1.5">
<h1 style="font-size: 1.25rem">%s</h1>
<p>%s</p>
</body></html>
`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(message))
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.Header().Set("x-content-type-options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// redirect answers with a 302 that is never cached: the same URL means
// something different once a session exists.
func redirect(w http.ResponseWriter, r *http.Request, target string) {
	w.Header().Set("cache-control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// serviceName is what the reconciler puts in the upstream header: an
// in-cluster Service address, <service>.<namespace>.svc[.cluster.local].
var serviceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`)

// parseUpstream reads and checks the application address from the route.
//
// The Gateway sets the header with a filter that overwrites whatever the
// client sent, so it cannot be forged from outside. It is checked anyway: the
// gate is a proxy, and a proxy that forwards to any address it is told to is
// a way out of the cluster.
func parseUpstream(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("no %s header on the request", UpstreamHeader)
	}
	host, port := value, "80"
	if h, p, found := strings.Cut(value, ":"); found {
		host, port = h, p
	}
	if !serviceName.MatchString(host) {
		return nil, fmt.Errorf("%q is not a Service address", value)
	}
	labels := strings.Split(host, ".")
	if len(labels) < 3 || labels[2] != "svc" {
		return nil, fmt.Errorf("%q is not an in-cluster Service address", value)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return nil, fmt.Errorf("%q has no usable port", value)
	}
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}, nil
}

// forwardedProto is the scheme the visitor's browser used. The Gateway
// terminates TLS, so the connection the gate sees says nothing about it.
func forwardedProto(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		// A comma-separated chain means several hops; the first is the client's.
		if first, _, found := strings.Cut(proto, ","); found {
			proto = first
		}
		if proto = strings.TrimSpace(proto); proto == schemeHTTP || proto == schemeHTTPS {
			return proto
		}
	}
	if r.TLS != nil {
		return schemeHTTPS
	}
	return schemeHTTP
}

// stripCookie removes one cookie from a request's Cookie header, leaving the
// application's own cookies alone.
func stripCookie(header http.Header, name string) {
	cookies := (&http.Request{Header: header}).Cookies()
	kept := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie.Name != name {
			kept = append(kept, cookie.String())
		}
	}
	header.Del("Cookie")
	if len(kept) > 0 {
		header.Set("Cookie", strings.Join(kept, "; "))
	}
}

// DefaultLogger is the logger a gate gets when the caller has none.
func DefaultLogger() logr.Logger {
	return logf.Log.WithName("preview-gate")
}

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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

const (
	testPreviewHost = "shop-pr-42.apps.example.com"
	testGateHost    = "previews.apps.example.com"
	testClientID    = "gate-client"
	testSecret      = "0123456789abcdef0123456789abcdef0123456789"
	// testUpstream is what the Environment reconciler writes into the route:
	// the application's Service, as the gate insists on seeing it.
	testUpstream = "shop-pr-42.kitchen-shop.svc.cluster.local:80"
)

// fakeIssuer is an identity provider that signs everyone in.
type fakeIssuer struct {
	*httptest.Server
	// lastAuthorize is the query the gate sent a browser off with.
	lastAuthorize url.Values
	// lastExchange is the form the gate posted to the token endpoint.
	lastExchange url.Values
	// subject and email are what the ID token says.
	subject, email string
	// audience overrides the ID token's audience.
	audience string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	issuer := &fakeIssuer{subject: "user-1", email: "dev@example.com"}
	mux := http.NewServeMux()
	issuer.Server = httptest.NewServer(mux)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                 issuer.URL,
			"authorization_endpoint": issuer.URL + "/oauth2/authorize",
			"token_endpoint":         issuer.URL + "/oauth2/token",
			"registration_endpoint":  issuer.URL + "/oauth2/register",
			"jwks_uri":               issuer.URL + "/jwks",
		})
	})
	mux.HandleFunc("/oauth2/authorize", func(w http.ResponseWriter, r *http.Request) {
		issuer.lastAuthorize = r.URL.Query()
		// Straight back to the gate, as a signed-in browser would be.
		back, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
		back.RawQuery = url.Values{
			"code":  {"the-code"},
			"state": {r.URL.Query().Get("state")},
		}.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		issuer.lastExchange = r.PostForm
		audience := issuer.audience
		if audience == "" {
			audience = testClientID
		}
		writeJSON(w, map[string]any{
			"access_token": "at",
			"token_type":   "Bearer",
			"id_token": idToken(map[string]any{
				"iss":   issuer.URL,
				"aud":   audience,
				"sub":   issuer.subject,
				"email": issuer.email,
				"exp":   time.Now().Add(time.Hour).Unix(),
			}),
		})
	})
	t.Cleanup(issuer.Close)
	return issuer
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// idToken builds an unsigned JWT: the gate reads the claims of a token it
// fetched itself over the back channel and checks them, which is what the
// specification allows there.
func idToken(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

// newTestGate builds a gate pointed at a fake issuer, with cookies that
// survive plain HTTP so the flow can be followed without TLS. The
// application, when there is one, is reached through a transport that stands
// in for cluster DNS: the gate only ever forwards to Service addresses, and
// nothing resolves those here.
func newTestGate(t *testing.T, issuer *fakeIssuer, app *upstream) *Server {
	t.Helper()
	gate := NewServer(Config{
		Issuer:       issuer.URL,
		ClientID:     testClientID,
		ClientSecret: "gate-secret",
		CallbackURL:  "http://" + testGateHost + CallbackPath,
		Scopes:       DefaultScopes,
		CookieSecret: testSecret,
		CookieSecure: false,
		SessionTTL:   time.Hour,
	}, issuer.Client(), logr.Discard())
	if app != nil {
		gate.proxy.Transport = clusterDNS{host: strings.TrimPrefix(app.URL, "http://")}
	}
	return gate
}

// clusterDNS sends a request addressed to a Service to the test server
// standing in for it.
type clusterDNS struct{ host string }

func (c clusterDNS) RoundTrip(r *http.Request) (*http.Response, error) {
	routed := r.Clone(r.Context())
	routed.URL.Host = c.host
	return http.DefaultTransport.RoundTrip(routed)
}

// upstream is the application behind the gate. It records what reached it.
type upstream struct {
	*httptest.Server
	lastRequest *http.Request
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	app := &upstream{}
	app.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app.lastRequest = r.Clone(r.Context())
		w.Header().Set("content-type", "text/plain")
		_, _ = fmt.Fprintf(w, "the application at %s%s", r.Host, r.URL.Path)
	}))
	t.Cleanup(app.Close)
	return app
}

// request builds a request as the Gateway would deliver it: the visitor's
// hostname, and the application's address in the upstream header.
func request(method, host, target string, app *upstream) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	if app != nil {
		r.Header.Set(UpstreamHeader, testUpstream)
	}
	return r
}

// TestAnonymousRequestIsSentToLogin is the first half of the acceptance
// criterion: an anonymous request to a protected preview lands on the
// platform login rather than on the application.
func TestAnonymousRequestIsSentToLogin(t *testing.T) {
	issuer := newFakeIssuer(t)
	app := newUpstream(t)
	gate := newTestGate(t, issuer, app)

	res := httptest.NewRecorder()
	gate.ServeHTTP(res, request(http.MethodGet, testPreviewHost, "/secret/page?a=b", app))

	if res.Code != http.StatusFound {
		t.Fatalf("expected a redirect, got %d: %s", res.Code, res.Body.String())
	}
	if app.lastRequest != nil {
		t.Fatal("the application was reached by an anonymous request")
	}
	location, err := url.Parse(res.Header().Get("location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Host != testGateHost || location.Path != StartPath {
		t.Fatalf("expected a redirect to the gate host, got %s", location)
	}
	if location.Query().Get("rd") == "" {
		t.Fatal("the redirect carries no signed return address")
	}
}

// TestSignedInVisitorReachesTheApplication walks the whole flow the way a
// browser would, and checks what the application ends up seeing.
func TestSignedInVisitorReachesTheApplication(t *testing.T) {
	issuer := newFakeIssuer(t)
	app := newUpstream(t)
	gate := newTestGate(t, issuer, app)

	session := signIn(t, gate, issuer, app, "/secret/page?a=b")

	// The visitor returns to where they were going, now with a session.
	res := httptest.NewRecorder()
	final := request(http.MethodGet, testPreviewHost, "/secret/page?a=b", app)
	final.AddCookie(session)
	// An application cookie of its own, and a forged identity header.
	final.AddCookie(&http.Cookie{Name: "app_session", Value: "keep-me"})
	final.Header.Set(UserHeader, "somebody-else")
	gate.ServeHTTP(res, final)

	if res.Code != http.StatusOK {
		t.Fatalf("expected the application's answer, got %d: %s", res.Code, res.Body.String())
	}
	if app.lastRequest == nil {
		t.Fatal("the application was never reached")
	}
	if got := app.lastRequest.URL.Path; got != "/secret/page" {
		t.Fatalf("the application saw the path %q", got)
	}
	if got := app.lastRequest.Host; got != testPreviewHost {
		t.Fatalf("the application saw the host %q, not the preview's", got)
	}
	if got := app.lastRequest.Header.Get(UserHeader); got != "user-1" {
		t.Fatalf("the application was told the visitor is %q", got)
	}
	if got := app.lastRequest.Header.Get(EmailHeader); got != "dev@example.com" {
		t.Fatalf("the application was told the email is %q", got)
	}
	if _, err := app.lastRequest.Cookie(CookieName); err == nil {
		t.Fatal("the platform's session cookie was forwarded to the application")
	}
	if cookie, err := app.lastRequest.Cookie("app_session"); err != nil || cookie.Value != "keep-me" {
		t.Fatal("the application's own cookie did not survive the gate")
	}
	if got := app.lastRequest.Header.Get(UpstreamHeader); got != "" {
		t.Fatalf("the routing header reached the application: %q", got)
	}
}

// signIn drives the login the way a browser would and returns the session
// cookie the preview host ends up with.
func signIn(t *testing.T, gate *Server, issuer *fakeIssuer, app *upstream, target string) *http.Cookie {
	t.Helper()

	// 1. Anonymous request → the gate's host, carrying a signed return address.
	first := httptest.NewRecorder()
	gate.ServeHTTP(first, request(http.MethodGet, testPreviewHost, target, app))
	start := mustLocation(t, first)

	// 2. The gate's host starts the OAuth flow.
	second := httptest.NewRecorder()
	gate.ServeHTTP(second, request(http.MethodGet, testGateHost, start.RequestURI(), nil))
	authorize := mustLocation(t, second)
	if !strings.HasPrefix(authorize.String(), issuer.URL) {
		t.Fatalf("expected a redirect to the issuer, got %s", authorize)
	}
	if authorize.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("the authorization request is not PKCE-protected")
	}
	flowCookie := cookieNamed(t, second, FlowCookieName)

	// 3. The issuer sends the browser back to the callback.
	back, err := browser(issuer).Get(authorize.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = back.Body.Close() }()
	callback, err := back.Location()
	if err != nil {
		t.Fatal(err)
	}

	third := httptest.NewRecorder()
	callbackRequest := request(http.MethodGet, testGateHost, callback.RequestURI(), nil)
	callbackRequest.AddCookie(flowCookie)
	gate.ServeHTTP(third, callbackRequest)
	if third.Code != http.StatusFound {
		t.Fatalf("the callback answered %d: %s", third.Code, third.Body.String())
	}
	handoff := mustLocation(t, third)
	if handoff.Host != testPreviewHost || handoff.Path != SessionPath {
		t.Fatalf("expected the login to be handed back to the preview, got %s", handoff)
	}

	// 4. The preview host turns the hand-off into a session cookie.
	fourth := httptest.NewRecorder()
	gate.ServeHTTP(fourth, request(http.MethodGet, testPreviewHost, handoff.RequestURI(), app))
	if fourth.Code != http.StatusFound {
		t.Fatalf("the hand-off answered %d: %s", fourth.Code, fourth.Body.String())
	}
	if got := mustLocation(t, fourth).RequestURI(); got != target {
		t.Fatalf("the visitor was returned to %q, not %q", got, target)
	}
	return cookieNamed(t, fourth, CookieName)
}

func TestSessionsDoNotCrossPreviews(t *testing.T) {
	issuer := newFakeIssuer(t)
	app := newUpstream(t)
	gate := newTestGate(t, issuer, app)

	session := signIn(t, gate, issuer, app, "/")

	// The same cookie, presented on a different preview's hostname, is not a
	// session there — it was signed for one host and says so.
	res := httptest.NewRecorder()
	other := request(http.MethodGet, "other-pr-1.apps.example.com", "/", app)
	other.AddCookie(session)
	gate.ServeHTTP(res, other)

	if res.Code != http.StatusFound {
		t.Fatalf("expected a redirect to log in, got %d", res.Code)
	}
	if app.lastRequest != nil {
		t.Fatal("a session for another preview reached the application")
	}
}

func TestTamperedSessionIsRefused(t *testing.T) {
	issuer := newFakeIssuer(t)
	app := newUpstream(t)
	gate := newTestGate(t, issuer, app)

	session := signIn(t, gate, issuer, app, "/")
	app.lastRequest = nil

	// Re-sign the claims with another key: same shape, wrong signature.
	forged := forge(t, claims{Purpose: purposeSession, Host: testPreviewHost, Subject: "root"})
	res := httptest.NewRecorder()
	r := request(http.MethodGet, testPreviewHost, "/", app)
	r.AddCookie(&http.Cookie{Name: session.Name, Value: forged})
	gate.ServeHTTP(res, r)

	if res.Code != http.StatusFound {
		t.Fatalf("expected a redirect to log in, got %d", res.Code)
	}
	if app.lastRequest != nil {
		t.Fatal("a forged session reached the application")
	}
}

func forge(t *testing.T, c claims) string {
	t.Helper()
	other := newSigner("a-different-signing-key-entirely-0000")
	token, err := other.mint(c, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// TestExpiredSessionIsRefused checks the clock is actually consulted.
func TestExpiredSessionIsRefused(t *testing.T) {
	issuer := newFakeIssuer(t)
	app := newUpstream(t)
	gate := newTestGate(t, issuer, app)

	stale, err := gate.sign.mint(claims{
		Purpose: purposeSession, Host: testPreviewHost, Subject: "user-1",
	}, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	r := request(http.MethodGet, testPreviewHost, "/", app)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: stale})
	gate.ServeHTTP(res, r)

	if res.Code != http.StatusFound {
		t.Fatalf("expected a redirect to log in, got %d", res.Code)
	}
	if app.lastRequest != nil {
		t.Fatal("an expired session reached the application")
	}
}

// TestCallbackRefusesForeignState is the CSRF check: a callback that did not
// start at this gate has no matching flow cookie.
func TestCallbackRefusesForeignState(t *testing.T) {
	issuer := newFakeIssuer(t)
	gate := newTestGate(t, issuer, nil)

	res := httptest.NewRecorder()
	gate.ServeHTTP(res, request(http.MethodGet, testGateHost, CallbackPath+"?code=x&state=y", nil))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected the callback to be refused, got %d", res.Code)
	}

	// With a flow cookie, but state from somewhere else.
	flow, err := gate.sign.mint(claims{
		Purpose:   purposeFlow,
		Host:      testGateHost,
		Nonce:     "the-real-nonce",
		Verifier:  "v",
		ReturnURL: "http://" + testPreviewHost + "/",
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	withCookie := httptest.NewRecorder()
	r := request(http.MethodGet, testGateHost, CallbackPath+"?code=x&state=someone-elses", nil)
	r.AddCookie(&http.Cookie{Name: FlowCookieName, Value: flow})
	gate.ServeHTTP(withCookie, r)
	if withCookie.Code != http.StatusBadRequest {
		t.Fatalf("expected mismatched state to be refused, got %d", withCookie.Code)
	}
}

// TestStartRefusesUnsignedReturnURL keeps the gate host from being turned
// into an open redirector.
func TestStartRefusesUnsignedReturnURL(t *testing.T) {
	gate := newTestGate(t, newFakeIssuer(t), nil)

	res := httptest.NewRecorder()
	target := StartPath + "?rd=" + url.QueryEscape("https://evil.example.com/")
	gate.ServeHTTP(res, request(http.MethodGet, testGateHost, target, nil))

	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected an unsigned return address to be refused, got %d", res.Code)
	}
}

// TestIDTokenForAnotherClientIsRefused: the gate must not accept a token
// minted for someone else's client.
func TestIDTokenForAnotherClientIsRefused(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.audience = "another-client"
	app := newUpstream(t)
	gate := newTestGate(t, issuer, app)

	// Drive the flow up to the callback by hand: signIn asserts success.
	first := httptest.NewRecorder()
	gate.ServeHTTP(first, request(http.MethodGet, testPreviewHost, "/", app))
	second := httptest.NewRecorder()
	gate.ServeHTTP(second, request(http.MethodGet, testGateHost, mustLocation(t, first).RequestURI(), nil))
	flowCookie := cookieNamed(t, second, FlowCookieName)

	back, err := browser(issuer).Get(mustLocation(t, second).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = back.Body.Close() }()
	callback, err := back.Location()
	if err != nil {
		t.Fatal(err)
	}

	res := httptest.NewRecorder()
	r := request(http.MethodGet, testGateHost, callback.RequestURI(), nil)
	r.AddCookie(flowCookie)
	gate.ServeHTTP(res, r)

	if res.Code != http.StatusBadGateway {
		t.Fatalf("expected the exchange to fail, got %d: %s", res.Code, res.Body.String())
	}
}

// TestUnroutedHostIsNotProxied: the gate only forwards to addresses the
// platform put on the request.
func TestUnroutedHostIsNotProxied(t *testing.T) {
	issuer := newFakeIssuer(t)
	app := newUpstream(t)
	gate := newTestGate(t, issuer, app)

	session := signIn(t, gate, issuer, app, "/")
	app.lastRequest = nil

	for name, upstreamHeader := range map[string]string{
		"missing":       "",
		"outside":       "evil.example.com:80",
		"not a service": "shop.kitchen-shop.cluster.local:80",
	} {
		t.Run(name, func(t *testing.T) {
			res := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Host = testPreviewHost
			if upstreamHeader != "" {
				r.Header.Set(UpstreamHeader, upstreamHeader)
			}
			r.AddCookie(session)
			gate.ServeHTTP(res, r)

			if res.Code != http.StatusBadGateway {
				t.Fatalf("expected the request to be refused, got %d", res.Code)
			}
			if app.lastRequest != nil {
				t.Fatal("the request was proxied somewhere")
			}
		})
	}
}

// TestNonBrowserRequestIsToldToSignIn: a redirect would silently drop the
// body of a POST, so the gate says so instead.
func TestNonBrowserRequestIsToldToSignIn(t *testing.T) {
	gate := newTestGate(t, newFakeIssuer(t), nil)
	app := newUpstream(t)

	res := httptest.NewRecorder()
	gate.ServeHTTP(res, request(http.MethodPost, testPreviewHost, "/api/orders", app))

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.Code)
	}
	if app.lastRequest != nil {
		t.Fatal("an unauthenticated POST reached the application")
	}
}

func TestSignOutDropsTheSession(t *testing.T) {
	issuer := newFakeIssuer(t)
	app := newUpstream(t)
	gate := newTestGate(t, issuer, app)

	session := signIn(t, gate, issuer, app, "/")
	res := httptest.NewRecorder()
	r := request(http.MethodGet, testPreviewHost, SignOutPath, app)
	r.AddCookie(session)
	gate.ServeHTTP(res, r)

	if res.Code != http.StatusOK {
		t.Fatalf("expected the sign-out page, got %d", res.Code)
	}
	dropped := cookieNamed(t, res, CookieName)
	if dropped.Value != "" || dropped.MaxAge >= 0 {
		t.Fatalf("the session cookie was not cleared: %v", dropped)
	}
}

// TestGateHostServesNoApplication: the gate's own hostname has nothing behind
// it, and must not send visitors round a login loop to find that out.
func TestGateHostServesNoApplication(t *testing.T) {
	gate := newTestGate(t, newFakeIssuer(t), nil)

	res := httptest.NewRecorder()
	gate.ServeHTTP(res, request(http.MethodGet, testGateHost, "/", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on the gate host, got %d", res.Code)
	}
}

func TestForwardedProtoIsPreserved(t *testing.T) {
	issuer := newFakeIssuer(t)
	app := newUpstream(t)
	gate := newTestGate(t, issuer, app)

	// The edge terminates TLS, so the only record of the visitor's scheme is
	// the header the Gateway set.
	res := httptest.NewRecorder()
	r := request(http.MethodGet, testPreviewHost, "/", app)
	r.Header.Set("X-Forwarded-Proto", "https")
	gate.ServeHTTP(res, r)

	location := mustLocation(t, res)
	rd, err := gate.sign.verify(location.Query().Get("rd"), purposeReturn, testPreviewHost)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rd.ReturnURL, "https://") {
		t.Fatalf("the return address lost the visitor's scheme: %s", rd.ReturnURL)
	}
}

// browser talks to the fake issuer without following redirects: the next hop
// is a hostname only the Gateway knows, and the test drives it by hand.
func browser(issuer *fakeIssuer) *http.Client {
	client := issuer.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func mustLocation(t *testing.T, res *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	if res.Code != http.StatusFound {
		t.Fatalf("expected a redirect, got %d: %s", res.Code, res.Body.String())
	}
	location, err := url.Parse(res.Header().Get("location"))
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func cookieNamed(t *testing.T, res *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range (&http.Response{Header: res.Header()}).Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("no %s cookie was set", name)
	return nil
}

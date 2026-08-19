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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/idp"
)

// discoveryTTL is how long the issuer's metadata is reused. Endpoints do not
// move, but a gate that cached them at start would never notice an issuer
// that did.
const discoveryTTL = 15 * time.Minute

// oidcClient is the gate's half of the OpenID Connect conversation: where to
// send a browser, and how to turn the code it comes back with into claims.
type oidcClient struct {
	cfg  Config
	http *http.Client

	mu       sync.Mutex
	metadata *idp.Metadata
	fetched  time.Time
	now      func() time.Time
}

func newOIDCClient(cfg Config, httpClient *http.Client) *oidcClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &oidcClient{cfg: cfg, http: httpClient, now: time.Now}
}

// endpoints returns the issuer's metadata, fetching it at most once every
// discoveryTTL.
//
// The authorization endpoint is the one address a browser has to reach, so it
// is always the public one, even when the gate itself talks to the issuer
// over a cluster-internal address.
func (o *oidcClient) endpoints(ctx context.Context) (*idp.Metadata, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.metadata != nil && o.now().Sub(o.fetched) < discoveryTTL {
		return o.metadata, nil
	}

	metadata, err := idp.New(idp.Config{
		Issuer:  o.cfg.Issuer,
		BaseURL: o.cfg.issuerBase(),
	}).WithHTTPClient(o.http).Discover(ctx)
	if err != nil {
		return nil, err
	}
	if metadata.Issuer != o.cfg.Issuer {
		return nil, fmt.Errorf("the issuer at %s calls itself %q, not %q",
			o.cfg.issuerBase(), metadata.Issuer, o.cfg.Issuer)
	}
	if metadata.AuthorizationEndpoint == "" || metadata.TokenEndpoint == "" {
		return nil, fmt.Errorf("the issuer at %s advertises no authorization or token endpoint", o.cfg.issuerBase())
	}
	// Browsers cannot use a cluster-internal address.
	metadata.AuthorizationEndpoint = rebasePublic(metadata.AuthorizationEndpoint, o.cfg.issuerBase(), o.cfg.Issuer)

	o.metadata, o.fetched = metadata, o.now()
	return metadata, nil
}

// authorizeURL is where the visitor is sent to prove who they are.
func (o *oidcClient) authorizeURL(ctx context.Context, state, verifier string) (string, error) {
	metadata, err := o.endpoints(ctx)
	if err != nil {
		return "", err
	}
	endpoint, err := url.Parse(metadata.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	query := endpoint.Query()
	query.Set("response_type", "code")
	query.Set("client_id", o.cfg.ClientID)
	query.Set("redirect_uri", o.cfg.CallbackURL)
	query.Set("scope", o.cfg.Scopes)
	query.Set("state", state)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	query.Set("code_challenge_method", "S256")
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

// identity is who the identity provider says the visitor is.
type identity struct {
	Subject string
	Email   string
	// EmailVerified is the issuer's own answer to whether it has checked the
	// address. It travels with the address everywhere it goes, because an
	// access grant naming an address is a grant to whoever the issuer says
	// holds it — and an unverified address is only what the account said
	// about itself.
	EmailVerified bool
}

// tokenResponse is the part of the token endpoint's answer the gate reads.
type tokenResponse struct {
	IDToken string `json:"id_token"`
	Error   string `json:"error"`
	Details string `json:"error_description"`
}

// exchange turns an authorization code into the visitor's identity.
func (o *oidcClient) exchange(ctx context.Context, code, verifier string) (identity, error) {
	metadata, err := o.endpoints(ctx)
	if err != nil {
		return identity{}, err
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {o.cfg.CallbackURL},
		"code_verifier": {verifier},
		// Sent as well as the Basic header: some issuers look for the client
		// id in the body even when the client authenticates in the header.
		"client_id": {o.cfg.ClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, metadata.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return identity{}, err
	}
	// Reached inside the cluster, the issuer is still asked for by name.
	if host := idp.HostHeaderFor(idp.Config{Issuer: o.cfg.Issuer, BaseURL: o.cfg.issuerBase()}, metadata.TokenEndpoint); host != "" {
		req.Host = host
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(o.cfg.ClientID), url.QueryEscape(o.cfg.ClientSecret))

	res, err := o.http.Do(req)
	if err != nil {
		return identity{}, fmt.Errorf("token exchange: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return identity{}, err
	}
	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return identity{}, fmt.Errorf("token exchange: %s: unreadable response", res.Status)
	}
	if res.StatusCode != http.StatusOK || token.Error != "" {
		return identity{}, fmt.Errorf("token exchange: %s: %s %s", res.Status, token.Error, token.Details)
	}
	if token.IDToken == "" {
		return identity{}, fmt.Errorf("token exchange: the issuer returned no ID token")
	}
	return o.identityFromIDToken(token.IDToken)
}

// idTokenClaims is what the gate needs out of an ID token.
type idTokenClaims struct {
	Issuer   string          `json:"iss"`
	Subject  string          `json:"sub"`
	Email    string          `json:"email"`
	Audience json.RawMessage `json:"aud"`
	// EmailVerified is read raw because issuers disagree about its type: the
	// specification says boolean and several send the string "true". Anything
	// else at all reads as unverified, which is the safe direction — the
	// claim only ever widens what a visitor may reach.
	EmailVerified json.RawMessage `json:"email_verified"`
	ExpiresAt     int64           `json:"exp"`
}

// identityFromIDToken reads and checks an ID token.
//
// The signature is not checked, and does not need to be: the token came back
// on the gate's own back-channel call to the token endpoint, authenticated
// with the client secret — which OpenID Connect Core 3.1.3.7 accepts in place
// of validating the signature, when the transport itself is trusted. Here
// that is either HTTPS or the cluster network to the issuer's own Service.
// What still has to be checked is that the token is for this issuer, for this
// client, and not expired.
func (o *oidcClient) identityFromIDToken(idToken string) (identity, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return identity{}, fmt.Errorf("the ID token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return identity{}, fmt.Errorf("the ID token payload is not readable: %w", err)
	}
	var c idTokenClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return identity{}, fmt.Errorf("the ID token payload is not readable: %w", err)
	}
	if c.Issuer != o.cfg.Issuer {
		return identity{}, fmt.Errorf("the ID token was issued by %q, not %q", c.Issuer, o.cfg.Issuer)
	}
	if !audienceContains(c.Audience, o.cfg.ClientID) {
		return identity{}, fmt.Errorf("the ID token is not for this client")
	}
	if c.ExpiresAt > 0 && c.ExpiresAt <= o.now().Unix() {
		return identity{}, fmt.Errorf("the ID token has expired")
	}
	if c.Subject == "" {
		return identity{}, fmt.Errorf("the ID token names no subject")
	}
	return identity{
		Subject:       c.Subject,
		Email:         c.Email,
		EmailVerified: access.VerifiedClaim(c.EmailVerified),
	}, nil
}

// audienceContains handles the audience being either a string or an array,
// both of which the specification allows.
func audienceContains(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == want
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return false
	}
	for _, audience := range many {
		if audience == want {
			return true
		}
	}
	return false
}

// newVerifier returns a PKCE code verifier.
func newVerifier() (string, error) {
	return randomString(32)
}

// randomString returns n random bytes, URL-safe.
func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// rebasePublic is the inverse of the rewrite the discovery client does: an
// endpoint the gate fetched over an internal address, handed to a browser.
func rebasePublic(endpoint, from, to string) string {
	if from == to {
		return endpoint
	}
	base, err := url.Parse(to)
	if err != nil {
		return endpoint
	}
	origin, err := url.Parse(from)
	if err != nil {
		return endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != origin.Scheme || parsed.Host != origin.Host {
		return endpoint
	}
	parsed.Scheme, parsed.Host = base.Scheme, base.Host
	return parsed.String()
}

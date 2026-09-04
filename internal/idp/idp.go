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

// Package idp speaks to the platform's identity provider the way the auth
// architecture says everything should: plain OpenID Connect discovery plus
// dynamic client registration, no knowledge of which implementation is behind
// the issuer. It is what the operator uses to mint OAuth clients — the
// preview gate's today, per-app clients later.
package idp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Secret keys the chart writes into the identity provider's secret.
const (
	SecretKeyIssuer       = "issuer"
	SecretKeyServiceKey   = "serviceKey"
	SecretKeyInternalURL  = "internalURL"
	SecretKeyDirectoryURL = "directoryURL"
)

// DiscoveryPath is where every OIDC client looks for the issuer's metadata.
const DiscoveryPath = "/.well-known/openid-configuration"

// serviceKeyHeader authenticates the operator against the issuer. The api-key
// plugin turns the key into a session, which is what client registration
// requires.
const serviceKeyHeader = "x-api-key"

// Config is a resolved connection to the identity provider.
type Config struct {
	// Issuer is the public issuer URL: what tokens are signed with and what
	// browsers are sent to.
	Issuer string

	// BaseURL is where the operator reaches the same issuer. It differs from
	// Issuer on clusters that cannot resolve their own base domain from the
	// inside, where the chart points it at the service instead.
	BaseURL string

	// DirectoryURL is where the operator reaches the `/kitchen` prefix: the
	// account directory, the CI keys and the management of the clients it
	// registered. It is a different address from BaseURL because it is a
	// different listener — the chart publishes the issuer on the shared
	// Gateway and does not publish this one, which is what keeps a surface
	// that mints CI keys and rewrites redirect lists off the internet.
	//
	// It falls back to BaseURL, which is what a federated issuer gets: it
	// serves no such prefix either way, and the operator finds that out with
	// a 404 and ErrNoDirectory.
	DirectoryURL string

	// ServiceKey authenticates client registration.
	ServiceKey string
}

// ConfigFromSecret reads the identity provider's details out of the secret
// the chart writes.
func ConfigFromSecret(secret *corev1.Secret) (Config, error) {
	cfg := Config{
		Issuer:       strings.TrimSuffix(strings.TrimSpace(string(secret.Data[SecretKeyIssuer])), "/"),
		BaseURL:      strings.TrimSuffix(strings.TrimSpace(string(secret.Data[SecretKeyInternalURL])), "/"),
		DirectoryURL: strings.TrimSuffix(strings.TrimSpace(string(secret.Data[SecretKeyDirectoryURL])), "/"),
		ServiceKey:   strings.TrimSpace(string(secret.Data[SecretKeyServiceKey])),
	}
	var missing []string
	if cfg.Issuer == "" {
		missing = append(missing, SecretKeyIssuer)
	}
	if cfg.ServiceKey == "" {
		missing = append(missing, SecretKeyServiceKey)
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("secret %s/%s is missing the keys: %s",
			secret.Namespace, secret.Name, strings.Join(missing, ", "))
	}
	return defaults(cfg), nil
}

// defaults fills in the two addresses that are allowed to be absent. An
// installation whose chart predates the private listener has no directoryURL
// in its secret, and one federating to an issuer of its own has neither
// address: both then reach the issuer the way everybody else does.
func defaults(cfg Config) Config {
	if cfg.BaseURL == "" {
		cfg.BaseURL = cfg.Issuer
	}
	if cfg.DirectoryURL == "" {
		cfg.DirectoryURL = cfg.BaseURL
	}
	return cfg
}

// Metadata is the part of the issuer's discovery document Kitchen uses.
type Metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// Client talks to one identity provider.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client with a timeout, so an issuer that stops answering
// costs a reconcile rather than a controller worker.
// A Config assembled by hand rather than read from the secret is defaulted
// here too, so that a caller which knows only the issuer still reaches it.
func New(cfg Config) *Client {
	return &Client{cfg: defaults(cfg), http: &http.Client{Timeout: 15 * time.Second}}
}

// WithHTTPClient replaces the HTTP client, for tests and for callers that
// bring their own transport.
func (c *Client) WithHTTPClient(httpClient *http.Client) *Client {
	c.http = httpClient
	return c
}

// Issuer returns the public issuer URL.
func (c *Client) Issuer() string { return c.cfg.Issuer }

// Discover fetches the issuer's metadata.
//
// The endpoints in the document are the issuer's public URLs. When the
// operator reaches the issuer somewhere else — a cluster-internal address for
// a hostname it cannot resolve — those URLs are of no use to it, so each
// endpoint on the issuer's own origin is rewritten onto the address the
// document was fetched from. Endpoints elsewhere (a federated issuer, say)
// are left alone.
func (c *Client) Discover(ctx context.Context) (*Metadata, error) {
	req, err := c.request(ctx, http.MethodGet, c.cfg.BaseURL+DiscoveryPath, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openid discovery at %s: %w", c.cfg.BaseURL, err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openid discovery at %s: %s: %s",
			c.cfg.BaseURL, res.Status, summarize(body))
	}

	metadata := &Metadata{}
	if err := json.Unmarshal(body, metadata); err != nil {
		return nil, fmt.Errorf("openid discovery at %s: %w", c.cfg.BaseURL, err)
	}
	if metadata.Issuer == "" {
		return nil, fmt.Errorf("openid discovery at %s: the document names no issuer", c.cfg.BaseURL)
	}
	for _, endpoint := range []*string{
		&metadata.AuthorizationEndpoint,
		&metadata.TokenEndpoint,
		&metadata.RegistrationEndpoint,
		&metadata.JWKSURI,
	} {
		*endpoint = rebase(*endpoint, metadata.Issuer, c.cfg.BaseURL)
	}
	return metadata, nil
}

// ClientRegistration is the client the operator wants to exist.
type ClientRegistration struct {
	Name         string
	RedirectURIs []string
	GrantTypes   []string
	Scopes       []string
}

// RegisteredClient is what the issuer handed back.
type RegisteredClient struct {
	ID     string
	Secret string

	// Management is how to get back to this client — the RFC 7592 client
	// configuration endpoint and its token, when the issuer named them. Its
	// zero value means the issuer offered no way to change the client, which
	// is what ErrNoClientManagement is about.
	Management ClientHandle
}

// registrationResponse is RFC 7591's client information response.
type registrationResponse struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`

	// RFC 7592's two fields. An issuer that does not implement client
	// management leaves both out, which is not an error at registration time.
	RegistrationClientURI   string `json:"registration_client_uri"`
	RegistrationAccessToken string `json:"registration_access_token"`
}

// Register creates an OAuth client at the issuer and returns its credentials.
// The credentials are only ever handed out once, so the caller has to keep
// them; a lost secret means registering again.
func (c *Client) Register(ctx context.Context, want ClientRegistration) (*RegisteredClient, error) {
	metadata, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	if metadata.RegistrationEndpoint == "" {
		return nil, fmt.Errorf("issuer %s does not support dynamic client registration", metadata.Issuer)
	}

	body, err := json.Marshal(clientRequest(want))
	if err != nil {
		return nil, err
	}

	req, err := c.request(ctx, http.MethodPost, metadata.RegistrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set(serviceKeyHeader, c.cfg.ServiceKey)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registering the OAuth client %q: %w", want.Name, err)
	}
	defer func() { _ = res.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("registering the OAuth client %q: %s: %s",
			want.Name, res.Status, summarize(answer))
	}

	registered := &registrationResponse{}
	if err := json.Unmarshal(answer, registered); err != nil {
		return nil, fmt.Errorf("registering the OAuth client %q: %w", want.Name, err)
	}
	if registered.ClientID == "" || registered.ClientSecret == "" {
		return nil, fmt.Errorf("registering the OAuth client %q: the issuer returned no credentials", want.Name)
	}
	return &RegisteredClient{
		ID:     registered.ClientID,
		Secret: registered.ClientSecret,
		Management: ClientHandle{
			ID:                registered.ClientID,
			RegistrationURI:   registered.RegistrationClientURI,
			RegistrationToken: registered.RegistrationAccessToken,
		},
	}, nil
}

// request builds a request to the identity provider.
//
// Reached at a cluster-internal address, it is still asked for by the issuer's
// hostname: the issuer serves that name, and a virtual host it does not know
// is a good way to be answered by something else entirely.
func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if host := HostHeaderFor(c.cfg, endpoint); host != "" {
		req.Host = host
	}
	return req, nil
}

// HostHeaderFor returns the Host header a request to one of the issuer's
// endpoints should carry, or "" when the address speaks for itself.
//
// The `/kitchen` prefix is one of the addresses that speak for themselves: it
// is served on a listener of its own, which routes on the path alone and has
// no virtual host to be told about.
func HostHeaderFor(cfg Config, endpoint string) string {
	if cfg.BaseURL == cfg.Issuer {
		return ""
	}
	issuer, err := url.Parse(cfg.Issuer)
	if err != nil {
		return ""
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return ""
	}
	target, err := url.Parse(endpoint)
	if err != nil || target.Host != base.Host {
		return ""
	}
	return issuer.Host
}

// rebase moves an endpoint from one origin to another, leaving anything that
// is not on `from` — or not a URL at all — untouched.
func rebase(endpoint, from, to string) string {
	if endpoint == "" || from == to {
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
	parsed.Path = strings.TrimSuffix(base.Path, "/") + parsed.Path
	return parsed.String()
}

// readLimited reads an answer from the issuer, refusing to be handed an
// unbounded one.
func readLimited(body io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, 1<<20))
}

// bytesReader is a reader over a request body, and nil for a request that
// carries none — an empty non-nil reader would have a DELETE announce a body
// it does not have.
func bytesReader(body []byte) io.Reader {
	if body == nil {
		return nil
	}
	return bytes.NewReader(body)
}

// summarize keeps an error message readable when the issuer answers with an
// HTML error page rather than the JSON it promised.
func summarize(body []byte) string {
	const limit = 200
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > limit {
		return text[:limit] + "…"
	}
	if text == "" {
		return "(empty response)"
	}
	return text
}

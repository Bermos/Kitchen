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

package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// RegistryProbe checks registry credentials the way `docker login` does: ask
// `/v2/` with basic auth, and when the registry answers with a Bearer token
// challenge instead of ruling directly, follow it to the auth service with
// the same credentials.
type RegistryProbe struct {
	// BaseURL is the registry's root including the scheme, e.g.
	// "https://harbor.example.com".
	BaseURL  string
	Username string
	Password string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// newRegistryProbe builds the probe from the Connection's config.url and the
// dockerconfigjson secret internal/api writes for the registry provider.
func newRegistryProbe(conn *kitchenv1alpha1.Connection, creds *corev1.Secret) (*RegistryProbe, error) {
	server, base, err := registryBaseURL(conn)
	if err != nil {
		return nil, err
	}
	username, password, err := registryCredentials(creds, server)
	if err != nil {
		return nil, err
	}
	return &RegistryProbe{BaseURL: base, Username: username, Password: password}, nil
}

// registryBaseURL derives the host builds authenticate against from the
// registry prefix images are pushed under: "harbor.example.com/kitchen"
// probes https://harbor.example.com. An explicit http:// prefix is kept — a
// plaintext registry is unusual but real (and it is what tests use).
func registryBaseURL(conn *kitchenv1alpha1.Connection) (server, base string, err error) {
	var cfg struct {
		URL string `json:"url"`
	}
	if conn.Spec.Config != nil {
		if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
			return "", "", fmt.Errorf("invalid dockerRegistry config: %w", err)
		}
	}
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return "", "", fmt.Errorf("dockerRegistry connection %q has no config.url", conn.Name)
	}
	scheme := "https"
	if strings.HasPrefix(raw, "http://") {
		scheme = "http"
	}
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	server, _, _ = strings.Cut(raw, "/")
	return server, scheme + "://" + server, nil
}

// registryCredentials reads the username and password back out of the
// dockerconfigjson, preferring the entry for the server the probe targets.
func registryCredentials(creds *corev1.Secret, server string) (username, password string, err error) {
	raw, ok := creds.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return "", "", fmt.Errorf("credentials secret %q has no %q key", creds.Name, corev1.DockerConfigJsonKey)
	}
	var cfg struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", "", fmt.Errorf("credentials secret %q holds an unparseable dockerconfigjson: %w", creds.Name, err)
	}
	entry, ok := cfg.Auths[server]
	if !ok {
		return "", "", fmt.Errorf("credentials secret %q has no auth entry for %q", creds.Name, server)
	}
	if entry.Username != "" && entry.Password != "" {
		return entry.Username, entry.Password, nil
	}
	// An entry written by hand may only carry auth: base64(user:pass).
	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err == nil {
		if user, pass, found := strings.Cut(string(decoded), ":"); found {
			return user, pass, nil
		}
	}
	return "", "", fmt.Errorf("credentials secret %q entry for %q has neither username/password nor a decodable auth", creds.Name, server)
}

// Probe asks /v2/ with basic auth. A direct 200 or 401 is the registry's own
// ruling; a Bearer challenge delegates the ruling to the named auth service,
// which is where Harbor and Docker Hub actually check passwords.
func (p *RegistryProbe) Probe(ctx context.Context) Result {
	resp, err := p.get(ctx, p.BaseURL+"/v2/")
	if err != nil {
		return unreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode < 300:
		return accepted("registry accepted the credential")
	case resp.StatusCode == http.StatusUnauthorized:
		challenge := resp.Header.Get("WWW-Authenticate")
		if realm, service, ok := bearerChallenge(challenge); ok {
			return p.probeTokenService(ctx, realm, service)
		}
		return rejected(fmt.Sprintf("registry returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	case resp.StatusCode == http.StatusForbidden:
		return rejected(fmt.Sprintf("registry returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	default:
		return unjudged(fmt.Sprintf("registry returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	}
}

// probeTokenService asks the challenge's auth service for a login token, the
// same request `docker login` settles for. Its 200 or 401 is the ruling on
// the credential; the registry itself already answered, so a token service
// that is down leaves the credential unjudged rather than unreachable.
func (p *RegistryProbe) probeTokenService(ctx context.Context, realm, service string) Result {
	tokenURL, err := url.Parse(realm)
	if err != nil {
		return unjudged(fmt.Sprintf("registry sent an unparseable auth realm %q: %s", realm, err))
	}
	if service != "" {
		query := tokenURL.Query()
		query.Set("service", service)
		tokenURL.RawQuery = query.Encode()
	}

	resp, err := p.get(ctx, tokenURL.String())
	if err != nil {
		return unjudged(fmt.Sprintf("registry auth service %s is unreachable: %s", tokenURL.Host, err))
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode < 300:
		return accepted("registry accepted the credential")
	case deniedStatus(resp.StatusCode):
		return rejected(fmt.Sprintf("registry auth service returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	default:
		return unjudged(fmt.Sprintf("registry auth service returned %d: %s", resp.StatusCode, bodySnippet(resp.Body)))
	}
}

func (p *RegistryProbe) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(p.Username, p.Password)
	return httpClientOr(p.HTTPClient).Do(req)
}

// bearerChallenge picks realm and service out of a WWW-Authenticate header
// like `Bearer realm="https://auth.example.com/token",service="registry"`.
func bearerChallenge(header string) (realm, service string, ok bool) {
	scheme, params, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", "", false
	}
	for _, param := range strings.Split(params, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(param), "=")
		if !found {
			continue
		}
		value = strings.Trim(value, `"`)
		switch strings.ToLower(key) {
		case "realm":
			realm = value
		case "service":
			service = value
		}
	}
	return realm, service, realm != ""
}

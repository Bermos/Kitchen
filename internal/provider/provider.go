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

// Package provider generalizes the shape of internal/gitprovider across every
// Connection provider: one implementation per provider, resolved from the
// Connection's spec.provider, credentials taken from the referenced Secret.
// This package owns credential validation — the probe a reconciler runs to
// answer "is the provider reachable, and does the credential work" — while
// git behavior stays in internal/gitprovider. Every probe keeps its API URL
// and HTTP client injectable so tests run against httptest servers.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/cache"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

// ErrNotImplemented marks a provider the CRD enum admits but the platform has
// no implementation for yet. A reconciler reports it as an Unknown condition
// rather than pretending to have checked anything.
var ErrNotImplemented = errors.New("provider not implemented")

// Result is the outcome of one probe. Reachability and credential validity
// are deliberately independent conditions: a registry that is down and a
// password that is wrong must read differently.
type Result struct {
	// Reachable is whether the provider answered at all — any HTTP status
	// counts, only a transport failure clears it.
	Reachable bool
	// CredentialChecked is whether the provider judged the credential. An
	// unreachable provider, or one answering with a server error, has not.
	CredentialChecked bool
	// CredentialValid is whether the provider accepted the credential. Only
	// meaningful when CredentialChecked.
	CredentialValid bool
	// Message carries the provider's own words — who the credential
	// authenticates as on success, the provider's error otherwise. It never
	// contains the credential.
	Message string
	// Warnings are things an accepted credential is nonetheless not allowed
	// to do: a GitHub token that can register the repository's webhook but
	// could not post a commit status. They are not failures — the platform
	// works — and exist so a token is fixed while someone is looking at it
	// rather than at the first request that needs the permission.
	Warnings []string
}

// withWarnings attaches what an accepted credential still cannot do.
func (r Result) withWarnings(warnings ...string) Result {
	r.Warnings = append(r.Warnings, warnings...)
	return r
}

// Probe validates one Connection's credential against the live provider.
type Probe interface {
	Probe(ctx context.Context) Result
}

// Factory builds the Probe for a Connection from its credentials Secret. An
// error means the probe could not even be constructed — a secret missing the
// key the provider needs, or a provider nothing implements. Tests inject
// their own.
type Factory func(conn *kitchenv1alpha1.Connection, creds *corev1.Secret) (Probe, error)

// Default resolves the built-in providers.
func Default(conn *kitchenv1alpha1.Connection, creds *corev1.Secret) (Probe, error) {
	switch conn.Spec.Provider {
	case "github":
		token, err := tokenFrom(creds)
		if err != nil {
			return nil, err
		}
		apiURL, err := configuredAPIURL(conn, "https://api.github.com")
		if err != nil {
			return nil, err
		}
		return &GitHubProbe{APIURL: apiURL, Token: token}, nil
	case "gitlab":
		token, err := tokenFrom(creds)
		if err != nil {
			return nil, err
		}
		apiURL, err := configuredAPIURL(conn, "https://gitlab.com/api/v4")
		if err != nil {
			return nil, err
		}
		return &GitLabProbe{APIURL: apiURL, Token: token}, nil
	case "gitea":
		token, err := tokenFrom(creds)
		if err != nil {
			return nil, err
		}
		apiURL, err := configuredAPIURL(conn, "https://gitea.com/api/v1")
		if err != nil {
			return nil, err
		}
		return &GiteaProbe{APIURL: apiURL, Token: token}, nil
	case "neon":
		token, err := tokenFrom(creds)
		if err != nil {
			return nil, err
		}
		apiURL, err := configuredAPIURL(conn, "https://console.neon.tech/api/v2")
		if err != nil {
			return nil, err
		}
		return &NeonProbe{APIURL: apiURL, Token: token}, nil
	case "inngest":
		token, err := tokenFrom(creds)
		if err != nil {
			return nil, err
		}
		apiURL, err := configuredAPIURL(conn, "https://api.inngest.com/v2")
		if err != nil {
			return nil, err
		}
		return &InngestProbe{APIURL: apiURL, Token: token}, nil
	case "dockerRegistry":
		return newRegistryProbe(conn, creds)
	case objectstore.ProviderS3:
		return newS3Probe(conn, creds)
	case cache.ProviderRedis:
		return newRedisProbe(creds)
	default:
		return nil, fmt.Errorf("%w: %s", ErrNotImplemented, conn.Spec.Provider)
	}
}

// Capabilities is what the platform can actually do through a provider — the
// operator matches Connections on these, never on provider names.
func Capabilities(providerName string) []kitchenv1alpha1.Capability {
	switch providerName {
	case "github":
		return []kitchenv1alpha1.Capability{
			kitchenv1alpha1.CapabilityGitSource,
			kitchenv1alpha1.CapabilityStatusChecks,
		}
	case "gitlab", "gitea":
		return []kitchenv1alpha1.Capability{
			kitchenv1alpha1.CapabilityGitSource,
			kitchenv1alpha1.CapabilityStatusChecks,
		}
	case "dockerRegistry":
		return []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityImageStore}
	case "neon", ProviderCNPG:
		return []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityDatabase}
	case objectstore.ProviderS3:
		return []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityObjectStore}
	case "inngest":
		return []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityBackgroundJobs}
	case cache.ProviderValkey, cache.ProviderRedis:
		return []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityCache}
	default:
		return nil
	}
}

// tokenKey is where the token-authenticated providers keep their credential,
// matching what internal/api writes and internal/controller reads.
const tokenKey = "token"

func tokenFrom(creds *corev1.Secret) (string, error) {
	token := string(creds.Data[tokenKey])
	if token == "" {
		return "", fmt.Errorf("credentials secret %q has no %q key", creds.Name, tokenKey)
	}
	return token, nil
}

// configuredAPIURL returns the Connection config's "apiUrl" (a self-hosted
// instance, or a test server) or the provider's public default.
func configuredAPIURL(conn *kitchenv1alpha1.Connection, fallback string) (string, error) {
	if conn.Spec.Config == nil {
		return fallback, nil
	}
	var cfg struct {
		APIURL string `json:"apiUrl"`
	}
	if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
		return "", fmt.Errorf("invalid %s config: %w", conn.Spec.Provider, err)
	}
	if cfg.APIURL != "" {
		return cfg.APIURL, nil
	}
	return fallback, nil
}

// The Result constructors keep the per-provider probes down to classifying
// HTTP responses.

func unreachable(err error) Result {
	return Result{Message: err.Error()}
}

// unreachableBecause is a provider that could not be reached, where what
// there is to say is a sentence rather than a transport error.
func unreachableBecause(message string) Result {
	return Result{Message: message}
}

func rejected(message string) Result {
	return Result{Reachable: true, CredentialChecked: true, Message: message}
}

func accepted(message string) Result {
	return Result{Reachable: true, CredentialChecked: true, CredentialValid: true, Message: message}
}

// unjudged is a provider that answered but did not rule on the credential —
// a 500, an unparseable body.
func unjudged(message string) Result {
	return Result{Reachable: true, Message: message}
}

func httpClientOr(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return http.DefaultClient
}

// bodySnippet is the leading slice of an error response, enough to quote the
// provider without storing an unbounded body in a condition message.
func bodySnippet(r io.Reader) string {
	snippet, _ := io.ReadAll(io.LimitReader(r, 512))
	return string(snippet)
}

// deniedStatus is a status code that rules on the credential: the provider
// understood the request and turned it down.
func deniedStatus(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden
}

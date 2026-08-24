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

// Package gitprovider abstracts git hosting providers behind a small
// interface so Connections can plug in different implementations. The
// operator matches on the Connection's capabilities; this package supplies
// the gitSource behavior.
package gitprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// ErrUnsupportedProvider is returned by Default for providers without an
// implementation yet.
var ErrUnsupportedProvider = errors.New("unsupported git provider")

// WebhookSpec describes the webhook a Provider should ensure on a repository.
type WebhookSpec struct {
	// URL the provider delivers events to.
	URL string
	// Secret used to sign deliveries.
	Secret string
	// Events to subscribe to, in the provider's vocabulary.
	Events []string
}

// Provider is a git hosting provider bound to one Connection.
type Provider interface {
	// EnsureWebhook idempotently creates or updates a webhook on the
	// repository (in owner/name form) and returns its provider-side ID.
	EnsureWebhook(ctx context.Context, repo string, spec WebhookSpec) (string, error)
	// DeleteWebhook removes a webhook by its provider-side ID. Deleting an
	// already-absent webhook is not an error.
	DeleteWebhook(ctx context.Context, repo, id string) error
}

// Provider names. They are constants because they are also what provider
// claims are attributed to in evidence, and the spellings must stay aligned.
const (
	ProviderGitHub = "github"
	ProviderGitLab = "gitlab"
	ProviderGitea  = "gitea"
)

// Factory builds a Provider for a Connection. The token comes from the
// Connection's credentials secret.
type Factory func(conn *kitchenv1alpha1.Connection, token string) (Provider, error)

// Default resolves the built-in providers.
func Default(conn *kitchenv1alpha1.Connection, token string) (Provider, error) {
	switch conn.Spec.Provider {
	case ProviderGitHub:
		apiURL, err := configuredAPIURL(conn, "https://api.github.com")
		if err != nil {
			return nil, err
		}
		return &GitHub{APIURL: apiURL, Token: token}, nil
	case ProviderGitLab:
		apiURL, err := configuredAPIURL(conn, "https://gitlab.com/api/v4")
		if err != nil {
			return nil, err
		}
		return &GitLab{APIURL: apiURL, Token: token}, nil
	case ProviderGitea:
		// Gitea has no hosted default worth assuming — the audience for it
		// self-hosts — but gitea.com exists and a Connection that names no
		// apiUrl has to resolve to something.
		apiURL, err := configuredAPIURL(conn, "https://gitea.com/api/v1")
		if err != nil {
			return nil, err
		}
		return &Gitea{APIURL: apiURL, Token: token}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProvider, conn.Spec.Provider)
	}
}

// configuredAPIURL is the Connection's own apiUrl, or the provider's hosted
// default. Every provider here is one somebody can self-host, so the override
// is the interesting case rather than the exception.
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
	if cfg.APIURL == "" {
		return fallback, nil
	}
	return cfg.APIURL, nil
}

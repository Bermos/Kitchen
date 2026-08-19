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

package idp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// AccountsPath is the identity provider's account directory: who exists, and
// which account holds a given address.
//
// It is the one part of this package that is not plain OpenID Connect, and it
// is deliberate rather than an oversight. OIDC answers "who is the holder of
// this token" and nothing else — there is no standard way to enumerate
// accounts or to resolve an address to a `sub`, and the platform needs both:
// the operator list is seeded from the accounts that exist, and the
// dashboard's people picker writes an access entry naming a `sub` it has to
// get from somewhere. So this rides on the identity provider the chart ships,
// under a prefix that is Kitchen's, authenticated by the operator's service
// credential; an installation federating to an issuer of its own keeps
// discovery and dynamic client registration, and loses only the seeding.
const AccountsPath = "/kitchen/accounts"

// ErrAccountNotFound is what AccountByEmail returns when nobody holds the
// address. It is an error rather than a nil result so that a caller which
// forgets to check gets an error to propagate instead of a nil dereference.
var ErrAccountNotFound = errors.New("no account with that address")

// ErrNoDirectory says the issuer serves no account directory. It is a
// federated issuer, or one older than this endpoint: everything else about
// the integration still works, and what the platform loses is the ability to
// enumerate accounts. Callers report it rather than retry it.
var ErrNoDirectory = errors.New("the issuer serves no account directory")

// Account is one account at the identity provider.
type Account struct {
	// Subject is the issuer's `sub`: the canonical, opaque identifier an
	// access entry names.
	Subject string `json:"subject"`

	// Email is the account's address, and Name its display name. Both are
	// informational here — an access entry carries the address only so that
	// a list of opaque subjects still reads.
	Email string `json:"email"`
	Name  string `json:"name"`

	// EmailVerified is whether the issuer has confirmed the address. An
	// access entry that names an address rather than a `sub` is honoured
	// only for a verified one, so writing such an entry for an account
	// without this is writing a grant that resolves to nothing.
	EmailVerified bool `json:"emailVerified"`
}

// accountsResponse is what the directory answers a list with.
type accountsResponse struct {
	Accounts []Account `json:"accounts"`
}

// Accounts is every account at the identity provider, excluding the service
// account the operator itself signs in as — it is a credential, not a person.
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	body, err := c.getDirectory(ctx, c.cfg.BaseURL+AccountsPath, "listing the accounts")
	if errors.Is(err, errDirectoryNotFound) {
		return nil, fmt.Errorf("listing the accounts at %s: %w", c.cfg.BaseURL, ErrNoDirectory)
	}
	if err != nil {
		return nil, err
	}
	answer := &accountsResponse{}
	if err := json.Unmarshal(body, answer); err != nil {
		return nil, fmt.Errorf("listing the accounts at %s: %w", c.cfg.BaseURL, err)
	}
	return answer.Accounts, nil
}

// AccountByEmail is the account holding an address, or ErrAccountNotFound.
// The address is matched case-insensitively, addresses being what they are.
func (c *Client) AccountByEmail(ctx context.Context, email string) (*Account, error) {
	endpoint := c.cfg.BaseURL + AccountsPath + "?" + url.Values{"email": []string{email}}.Encode()
	body, err := c.getDirectory(ctx, endpoint, fmt.Sprintf("resolving the account %q", email))
	if errors.Is(err, errDirectoryNotFound) {
		// The directory answers 404 for an address nobody holds. An issuer
		// that has never heard of the prefix answers 404 too, and the caller
		// asking about one account can only act on both the same way.
		return nil, fmt.Errorf("resolving the account %q: %w", email, ErrAccountNotFound)
	}
	if err != nil {
		return nil, err
	}
	account := &Account{}
	if err := json.Unmarshal(body, account); err != nil {
		return nil, fmt.Errorf("resolving the account %q: %w", email, err)
	}
	if account.Subject == "" {
		return nil, fmt.Errorf("resolving the account %q: the issuer named no subject", email)
	}
	return account, nil
}

// getDirectory performs one authenticated read of the account directory.
// what names the operation the error message is about.
func (c *Client) getDirectory(ctx context.Context, endpoint, what string) ([]byte, error) {
	req, err := c.request(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set(serviceKeyHeader, c.cfg.ServiceKey)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return nil, errDirectoryNotFound
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: %s: %s", what, res.Status, summarize(body))
	}
	return body, nil
}

// errDirectoryNotFound is the unwrapped 404, which means one thing to a
// listing and another to a lookup; each caller turns it into the one the
// caller above it can act on.
var errDirectoryNotFound = errors.New("404")

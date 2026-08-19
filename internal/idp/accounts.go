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
	"bytes"
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
// discovery and dynamic client registration, and has to name its operators
// itself rather than have them seeded — see ErrNoDirectory for what that
// means in practice.
const AccountsPath = "/kitchen/accounts"

// ErrAccountNotFound is what AccountByEmail returns when nobody holds the
// address. It is an error rather than a nil result so that a caller which
// forgets to check gets an error to propagate instead of a nil dereference.
var ErrAccountNotFound = errors.New("no account with that address")

// ErrNoDirectory says the issuer serves no account directory. It is a
// federated issuer, or one older than this endpoint: everything else about
// the integration still works — discovery, token validation, dynamic client
// registration — and it is not a fault to be fixed, since OIDC never offered
// account enumeration in the first place.
//
// What it costs is not "the ability to enumerate accounts". It is two things
// that are built on top of enumerating them, and the first is load-bearing:
//
//   - **The operator list cannot be seeded.** The platform's first operator
//     is otherwise the account that exists, and on such an issuer there is no
//     way to ask which one that is — so nothing is written, nobody holds the
//     operator role, and every operator-only route refuses everybody,
//     including the one that names an operator. An installation on a
//     federated issuer has to name its operators at install time, which is
//     what the chart value `kitchen.access.operators` is for.
//   - The dashboard cannot resolve an address to a `sub` when writing a
//     grant. Grants there name a verified email address instead, with the
//     condition that comes with one.
//
// Callers report it rather than retry it: an absent endpoint does not become
// present by being asked again.
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

// getDirectory performs one authenticated read of the directory. `what` names
// the operation the error message is about.
func (c *Client) getDirectory(ctx context.Context, endpoint, what string) ([]byte, error) {
	return c.callDirectory(ctx, http.MethodGet, endpoint, nil, what)
}

// callDirectory performs one authenticated call into the operator's prefix on
// the identity provider, and is the whole of how this package talks to it.
//
// The two statuses that mean something to a caller are lifted out as
// sentinels: a 404 means the thing asked about is not there — or that the
// issuer has never heard of the prefix, which a caller can only act on the
// same way — and a 409 means the thing asked for is already there. Everything
// else is a fault report, carrying the issuer's own words.
func (c *Client) callDirectory(
	ctx context.Context,
	method, endpoint string,
	body []byte,
	what string,
) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := c.request(ctx, method, endpoint, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	req.Header.Set(serviceKeyHeader, c.cfg.ServiceKey)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = res.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return nil, errDirectoryNotFound
	case res.StatusCode == http.StatusConflict:
		return nil, fmt.Errorf("%w: %s", errDirectoryConflict, summarize(answer))
	case res.StatusCode < 200 || res.StatusCode > 299:
		return nil, fmt.Errorf("%s: %s: %s", what, res.Status, summarize(answer))
	}
	return answer, nil
}

// errDirectoryNotFound is the unwrapped 404, which means one thing to a
// listing and another to a lookup; each caller turns it into the one the
// caller above it can act on.
var errDirectoryNotFound = errors.New("404")

// errDirectoryConflict is the unwrapped 409: the name a write asked for is
// taken.
var errDirectoryConflict = errors.New("409")

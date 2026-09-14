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
	"net/url"
	"time"
)

// PersonalKeysPath is where the identity provider keeps somebody's own keys:
// the credentials an account signs its own automation with (#593).
//
// It is a third path beside the project keys' and the platform's, and the
// reason is the same one that made those two separate: what a credential
// *is* is decided by the account it belongs to, and these belong to accounts
// the platform did not create. There is no machine account behind one and
// nothing to create — the account is the person's, and that is the whole of
// the feature and the whole of what has to be bounded about it.
const PersonalKeysPath = "/kitchen/personal-keys"

// ErrNoPersonalKeyDirectory says the issuer serves no personal-key endpoints:
// a federated issuer, or one older than this. It is separate from
// ErrNoKeyDirectory because the features fail independently — an issuer may
// well have shipped CI keys before it shipped these.
var ErrNoPersonalKeyDirectory = errors.New("the issuer issues no personal keys")

// ErrNotAPerson says the account a personal key was asked for is a
// credential's rather than somebody's. It is the issuer's half of the rule
// the API enforces at its own door: a credential does not get a copy of a
// person.
var ErrNotAPerson = errors.New("that account is a credential's, not a person's")

// PersonalKey is one personal key as everything outside the identity provider
// reads it — never its value, which exists in one response and nowhere else.
type PersonalKey struct {
	// Name is what the key is called, and it is unique per account rather
	// than per platform: one person's `laptop` and another's are two
	// credentials.
	Name string `json:"name"`

	// Subject is the account's `sub` — the person's own, which is the point:
	// a token minted from this key carries it, so every role they hold is
	// resolved from it exactly as it is when they sign in.
	Subject string `json:"subject"`

	// Email is that account's address. Informational, as on any other entry.
	Email string `json:"email"`

	// Prefix is the key's first few characters: enough to tell two apart in a
	// list, useless as a credential.
	Prefix string `json:"prefix"`

	// Created is when it was issued, Expires when it stops being honoured —
	// every personal key has one — and LastUsed when it was last exchanged
	// for a token, nil for one nothing has used yet.
	Created  time.Time  `json:"created"`
	Expires  time.Time  `json:"expires"`
	LastUsed *time.Time `json:"lastUsed,omitempty"`
}

// Expired reports whether this key has already lapsed. The issuer refuses one
// on presentation and deletes the row, so this is what a list shows in the
// window between the two.
func (k PersonalKey) Expired(now time.Time) bool {
	return !k.Expires.IsZero() && now.After(k.Expires)
}

// IssuedPersonalKey is a key together with its value, which the issuer hands
// back exactly once.
type IssuedPersonalKey struct {
	PersonalKey
	Secret string `json:"key"`
}

// personalKeysResponse is what the issuer answers a listing with.
type personalKeysResponse struct {
	Keys []PersonalKey `json:"keys"`
}

// PersonalKeys is every personal key of one account, oldest first.
func (c *Client) PersonalKeys(ctx context.Context, subject string) ([]PersonalKey, error) {
	what := "listing an account's personal keys"
	query := url.Values{"subject": []string{subject}}
	body, err := c.callDirectory(ctx, "GET", c.cfg.DirectoryURL+PersonalKeysPath+"?"+query.Encode(), nil, what)
	switch {
	case errors.Is(err, errDirectoryNotFound):
		return nil, fmt.Errorf("%s: %w", what, ErrNoPersonalKeyDirectory)
	case errors.Is(err, errDirectoryRefused):
		// The account is a credential's rather than a person's, which is the
		// only 400 this read can earn: everything else it sends is built
		// here. A credential asking about its own personal keys holds none,
		// and the caller decides whether that is an empty list or a refusal.
		return nil, fmt.Errorf("%s: %w: %s", what, ErrNotAPerson, err)
	case err != nil:
		return nil, err
	}
	answer := &personalKeysResponse{}
	if err := json.Unmarshal(body, answer); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return answer.Keys, nil
}

// CreatePersonalKey issues a personal key for an account, to expire at a time
// the caller chooses.
//
// The expiry is passed rather than defaulted here because there is exactly one
// place a credential's life is decided and it is not this one: the platform's
// own rules say how long, and this is the wire. The issuer refuses a request
// without one, which is the second half of the same statement.
func (c *Client) CreatePersonalKey(
	ctx context.Context,
	subject, name string,
	expires time.Time,
) (*IssuedPersonalKey, error) {
	what := fmt.Sprintf("issuing the personal key %q", name)
	payload, err := json.Marshal(map[string]string{
		"subject": subject,
		"name":    name,
		"expires": expires.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	body, err := c.callDirectory(ctx, "POST", c.cfg.DirectoryURL+PersonalKeysPath, payload, what)
	switch {
	case errors.Is(err, errDirectoryNotFound):
		// The issuer answers 404 both for a prefix it has never heard of and
		// for an account it cannot find. The second cannot happen to a caller
		// the API has just authenticated against this same issuer, so the
		// reading that helps is the first.
		return nil, fmt.Errorf("%s: %w", what, ErrNoPersonalKeyDirectory)
	case errors.Is(err, errDirectoryConflict):
		return nil, fmt.Errorf("%s: %w", what, ErrKeyExists)
	case errors.Is(err, errDirectoryRefused):
		// Every other thing this route refuses with a 400 — a missing
		// subject, a name that is not a label, an expiry that is not a future
		// timestamp — is checked by the API before the call is made, so the
		// one left is the account being a credential's. That is a refusal
		// somebody asked for rather than a fault, and it keeps the issuer's
		// own words.
		return nil, fmt.Errorf("%s: %w: %s", what, ErrNotAPerson, err)
	case err != nil:
		return nil, err
	}
	issued := &IssuedPersonalKey{}
	if err := json.Unmarshal(body, issued); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if issued.Subject == "" || issued.Secret == "" {
		return nil, fmt.Errorf("%s: the issuer returned no credential", what)
	}
	return issued, nil
}

// DeletePersonalKey revokes one personal key of one account, answering what
// was removed. The account stays: it is a person's, and it was there before
// the key was.
func (c *Client) DeletePersonalKey(ctx context.Context, subject, name string) (*PersonalKey, error) {
	what := fmt.Sprintf("revoking the personal key %q", name)
	query := url.Values{"subject": []string{subject}, "name": []string{name}}
	body, err := c.callDirectory(ctx, "DELETE", c.cfg.DirectoryURL+PersonalKeysPath+"?"+query.Encode(), nil, what)
	switch {
	case errors.Is(err, errDirectoryNotFound):
		return nil, fmt.Errorf("%s: %w", what, ErrKeyNotFound)
	case errors.Is(err, errDirectoryRefused):
		return nil, fmt.Errorf("%s: %w: %s", what, ErrNotAPerson, err)
	case err != nil:
		return nil, err
	}
	removed := &PersonalKey{}
	if err := json.Unmarshal(body, removed); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return removed, nil
}

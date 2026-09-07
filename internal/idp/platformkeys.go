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
	"strings"
	"time"
)

// PlatformKeysPath is where the identity provider keeps the platform's own
// machine credentials: the accounts that own them, and the credentials
// themselves.
//
// It is a second path rather than a project on the first, because a platform
// credential is not a project's key and must never be reachable as one. The
// two write through the same machinery at the issuer and are told apart by the
// domain their owning account sits under, which is the only distinction
// anything downstream can make: an access entry names a `sub`, and a `sub`
// says nothing about what kind of account is behind it.
const PlatformKeysPath = "/kitchen/platform-keys"

// PlatformAccountDomain is the reserved domain every platform credential's
// account sits under.
//
// It is deliberately not MachineAccountDomain. A CI key's address is
// `<project>.<key>@machines.kitchen.local`, two labels under one domain, and a
// platform credential could only join it by reserving a project name — which
// would either collide with a project somebody has already created or make
// "is this address a project's key" depend on a list. A second reserved domain
// with a single-label local part cannot collide with the first at all: a
// project name is one DNS label and cannot contain a dot, so no CI key's
// address is ever shaped like a platform credential's and no platform
// credential's is ever shaped like a CI key's.
//
// Like MachineAccountDomain it resolves nothing and grants nothing. What a
// platform credential may do is `spec.access.credentials` on the Kitchen
// singleton, resolved from the subject alone. This constant decides two other
// things: that such an account is never counted as a person (so it cannot seed
// itself into the operator list on upgrade, and cannot create a project), and
// that a credential can be displayed by the name it was issued under.
const PlatformAccountDomain = "platform.kitchen.local"

// ErrNoPlatformKeyDirectory says the issuer serves no platform-credential
// endpoints: a federated issuer, or one older than this. It is separate from
// ErrNoKeyDirectory because the two features fail independently — an issuer
// may well have shipped CI keys before it shipped these.
var ErrNoPlatformKeyDirectory = errors.New("the issuer issues no platform credentials")

// PlatformKey is one platform credential as everything outside the identity
// provider reads it — never its value, which exists in one response and
// nowhere else.
type PlatformKey struct {
	// Name is what the credential is called, and the local part of its
	// account's address.
	Name string `json:"name"`

	// Subject is the account's `sub`, and is what
	// `spec.access.credentials` on the Kitchen singleton names.
	Subject string `json:"subject"`

	// Email is the account's address under PlatformAccountDomain.
	// Informational, like the address on any other access entry, and
	// deliberately unverified at the issuer so that nothing can resolve
	// against it.
	Email string `json:"email"`

	// Prefix is the credential's first few characters: enough to tell two
	// apart in a list, useless as a credential.
	Prefix string `json:"prefix"`

	// Created is when it was issued, and LastUsed when it was last exchanged
	// for a token. LastUsed is nil for one nothing has used yet.
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"lastUsed,omitempty"`
}

// IssuedPlatformKey is a credential together with its value, which the issuer
// hands back exactly once.
type IssuedPlatformKey struct {
	PlatformKey
	Secret string `json:"key"`
}

// platformKeysResponse is what the issuer answers a listing with.
type platformKeysResponse struct {
	Keys []PlatformKey `json:"keys"`
}

// PlatformKeys is every platform credential this installation has issued,
// oldest first.
func (c *Client) PlatformKeys(ctx context.Context) ([]PlatformKey, error) {
	what := "listing the platform's credentials"
	body, err := c.callDirectory(ctx, "GET", c.cfg.DirectoryURL+PlatformKeysPath, nil, what)
	if errors.Is(err, errDirectoryNotFound) {
		return nil, fmt.Errorf("%s: %w", what, ErrNoPlatformKeyDirectory)
	}
	if err != nil {
		return nil, err
	}
	answer := &platformKeysResponse{}
	if err := json.Unmarshal(body, answer); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return answer.Keys, nil
}

// CreatePlatformKey issues a platform credential and the account that owns it.
//
// The value in the result is the only copy that will ever exist outside the
// caller, exactly as for a CI key: a credential the caller then fails to make
// use of is one to delete, not one to look up again.
func (c *Client) CreatePlatformKey(ctx context.Context, name string) (*IssuedPlatformKey, error) {
	what := fmt.Sprintf("issuing the platform credential %q", name)
	payload, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	body, err := c.callDirectory(ctx, "POST", c.cfg.DirectoryURL+PlatformKeysPath, payload, what)
	switch {
	case errors.Is(err, errDirectoryNotFound):
		return nil, fmt.Errorf("%s: %w", what, ErrNoPlatformKeyDirectory)
	case errors.Is(err, errDirectoryConflict):
		return nil, fmt.Errorf("%s: %w", what, ErrKeyExists)
	case err != nil:
		return nil, err
	}
	issued := &IssuedPlatformKey{}
	if err := json.Unmarshal(body, issued); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if issued.Subject == "" || issued.Secret == "" {
		return nil, fmt.Errorf("%s: the issuer returned no credential", what)
	}
	return issued, nil
}

// DeletePlatformKey revokes a platform credential and removes the account that
// owned it, answering what was removed so the caller can take its grant off
// the singleton.
func (c *Client) DeletePlatformKey(ctx context.Context, name string) (*PlatformKey, error) {
	what := fmt.Sprintf("revoking the platform credential %q", name)
	query := url.Values{"name": []string{name}}
	body, err := c.callDirectory(ctx, "DELETE", c.cfg.DirectoryURL+PlatformKeysPath+"?"+query.Encode(), nil, what)
	if errors.Is(err, errDirectoryNotFound) {
		return nil, fmt.Errorf("%s: %w", what, ErrKeyNotFound)
	}
	if err != nil {
		return nil, err
	}
	removed := &PlatformKey{}
	if err := json.Unmarshal(body, removed); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return removed, nil
}

// IsPlatformAccount reports whether an address belongs to an account holding a
// platform credential rather than to a person or to a project's CI key.
//
// Like IsMachineAccount it reads an address and not a subject, and an access
// entry carrying no address therefore reads as a person's — the platform never
// calls something a credential on a guess.
func IsPlatformAccount(email string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(email)), "@"+PlatformAccountDomain)
}

// PlatformKeyName is the name a platform credential's address carries, and
// false for any other address. The local part is a single DNS label, which is
// what keeps it distinguishable from a CI key's two.
func PlatformKeyName(email string) (string, bool) {
	if !IsPlatformAccount(email) {
		return "", false
	}
	local := strings.ToLower(strings.TrimSpace(email))
	name := local[:len(local)-len(PlatformAccountDomain)-1]
	if name == "" || strings.Contains(name, ".") {
		return "", false
	}
	return name, true
}

// PlatformAddress is the address the account owning a named platform
// credential is created as. It is here rather than only at the issuer because
// the operator has to be able to recognise and display one.
func PlatformAddress(name string) string {
	return name + "@" + PlatformAccountDomain
}

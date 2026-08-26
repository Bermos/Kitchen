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

// KeysPath is where the identity provider keeps a project's CI keys: the
// machine accounts that own them, and the credentials themselves.
//
// It rides on the same Kitchen-owned prefix as the account directory, and for
// the same reason — OpenID Connect has nothing to say about issuing a
// long-lived credential to a machine. What is different is that this one
// writes: a key can only exist where a key is verified, which is the issuer,
// and revoking one has to take effect in the same one place.
const KeysPath = "/kitchen/keys"

// MachineAccountDomain is the reserved domain every machine account's address
// sits under.
//
// The operator does not need this to *use* a key — it is handed the machine
// account's `sub`, and a grant naming a `sub` is a grant like any other. It
// needs it to *read one back*: a grant in `spec.access` carries the account's
// address beside its subject so the list is legible, and telling a key's grant
// from a person's is what lets the members list say which it is looking at.
//
// It resolves no role and it grants nothing: internal/access answers "what may
// this caller do" from the subject alone and knows nothing about this. There
// is one route that asks a different question — `POST /api/v1/projects`, whose
// caller becomes the new project's admin, and which therefore refuses a
// machine account outright (`Caller.isMachine` in internal/api, docs/AUTH.md
// "Machine accounts"). That is a refusal rather than a grant, in the one place
// where a credential could otherwise widen its own access, and it is the whole
// of what this constant decides.
const MachineAccountDomain = "machines.kitchen.local"

// ErrKeyNotFound is what a delete answers when the project has no key by that
// name — or when the issuer serves no key endpoints at all, which a caller
// asking about one key can only act on the same way.
var ErrKeyNotFound = errors.New("no such key")

// ErrKeyExists is what a create answers when the name is taken. One name per
// project, because the name is what the key is addressed by.
var ErrKeyExists = errors.New("that project already has a key with that name")

// ErrNoKeyDirectory says the issuer serves no key endpoints: a federated
// issuer, or one older than this. Discovery and client registration still
// work; what the platform loses is the ability to issue a CI credential.
var ErrNoKeyDirectory = errors.New("the issuer issues no CI keys")

// Key is one CI key as everything outside the identity provider reads it —
// never its value, which exists in one response and nowhere else.
type Key struct {
	// Name is what the key is called, and Project the project it was made
	// for. Together they address it: they are the two halves of the machine
	// account's own address at the issuer.
	Name    string `json:"name"`
	Project string `json:"project"`

	// Subject is the machine account's `sub`, and is what the project's
	// `spec.access` names. It is the only part of a key the platform's
	// authorization ever sees.
	Subject string `json:"subject"`

	// Email is the machine account's address. Informational, like the address
	// on any other access entry — nothing resolves against it, and it is
	// deliberately unverified at the issuer so that nothing can.
	Email string `json:"email"`

	// Prefix is the key's first few characters, which is all that is kept of
	// the value: enough to tell two keys apart in a list, useless as a
	// credential.
	Prefix string `json:"prefix"`

	// Created is when the key was issued, and LastUsed when it was last
	// exchanged for a token. LastUsed is nil for a key nothing has used yet,
	// which is a different statement from "used at the zero time".
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"lastUsed,omitempty"`
}

// IssuedKey is a key together with its value, which the issuer hands back
// exactly once. Nothing can read it again: it is stored hashed, so a lost key
// is a key to delete and reissue.
type IssuedKey struct {
	Key
	Secret string `json:"key"`
}

// keysResponse is what the issuer answers a listing with.
type keysResponse struct {
	Keys []Key `json:"keys"`
}

// Keys is every CI key belonging to one project, oldest first.
func (c *Client) Keys(ctx context.Context, project string) ([]Key, error) {
	what := fmt.Sprintf("listing %s's keys", project)
	endpoint := c.cfg.BaseURL + KeysPath + "?" + url.Values{"project": []string{project}}.Encode()
	body, err := c.callDirectory(ctx, "GET", endpoint, nil, what)
	if errors.Is(err, errDirectoryNotFound) {
		return nil, fmt.Errorf("%s: %w", what, ErrNoKeyDirectory)
	}
	if err != nil {
		return nil, err
	}
	answer := &keysResponse{}
	if err := json.Unmarshal(body, answer); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return answer.Keys, nil
}

// CreateKey issues a key for a project, and the machine account that owns it.
//
// The value in the result is the only copy that will ever exist outside the
// caller. A key the caller then fails to make use of is a key to delete, not
// one to look up again.
func (c *Client) CreateKey(ctx context.Context, project, name string) (*IssuedKey, error) {
	what := fmt.Sprintf("issuing the key %q for %s", name, project)
	payload, err := json.Marshal(map[string]string{"project": project, "name": name})
	if err != nil {
		return nil, err
	}
	body, err := c.callDirectory(ctx, "POST", c.cfg.BaseURL+KeysPath, payload, what)
	switch {
	case errors.Is(err, errDirectoryNotFound):
		return nil, fmt.Errorf("%s: %w", what, ErrNoKeyDirectory)
	case errors.Is(err, errDirectoryConflict):
		return nil, fmt.Errorf("%s: %w", what, ErrKeyExists)
	case err != nil:
		return nil, err
	}
	issued := &IssuedKey{}
	if err := json.Unmarshal(body, issued); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if issued.Subject == "" || issued.Secret == "" {
		return nil, fmt.Errorf("%s: the issuer returned no credential", what)
	}
	return issued, nil
}

// DeleteKey revokes a key and removes the machine account that owned it,
// answering what was removed so the caller can take its grant off the project.
func (c *Client) DeleteKey(ctx context.Context, project, name string) (*Key, error) {
	what := fmt.Sprintf("revoking the key %q of %s", name, project)
	query := url.Values{"project": []string{project}, "name": []string{name}}
	body, err := c.callDirectory(ctx, "DELETE", c.cfg.BaseURL+KeysPath+"?"+query.Encode(), nil, what)
	if errors.Is(err, errDirectoryNotFound) {
		return nil, fmt.Errorf("%s: %w", what, ErrKeyNotFound)
	}
	if err != nil {
		return nil, err
	}
	removed := &Key{}
	if err := json.Unmarshal(body, removed); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return removed, nil
}

// IsMachineAccount reports whether an address belongs to a machine account
// holding a CI key rather than to a person.
//
// It reads an address, not a subject: the subject is opaque and says nothing
// about what kind of account it is. An access entry that carries no address
// therefore reads as a person's, which is the right way round — the platform
// should never call something a robot on a guess.
func IsMachineAccount(email string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(email)), "@"+MachineAccountDomain)
}

// MachineIdentity is the project and key name a machine account's address
// names, and false for any other address. The local part is `<project>.<key>`,
// both DNS labels, so the split is unambiguous.
func MachineIdentity(email string) (project, key string, ok bool) {
	if !IsMachineAccount(email) {
		return "", "", false
	}
	local := strings.ToLower(strings.TrimSpace(email))
	local = local[:len(local)-len(MachineAccountDomain)-1]
	project, key, ok = strings.Cut(local, ".")
	if !ok || project == "" || key == "" || strings.Contains(key, ".") {
		return "", "", false
	}
	return project, key, true
}

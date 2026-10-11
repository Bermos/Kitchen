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

// Package registrytoken is the token service the bundled registry trusts.
//
// zot is configured for the Docker token flow
// (https://distribution.github.io/distribution/spec/auth/token/): a client
// that asks it anything without a token is told to fetch one from the realm,
// which is this service; it authenticates there with the username and
// password its docker config holds, asks for the repositories and actions it
// wants, and gets back a JWT naming the ones it may have. zot checks the
// token's signature against the certificate it was given and the request
// against the token's `access` claim, and nothing else — which is what lets
// an account that zot has never heard of be scoped to one project.
//
// It exists because a registry account per project is the only way to keep
// one project's credential out of another project's images, and zot's own
// accounts are files it reads at startup: adding one per project created
// would be a registry restart per project created.
package registrytoken

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Actions a registry token can carry. zot asks for these three and no other.
const (
	ActionPull   = "pull"
	ActionPush   = "push"
	ActionDelete = "delete"
)

// issuer is the `iss` of every token. zot does not check it; it is there so a
// token found in a log says what minted it.
const issuer = "kitchen"

// Grant is what one account may do to the registry.
type Grant struct {
	// Subject is the account's username, the token's `sub`.
	Subject string

	// Everywhere is what the account may do to any repository at all. It is
	// the platform's own two accounts' grant and nobody else's.
	Everywhere []string

	// Project is the project whose repositories this account reaches: its
	// own name and everything under `<project>/`. ProjectActions is what it
	// may do there.
	Project        string
	ProjectActions []string

	// Legacy are repositories outside the project's own name that it may
	// still pull: the `<project>-<workload>` images it pushed before its
	// workloads moved under its name. They are read-only and named one by
	// one, from the Releases that run them, so nothing new can be added to
	// the list by naming a repository.
	Legacy map[string]bool
}

// allowed is what this grant permits of what was asked for one repository.
func (g Grant) allowed(repository string, asked []string) []string {
	var permitted []string
	switch {
	case len(g.Everywhere) > 0:
		permitted = g.Everywhere
	case g.Project != "" && (repository == g.Project || strings.HasPrefix(repository, g.Project+"/")):
		permitted = g.ProjectActions
	case g.Legacy[repository]:
		permitted = []string{ActionPull}
	default:
		return nil
	}
	var granted []string
	for _, action := range asked {
		if action == "*" {
			for _, each := range permitted {
				if !slices.Contains(granted, each) {
					granted = append(granted, each)
				}
			}
			continue
		}
		if slices.Contains(permitted, action) && !slices.Contains(granted, action) {
			granted = append(granted, action)
		}
	}
	return granted
}

// ResourceAccess is one entry of a token's `access` claim, as the
// distribution spec and zot spell it.
type ResourceAccess struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// repositoryName is the distribution spec's grammar for a repository name. A
// scope naming anything else is not granted, whatever the account: the prefix
// check below is only a boundary for names this grammar admits.
var repositoryName = regexp.MustCompile(
	`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)

// parseScope reads one `scope` parameter: `repository:<name>:<actions>`. The
// name may itself hold a colon in principle (a registry host with a port), so
// the type is cut at the first and the actions at the last. Anything that is
// not a repository scope — `registry:catalog:*` — is ignored: nothing in the
// platform lists the catalog, and nothing is granted for it.
func parseScope(scope string) (name string, actions []string, ok bool) {
	kind, rest, found := strings.Cut(scope, ":")
	if !found || kind != "repository" {
		return "", nil, false
	}
	separator := strings.LastIndex(rest, ":")
	if separator <= 0 {
		return "", nil, false
	}
	name, list := rest[:separator], rest[separator+1:]
	if !repositoryName.MatchString(name) {
		return "", nil, false
	}
	for _, action := range strings.Split(list, ",") {
		if action = strings.TrimSpace(action); action != "" {
			actions = append(actions, action)
		}
	}
	return name, actions, len(actions) > 0
}

// accessFor is the `access` claim for every scope a request asked for,
// narrowed to what the grant permits. A scope that is granted nothing is left
// out rather than refused: the token flow's answer to "you may not" is a
// token without it, and the registry's own 401 that follows.
func accessFor(grant Grant, scopes []string) []ResourceAccess {
	access := []ResourceAccess{}
	for _, raw := range scopes {
		// A client may send several scopes in one parameter, space-separated.
		for _, scope := range strings.Fields(raw) {
			name, asked, ok := parseScope(scope)
			if !ok {
				continue
			}
			granted := grant.allowed(name, asked)
			if len(granted) == 0 {
				continue
			}
			merged := false
			for i := range access {
				if access[i].Name == name {
					for _, action := range granted {
						if !slices.Contains(access[i].Actions, action) {
							access[i].Actions = append(access[i].Actions, action)
						}
					}
					merged = true
				}
			}
			if !merged {
				access = append(access, ResourceAccess{Type: "repository", Name: name, Actions: granted})
			}
		}
	}
	return access
}

// claims is the token's payload.
type claims struct {
	Issuer    string           `json:"iss"`
	Subject   string           `json:"sub"`
	Audience  string           `json:"aud,omitempty"`
	ExpiresAt int64            `json:"exp"`
	NotBefore int64            `json:"nbf"`
	IssuedAt  int64            `json:"iat"`
	ID        string           `json:"jti"`
	Access    []ResourceAccess `json:"access"`
}

// clockSkew is how far back a token's start is dated. zot refuses a token
// issued in its future, and the operator and the registry run on different
// nodes with clocks of their own.
const clockSkew = 30 * time.Second

// sign mints a token: RS256 over the claims, which is what the chart's
// certificate verifies and what zot accepts.
func sign(key *rsa.PrivateKey, service string, grant Grant, access []ResourceAccess, now time.Time, ttl time.Duration) (string, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": keyID(&key.PublicKey)})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims{
		Issuer:    issuer,
		Subject:   grant.Subject,
		Audience:  service,
		ExpiresAt: now.Add(ttl).Unix(),
		NotBefore: now.Add(-clockSkew).Unix(),
		IssuedAt:  now.Add(-clockSkew).Unix(),
		ID:        hex.EncodeToString(id),
		Access:    access,
	})
	if err != nil {
		return "", err
	}
	encoding := base64.RawURLEncoding
	signed := encoding.EncodeToString(header) + "." + encoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signed + "." + encoding.EncodeToString(signature), nil
}

// keyID names the signing key in the token's header, so a token minted under
// a key that has since been rotated says so when somebody reads it.
func keyID(key *rsa.PublicKey) string {
	sum := sha256.Sum256(key.N.Bytes())
	return hex.EncodeToString(sum[:8])
}

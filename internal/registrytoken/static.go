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

package registrytoken

import (
	"context"
	"crypto/rsa"
)

// StaticAccounts is a fixed set of accounts and a key: what the tests and the
// registry check in hack/ run the token service with, where there is no
// cluster to read the real ones from.
type StaticAccounts struct {
	Key *rsa.PrivateKey
	// Accounts maps a username to its password and grant. The grant's
	// Subject is filled in from the username.
	Accounts map[string]StaticAccount
}

// StaticAccount is one entry of StaticAccounts.
type StaticAccount struct {
	Password string
	Grant    Grant
}

// PlatformGrant, ReadGrant and ProjectGrant are the three shapes an account
// on the bundled registry takes — the same ones ClusterAccounts answers.
func PlatformGrant() Grant { return Grant{Everywhere: []string{ActionPull, ActionPush, ActionDelete}} }
func ReadGrant() Grant     { return Grant{Everywhere: []string{ActionPull}} }
func ProjectGrant(project string, push bool) Grant {
	actions := []string{ActionPull}
	if push {
		actions = append(actions, ActionPush)
	}
	return Grant{Project: project, ProjectActions: actions}
}

// Authenticate implements Accounts.
func (s *StaticAccounts) Authenticate(_ context.Context, username, password string) (*Grant, error) {
	account, ok := s.Accounts[username]
	if !ok || !matches([]byte(account.Password), password) {
		return nil, nil
	}
	grant := account.Grant
	grant.Subject = username
	return &grant, nil
}

// SigningKey implements Accounts.
func (s *StaticAccounts) SigningKey(context.Context) (*rsa.PrivateKey, error) { return s.Key, nil }

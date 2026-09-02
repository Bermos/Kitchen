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

package cache

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// The tenancy half of the in-cluster provider needs three things from a
// server that is already running: make a tenant, unmake one, and empty what
// one left behind. They are behind an interface for the reason
// objectstore's AdminAPI is: what the provisioner *does* is then tested
// without a server, and what this does is a wrapper each.

// Tenant is one claim's tenancy in a shared server, as the server is told
// about it.
type Tenant struct {
	// Username is the ACL user, and the name every operation addresses the
	// tenancy by.
	Username string
	// Password is the tenant's own, minted by the provisioner and kept in a
	// Secret so that ensuring the same tenant twice is the same tenant.
	Password string
	// Prefix is what the tenant's keys and pub/sub channels have to start
	// with. The ACL admits nothing outside it.
	Prefix string
}

// Keyspace is a running server the provisioner makes tenancies in.
type Keyspace interface {
	// EnsureTenant creates the tenant, or resets an existing one to exactly
	// this password and prefix. It is called on every reconcile, which is
	// what puts a tenancy back after a server has been rebuilt from an
	// empty volume.
	EnsureTenant(ctx context.Context, tenant Tenant) error
	// RemoveTenant deletes the ACL user and nothing else: the keys under
	// its prefix are the claim's data, and destroying those is what
	// deletionPolicy Delete opts into.
	RemoveTenant(ctx context.Context, username string) error
	// DeleteKeys removes every key under the prefix. It is the destructive
	// half, called for a preview's tenancy and for a claim under
	// deletionPolicy Delete.
	DeleteKeys(ctx context.Context, prefix string) error
	// Close releases the connection.
	Close() error
}

// KeyspaceFactory dials one server. address is host:port; password is the
// server's default user's, which the provisioner minted and holds.
type KeyspaceFactory func(address, password string) (Keyspace, error)

// tenantRules are the commands a tenancy may run, beyond the keys and
// channels its prefix admits.
//
// `+@all -@admin -@dangerous` is the whole boundary and each half earns its
// place: -@admin removes CONFIG, REPLICAOF, SHUTDOWN and the rest of the
// server's own controls, and -@dangerous removes the commands that reach
// past a key pattern — FLUSHALL and FLUSHDB, which are the objection this
// shape had to answer, plus KEYS, SWAPDB, MONITOR and CLIENT KILL.
//
// +info is added back deliberately. It is @dangerous because it reports on
// the whole server, and every queue library in circulation calls it on
// connect; a tenancy that refuses it is a tenancy Sidekiq and BullMQ cannot
// use, which would make the shape unusable for exactly the workloads it
// exists for. What it discloses is the server's own statistics, not another
// tenant's keys.
var tenantRules = []string{"+@all", "-@admin", "-@dangerous", "+info"}

// aclRules is the whole ACL SETUSER line for a tenant.
//
// It begins with `reset` so that the rule is a statement of what the tenant
// is rather than an accumulation: ensuring the same tenant twice leaves
// exactly these rules, and a rule this platform stops granting is gone the
// next time the claim reconciles rather than surviving in a user nobody
// rewrites.
func aclRules(tenant Tenant) []string {
	rules := []string{
		"reset",
		"on",
		">" + tenant.Password,
		"~" + tenant.Prefix + "*",
		"&" + tenant.Prefix + "*",
	}
	return append(rules, tenantRules...)
}

// valkeyKeyspace is Keyspace over go-redis.
type valkeyKeyspace struct{ client *redis.Client }

// DialKeyspace is the real client, and the default when a Connection does
// not inject one.
func DialKeyspace(address, password string) (Keyspace, error) {
	if strings.TrimSpace(address) == "" {
		return nil, fmt.Errorf("a shared server needs an address to be reached at")
	}
	return &valkeyKeyspace{client: redis.NewClient(&redis.Options{Addr: address, Password: password})}, nil
}

func (k *valkeyKeyspace) EnsureTenant(ctx context.Context, tenant Tenant) error {
	args := []any{"acl", "setuser", tenant.Username}
	for _, rule := range aclRules(tenant) {
		args = append(args, rule)
	}
	if err := k.client.Do(ctx, args...).Err(); err != nil {
		return fmt.Errorf("granting %s its tenancy: %w", tenant.Username, err)
	}
	return k.save(ctx)
}

func (k *valkeyKeyspace) RemoveTenant(ctx context.Context, username string) error {
	if err := k.client.Do(ctx, "acl", "deluser", username).Err(); err != nil {
		return fmt.Errorf("removing the tenancy %s: %w", username, err)
	}
	return k.save(ctx)
}

// save writes the ACL users to the server's ACL file. Without it every
// tenancy would live only in the running process's memory, and a restart —
// a node drained, an image bumped — would leave every application on the
// server authenticating as a user that no longer exists.
func (k *valkeyKeyspace) save(ctx context.Context) error {
	if err := k.client.Do(ctx, "acl", "save").Err(); err != nil {
		return fmt.Errorf("saving the server's ACL file: %w", err)
	}
	return nil
}

// DeleteKeys removes the prefix's keys in batches, over SCAN rather than
// KEYS: the server is shared, and a KEYS over a keyspace other projects are
// reading blocks all of them for as long as it takes.
func (k *valkeyKeyspace) DeleteKeys(ctx context.Context, prefix string) error {
	const batch = 500
	var cursor uint64
	for {
		keys, next, err := k.client.Scan(ctx, cursor, prefix+"*", batch).Result()
		if err != nil {
			return fmt.Errorf("scanning %s*: %w", prefix, err)
		}
		if len(keys) > 0 {
			if err := k.client.Unlink(ctx, keys...).Err(); err != nil {
				return fmt.Errorf("deleting the keys under %s*: %w", prefix, err)
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

func (k *valkeyKeyspace) Close() error { return k.client.Close() }

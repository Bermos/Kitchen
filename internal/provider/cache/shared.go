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
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The shared half of the in-cluster provider: a tenancy in a server the
// platform already runs, rather than a server of the claim's own.
//
// Two servers, not one, and that is the point. maxmemory-policy is
// server-wide, so a single shared server would have to offer one eviction
// policy to a cache and a queue alike — which is the objection that made
// "one instance per claim" look like the only honest answer. It is not: the
// policy belongs to the *server*, so the platform runs one per usage and the
// claim's usage picks which it joins. A cache tenancy lands in an evicting
// server; a queue tenancy lands in one that refuses writes when it is full
// and appends every write to disk.
//
// What remains genuinely unshareable is a memory limit for one tenant, and
// that is what sends a claim to a server of its own — see ResolveTenancy.

const (
	// The shared servers, one per usage. They are named without the
	// "kitchen-" prefix every claim's instance carries (instanceName in the
	// reconciler), so a claim called `shared-cache` cannot collide with one.
	sharedCacheServer = "shared-cache"
	sharedQueueServer = "shared-queue"

	// sharedIDScheme marks an instance ID as a tenancy rather than a server:
	// "shared:<namespace>/<server>/<user>". Instance.ID was always opaque
	// and this is what that buys — a tenancy handle needs no second
	// interface, only a second thing to put in the same field.
	sharedIDScheme = "shared:"

	// tenantSecretSuffix names the Secret a tenant's own password is kept
	// in, beside the servers' in the cache namespace. It is kept so that
	// ensuring the same tenancy twice grants the same password, and so that
	// a claim recreated over retained keys can read them again.
	tenantSecretSuffix = "-tenant"

	// aclFilePath is where a shared server keeps its users. It is on the
	// server's volume rather than in memory because ACL users created with
	// ACL SETUSER do not survive a restart, and a restart that forgot every
	// tenancy would leave every application on the server authenticating as
	// a user that no longer exists.
	aclFilePath = "/data/users.acl"
)

// SharedServerName is the server a usage's tenancies live in.
func SharedServerName(usage Usage) string {
	if usage.Durable() {
		return sharedQueueServer
	}
	return sharedCacheServer
}

// sharedID is a tenancy as Instance.ID carries it.
func sharedID(namespace, server, username string) string {
	return sharedIDScheme + namespace + "/" + server + "/" + username
}

// parseSharedID takes a tenancy handle apart; ok is false for an ID that is
// a server's rather than a tenancy's, which is how every operation tells the
// two shapes apart without the claim having to say.
func parseSharedID(instanceID string) (namespace, server, username string, ok bool) {
	rest, found := strings.CutPrefix(instanceID, sharedIDScheme)
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// tenantPrefix is what a tenancy's keys and channels have to start with. The
// colon is the convention every client library's prefix option assumes, and
// nothing else in this package puts one in a name.
func tenantPrefix(username string) string { return username + ":" }

// tenantBranchName is a preview's tenancy beside the claim's.
//
// The separator is a dot, which a claim name cannot contain — claim names
// are DNS labels — so a preview's tenancy can never be spelled the same way
// as some other claim's. For a server of its own the separator has to stay a
// hyphen, because that name is a Service's and a dot is not a DNS label;
// here the name is only an ACL user's, and the stronger separator is free.
func tenantBranchName(username, environment string) string {
	return truncateName(username + "." + environment)
}

// tenantSecretName is where a tenant's password is kept.
func tenantSecretName(username string) string {
	return truncateName(username) + tenantSecretSuffix
}

// ResolveTenancy answers which shape serves a claim, and why.
//
// The rules, in order:
//
//   - A claim that named a shape gets it, unless it named `shared` and asked
//     for something a shared server cannot give — then it is refused as a
//     claim, with the thing it asked for named, rather than quietly given
//     the other shape.
//   - A claim that named none gets a server of its own exactly when it asked
//     for something a shared server cannot give.
//   - Otherwise it gets a tenancy. That is the default because a server per
//     claim per environment does not fit on the clusters this platform
//     supports, which is the whole of #275.
func (v *Valkey) ResolveTenancy(req Requirements) (Tenancy, string, error) {
	if req.Tenancy != "" && !req.Tenancy.Known() {
		return "", "", fmt.Errorf("%w: tenancy %q is not one of %s", ErrUnsatisfiable, req.Tenancy, tenancyList())
	}
	unshareable := v.unshareable(req)
	switch req.Tenancy {
	case TenancyDedicated:
		return TenancyDedicated, "the claim asked for a server of its own", nil
	case TenancyShared:
		if unshareable != "" {
			return "", "", fmt.Errorf("%w: the claim asks to share the platform's server and %s. Drop that "+
				"from the claim, or ask for tenancy %s and be given a server of its own",
				ErrUnsatisfiable, unshareable, TenancyDedicated)
		}
		return TenancyShared, "the claim asked to share the platform's server", nil
	}
	if unshareable != "" {
		return TenancyDedicated, unshareable, nil
	}
	return TenancyShared, "", nil
}

// unshareable names what the claim asked for that a tenancy cannot carry,
// and is empty when there is nothing.
//
// There are two, and neither is a policy choice. A memory limit is the
// server's own configuration and Redis has no per-user quota, so a tenant
// cannot be held to one. A version is the server's binary, and the shared
// servers run one.
func (v *Valkey) unshareable(req Requirements) string {
	if req.MaxMemory != "" {
		return "a memory limit is the whole server's, and a shared server cannot hold one tenant to one"
	}
	if req.Version != "" && req.Version != v.sharedMajor() {
		return fmt.Sprintf("it asks for Valkey %s and the platform's shared servers run %s",
			req.Version, v.sharedMajor())
	}
	return ""
}

// sharedMajor is the version the shared servers run: the connection's
// default, which is what a claim naming no version would have been given
// anyway.
func (v *Valkey) sharedMajor() string { return DefaultValkeyMajor }

func tenancyList() string {
	names := make([]string, 0, len(Tenancies))
	for _, tenancy := range Tenancies {
		names = append(names, string(tenancy))
	}
	return strings.Join(names, " or ")
}

// provisionTenancy gives the claim an ACL user restricted to a prefix in the
// shared server for its usage, creating that server the first time anybody
// asks for one.
func (v *Valkey) provisionTenancy(ctx context.Context, name string, req Requirements, why string) (Instance, error) {
	tenant, server, err := v.ensureTenancy(ctx, name, req.Usage)
	if err != nil {
		return Instance{}, err
	}
	return Instance{
		ID:      sharedID(v.Namespace, server, tenant.Username),
		Binding: v.tenantBinding(server, tenant),
		// A tenancy the platform provisions for a project holds that
		// project's own data, and the claim's is production's. Only a
		// preview's is synthetic, and CreateBranch is where that is said.
		Provenance:  ProvenanceProduction,
		Tenancy:     TenancyShared,
		TenancyNote: sharedNote(server, tenant.Prefix, why),
	}, nil
}

// createTenancyBranch gives a preview environment a tenancy of its own in
// the same shared server: its own ACL user, its own prefix, and none of
// production's keys.
func (v *Valkey) createTenancyBranch(ctx context.Context, instanceID, environment string) (Branch, error) {
	_, server, username, ok := parseSharedID(instanceID)
	if !ok {
		return Branch{}, fmt.Errorf("%q is not a tenancy id", instanceID)
	}
	usage := usageOfServer(server)
	tenant, server, err := v.ensureTenancy(ctx, tenantBranchName(username, environment), usage)
	if err != nil {
		return Branch{}, err
	}
	return Branch{
		ID:          sharedID(v.Namespace, server, tenant.Username),
		Binding:     v.tenantBinding(server, tenant),
		Provenance:  ProvenanceSynthetic,
		Tenancy:     TenancyShared,
		TenancyNote: sharedNote(server, tenant.Prefix, "a preview environment's own tenancy"),
	}, nil
}

// ensureTenancy is the whole of making one: the shared server for the usage,
// the tenant's own password, and the ACL that admits it to its prefix and
// nothing else.
func (v *Valkey) ensureTenancy(ctx context.Context, username string, usage Usage) (Tenant, string, error) {
	if usage == "" {
		usage = UsageCache
	}
	if !usage.Known() {
		return Tenant{}, "", fmt.Errorf("%w: usage %q is not one of %s", ErrUnsatisfiable, usage, usageList())
	}
	server := SharedServerName(usage)
	keyspace, err := v.dialShared(ctx, server, usage)
	if err != nil {
		return Tenant{}, "", err
	}
	defer func() { _ = keyspace.Close() }()

	password, err := v.ensureTenantSecret(ctx, username)
	if err != nil {
		return Tenant{}, "", err
	}
	tenant := Tenant{Username: username, Password: password, Prefix: tenantPrefix(username)}
	if err := keyspace.EnsureTenant(ctx, tenant); err != nil {
		return Tenant{}, "", err
	}
	return tenant, server, nil
}

// dialShared brings the usage's server up if it is not up, and connects to
// it as its default user.
func (v *Valkey) dialShared(ctx context.Context, server string, usage Usage) (Keyspace, error) {
	cfg, err := v.sharedSettings(usage)
	if err != nil {
		return nil, err
	}
	password, err := v.ensureServer(ctx, server, cfg)
	if err != nil {
		return nil, err
	}
	return v.keyspaces()(v.address(server), password)
}

// dialExistingShared connects to a server that is already there, for the two
// operations that unmake a tenancy. A server that is gone is not an error:
// a claim whose finalizer cannot run blocks its project's teardown behind
// it, and there is nothing left to take back anyway.
func (v *Valkey) dialExistingShared(ctx context.Context, namespace, server string) (Keyspace, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: server}
	if err := v.Client.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	password := string(secret.Data[BindingKeyPassword])
	if password == "" {
		return nil, fmt.Errorf("the secret for %s holds no %s", server, BindingKeyPassword)
	}
	return v.keyspaces()(v.address(server), password)
}

// releaseTenancy removes the ACL user and leaves the keys. It is what a
// claim's release does under either deletionPolicy: the credential is the
// platform's to take back, the data is not.
func (v *Valkey) releaseTenancy(ctx context.Context, instanceID string) error {
	namespace, server, username, ok := parseSharedID(instanceID)
	if !ok {
		return fmt.Errorf("%q is not a tenancy id", instanceID)
	}
	keyspace, err := v.dialExistingShared(ctx, namespace, server)
	if err != nil || keyspace == nil {
		return err
	}
	defer func() { _ = keyspace.Close() }()
	return keyspace.RemoveTenant(ctx, username)
}

// destroyTenancy removes the ACL user and everything under its prefix, and
// touches no other tenant: the delete is by prefix, and the prefix is the
// only one this user could ever write under.
func (v *Valkey) destroyTenancy(ctx context.Context, instanceID string) error {
	namespace, server, username, ok := parseSharedID(instanceID)
	if !ok {
		return fmt.Errorf("%q is not a tenancy id", instanceID)
	}
	keyspace, err := v.dialExistingShared(ctx, namespace, server)
	if err != nil || keyspace == nil {
		return err
	}
	defer func() { _ = keyspace.Close() }()
	if err := keyspace.RemoveTenant(ctx, username); err != nil {
		return err
	}
	if err := keyspace.DeleteKeys(ctx, tenantPrefix(username)); err != nil {
		return err
	}
	// The password goes with the keys. Under Retain it is kept, and kept on
	// purpose: it is what lets a claim of the same name be granted the same
	// tenancy over what was retained. Nothing is being retained here.
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      tenantSecretName(username),
		Namespace: namespace,
	}}
	return client.IgnoreNotFound(v.Client.Delete(ctx, secret))
}

// ensureTenantSecret mints a tenant's password once and reads it back
// afterwards, the way an instance's is minted.
func (v *Valkey) ensureTenantSecret(ctx context.Context, username string) (string, error) {
	return v.ensureSecret(ctx, tenantSecretName(username))
}

// address is where the operator reaches a server in this cluster.
func (v *Valkey) address(server string) string {
	return fmt.Sprintf("%s.%s.svc:%d", server, v.Namespace, valkeyPort)
}

// tenantBinding is what the application connects with: the shared server's
// address, the tenant's own user and password, and the prefix its ACL admits
// it to.
func (v *Valkey) tenantBinding(server string, tenant Tenant) Binding {
	host := fmt.Sprintf("%s.%s.svc", server, v.Namespace)
	port := strconv.Itoa(valkeyPort)
	return Binding{
		URL: (&url.URL{
			Scheme: "redis",
			User:   url.UserPassword(tenant.Username, tenant.Password),
			Host:   host + ":" + port,
		}).String(),
		Host:      host,
		Port:      port,
		Password:  tenant.Password,
		Username:  tenant.Username,
		KeyPrefix: tenant.Prefix,
		// In-cluster and unencrypted, like every other in-cluster address
		// the platform hands out.
		TLS: false,
	}
}

// sharedNote is the sentence the claim records about its tenancy.
func sharedNote(server, prefix, why string) string {
	note := fmt.Sprintf("a tenancy in the platform's shared %s: an ACL user of its own admitted to keys and "+
		"channels under %q and to nothing else, on a server other projects also use", server, prefix)
	if why != "" {
		note += " — " + why
	}
	return note
}

// dedicatedNote is the same sentence for the other shape.
func dedicatedNote(usage Usage, why string) string {
	note := fmt.Sprintf("a %s server of this claim's own: its own process, its own memory and its own "+
		"failure domain", usage)
	if why != "" {
		note += " — " + why
	}
	return note
}

// usageOfServer reads a shared server's usage back off its name, for the
// branch operations, which are given an ID and nothing else.
func usageOfServer(server string) Usage {
	if server == sharedQueueServer {
		return UsageQueue
	}
	return UsageCache
}

// keyspaces is the factory this provisioner dials with.
func (v *Valkey) keyspaces() KeyspaceFactory {
	if v.Keyspaces != nil {
		return v.Keyspaces
	}
	return DialKeyspace
}

// sharedSettings is how a shared server is configured: the usage's eviction
// policy, the connection's own memory ceiling for the whole server, and the
// version the installation runs.
func (v *Valkey) sharedSettings(usage Usage) (settings, error) {
	cfg, err := v.resolve(Requirements{Usage: usage, MaxMemory: v.SharedMaxMemory})
	if err != nil {
		return settings{}, err
	}
	cfg.shared = true
	return cfg, nil
}

// aclInitScript writes the server's ACL file the first time it starts, with
// the default user's password in it, because a server pointed at an ACL file
// that is not there refuses to start at all.
//
// The default user is defined here rather than through requirepass because
// the two cannot both be used: an ACL file is the whole of the server's user
// list. Nothing in this script comes from a request — the password is the
// one this provisioner minted, read from the Secret through the environment
// — which is what makes a shell acceptable where the platform's install job
// refuses one.
const aclInitScript = `test -s "$ACL_FILE" || printf 'user default on >%s ~* &* +@all\n' "$VALKEY_PASSWORD" > "$ACL_FILE"`

// sharedInitContainer is that script as the pod runs it.
func sharedInitContainer(name string, cfg settings) corev1.Container {
	return corev1.Container{
		Name:    "acl-init",
		Image:   cfg.image,
		Command: []string{"sh", "-c", aclInitScript},
		Env: []corev1.EnvVar{
			{Name: "ACL_FILE", Value: aclFilePath},
			{
				Name: "VALKEY_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
					Key:                  BindingKeyPassword,
				}},
			},
		},
		SecurityContext: containerSecurityContext(),
		VolumeMounts:    volumeMounts(),
	}
}

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
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fakeKeyspace is a running server as this package talks to it: the ACL
// users it has been told about, and the keys somebody put in it. It is the
// same seam objectstore's AdminAPI is, and for the same reason — what the
// provisioner does is then tested without a server.
type fakeKeyspace struct {
	tenants map[string]Tenant
	keys    map[string]string
	closed  bool
}

func newFakeKeyspace() *fakeKeyspace {
	return &fakeKeyspace{tenants: map[string]Tenant{}, keys: map[string]string{}}
}

func (k *fakeKeyspace) EnsureTenant(_ context.Context, tenant Tenant) error {
	k.tenants[tenant.Username] = tenant
	return nil
}

func (k *fakeKeyspace) RemoveTenant(_ context.Context, username string) error {
	delete(k.tenants, username)
	return nil
}

func (k *fakeKeyspace) DeleteKeys(_ context.Context, prefix string) error {
	for key := range k.keys {
		if strings.HasPrefix(key, prefix) {
			delete(k.keys, key)
		}
	}
	return nil
}

func (k *fakeKeyspace) Close() error {
	k.closed = true
	return nil
}

// servers is every shared server the provisioner dialled, by address.
type servers map[string]*fakeKeyspace

func (s servers) factory() KeyspaceFactory {
	return func(address, _ string) (Keyspace, error) {
		if s[address] == nil {
			s[address] = newFakeKeyspace()
		}
		return s[address], nil
	}
}

// only is the one server the test expects to exist, and fails when there are
// two — a shape whose whole point is not costing a pod per claim should be
// asserted on the number of pods.
func (s servers) only(t *testing.T) *fakeKeyspace {
	t.Helper()
	if len(s) != 1 {
		t.Fatalf("want one shared server, got %d: %v", len(s), keysOf(s))
	}
	for _, keyspace := range s {
		return keyspace
	}
	return nil
}

func keysOf(s servers) []string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	return names
}

func newShared(t *testing.T) (*Valkey, client.Client, servers) {
	t.Helper()
	provisioner, c := newValkey(t)
	dialled := servers{}
	provisioner.Keyspaces = dialled.factory()
	return provisioner, c, dialled
}

// bind provisions until the server it needs is serving: the call that
// creates one answers ErrNotReady, and a claim joining a server that is
// already up binds on the first.
func bind(t *testing.T, provisioner *Valkey, name string, req Requirements) Instance {
	t.Helper()
	ctx := context.Background()
	instance, err := provisioner.ProvisionWith(ctx, name, req)
	if err == nil {
		return instance
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady while the server starts, got %v", err)
	}
	usage := req.Usage
	if usage == "" {
		usage = UsageCache
	}
	ready(t, provisioner.Client, provisioner.Namespace, SharedServerName(usage))
	instance, err = provisioner.ProvisionWith(ctx, name, req)
	if err != nil {
		t.Fatal(err)
	}
	return instance
}

// The default is a tenancy, because a server per claim per environment does
// not fit on the single-node clusters this platform supports. A claim that
// asked for nothing in particular costs the cluster no pod of its own.
func TestAClaimThatAsksForNothingSharesAServer(t *testing.T) {
	provisioner, c, dialled := newShared(t)

	instance := bind(t, provisioner, "kitchen-shop-cache", Requirements{})

	if instance.Tenancy != TenancyShared {
		t.Fatalf("want a tenancy by default, got %q", instance.Tenancy)
	}
	if instance.Binding.Username != "kitchen-shop-cache" {
		t.Errorf("a tenant authenticates as its own user, got %q", instance.Binding.Username)
	}
	if instance.Binding.KeyPrefix != "kitchen-shop-cache:" {
		t.Errorf("the binding must carry the prefix the ACL admits, got %q", instance.Binding.KeyPrefix)
	}
	if !strings.Contains(instance.Binding.URL, instance.Binding.Username+":") {
		t.Errorf("the url must authenticate as the tenant, got %s", instance.Binding.URL)
	}
	if instance.Provenance != ProvenanceProduction {
		t.Errorf("the claim's own tenancy holds production's data: %q", instance.Provenance)
	}

	// One server, no instance of the claim's own beside it.
	dialled.only(t)
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: DefaultCacheNamespace, Name: "kitchen-shop-cache",
	}, &appsv1.StatefulSet{}); err == nil {
		t.Error("a shared claim must not also get a server of its own")
	}
}

// A tenancy is an ACL user admitted to its own prefix and to nothing else,
// and refused the commands that reach past one. This is the boundary the
// whole shape rests on, so it is asserted on the rules themselves.
func TestATenancyIsConfinedToItsPrefix(t *testing.T) {
	rules := strings.Join(aclRules(Tenant{Username: "kitchen-shop", Password: "secret", Prefix: "kitchen-shop:"}), " ")

	for _, want := range []string{"reset", "on", ">secret", "~kitchen-shop:*", "&kitchen-shop:*", "-@dangerous", "-@admin"} {
		if !strings.Contains(rules, want) {
			t.Errorf("a tenancy must be granted %q: %s", want, rules)
		}
	}
	// FLUSHALL from one tenant emptying another's keyspace is the objection
	// a logical database number could not answer, and -@dangerous is the
	// answer: it is not a command this user has.
	if !strings.Contains(rules, "-@dangerous") {
		t.Errorf("a tenant must not be able to flush the server: %s", rules)
	}
}

// Two claims in one server are two prefixes and two users, and neither can
// address the other's keys.
func TestTwoClaimsShareAServerAndNotAKeyspace(t *testing.T) {
	provisioner, _, dialled := newShared(t)

	first := bind(t, provisioner, "kitchen-shop-cache", Requirements{})
	second := bind(t, provisioner, "kitchen-blog-cache", Requirements{})

	server := dialled.only(t)
	if len(server.tenants) != 2 {
		t.Fatalf("want two tenancies in the one server, got %d", len(server.tenants))
	}
	if first.Binding.KeyPrefix == second.Binding.KeyPrefix {
		t.Fatal("two claims got one keyspace")
	}
	if first.Binding.Password == second.Binding.Password {
		t.Fatal("two claims got one credential")
	}
	for name, tenant := range server.tenants {
		if !strings.HasPrefix(tenant.Prefix, name) {
			t.Errorf("%s is admitted to %q, which is not its own", name, tenant.Prefix)
		}
	}
}

// The usage picks the server rather than being given up on: maxmemory-policy
// is server-wide, so a cache and a queue join different servers and each
// gets the policy it needs.
func TestUsagePicksTheSharedServer(t *testing.T) {
	provisioner, c, dialled := newShared(t)

	cache := bind(t, provisioner, "kitchen-shop-cache", Requirements{Usage: UsageCache})
	queue := bind(t, provisioner, "kitchen-shop-jobs", Requirements{Usage: UsageQueue})

	if cache.Binding.Host == queue.Binding.Host {
		t.Fatal("a cache and a queue cannot share a server: maxmemory-policy is server-wide")
	}
	if len(dialled) != 2 {
		t.Fatalf("want one server per usage, got %d", len(dialled))
	}

	for _, testCase := range []struct {
		server string
		wants  string
	}{
		{sharedCacheServer, "allkeys-lru"},
		{sharedQueueServer, "noeviction"},
	} {
		set := &appsv1.StatefulSet{}
		key := types.NamespacedName{Namespace: DefaultCacheNamespace, Name: testCase.server}
		if err := c.Get(context.Background(), key, set); err != nil {
			t.Fatal(err)
		}
		args := strings.Join(set.Spec.Template.Spec.Containers[0].Args, " ")
		if !strings.Contains(args, testCase.wants) {
			t.Errorf("%s must be started with %q: %s", testCase.server, testCase.wants, args)
		}
		// Every shared server keeps its users on a volume: ACL SETUSER
		// lives in memory until it is saved, and a server that forgot its
		// tenants would leave every application on it unable to sign in.
		if !strings.Contains(args, aclFilePath) {
			t.Errorf("%s must keep its users in an ACL file: %s", testCase.server, args)
		}
		if len(set.Spec.VolumeClaimTemplates) == 0 {
			t.Errorf("%s must keep that file on a volume", testCase.server)
		}
		if len(set.Spec.Template.Spec.InitContainers) == 0 {
			t.Errorf("%s must be given an ACL file before it starts, or it refuses to", testCase.server)
		}
	}
}

// A memory limit is the whole server's, so a claim that names one is given a
// server of its own — and told that is why.
func TestAMemoryLimitAsksForAServerOfItsOwn(t *testing.T) {
	provisioner, _, _ := newShared(t)

	tenancy, why, err := provisioner.ResolveTenancy(Requirements{MaxMemory: "512Mi"})
	if err != nil {
		t.Fatal(err)
	}
	if tenancy != TenancyDedicated {
		t.Fatalf("want a server of its own for a memory limit, got %q", tenancy)
	}
	if !strings.Contains(why, "memory limit") {
		t.Errorf("the reason must name what could not be shared: %q", why)
	}
}

// A claim that insists on sharing and asks for something a shared server
// cannot give is refused as a claim, rather than quietly given the other
// shape.
func TestSharingAndAMemoryLimitIsRefused(t *testing.T) {
	provisioner, _, _ := newShared(t)

	_, _, err := provisioner.ResolveTenancy(Requirements{Tenancy: TenancyShared, MaxMemory: "512Mi"})
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("want ErrUnsatisfiable, got %v", err)
	}
	if !strings.Contains(err.Error(), string(TenancyDedicated)) {
		t.Errorf("the refusal must name the shape that would work: %v", err)
	}
}

// A preview gets a tenancy of its own in the same server: its own user, its
// own prefix, none of production's keys — isolated and empty, and it says
// which.
func TestAPreviewGetsItsOwnTenancy(t *testing.T) {
	provisioner, _, dialled := newShared(t)
	ctx := context.Background()

	instance := bind(t, provisioner, "kitchen-shop-cache", Requirements{})
	branch, err := provisioner.CreateBranch(ctx, instance.ID, "pr-7")
	if err != nil {
		t.Fatal(err)
	}

	if branch.Provenance != ProvenanceSynthetic {
		t.Errorf("a preview's tenancy is empty and says so: %q", branch.Provenance)
	}
	if branch.Tenancy != TenancyShared {
		t.Errorf("a preview of a tenancy is a tenancy: %q", branch.Tenancy)
	}
	if branch.Binding.KeyPrefix == instance.Binding.KeyPrefix {
		t.Fatal("a preview must not share production's keyspace")
	}
	if branch.Binding.Password == instance.Binding.Password {
		t.Fatal("a preview must not share production's credential")
	}
	// And it costs no second server.
	if len(dialled) != 1 {
		t.Fatalf("a preview must not cost a server, got %d", len(dialled))
	}
}

// Releasing a claim takes back the access and leaves the data, and touches
// no other tenant. Destroying it is what deletionPolicy Delete opts into.
func TestReleaseKeepsTheDataAndDeleteDoesNot(t *testing.T) {
	provisioner, _, dialled := newShared(t)
	ctx := context.Background()

	kept := bind(t, provisioner, "kitchen-shop-cache", Requirements{})
	neighbour := bind(t, provisioner, "kitchen-blog-cache", Requirements{})
	server := dialled.only(t)
	server.keys["kitchen-shop-cache:sessions"] = "held"
	server.keys["kitchen-blog-cache:sessions"] = "held"

	if err := provisioner.Release(ctx, kept.ID); err != nil {
		t.Fatal(err)
	}
	if _, live := server.tenants["kitchen-shop-cache"]; live {
		t.Error("a released claim must not leave a working credential in a shared server")
	}
	if _, held := server.keys["kitchen-shop-cache:sessions"]; !held {
		t.Error("releasing a claim must not destroy what it held: that is what Delete is for")
	}
	if _, held := server.keys["kitchen-blog-cache:sessions"]; !held {
		t.Error("releasing one claim must not touch another tenant")
	}

	if err := provisioner.Deprovision(ctx, kept.ID); err != nil {
		t.Fatal(err)
	}
	if _, held := server.keys["kitchen-shop-cache:sessions"]; held {
		t.Error("Delete destroys the keys under the claim's prefix")
	}
	if _, held := server.keys["kitchen-blog-cache:sessions"]; !held {
		t.Error("destroying one tenancy must touch no other tenant")
	}
	if _, live := server.tenants[neighbour.Binding.Username]; !live {
		t.Error("the neighbour's tenancy must survive")
	}
}

// A retained tenancy keeps its password, which is what lets a claim of the
// same name be granted the same user over the keys that were retained. A
// destroyed one does not.
func TestRetainKeepsTheTenantPasswordAndDeleteDoesNot(t *testing.T) {
	provisioner, c, _ := newShared(t)
	ctx := context.Background()

	instance := bind(t, provisioner, "kitchen-shop-cache", Requirements{})
	key := types.NamespacedName{
		Namespace: DefaultCacheNamespace,
		Name:      tenantSecretName("kitchen-shop-cache"),
	}

	if err := provisioner.Release(ctx, instance.ID); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, key, secret); err != nil {
		t.Fatalf("a retained tenancy must keep its password: %v", err)
	}
	again := bind(t, provisioner, "kitchen-shop-cache", Requirements{})
	if again.Binding.Password != instance.Binding.Password {
		t.Error("a claim recreated over retained keys must be granted the same password")
	}

	if err := provisioner.Deprovision(ctx, instance.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &corev1.Secret{}); err == nil {
		t.Error("a destroyed tenancy must not leave its password behind")
	}
}

// A finalizer that cannot run blocks its project's teardown behind it, so a
// server that is already gone is not an error.
func TestReleasingATenancyWhoseServerIsGoneSucceeds(t *testing.T) {
	provisioner, _, _ := newShared(t)
	id := sharedID(DefaultCacheNamespace, sharedCacheServer, "kitchen-shop-cache")

	if err := provisioner.Release(context.Background(), id); err != nil {
		t.Fatalf("releasing a tenancy in a server that is gone must succeed: %v", err)
	}
	if err := provisioner.Deprovision(context.Background(), id); err != nil {
		t.Fatalf("destroying a tenancy in a server that is gone must succeed: %v", err)
	}
}

// A server of one claim's own is released by doing nothing to it: there is
// no credential the platform minted into somebody else's server to take
// back, and Retain has to keep the instance.
func TestReleasingAServerOfItsOwnDoesNothing(t *testing.T) {
	provisioner, c, _ := newShared(t)
	ctx := context.Background()

	dedicated := Requirements{Tenancy: TenancyDedicated}
	if _, err := provisioner.ProvisionWith(ctx, "kitchen-shop-cache", dedicated); !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady, got %v", err)
	}
	if err := provisioner.Release(ctx, DefaultCacheNamespace+"/kitchen-shop-cache"); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: DefaultCacheNamespace, Name: "kitchen-shop-cache"}
	if err := c.Get(ctx, key, &appsv1.StatefulSet{}); err != nil {
		t.Errorf("releasing a retained instance must leave it running: %v", err)
	}
}

// A preview's tenancy is spelled with a separator a claim name cannot
// contain, so it can never be the same user as some other claim's.
func TestAPreviewTenancyCannotCollideWithAClaim(t *testing.T) {
	branch := tenantBranchName("kitchen-shop", "pr-7")
	if !strings.Contains(branch, ".") {
		t.Fatalf("a preview's tenancy must be told apart from a claim called shop-pr-7: %q", branch)
	}
	if branch == resourceName("kitchen-shop-pr-7") {
		t.Fatalf("a preview's tenancy collided with a claim's: %q", branch)
	}
}
